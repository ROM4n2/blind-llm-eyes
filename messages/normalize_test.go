package messages

import (
	"encoding/json"
	"testing"
)

func TestNormalizeSystemMessages_NoSystemMessages(t *testing.T) {
	req := &Request{
		Messages: []Message{
			{Role: "user", Content: MessageContent{{Type: "text", Text: "hello"}}},
			{Role: "assistant", Content: MessageContent{{Type: "text", Text: "hi"}}},
		},
		System: SystemContent{{Type: "text", Text: "existing system"}},
	}
	moved := NormalizeSystemMessages(req)
	if moved != 0 {
		t.Errorf("want 0 moved, got %d", moved)
	}
	if len(req.Messages) != 2 {
		t.Errorf("messages len should be unchanged, got %d", len(req.Messages))
	}
	if len(req.System) != 1 {
		t.Errorf("system len should be unchanged, got %d", len(req.System))
	}
}

func TestNormalizeSystemMessages_ExtractsSystemFromMessages(t *testing.T) {
	req := &Request{
		Messages: []Message{
			{Role: "user", Content: MessageContent{{Type: "text", Text: "hello"}}},
			{Role: "system", Content: MessageContent{{Type: "text", Text: "system instruction"}}},
			{Role: "assistant", Content: MessageContent{{Type: "text", Text: "hi"}}},
		},
	}
	moved := NormalizeSystemMessages(req)
	if moved != 1 {
		t.Fatalf("want 1 moved, got %d", moved)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages len: want 2, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "user" || req.Messages[1].Role != "assistant" {
		t.Errorf("remaining messages order wrong: %s, %s",
			req.Messages[0].Role, req.Messages[1].Role)
	}
	if len(req.System) != 1 {
		t.Fatalf("system len: want 1, got %d", len(req.System))
	}
	if req.System[0].Text != "system instruction" {
		t.Errorf("system text: want %q, got %q", "system instruction", req.System[0].Text)
	}
}

func TestNormalizeSystemMessages_MultipleSystemMessages(t *testing.T) {
	req := &Request{
		Messages: []Message{
			{Role: "system", Content: MessageContent{{Type: "text", Text: "sys1"}}},
			{Role: "user", Content: MessageContent{{Type: "text", Text: "hello"}}},
			{Role: "system", Content: MessageContent{{Type: "text", Text: "sys2"}}},
			{Role: "assistant", Content: MessageContent{{Type: "text", Text: "hi"}}},
		},
		System: SystemContent{{Type: "text", Text: "existing"}},
	}
	moved := NormalizeSystemMessages(req)
	if moved != 2 {
		t.Fatalf("want 2 moved, got %d", moved)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages len: want 2, got %d", len(req.Messages))
	}
	if len(req.System) != 3 {
		t.Fatalf("system len: want 3 (1 existing + 2 moved), got %d", len(req.System))
	}
	if req.System[0].Text != "existing" {
		t.Errorf("system[0]: want %q, got %q", "existing", req.System[0].Text)
	}
	if req.System[1].Text != "sys1" {
		t.Errorf("system[1]: want %q, got %q", "sys1", req.System[1].Text)
	}
	if req.System[2].Text != "sys2" {
		t.Errorf("system[2]: want %q, got %q", "sys2", req.System[2].Text)
	}
}

func TestNormalizeSystemMessages_EmptyMessages(t *testing.T) {
	req := &Request{}
	moved := NormalizeSystemMessages(req)
	if moved != 0 {
		t.Errorf("want 0 moved for empty messages, got %d", moved)
	}
}

func TestNormalizeSystemMessages_PreservesRawJSON(t *testing.T) {
	// 模拟 Claude Code 发来的原始 JSON：system 消息在 messages 数组中
	rawJSON := `{
		"model": "test-model",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "system", "content": "you are helpful"},
			{"role": "assistant", "content": "hi there"}
		],
		"max_tokens": 1024
	}`
	var req Request
	if err := json.Unmarshal([]byte(rawJSON), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	moved := NormalizeSystemMessages(&req)
	if moved != 1 {
		t.Fatalf("want 1 moved, got %d", moved)
	}

	// 验证 Validate 通过（之前会失败）
	if err := req.Validate(); err != nil {
		t.Fatalf("validation should pass after normalization: %v", err)
	}

	// 验证重新序列化后，messages 数组中没有 system 角色
	out, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var check struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		System any `json:"system"`
	}
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	for i, msg := range check.Messages {
		if msg.Role == "system" {
			t.Errorf("messages[%d] still has role system after normalization", i)
		}
	}
	if check.System == nil {
		t.Error("system field should be non-nil after normalization")
	}
}
