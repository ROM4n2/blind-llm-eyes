package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
	"github.com/ROM4n2/blind-llm-eyes/vision"
	dto "github.com/prometheus/client_model/go"
)

// mockVisionProvider 实现 vision.VisionProvider 接口，用于测试。
type mockVisionProvider struct {
	calls int
	desc  string
}

func (m *mockVisionProvider) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	m.calls++
	return m.desc, nil
}

// 假上游：收到请求后把 body 原样回显
func fakeUpstream(_ *testing.T, gotBody *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestHandler_ImageReplaceAndCache(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	// mock vision
	mockVis := &mockVisionProvider{desc: "MockDesc-A"}

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	// ==== 第 1 次请求：缓存 miss，应该调 vision ====
	rr1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer sk-test-upstream")
	h.ServeHTTP(rr1, req1)

	if rr1.Code != 200 {
		t.Fatalf("1st status: %d body=%s", rr1.Code, rr1.Body.String())
	}
	if mockVis.calls != 1 {
		t.Errorf("1st vision calls: want 1, got %d", mockVis.calls)
	}
	if hdr := rr1.Header().Get("X-Blind-Llm-Eyes"); !strings.Contains(hdr, "1 rewritten") {
		t.Errorf("1st header wrong: %q", hdr)
	}
	// 断言上游收到的 body 里：image 块没了，出现 MockDesc-A
	var upstreamReq map[string]any
	json.Unmarshal(upstreamGot, &upstreamReq)
	msgs := upstreamReq["messages"].([]any)
	firstMsg := msgs[0].(map[string]any)
	content := firstMsg["content"].([]any)
	secondBlock := content[1].(map[string]any)
	if secondBlock["type"] != "text" {
		t.Errorf("1st upstream: 2nd block not text, type=%v", secondBlock["type"])
	}
	if !strings.Contains(secondBlock["text"].(string), "MockDesc-A") {
		t.Errorf("1st upstream: desc missing in text=%v", secondBlock["text"])
	}

	// ==== 第 2 次请求：同一图，缓存 hit，visionCalls 不应增加 ====
	rr2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr2, req2)

	if mockVis.calls != 1 {
		t.Errorf("2nd vision calls: still want 1, got %d (cache not hit!)", mockVis.calls)
	}
	hdr := rr2.Header().Get("X-Blind-Llm-Eyes")
	if !strings.Contains(hdr, "1 cached") {
		t.Errorf("2nd header want 1 cached, got %q", hdr)
	}
}

func TestHandler_MockVisionProvider_Interface(t *testing.T) {
	// 验证 mock 实现确实满足 VisionProvider 接口
	var _ vision.VisionProvider = (*mockVisionProvider)(nil)

	mock := &mockVisionProvider{desc: "hello"}
	ctx := context.Background()
	desc, err := mock.DescribeImage(ctx, "base64data", "image/png", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desc != "hello" {
		t.Fatalf("want 'hello', got %q", desc)
	}
	if mock.calls != 1 {
		t.Fatalf("want 1 call, got %d", mock.calls)
	}
}

func TestHandler_GracefulShutdown_WaitGroup(t *testing.T) {
	var wg sync.WaitGroup

	// 模拟在途请求
	wg.Add(1)
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		close(done)
	}()

	// 等待 goroutine 完成
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("test goroutine timed out")
	}

	// Shutdown 应该立即返回（没有在途请求）
	wg.Wait() // 应立即返回
}

