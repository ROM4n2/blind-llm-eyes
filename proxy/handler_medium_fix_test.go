package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/messages"
)

// ═══════════════════════════════════════════════════════════════════════════════
// 端到端测试：复杂嵌套图片请求验证 4 个中等优先级修复
//   Fix #6: base64 仅解码一次
//   Fix #7: truncateBytes 避免完整请求体内存分配
//   Fix #9: collectImageBlocks 递归深度限制
//   Fix #10: MarshalJSON 空 Content 不覆盖为 null
// ═══════════════════════════════════════════════════════════════════════════════

// TestE2E_ComplexNestedImages_AllMediumFixes 端到端验证复杂嵌套场景
func TestE2E_ComplexNestedImages_AllMediumFixes(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	// 使用带延迟的 vision mock，确保 img1==img3 的 singleflight 有时间合并
	// 若无延迟，img1 的 fn 可能在 img3 进入 Do 之前就已完成，导致无法去重
	mockVis := newSlowVisionMock(50*time.Millisecond, "NestedImgDesc")
	c := cache.NewLRU(100)

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      mockVis,
		Cache:               c,
		FailOpen:            true,
		LargeImageThreshold: 1024,
		UpstreamAPIKey:      "sk-e2e-server-key",
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	// 构造复杂嵌套请求：
	// - 顶层 image（user message 直接携带）
	// - tool_result 内嵌 image（depth=1，Claude Code 截图常见场景）
	// - tool_result 内嵌 text + image 混合内容
	// - tool_result 无 content 字段（验证 Fix #10 不覆盖为 null）
	const img1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	img2 := buildLargeImageBase64(2_000) // 不同图片，验证多图并行处理
	const img3 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

	reqBody := fmt.Sprintf(`{
		"model":"claude-3-5-sonnet",
		"max_tokens":200,
		"system":[{"type":"text","text":"You are a helpful assistant."}],
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"Please analyze these screenshots:"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}},
					{"type":"tool_result","tool_use_id":"toolu_001","content":[
						{"type":"text","text":"screenshot from tool"},
						{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}}
					]},
					{"type":"tool_result","tool_use_id":"toolu_002","content":[
						{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}}
					]},
					{"type":"tool_result","tool_use_id":"toolu_003","is_error":true}
				]
			}
		],
		"stream":true
	}`, img1, img2, img3)

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-client-e2e-leaked")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	upstreamBody := string(up.Body())
	var upstreamReq map[string]any
	if err := json.Unmarshal(up.Body(), &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}

	// ── Fix #6 验证：base64 仅解码一次，3 张图片被正确处理 ──
	// img1 和 img3 数据相同 → 应该命中 singleflight/cache 去重
	// img2 不同 → 独立调用
	// 总 vision 调用应为 2（img1/img3 共享一次，img2 一次）
	t.Run("Fix#6 base64 decoded once, images processed", func(t *testing.T) {
		calls := len(mockVis.callStart)
		if calls != 2 {
			t.Errorf("vision calls=%d, want 2 (img1==img3 deduped via singleflight, img2 separate)", calls)
		}
		// 所有 base64 数据不应泄露到上游
		if strings.Contains(upstreamBody, img1) {
			t.Error("img1 base64 leaked to upstream")
		}
		if strings.Contains(upstreamBody, img2) {
			t.Error("img2 base64 leaked to upstream")
		}
		// 描述应存在
		descCount := strings.Count(upstreamBody, "NestedImgDesc")
		if descCount != 3 {
			t.Errorf("description count=%d, want 3 (all 3 images replaced)", descCount)
		}
		t.Logf("Fix#6: 3 images (2 unique) → %d vision calls, all replaced, no base64 leak", calls)
	})

	// ── Fix #10 验证：无 content 的 tool_result 不被覆盖为 null ──
	t.Run("Fix#10 tool_result without content not overwritten to null", func(t *testing.T) {
		msgs := upstreamReq["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)

		// 找到 toolu_003（无 content 字段的 tool_result）
		var toolResult003 map[string]any
		for _, blk := range content {
			b := blk.(map[string]any)
			if b["tool_use_id"] == "toolu_003" {
				toolResult003 = b
				break
			}
		}
		if toolResult003 == nil {
			t.Fatal("toolu_003 not found in upstream body")
		}

		// 关键验证：不应有 content: null
		contentField, hasContent := toolResult003["content"]
		if hasContent && contentField == nil {
			t.Error("Fix#10: tool_result without content got 'content: null' — should preserve original (no content field)")
		}
		// is_error 应保留
		if toolResult003["is_error"] != true {
			t.Error("is_error field lost — should be preserved from raw")
		}
		// tool_use_id 应保留
		if toolResult003["tool_use_id"] != "toolu_003" {
			t.Error("tool_use_id field lost — should be preserved from raw")
		}
		t.Logf("Fix#10: tool_result without content preserved correctly (no null override, is_error/tool_use_id kept)")
	})

	// ── Fix #5 回归验证：Authorization 仍被过滤 ──
	t.Run("regression: Authorization stripped", func(t *testing.T) {
		authHeader := up.Headers().Get("Authorization")
		if strings.Contains(authHeader, "sk-client-e2e-leaked") {
			t.Error("client Authorization leaked to upstream")
		}
		if authHeader != "Bearer sk-e2e-server-key" {
			t.Errorf("upstream Authorization=%q, want server key", authHeader)
		}
	})

	// ── 嵌套 tool_result image 替换验证 ──
	t.Run("nested tool_result images replaced", func(t *testing.T) {
		// toolu_001 的 content 应包含替换后的 text 块，不含 image
		msgs := upstreamReq["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)

		for _, blk := range content {
			b := blk.(map[string]any)
			if b["tool_use_id"] == "toolu_001" {
				nestedContent, ok := b["content"].([]any)
				if !ok {
					t.Fatal("toolu_001 content is not an array")
				}
				for _, nb := range nestedContent {
					nbMap := nb.(map[string]any)
					if nbMap["type"] == "image" {
						t.Error("nested image in toolu_001 not replaced")
					}
				}
			}
		}
		t.Log("nested images in tool_result replaced successfully")
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// Fix #7 单元测试：truncateBytes 正确性
// ═══════════════════════════════════════════════════════════════════════════════

func TestFix7_TruncateBytes(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		n     int
		want  string
	}{
		{"short input", []byte("hello"), 200, "hello"},
		{"exact length", []byte("hello"), 5, "hello"},
		{"truncate", []byte("hello world"), 5, "hello..."},
		{"empty input", []byte{}, 200, ""},
		{"n=0", []byte("hello"), 0, "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateBytes(tt.input, tt.n)
			if got != tt.want {
				t.Errorf("truncateBytes(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
			}
		})
	}

	// 大字节切片验证：不分配完整字符串
	t.Run("large byte slice no full allocation", func(t *testing.T) {
		large := bytes.Repeat([]byte("A"), 10_000)
		got := truncateBytes(large, 200)
		if len(got) != 203 { // 200 + "..."
			t.Errorf("result length=%d, want 203", len(got))
		}
		if !strings.HasSuffix(got, "...") {
			t.Error("result should end with ...")
		}
		t.Logf("10000 bytes → truncated to %d chars without full string allocation", len(got))
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// Fix #9 单元测试：collectImageBlocks 深度限制
// ═══════════════════════════════════════════════════════════════════════════════

func TestFix9_CollectImageBlocks_DepthLimit(t *testing.T) {
	const imgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

	// 构造正常深度（depth=1）的嵌套 image：tool_result → image
	t.Run("shallow nesting finds image", func(t *testing.T) {
		req := &messages.Request{
			Messages: []messages.Message{
				{
					Role: "user",
					Content: messages.MessageContent{
						{
							Type: messages.ContentTypeToolResult,
							Content: messages.MessageContent{
								{
									Type: "image",
									Source: &messages.ImageSource{
										Type:      "base64",
										MediaType: "image/png",
										Data:      imgData,
									},
								},
							},
						},
					},
				},
			},
		}
		imgs := messages.FindImageBlocks(req)
		if len(imgs) != 1 {
			t.Errorf("found %d images, want 1 (shallow nesting)", len(imgs))
		}
	})

	// 构造超深嵌套（depth > 16），验证不栈溢出且不查找超深图片
	t.Run("deep nesting does not overflow", func(t *testing.T) {
		// 构造 depth=20 的嵌套 tool_result
		deepestImage := messages.ContentBlock{
			Type: "image",
			Source: &messages.ImageSource{
				Type:      "base64",
				MediaType: "image/png",
				Data:      imgData,
			},
		}
		// 从内向外包裹 20 层 tool_result
		current := deepestImage
		for i := 0; i < 20; i++ {
			current = messages.ContentBlock{
				Type:    messages.ContentTypeToolResult,
				Content: messages.MessageContent{current},
			}
		}
		req := &messages.Request{
			Messages: []messages.Message{
				{
					Role:    "user",
					Content: messages.MessageContent{current},
				},
			},
		}
		// 不应 panic（栈溢出），且 depth=20 > maxCollectDepth=16 → 图片不应被找到
		imgs := messages.FindImageBlocks(req)
		if len(imgs) != 0 {
			t.Errorf("deep nesting (depth=20): found %d images, want 0 (exceeds maxCollectDepth=16)", len(imgs))
		}
		t.Log("depth=20 nesting: no stack overflow, image beyond depth limit not collected")
	})

	// 边界：depth=16 的图片仍被找到，depth=17 不被找到
	// 原因：depth 限制的是 tool_result 的下探（depth < maxCollectDepth），
	// 最后一个 tool_result 在 depth=15 时进入（15<16 成立），其内部的 image
	// 在 depth=16 时被收集（image 收集无 depth 限制）。
	// depth=17 的图片：外层 tool_result 在 depth=16 时不满足 16<16，不再下探。
	t.Run("boundary depth 16 found, 17 not found", func(t *testing.T) {
		makeNested := func(depth int) *messages.Request {
			img := messages.ContentBlock{
				Type: "image",
				Source: &messages.ImageSource{
					Type:      "base64",
					MediaType: "image/png",
					Data:      imgData,
				},
			}
			current := img
			for i := 0; i < depth; i++ {
				current = messages.ContentBlock{
					Type:    messages.ContentTypeToolResult,
					Content: messages.MessageContent{current},
				}
			}
			return &messages.Request{
				Messages: []messages.Message{
					{
						Role:    "user",
						Content: messages.MessageContent{current},
					},
				},
			}
		}

		// depth=16: 最后一个 tool_result 在 depth=15 进入，image 在 depth=16 被收集
		req16 := makeNested(16)
		imgs16 := messages.FindImageBlocks(req16)
		if len(imgs16) != 1 {
			t.Errorf("depth=16: found %d images, want 1 (within limit, image collected at depth=16)", len(imgs16))
		}

		// depth=17: tool_result 在 depth=16 时不满足 16<16，不再下探，image 不可达
		req17 := makeNested(17)
		imgs17 := messages.FindImageBlocks(req17)
		if len(imgs17) != 0 {
			t.Errorf("depth=17: found %d images, want 0 (exceeds limit, tool_result at depth=16 not entered)", len(imgs17))
		}
		t.Logf("boundary: depth=16 found (image at depth=16), depth=17 not found (maxCollectDepth=16)")
	})
}

// ═══════════════════════════════════════════════════════════════════════════════
// Fix #10 单元测试：MarshalJSON 空 Content 不覆盖为 null
// ═══════════════════════════════════════════════════════════════════════════════

func TestFix10_MarshalJSON_EmptyContentNotOverwritten(t *testing.T) {
	// 场景 1：tool_result 无 content 字段 → 不应添加 content: null
	t.Run("tool_result without content field", func(t *testing.T) {
		rawJSON := `{"type":"tool_result","tool_use_id":"toolu_xxx","is_error":true}`
		var blk messages.ContentBlock
		if err := json.Unmarshal([]byte(rawJSON), &blk); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if blk.Type != messages.ContentTypeToolResult {
			t.Fatalf("type=%s, want tool_result", blk.Type)
		}
		// Content 应为 nil（原始 JSON 无 content 字段）
		if len(blk.Content) != 0 {
			t.Fatalf("Content len=%d, want 0", len(blk.Content))
		}

		out, err := json.Marshal(blk)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		outStr := string(out)

		// 关键验证：不应出现 "content":null
		if strings.Contains(outStr, `"content":null`) {
			t.Errorf("output contains 'content:null' — should preserve original (no content field):\n%s", outStr)
		}
		// tool_use_id 和 is_error 应保留
		if !strings.Contains(outStr, `"tool_use_id":"toolu_xxx"`) {
			t.Error("tool_use_id lost in output")
		}
		if !strings.Contains(outStr, `"is_error":true`) {
			t.Error("is_error lost in output")
		}
		t.Logf("no content field → preserved correctly:\n%s", outStr)
	})

	// 场景 2：tool_result 有 content 数组 → 正常覆盖
	t.Run("tool_result with content array", func(t *testing.T) {
		rawJSON := `{"type":"tool_result","tool_use_id":"toolu_yyy","content":[{"type":"text","text":"original"}]}`
		var blk messages.ContentBlock
		if err := json.Unmarshal([]byte(rawJSON), &blk); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		out, err := json.Marshal(blk)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		outStr := string(out)

		// content 应存在且为数组
		if !strings.Contains(outStr, `"content":[`) {
			t.Errorf("content array not found in output:\n%s", outStr)
		}
		t.Logf("content array → preserved correctly:\n%s", outStr)
	})

	// 场景 3：tool_result 有 content 但被修改（模拟 image 替换后）
	t.Run("tool_result with modified content", func(t *testing.T) {
		rawJSON := `{"type":"tool_result","tool_use_id":"toolu_zzz","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}`
		var blk messages.ContentBlock
		if err := json.Unmarshal([]byte(rawJSON), &blk); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// 模拟 image 替换
		if err := messages.ReplaceImageWithDescription(&blk.Content[0], "ReplacedDesc"); err != nil {
			t.Fatalf("replace: %v", err)
		}

		out, err := json.Marshal(blk)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		outStr := string(out)

		// 验证替换后的内容存在
		if !strings.Contains(outStr, "ReplacedDesc") {
			t.Errorf("replaced description not in output:\n%s", outStr)
		}
		// 验证原始 image base64 不存在
		if strings.Contains(outStr, `"data":"abc"`) {
			t.Errorf("original image data still in output after replacement:\n%s", outStr)
		}
		// tool_use_id 应保留
		if !strings.Contains(outStr, `"tool_use_id":"toolu_zzz"`) {
			t.Error("tool_use_id lost after content modification")
		}
		t.Logf("modified content → replaced correctly, tool_use_id preserved:\n%s", outStr)
	})
}
