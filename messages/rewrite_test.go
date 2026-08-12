package messages

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReplaceImageWithDescription(t *testing.T) {
	body := `{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "描述这张图"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "abc"}}
			]}
		]
	}`
	var req Request
	json.NewDecoder(strings.NewReader(body)).Decode(&req)

	imgs := FindImageBlocks(&req)
	ReplaceImageWithDescription(imgs[0], "这是一张 1x1 红色 PNG 图片")

	// 断言：content 长度没变（原位替换）
	if len(req.Messages[0].Content) != 2 {
		t.Fatalf("content len changed: %d", len(req.Messages[0].Content))
	}
	// 断言：image 块变成了 text 块
	blk := req.Messages[0].Content[1]
	if blk.Type != "text" {
		t.Errorf("want type=text, got %s", blk.Type)
	}
	if blk.Text != "[Image Description: 这是一张 1x1 红色 PNG 图片]" {
		t.Errorf("text mismatch: %s", blk.Text)
	}
	// 断言：source 被清空（避免序列化时带冗余字段）
	if blk.Source != nil {
		t.Errorf("source should be nil after replace")
	}
}