func TestHandler_MetricsIntegration(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "MockDesc-Metrics"}
	m := metrics.NewMetrics()

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Metrics:             m,
	}
	h := NewHandler(deps)

	reqBody := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	// 第 1 次：cache miss → vision 调用
	req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req1.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req1)

	// 第 2 次：cache hit
	req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req2)

	// 第 3 次：不同方法 → 405
	req3, _ := http.NewRequest("GET", "/v1/messages", nil)
	h.ServeHTTP(httptest.NewRecorder(), req3)

	// 验证 metrics
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	// 收集计数器值
	counterValues := make(map[string]float64)
	for _, mf := range families {
		for _, metric := range mf.GetMetric() {
			labels := metric.GetLabel()
			key := mf.GetName() + "|" + labelKey(labels)
			if mf.GetName() == "blind_llm_eyes_http_requests_total" {
				counterValues[key] = metric.GetCounter().GetValue()
			}
		}
	}

	// 验证 POST /v1/messages 200 出现了 2 次
	found := false
	for k, v := range counterValues {
		if strings.Contains(k, "http_requests_total") && strings.Contains(k, "POST") && strings.Contains(k, "200") {
			if v == 2 {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected 2 POST/200 requests in metrics")
	}

	// 验证 GET /v1/messages 405 出现了 1 次
	found405 := false
	for k, v := range counterValues {
		if strings.Contains(k, "http_requests_total") && strings.Contains(k, "GET") && strings.Contains(k, "405") {
			if v == 1 {
				found405 = true
			}
		}
	}
	if !found405 {
		t.Error("expected 1 GET/405 request in metrics")
	}

	// 验证图片处理指标
	imgMap := make(map[string]float64)
	for _, mf := range families {
		if mf.GetName() == "blind_llm_eyes_images_processed_total" {
			for _, metric := range mf.GetMetric() {
				labels := metric.GetLabel()
				outcome := ""
				for _, l := range labels {
					if l.GetName() == "outcome" {
						outcome = l.GetValue()
					}
				}
				imgMap[outcome] = metric.GetCounter().GetValue()
			}
		}
	}
	if imgMap["rewritten"] != 1 {
		t.Errorf("images rewritten: want 1, got %f", imgMap["rewritten"])
	}
	if imgMap["cached"] != 1 {
		t.Errorf("images cached: want 1, got %f", imgMap["cached"])
	}
}

func labelKey(labels []*dto.LabelPair) string {
	var parts []string
	for _, l := range labels {
		parts = append(parts, l.GetName()+"="+l.GetValue())
	}
	return strings.Join(parts, ",")
}

func TestHandler_ValidationErrors(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      &mockVisionProvider{desc: "test"},
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	// 缺少 model → 400
	t.Run("missing model", func(t *testing.T) {
		reqBody := `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("want 400 for missing model, got %d", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "validation failed") {
			t.Errorf("want validation error, got: %s", rr.Body.String())
		}
	})

	// 无效 role → 400
	t.Run("invalid role", func(t *testing.T) {
		reqBody := `{"model":"test","messages":[{"role":"system","content":[{"type":"text","text":"hi"}]}]}`
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("want 400 for invalid role, got %d", rr.Code)
		}
	})

	// 无效媒体类型 → 400
	t.Run("invalid media type", func(t *testing.T) {
		reqBody := `{"model":"test","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/bmp","data":"abc"}}]}]}`
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("want 400 for invalid media type, got %d", rr.Code)
		}
	})

	// data: 前缀 → 400
	t.Run("data prefix in base64", func(t *testing.T) {
		reqBody := `{"model":"test","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"data:image/png;base64,abc"}}]}]}`
		rr := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("want 400 for data: prefix, got %d", rr.Code)
		}
	})
}

