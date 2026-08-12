package messages

import (
	"encoding/json"
	"testing"
)

func TestRequest_SystemAsArray(t *testing.T) {
	body := `{
		"model": "deepseek-v4-flash",
		"system": [{"type": "text", "text": "You are a helpful assistant"}],
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "describe image"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "abc"}}
			]}
		],
		"stream": true
	}`

	var req Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.System) != 1 || req.System[0].Text != "You are a helpful assistant" {
		t.Errorf("system parse failed: %+v", req.System)
	}

	imgs := FindImageBlocks(&req)
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
}

func TestRequest_SystemAsString(t *testing.T) {
	body := `{
		"model": "deepseek-v4-flash",
		"system": "You are helpful",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`

	var req Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.System) != 1 || req.System[0].Text != "You are helpful" {
		t.Errorf("system string parse failed: %+v", req.System)
	}
}

func TestRequest_EmptySystem(t *testing.T) {
	body := `{"model": "m", "messages": [{"role": "user", "content": []}]}`
	var req Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.System) != 0 {
		t.Errorf("want empty system, got %d", len(req.System))
	}
}
