package messages

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── 辅助函数：快速构造 ContentBlock（通过 JSON 往返确保 raw 同步） ──
func textBlock(s string) ContentBlock {
	var b ContentBlock
	raw, _ := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: s})
	_ = b.UnmarshalJSON(raw)
	return b
}

func imageBlock() ContentBlock {
	var b ContentBlock
	raw, _ := json.Marshal(struct {
		Type   string       `json:"type"`
		Source *ImageSource `json:"source"`
	}{
		Type: "image",
		Source: &ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
		},
	})
	_ = b.UnmarshalJSON(raw)
	return b
}

func toolResultBlock(inner ...ContentBlock) ContentBlock {
	var b ContentBlock
	mc := MessageContent(inner)
	raw, _ := json.Marshal(struct {
		Type    string         `json:"type"`
		Content MessageContent `json:"content"`
	}{Type: ContentTypeToolResult, Content: mc})
	_ = b.UnmarshalJSON(raw)
	return b
}

// TestExtract_NilOrEmpty: nil req / 空 messages → ""
func TestExtract_NilOrEmpty(t *testing.T) {
	if got := ExtractConversationContext(nil, 3, 2000); got != "" {
		t.Errorf("nil req expected empty, got %q", got)
	}
	empty := &Request{Messages: nil}
	if got := ExtractConversationContext(empty, 3, 2000); got != "" {
		t.Errorf("nil messages expected empty, got %q", got)
	}
	empty2 := &Request{Messages: []Message{}}
	if got := ExtractConversationContext(empty2, 3, 2000); got != "" {
		t.Errorf("empty messages slice expected empty, got %q", got)
	}
}

// TestExtract_RecentRoundsZero: recentRounds=0 或 maxChars<=0 → ""
func TestExtract_RecentRoundsZero(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: "user", Content: MessageContent{textBlock("hello")}},
	}}
	if got := ExtractConversationContext(req, 0, 2000); got != "" {
		t.Errorf("recentRounds=0 expected empty, got %q", got)
	}
	if got := ExtractConversationContext(req, -1, 2000); got != "" {
		t.Errorf("recentRounds=-1 expected empty, got %q", got)
	}
	if got := ExtractConversationContext(req, 3, 0); got != "" {
		t.Errorf("maxChars=0 expected empty, got %q", got)
	}
}

// TestExtract_SingleRound: recentRounds=1 → 只保留最后 1 个 user 及对应 assistant
func TestExtract_SingleRound(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: "user", Content: MessageContent{textBlock("第N-2轮问题")}},
		{Role: "assistant", Content: MessageContent{textBlock("第N-2轮回答")}},
		{Role: "user", Content: MessageContent{textBlock("最后一个问题，不含图片")}},
	}}
	got := ExtractConversationContext(req, 1, 2000)
	// 最后 user 不含 image，被收集。recentRounds=1 → 遇到第 1 个 user 就停（从后向前计数）
	if !strings.Contains(got, "最后一个问题") {
		t.Errorf("expected last user preserved, got %q", got)
	}
	if strings.Contains(got, "第N-2轮") {
		t.Errorf("recentRounds=1 should skip earlier rounds, got %q", got)
	}
}

// TestExtract_MultiRounds_Truncation: 4 user，recentRounds=2 → 只取最后 2 个 user 及其对话
func TestExtract_MultiRounds_Truncation(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: "user", Content: MessageContent{textBlock("U1")}},
		{Role: "assistant", Content: MessageContent{textBlock("A1")}},
		{Role: "user", Content: MessageContent{textBlock("U2")}},
		{Role: "assistant", Content: MessageContent{textBlock("A2")}},
		{Role: "user", Content: MessageContent{textBlock("U3")}},
		{Role: "assistant", Content: MessageContent{textBlock("A3")}},
		{Role: "user", Content: MessageContent{textBlock("U4")}},
		{Role: "assistant", Content: MessageContent{textBlock("A4")}},
	}}
	got := ExtractConversationContext(req, 2, 10000)
	// recentRounds=2 → 从后向前找 2 个 user：U4 和 U3，对应 A4, A3, U3, U4？
	// 从尾向前：A4（assistant不计），U4（userCount=1，继续），A3（不计），U3（userCount=2→break）。
	// 收集顺序反转前：[A4,U4,A3,U3]，反转后：[U3,A3,U4,A4]
	for _, kw := range []string{"U3", "A3", "U4", "A4"} {
		if !strings.Contains(got, kw) {
			t.Errorf("expected %q in result (recentRounds=2), got %q", kw, got)
		}
	}
	if strings.Contains(got, "U1") || strings.Contains(got, "U2") || strings.Contains(got, "A1") || strings.Contains(got, "A2") {
		t.Errorf("recentRounds=2 should skip U1/U2/A1/A2, got %q", got)
	}
}