func TestHandler_BodyTooLarge_Returns413(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      &mockVisionProvider{desc: "x"},
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		MaxBodyBytes:        1024, // 1KB，便于测试
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	big := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"` +
		strings.Repeat("A", 2048) + `"}]}]}`
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(big))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandler_BodyTooLarge_Stream_Returns413(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      &mockVisionProvider{desc: "x"},
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		MaxBodyBytes:        1024,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	big := `{"model":"m","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"` +
		strings.Repeat("A", 2048) + `"}]}]}`
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(big))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 for stream request, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandler_LogOutput_NestedToolResultWithLargeImage 构造包含嵌套 tool_result 和大图的 mock 请求，
// 验证核心分支日志的输出效果。
func TestHandler_LogOutput_NestedToolResultWithLargeImage(t *testing.T) {
	// 构造较大的 base64 图片数据（~10KB 解码后）
	// 用 PNG 签名 + 随机填充，确保 base64 解码后 > LargeImageThreshold
	largeImageData := buildLargeImageBase64(10_000)

	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "MockDesc-Nested"}

	// 用 bytes.Buffer 捕获日志输出
	var logBuf bytes.Buffer
	logHandler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	logger := slog.New(logHandler)

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1024, // 1KB 阈值，10KB 图片会触发 is_large
		Log:                 logger,
	}
	h := NewHandler(deps)

	// 构造请求：system 消息在 messages 数组中 + 嵌套在 tool_result 中的 image
	reqBody := `{
		"model":"deepseek-chat",
		"max_tokens":100,
		"messages":[
			{"role":"system","content":[{"type":"text","text":"You are a helpful assistant."}]},
			{"role":"user","content":[
				{"type":"text","text":"Look at the screenshot:"},
				{"type":"tool_result","tool_use_id":"toolu_abc123","is_error":false,"content":[
					{"type":"text","text":"screenshot:"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + largeImageData + `"}}
				]}
			]}
		],
		"stream":true
	}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-upstream")
	h.ServeHTTP(rr, req)

	// 基本断言
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if mockVis.calls != 1 {
		t.Errorf("vision calls: want 1, got %d", mockVis.calls)
	}
	if hdr := rr.Header().Get("X-Blind-Llm-Eyes"); !strings.Contains(hdr, "1 rewritten") {
		t.Errorf("header wrong: %q", hdr)
	}

	// 验证上游收到的 body：tool_result 中的 image 已被替换为 text
	var upstreamReq map[string]any
	if err := json.Unmarshal(upstreamGot, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	msgs := upstreamReq["messages"].([]any)
	firstMsg := msgs[0].(map[string]any)
	if firstMsg["role"] != "user" {
		t.Errorf("want first user msg, got role=%v", firstMsg["role"])
	}
	content := firstMsg["content"].([]any)
	// content[0] = text "Look at the screenshot:"
	// content[1] = tool_result
	toolResult := content[1].(map[string]any)
	trContent := toolResult["content"].([]any)
	// trContent[0] = text "screenshot:"
	// trContent[1] = image block should be replaced with text
	replacedBlock := trContent[1].(map[string]any)
	if replacedBlock["type"] != "text" {
		t.Errorf("nested image not replaced: type=%v", replacedBlock["type"])
	}
	if !strings.Contains(replacedBlock["text"].(string), "MockDesc-Nested") {
		t.Errorf("desc missing in replaced text=%v", replacedBlock["text"])
	}

	// --- 验证日志输出 ---
	logOutput := logBuf.String()
	t.Logf("=== Captured Log Output (total %d bytes) ===", len(logOutput))
	t.Log(logOutput)

	// 提取所有 log line 并检查关键 stage 节点
	logLines := strings.Split(strings.TrimSpace(logOutput), "\n")
	if len(logLines) < 5 {
		t.Fatalf("expected at least 5 log lines, got %d", len(logLines))
	}

	// 用 stage 字段做索引
	var entries []testLogEntry
	for _, line := range logLines {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		e := testLogEntry{Raw: raw}
		if s, ok := raw["stage"].(string); ok {
			e.Stage = s
		}
		if s, ok := raw["status"].(string); ok {
			e.Status = s
		}
		if m, ok := raw["message"].(string); ok {
			e.Msg = m
		}
		entries = append(entries, e)
	}

	stageSet := map[string]bool{}
	for _, e := range entries {
		if e.Stage != "" {
			stageSet[e.Stage] = true
		}
	}

	// 必须出现的 stage 节点
	requiredStages := []string{
		"request_start",
		"body_read_complete",
		"json_parse_complete",
		"system_normalize_complete",
		"validate_complete",
		"find_images_complete",
		"image_cache_miss",
		"image_cache_hit", // 第 2 次请求时应该命中
		"singleflight_complete",
		"image_goroutine_start",
		"image_goroutine_complete",
		"parallel_images_start",
		"parallel_images_complete",
		"remarshal_complete",
		"upstream_request_start",
		"upstream_request_build",
		"upstream_response_received",
		"upstream_complete",
	}

	var missingStages []string
	for _, s := range requiredStages {
		if !stageSet[s] {
			missingStages = append(missingStages, s)
		}
	}
	// 注意：image_cache_hit 只在第 2 次请求出现，所以先跳过
	for i, s := range missingStages {
		if s == "image_cache_hit" {
			missingStages = append(missingStages[:i], missingStages[i+1:]...)
		}
	}
	if len(missingStages) > 0 {
		t.Errorf("missing required stages: %v", missingStages)
	}

	// 打印已发现的 stage 列表
	t.Logf("=== Discovered stages ===")
	for _, e := range entries {
		if e.Stage != "" {
			t.Logf("  stage=%s status=%s msg=%s", e.Stage, e.Status, e.Msg)
		}
	}

	// 验证关键日志字段
	t.Run("request_start fields", func(t *testing.T) {
		e := findEntry(entries, "request_start", "")
		if e == nil {
			t.Fatal("request_start not found")
		}
		if e.Raw["method"] != "POST" {
			t.Errorf("want method=POST, got %v", e.Raw["method"])
		}
		if e.Raw["path"] != "/v1/messages" {
			t.Errorf("want path=/v1/messages, got %v", e.Raw["path"])
		}
		if hasAuth, ok := e.Raw["has_auth"].(bool); !ok || !hasAuth {
			t.Errorf("want has_auth=true, got %v", e.Raw["has_auth"])
		}
	})

	t.Run("json_parse_complete fields", func(t *testing.T) {
		e := findEntry(entries, "json_parse_complete", "")
		if e == nil {
			t.Fatal("json_parse_complete not found")
		}
		if e.Raw["model"] != "deepseek-chat" {
			t.Errorf("want model=deepseek-chat, got %v", e.Raw["model"])
		}
		if msgs, ok := e.Raw["messages"].(float64); !ok || msgs != 2 {
			t.Errorf("want messages=2, got %v", e.Raw["messages"])
		}
		// 检查 role_counts
		if rc, ok := e.Raw["role_counts"].(map[string]any); ok {
			if userCount, ok := rc["user"].(float64); !ok || userCount != 1 {
				t.Errorf("want role_counts.user=1, got %v", rc["user"])
			}
		}
		// 检查 block_type_counts
		if btc, ok := e.Raw["block_type_counts"].(map[string]any); ok {
			if trCount, ok := btc["tool_result"].(float64); !ok || trCount != 1 {
				t.Errorf("want block_type_counts.tool_result=1, got %v", btc["tool_result"])
			}
		}
	})

	t.Run("system_normalize_complete moved", func(t *testing.T) {
		e := findEntry(entries, "system_normalize_complete", "moved")
		if e == nil {
			// 也可能是 noop 状态（如果 system 消息已被提前处理）
			e = findEntry(entries, "system_normalize_complete", "noop")
			if e == nil {
				t.Fatal("system_normalize_complete not found")
			}
		}
		t.Logf("system_normalize: stage=%s status=%s", e.Stage, e.Status)
	})

	t.Run("find_images_complete nested", func(t *testing.T) {
		e := findEntry(entries, "find_images_complete", "")
		if e == nil {
			t.Fatal("find_images_complete not found")
		}
		if count, ok := e.Raw["count"].(float64); !ok || count != 1 {
			t.Errorf("want count=1 nested image, got %v", e.Raw["count"])
		}
		if trBlocks, ok := e.Raw["tool_result_blocks"].(float64); !ok || trBlocks != 1 {
			t.Errorf("want tool_result_blocks=1, got %v", e.Raw["tool_result_blocks"])
		}
		if hasNested, ok := e.Raw["has_nested_source"].(bool); !ok || !hasNested {
			t.Errorf("want has_nested_source=true, got %v", e.Raw["has_nested_source"])
		}
		if isLarge, ok := e.Raw["is_large_request"].(bool); !ok || !isLarge {
			t.Errorf("want is_large_request=true (10KB > 1KB threshold), got %v", e.Raw["is_large_request"])
		}
	})

	t.Run("image_cache_miss large image", func(t *testing.T) {
		e := findEntry(entries, "image_cache_miss", "")
		if e == nil {
			t.Fatal("image_cache_miss not found")
		}
		if isLarge, ok := e.Raw["is_large"].(bool); !ok || !isLarge {
			t.Errorf("want is_large=true for 10KB image, got %v", e.Raw["is_large"])
		}
	})

	t.Run("remarshal_complete fields", func(t *testing.T) {
		e := findEntry(entries, "remarshal_complete", "")
		if e == nil {
			t.Fatal("remarshal_complete not found")
		}
		if status, ok := e.Raw["status"].(string); !ok || status != "ok" {
			t.Errorf("want status=ok, got %v", e.Raw["status"])
		}
		if rewritten, ok := e.Raw["rewritten_count"].(float64); !ok || rewritten != 1 {
			t.Errorf("want rewritten_count=1, got %v", e.Raw["rewritten_count"])
		}
		if systemMoved, ok := e.Raw["system_moved"].(float64); !ok || systemMoved != 1 {
			t.Errorf("want system_moved=1, got %v", e.Raw["system_moved"])
		}
		// delta_bytes 可以为负（大图 base64 → 短文本描述，请求体变小）
		if delta, ok := e.Raw["delta_bytes"].(float64); !ok {
			t.Errorf("want delta_bytes present, got %v", e.Raw["delta_bytes"])
		} else {
			t.Logf("delta_bytes=%v (negative means image base64 replaced by shorter text)", delta)
		}
	})

	t.Run("parallel_images_complete", func(t *testing.T) {
		e := findEntry(entries, "parallel_images_complete", "")
		if e == nil {
			t.Fatal("parallel_images_complete not found")
		}
		if rewritten, ok := e.Raw["rewritten"].(float64); !ok || rewritten != 1 {
			t.Errorf("want rewritten=1, got %v", e.Raw["rewritten"])
		}
	})

	// === 第 2 次请求：同一张图，验证 cache_hit 日志 ===
	logBuf.Reset()
	rr2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr2, req2)

	logOutput2 := logBuf.String()
	logLines2 := strings.Split(strings.TrimSpace(logOutput2), "\n")
	var entries2 []testLogEntry
	for _, line := range logLines2 {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		e := testLogEntry{Raw: raw}
		if s, ok := raw["stage"].(string); ok {
			e.Stage = s
		}
		entries2 = append(entries2, e)
	}

	t.Run("second request cache_hit", func(t *testing.T) {
		e := findEntry(entries2, "image_cache_hit", "")
		if e == nil {
			t.Fatal("image_cache_hit not found in 2nd request")
		}
		if descLen, ok := e.Raw["desc_len"].(float64); !ok || descLen <= 0 {
			t.Errorf("want desc_len > 0, got %v", e.Raw["desc_len"])
		}
		// 不应该有 singleflight_complete（因为缓存命中直接返回）
		if e := findEntry(entries2, "singleflight_complete", ""); e != nil {
			t.Errorf("unexpected singleflight_complete on cache hit request")
		}
	})
}

