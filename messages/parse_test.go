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
