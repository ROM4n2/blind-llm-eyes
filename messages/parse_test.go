package messages

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFindImageBlocks_UserMessageOnly(t *testing.T) {
	body := `{
		"model": "deepseek-chat",
		"max_tokens": 1024,
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "描述这张图"},
					{
						"type": "image",
						"source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}
					}
				]
			}
		],
		"stream": true
	}`

	var req Request
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	imgs := FindImageBlocks(&req)
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	want := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	if imgs[0].Source.Data != want {
		t.Errorf("data mismatch")
	}
	if imgs[0].Source.MediaType != "image/png" {
		t.Errorf("media_type mismatch: %s", imgs[0].Source.MediaType)
	}
}

func TestFindImageBlocks_NoImages(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`
	var req Request
	json.NewDecoder(strings.NewReader(body)).Decode(&req)
	if len(FindImageBlocks(&req)) != 0 {
		t.Errorf("want 0, got %d", len(FindImageBlocks(&req)))
	}
}

func TestFindImageBlocks_StringContent(t *testing.T) {
	// Claude Code 有时把 content 发成纯字符串
	body := `{"messages":[{"role":"user","content":"hello world"}]}`
	var req Request
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages[0].Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(req.Messages[0].Content))
	}
	if req.Messages[0].Content[0].Type != "text" {
		t.Errorf("want type=text, got %s", req.Messages[0].Content[0].Type)
	}
	if req.Messages[0].Content[0].Text != "hello world" {
		t.Errorf("text mismatch: %s", req.Messages[0].Content[0].Text)
	}
	if len(FindImageBlocks(&req)) != 0 {
		t.Errorf("want 0 images, got %d", len(FindImageBlocks(&req)))
	}
}

func TestFindImageBlocks_NestedInToolResult(t *testing.T) {
	body := `{
		"messages": [{"role":"user","content":[
			{"type":"text","text":"screenshot:"},
			{"type":"tool_result","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}},
				{"type":"text","text":"done"}
			]},
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"def"}}
		]}]
	}`
	var req Request
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	imgs := FindImageBlocks(&req)
	if len(imgs) != 2 {
		t.Fatalf("want 2 images (1 nested + 1 top), got %d", len(imgs))
	}
	// 顺序：嵌套的在前，顶层的在后
	if imgs[0].Source.Data != "abc" || imgs[1].Source.Data != "def" {
		t.Errorf("image order/data wrong: %s / %s", imgs[0].Source.Data, imgs[1].Source.Data)
	}
}