// testLogEntry 用于解析日志行的辅助结构
type testLogEntry struct {
	Stage  string
	Status string
	Msg    string
	Raw    map[string]any
}

// findEntry 在日志条目中查找匹配 stage + status 的条目。
func findEntry(entries []testLogEntry, stage, status string) *testLogEntry {
	for i := range entries {
		if entries[i].Stage == stage {
			if status == "" || entries[i].Status == status {
				return &entries[i]
			}
		}
	}
	return nil
}

// buildLargeImageBase64 构造一个 base64 编码的图片字符串，解码后约为 targetSize 字节。
// 使用合法 PNG 签名 + 填充字节，确保 Validate 不报错。
func buildLargeImageBase64(targetSize int) string {
	// PNG 文件头（8 字节）
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

	// IHDR chunk（25 字节）: length(4) + type(4) + data(13) + crc(4)
	ihdr := []byte{
		0x00, 0x00, 0x00, 0x0D, // length = 13
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, // width = 1
		0x00, 0x00, 0x00, 0x01, // height = 1
		0x08,             // bit depth = 8
		0x02,             // color type = 2 (RGB)
		0x00, 0x00, 0x00, // compression, filter, interlace
		0x90, 0x77, 0x53, 0xDE, // CRC (dummy)
	}

	// IDAT chunk: length(4) + type(4) + data + crc(4)
	// 填充数据使总大小接近 targetSize
	headerSize := len(pngHeader) + len(ihdr) + 12 // 12 for IDAT overhead
	dataSize := targetSize - headerSize - 12
	if dataSize < 100 {
		dataSize = 100
	}

	// IDAT length
	iddrLen := make([]byte, 4)
	iddrLen[0] = byte(dataSize >> 24)
	iddrLen[1] = byte(dataSize >> 16)
	iddrLen[2] = byte(dataSize >> 8)
	iddrLen[3] = byte(dataSize)

	idatType := []byte{0x49, 0x44, 0x41, 0x54} // "IDAT"

	// 填充数据：使用固定模式
	idatData := make([]byte, dataSize)
	for i := range idatData {
		idatData[i] = byte(i % 256)
	}

	idatCRC := []byte{0x00, 0x00, 0x00, 0x00} // dummy CRC

	// IEND chunk
	iend := []byte{0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82}

	var all []byte
	all = append(all, pngHeader...)
	all = append(all, ihdr...)
	all = append(all, iddrLen...)
	all = append(all, idatType...)
	all = append(all, idatData...)
	all = append(all, idatCRC...)
	all = append(all, iend...)

	return base64.StdEncoding.EncodeToString(all)
}