// TestExtract_MaxChars_Truncation: 小 maxChars，早期轮次被整轮次截断
func TestExtract_MaxChars_Truncation(t *testing.T) {
	long := strings.Repeat("X", 300)
	req := &Request{Messages: []Message{
		{Role: "user", Content: MessageContent{textBlock(long)}}, // user1 300 chars
		{Role: "assistant", Content: MessageContent{textBlock(long)}}, // a1 300
		{Role: "user", Content: MessageContent{textBlock("最新U")}}, // 5 chars
		{Role: "assistant", Content: MessageContent{textBlock("最新A")}}, // 5 chars
	}}
	// maxChars=200：只够装下最新 2 条（[user] 最新U=15，[assistant] 最新A=20，合计约 36 + \n）
	got := ExtractConversationContext(req, 10, 200)
	if !strings.Contains(got, "最新U") || !strings.Contains(got, "最新A") {
		t.Errorf("must keep latest 2 msgs, got %q", got)
	}
	if strings.Contains(got, strings.Repeat("X", 300)) {
		t.Errorf("early long messages should be truncated under maxChars=200, got len=%d", len(got))
	}
	if len(got) > 300 { // 允许最后几条完整保留（不切半条）
		t.Errorf("result too long (%d) for maxChars=200: %q", len(got), got)
	}
}

// TestExtract_MaxChars_TwoMessages_BothExceed: 2 条消息合起来超 maxChars，
// 各自单独不超 → 必须截断早期那条，只保留最新一条。
// 回归测试：修复前 i>0 条件导致 i=0 时无法截断，结果超出 maxChars。
func TestExtract_MaxChars_TwoMessages_BothExceed(t *testing.T) {
	medium := strings.Repeat("Y", 50) // 50 chars
	req := &Request{Messages: []Message{
		{Role: "user", Content: MessageContent{textBlock(medium)}},      // [user] + 50 ≈ 56 chars
		{Role: "assistant", Content: MessageContent{textBlock(medium)}}, // [assistant] + 50 ≈ 61 chars
	}}
	// maxChars=80：两条合起来 ≈ 118 > 80，但最新一条单独 ≈ 61 < 80
	got := ExtractConversationContext(req, 10, 80)
	if !strings.Contains(got, medium) {
		t.Errorf("must keep at least the latest message, got %q", got)
	}
	if len(got) > 80 {
		t.Errorf("result length %d exceeds maxChars=80, got %q", len(got), got)
	}
}

// TestExtract_SkipImages: 含 image 的消息只取 text，绝无 base64 泄露
func TestExtract_SkipImages(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: "user", Content: MessageContent{
			textBlock("请问这段代码"),
			imageBlock(),
		}},
		{Role: "assistant", Content: MessageContent{textBlock("哪一行报错")}},
	}}
	got := ExtractConversationContext(req, 3, 2000)
	if !strings.Contains(got, "请问这段代码") {
		t.Errorf("user text missing, got %q", got)
	}
	if !strings.Contains(got, "哪一行报错") {
		t.Errorf("assistant text missing, got %q", got)
	}
	if strings.Contains(got, "iVBORw0KG") {
		t.Fatalf("CRITICAL: image base64 leaked into context! preview: %q", got[:minInt(120, len(got))])
	}
}

// TestExtract_SkipNestedToolResultImages: tool_result 嵌套 image 跳过，text 保留
func TestExtract_SkipNestedToolResultImages(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: "assistant", Content: MessageContent{
			toolResultBlock(
				textBlock("stdout: PASS 2/3"),
				imageBlock(),
				textBlock("stderr: FAIL test_bar"),
			),
		}},
		{Role: "user", Content: MessageContent{textBlock("怎么修第二个失败")}},
	}}
	got := ExtractConversationContext(req, 3, 2000)
	if !strings.Contains(got, "stdout: PASS 2/3") || !strings.Contains(got, "stderr: FAIL test_bar") {
		t.Errorf("tool_result nested text missing, got %q", got)
	}
	if !strings.Contains(got, "怎么修第二个失败") {
		t.Errorf("last user text missing, got %q", got)
	}
	if strings.Contains(got, "iVBORw0KG") {
		t.Fatalf("CRITICAL: nested tool_result image base64 leaked! got %q", got)
	}
}

// TestExtract_LastUserWithImage_Skipped: 最后 user 含 image → 整体跳过（不计入上下文）
func TestExtract_LastUserWithImage_Skipped(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: "user", Content: MessageContent{textBlock("之前的问题：安装依赖失败")}},
		{Role: "assistant", Content: MessageContent{textBlock("之前的回答：试试 pip install -U")}},
		// 最后一条 user：含 image → 被整体跳过
		{Role: "user", Content: MessageContent{
			textBlock("当前附截图报错详细信息"),
			imageBlock(),
		}},
	}}
	got := ExtractConversationContext(req, 3, 2000)
	if strings.Contains(got, "当前附截图报错详细信息") {
		t.Errorf("last user with image should be excluded, got %q", got)
	}
	if !strings.Contains(got, "之前的问题：安装依赖失败") || !strings.Contains(got, "之前的回答：试试 pip install -U") {
		t.Errorf("earlier history should be preserved, got %q", got)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
