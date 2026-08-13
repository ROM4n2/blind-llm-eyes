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
	wantPrefix := "<BLIND_LLM_EYES_IMAGE>"
	if len(blk.Text) < len(wantPrefix) || blk.Text[:len(wantPrefix)] != wantPrefix {
		t.Errorf("want text to start with %q, got %q", wantPrefix, blk.Text)
	}
	if blk.Source != nil {
		t.Errorf("source should be nil after replace")
	}

	// 关键断言：Marshal 后输出的是新的 text 块（不是旧的 image 块 JSON）
	out, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	outStr := string(out)
	if !strings.Contains(outStr, `"type":"text"`) {
		t.Errorf("marshaled output should contain text block, got: %s", outStr[:min(len(outStr), 200)])
	}
	if strings.Contains(outStr, `"type":"image"`) {
		t.Errorf("marshaled output should NOT contain image block, got: %s", outStr[:min(len(outStr), 200)])
	}
	if strings.Contains(outStr, `"media_type"`) {
		t.Errorf("marshaled output should NOT contain original image metadata")
	}
}

func TestPreserveUnknownFields(t *testing.T) {
	// 模拟真实请求：包含 thinking 块（上游 CC Switch / deepseek 所需字段）
	body := `{
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]},
			{"role": "assistant", "content": [{"type": "thinking", "thinking": "let me think..."}]},
			{"role": "user", "content": [{"type": "text", "text": "ok"}]}
		]
	}`
	var req Request
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Marshal → 验证 thinking 字段被保留
	out, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"thinking":"let me think..."`) {
		t.Errorf("thinking field lost after round-trip: %s", string(out)[:300])
	}
	// 其他字段也被保留
	if !strings.Contains(string(out), `"role":"assistant"`) {
		t.Errorf("role field lost: %s", string(out)[:300])
	}
}

func TestReplaceNestedImageInToolResult(t *testing.T) {
	body := `{
		"messages": [{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"text","text":"screenshot:"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}
			]}
		]}]
	}`
	var req Request
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	imgs := FindImageBlocks(&req)
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	ReplaceImageWithDescription(imgs[0], "描述：一个红色界面")

	out, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	outStr := string(out)
	if !strings.Contains(outStr, `"type":"tool_result"`) {
		t.Errorf("tool_result wrapper lost: %s", outStr[:300])
	}
	if !strings.Contains(outStr, "描述：一个红色界面") {
		t.Errorf("description missing after replace: %s", outStr[:300])
	}
	if strings.Contains(outStr, `"type":"image"`) {
		t.Errorf("image block still present after replace: %s", outStr[:300])
	}
	if !strings.Contains(outStr, `"tool_use_id":"toolu_1"`) {
		t.Errorf("tool_use_id lost: %s", outStr[:300])
	}
}