// TestHandler_CacheHit_Scenarios 专门覆盖缓存命中场景，确保后续修改不会破坏缓存行为。
func TestHandler_CacheHit_Scenarios(t *testing.T) {
	smallImg := buildLargeImageBase64(500)    // 小图，不触发 is_large
	largeImg := buildLargeImageBase64(10_000) // 大图，触发 is_large
	otherImg := buildLargeImageBase64(2_000)  // 另一张不同的图

	t.Run("basic cache hit: same image skips vision on 2nd request", func(t *testing.T) {
		var upstreamGot []byte
		up := fakeUpstream(t, &upstreamGot)
		defer up.Close()

		mockVis := &mockVisionProvider{desc: "Desc-A"}
		c := cache.NewLRU(10)

		deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
			VisionProvider:      mockVis,
			Cache:               c,
			FailOpen:            true,
			LargeImageThreshold: 1_000_000,
			Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		}
		h := NewHandler(deps)

		body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}]}`

		// 第 1 次：cache miss → vision 调用
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req1.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req1)
		if mockVis.calls != 1 {
			t.Fatalf("1st call: want vision calls=1, got %d", mockVis.calls)
		}

		// 第 2 次：cache hit → vision 不应增加
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req2.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req2)
		if mockVis.calls != 1 {
			t.Fatalf("2nd call: want vision calls still=1 (cache hit), got %d", mockVis.calls)
		}

		// 验证上游收到的 body 含缓存描述
		var upstreamReq map[string]any
		json.Unmarshal(upstreamGot, &upstreamReq)
		msgs := upstreamReq["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		if content[1].(map[string]any)["type"] != "text" {
			t.Errorf("2nd upstream: image should be replaced with text by cache, got type=%v", content[1].(map[string]any)["type"])
		}
	})

	t.Run("different images do not share cache", func(t *testing.T) {
		var upstreamGot []byte
		up := fakeUpstream(t, &upstreamGot)
		defer up.Close()

		mockVis := &mockVisionProvider{desc: "Desc-B"}
		c := cache.NewLRU(10)

		deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
			VisionProvider:      mockVis,
			Cache:               c,
			FailOpen:            true,
			LargeImageThreshold: 1_000_000,
			Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		}
		h := NewHandler(deps)

		bodyA := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}]}`
		bodyB := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + otherImg + `"}}]}]}`

		// 第 1 次：图 A → miss
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyA))
		req1.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req1)
		if mockVis.calls != 1 {
			t.Fatalf("1st (imgA): want vision=1, got %d", mockVis.calls)
		}

		// 第 2 次：图 B → miss（不同 hash）
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyB))
		req2.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req2)
		if mockVis.calls != 2 {
			t.Fatalf("2nd (imgB): want vision=2 (different hash), got %d", mockVis.calls)
		}

		// 第 3 次：图 A 再次 → hit
		req3, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyA))
		req3.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req3)
		if mockVis.calls != 2 {
			t.Fatalf("3rd (imgA again): want vision still=2 (cache hit), got %d", mockVis.calls)
		}
	})

	t.Run("cache eviction: oldest entry is evicted when capacity exceeded", func(t *testing.T) {
		var upstreamGot []byte
		up := fakeUpstream(t, &upstreamGot)
		defer up.Close()

		mockVis := &mockVisionProvider{desc: "Desc-C"}
		c := cache.NewLRU(2) // 容量只有 2

		deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
			VisionProvider:      mockVis,
			Cache:               c,
			FailOpen:            true,
			LargeImageThreshold: 1_000_000,
			Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		}
		h := NewHandler(deps)

		// 3 张不同的图
		bodyA := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}]}`
		bodyB := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + otherImg + `"}}]}]}`
		bodyC := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + largeImg + `"}}]}]}`

		// 第 1、2 次：图 A、B → 缓存 [B, A]（B 最近）
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyA))
		req1.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req1)
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyB))
		req2.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req2)
		if mockVis.calls != 2 {
			t.Fatalf("after A+B: want vision=2, got %d", mockVis.calls)
		}

		// 第 3 次：图 C → miss → 缓存 [C, B]，A 被驱逐
		req3, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyC))
		req3.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req3)
		if mockVis.calls != 3 {
			t.Fatalf("after C: want vision=3, got %d", mockVis.calls)
		}

		// 第 4 次：图 A 再次 → miss（已被驱逐）
		req4, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyA))
		req4.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req4)
		if mockVis.calls != 4 {
			t.Fatalf("after A again (evicted): want vision=4, got %d", mockVis.calls)
		}

		// 此时缓存 [A, C]，B 已被驱逐
		// 第 5 次：图 C → hit（仍在缓存中，且是最近使用）
		req5, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyC))
		req5.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req5)
		if mockVis.calls != 4 {
			t.Fatalf("after C (still cached): want vision still=4, got %d", mockVis.calls)
		}

		// 第 6 次：图 B → miss（已被驱逐）
		req6, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bodyB))
		req6.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req6)
		if mockVis.calls != 5 {
			t.Fatalf("after B (evicted): want vision=5, got %d", mockVis.calls)
		}
	})

	t.Run("cache hit for nested tool_result images", func(t *testing.T) {
		var upstreamGot []byte
		up := fakeUpstream(t, &upstreamGot)
		defer up.Close()

		mockVis := &mockVisionProvider{desc: "Desc-Nested"}
		c := cache.NewLRU(10)

		deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
			VisionProvider:      mockVis,
			Cache:               c,
			FailOpen:            true,
			LargeImageThreshold: 1_000_000,
			Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		}
		h := NewHandler(deps)

		body := `{"model":"m","messages":[{"role":"user","content":[
			{"type":"text","text":"screenshot:"},
			{"type":"tool_result","tool_use_id":"toolu_x","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}
			]}
		]}]}`

		// 第 1 次：cache miss
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req1.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req1)
		if mockVis.calls != 1 {
			t.Fatalf("1st nested: want vision=1, got %d", mockVis.calls)
		}

		// 第 2 次：cache hit（同一张嵌套图）
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req2.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req2)
		if mockVis.calls != 1 {
			t.Fatalf("2nd nested: want vision still=1 (cache hit), got %d", mockVis.calls)
		}

		// 验证上游 body 中嵌套 image 已被替换
		var upstreamReq map[string]any
		json.Unmarshal(upstreamGot, &upstreamReq)
		msgs := upstreamReq["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		toolResult := content[1].(map[string]any)
		trContent := toolResult["content"].([]any)
		if trContent[0].(map[string]any)["type"] != "text" {
			t.Errorf("nested image not replaced: type=%v", trContent[0].(map[string]any)["type"])
		}
	})

	t.Run("cache hit produces correct log stages", func(t *testing.T) {
		var upstreamGot []byte
		up := fakeUpstream(t, &upstreamGot)
		defer up.Close()

		mockVis := &mockVisionProvider{desc: "Desc-Log"}
		c := cache.NewLRU(10)

		var logBuf bytes.Buffer
		logHandler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
		logger := slog.New(logHandler)

		deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
			VisionProvider:      mockVis,
			Cache:               c,
			FailOpen:            true,
			LargeImageThreshold: 1_000_000,
			Log:                 logger,
		}
		h := NewHandler(deps)

		body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}]}`

		// 第 1 次：miss
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req1.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req1)

		// 第 2 次：hit
		logBuf.Reset()
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req2.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req2)

		// 解析日志
		logLines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
		var entries []testLogEntry
		for _, line := range logLines {
			var raw map[string]any
			if err := json.Unmarshal([]byte(line), &raw); err != nil {
				continue
			}
			e := testLogEntry{Raw: raw}
			if s, ok := raw["stage"].(string); ok {
				e.Stage = s
			}
			entries = append(entries, e)
		}

		// 必须出现 image_cache_hit
		if e := findEntry(entries, "image_cache_hit", ""); e == nil {
			t.Fatal("cache hit request must have image_cache_hit stage")
		}

		// 不应出现 image_cache_miss（缓存命中直接返回，不查 vision）
		if e := findEntry(entries, "image_cache_miss", ""); e != nil {
			t.Error("cache hit request should NOT have image_cache_miss stage")
		}

		// 不应出现 singleflight_complete（缓存命中跳过 SF.Do）
		if e := findEntry(entries, "singleflight_complete", ""); e != nil {
			t.Error("cache hit request should NOT have singleflight_complete stage")
		}

		// 不应出现 image_goroutine_start/complete 之后的 vision 相关日志
		if e := findEntry(entries, "image_goroutine_start", ""); e != nil {
			// goroutine 仍然会启动，但 outcome 应为 cache_hit
			if outcome, ok := e.Raw["outcome"]; ok {
				t.Logf("goroutine started with outcome: %v", outcome)
			}
		}

		// 验证 parallel_images_complete 中 cached > 0
		for _, e := range entries {
			if e.Stage == "parallel_images_complete" {
				if cached, ok := e.Raw["cached"].(float64); !ok || cached < 1 {
					t.Errorf("parallel_images_complete: want cached >= 1, got %v", e.Raw["cached"])
				}
			}
		}
	})

	t.Run("cache hit header reports cached count correctly", func(t *testing.T) {
		var upstreamGot []byte
		up := fakeUpstream(t, &upstreamGot)
		defer up.Close()

		mockVis := &mockVisionProvider{desc: "Desc-Header"}
		c := cache.NewLRU(10)

		deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
			VisionProvider:      mockVis,
			Cache:               c,
			FailOpen:            true,
			LargeImageThreshold: 1_000_000,
			Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		}
		h := NewHandler(deps)

		body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}]}`

		// 第 1 次：miss → header 显示 "1 rewritten, 0 cached"
		rr1 := httptest.NewRecorder()
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req1.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr1, req1)
		hdr1 := rr1.Header().Get("X-Blind-Llm-Eyes")
		if !strings.Contains(hdr1, "1 rewritten") || !strings.Contains(hdr1, "0 cached") {
			t.Errorf("1st header: want '1 rewritten, 0 cached', got %q", hdr1)
		}

		// 第 2 次：hit → header 显示 "1 rewritten, 1 cached"
		rr2 := httptest.NewRecorder()
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req2.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr2, req2)
		hdr2 := rr2.Header().Get("X-Blind-Llm-Eyes")
		if !strings.Contains(hdr2, "1 rewritten") || !strings.Contains(hdr2, "1 cached") {
			t.Errorf("2nd header: want '1 rewritten, 1 cached', got %q", hdr2)
		}
	})

	t.Run("cache hit with fail_open=false still works", func(t *testing.T) {
		var upstreamGot []byte
		up := fakeUpstream(t, &upstreamGot)
		defer up.Close()

		mockVis := &mockVisionProvider{desc: "Desc-Strict"}
		c := cache.NewLRU(10)

		deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
			VisionProvider:      mockVis,
			Cache:               c,
			FailOpen:            false, // 严格模式
			LargeImageThreshold: 1_000_000,
			Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		}
		h := NewHandler(deps)

		body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}]}`

		// 第 1 次：miss
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req1.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req1)
		if mockVis.calls != 1 {
			t.Fatalf("1st: want vision=1, got %d", mockVis.calls)
		}

		// 第 2 次：hit（fail_open=false 不影响缓存命中）
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req2.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req2)
		if mockVis.calls != 1 {
			t.Fatalf("2nd: want vision still=1 (cache hit), got %d", mockVis.calls)
		}
	})

	t.Run("cache hit description is correctly placed in upstream body", func(t *testing.T) {
		var upstreamGot []byte
		up := fakeUpstream(t, &upstreamGot)
		defer up.Close()

		mockVis := &mockVisionProvider{desc: "Unique-Desc-42"}
		c := cache.NewLRU(10)

		deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
			VisionProvider:      mockVis,
			Cache:               c,
			FailOpen:            true,
			LargeImageThreshold: 1_000_000,
			Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		}
		h := NewHandler(deps)

		body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}]}`

		// 第 1 次：miss → 缓存 "Unique-Desc-42"
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req1.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req1)

		// 第 2 次：hit → 从缓存读取 "Unique-Desc-42"
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req2.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), req2)

		// 验证上游收到的 body 包含缓存中的描述
		var upstreamReq map[string]any
		json.Unmarshal(upstreamGot, &upstreamReq)
		msgs := upstreamReq["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		textBlock := content[0].(map[string]any)
		if textBlock["type"] != "text" {
			t.Fatalf("want text block, got type=%v", textBlock["type"])
		}
		textVal := textBlock["text"].(string)
		if !strings.Contains(textVal, "Unique-Desc-42") {
			t.Errorf("cached description not found in upstream body: %s", textVal)
		}
	})
}
