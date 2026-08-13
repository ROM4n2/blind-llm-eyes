package messages

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestToolResultMergeRaw_PreservesUnknownFields(t *testing.T) {
	// 构造含 tool_use_id + is_error + 嵌套 image 的 tool_result
	body := `{
		"messages": [{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"tool_result","tool_use_id":"toolu_abc123","is_error":false,"content":[
				{"type":"text","text":"screenshot:"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}
			]}
		]}]
	}`

	var req Request
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// 验证解析
	blk := req.Messages[0].Content[1]
	if blk.Type != "tool_result" {
		t.Fatalf("type: want tool_result, got %s", blk.Type)
	}
	if blk.IsError != false {
		t.Errorf("is_error: want false, got %v", blk.IsError)
	}
	if len(blk.Content) != 2 {
		t.Fatalf("nested content len: want 2, got %d", len(blk.Content))
	}
	if blk.Content[1].Type != "image" {
		t.Errorf("nested[1] type: want image, got %s", blk.Content[1].Type)
	}

	// 替换嵌套 image 为 text 描述
	blk.Content[1] = ContentBlock{
		Type: "text",
		Text: "[图片描述: 一个红色界面截图]",
	}

	// Marshal 并验证输出
	out, err := json.MarshalIndent(&req, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	outStr := string(out)

	fmt.Println("=== 合并后的 JSON 输出 ===")
	fmt.Println(outStr)
	fmt.Println("========================")

	// 断言：tool_use_id 保留（MarshalIndent 冒号后有空格）
	if !strings.Contains(outStr, `"tool_use_id": "toolu_abc123"`) {
		t.Errorf("tool_use_id lost in output:\n%s", outStr)
	}

	// 断言：is_error 保留
	if !strings.Contains(outStr, `"is_error": false`) {
		t.Errorf("is_error lost in output:\n%s", outStr)
	}

	// 断言：tool_result 外壳保留
	if !strings.Contains(outStr, `"type": "tool_result"`) {
		t.Errorf("tool_result wrapper lost:\n%s", outStr)
	}

	// 断言：嵌套 image 被替换为 text
	if strings.Contains(outStr, `"type": "image"`) {
		t.Errorf("image block still present after replace:\n%s", outStr)
	}
	if !strings.Contains(outStr, "[图片描述: 一个红色界面截图]") {
		t.Errorf("description missing in output:\n%s", outStr)
	}

	// 断言：嵌套 text 保留
	if !strings.Contains(outStr, "screenshot:") {
		t.Errorf("original nested text lost:\n%s", outStr)
	}

	t.Logf("PASS: all fields preserved, image replaced")
}

// TestToolResultUnmodified_RoundTrip 验证未修改的 tool_result 经过合并路径后语义等价。
// 即使没有替换嵌套内容，MarshalJSON 也会走合并 raw + 覆盖 content 路径，
// 输出的 JSON 应与原始输入语义一致（字段齐全、content 不丢失）。
func TestToolResultUnmodified_RoundTrip(t *testing.T) {
	body := `{
		"messages": [{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_xyz","is_error":true,"content":[
				{"type":"text","text":"command failed"}
			]}
		]}]
	}`

	var req Request
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// 不做任何修改，直接 round-trip
	out, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	outStr := string(out)

	// 所有原始字段必须保留
	if !strings.Contains(outStr, `"tool_use_id":"toolu_xyz"`) {
		t.Errorf("tool_use_id lost:\n%s", outStr)
	}
	if !strings.Contains(outStr, `"is_error":true`) {
		t.Errorf("is_error lost:\n%s", outStr)
	}
	if !strings.Contains(outStr, `"type":"tool_result"`) {
		t.Errorf("type lost:\n%s", outStr)
	}
	if !strings.Contains(outStr, "command failed") {
		t.Errorf("nested text content lost:\n%s", outStr)
	}

	// 验证 round-trip 后再解析，结构一致
	var req2 Request
	if err := json.Unmarshal(out, &req2); err != nil {
		t.Fatalf("re-decode failed: %v", err)
	}
	blk2 := req2.Messages[0].Content[0]
	if blk2.Type != "tool_result" {
		t.Errorf("re-decoded type: want tool_result, got %s", blk2.Type)
	}
	if blk2.IsError != true {
		t.Errorf("re-decoded is_error: want true, got %v", blk2.IsError)
	}
	if len(blk2.Content) != 1 || blk2.Content[0].Text != "command failed" {
		t.Errorf("re-decoded content mismatch: %+v", blk2.Content)
	}
}

// TestToolResultNoRaw_MarshalFromFields 验证 raw 为空时 tool_result 走 marshalFromFields 回退路径。
// 场景：代码中直接构造 ContentBlock{Type:"tool_result",...}（非从 JSON 解析），raw 为空。
func TestToolResultNoRaw_MarshalFromFields(t *testing.T) {
	blk := ContentBlock{
		Type:    ContentTypeToolResult,
		IsError: true,
		Content: MessageContent{
			{Type: "text", Text: "execution timeout"},
		},
	}
	// raw 为空，不经过 JSON 解析

	out, err := json.Marshal(&blk)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	outStr := string(out)

	// 回退路径应输出 type + content + is_error
	if !strings.Contains(outStr, `"type":"tool_result"`) {
		t.Errorf("type missing:\n%s", outStr)
	}
	if !strings.Contains(outStr, `"is_error":true`) {
		t.Errorf("is_error missing:\n%s", outStr)
	}
	if !strings.Contains(outStr, "execution timeout") {
		t.Errorf("content text missing:\n%s", outStr)
	}
	// raw 为空时不应包含 image（没有旧数据残留）
	if strings.Contains(outStr, `"type":"image"`) {
		t.Errorf("unexpected image block in fallback output:\n%s", outStr)
	}

	// 验证 is_error=false 时 omitempty 生效（不输出 is_error 字段）
	blk2 := ContentBlock{
		Type:    ContentTypeToolResult,
		IsError: false,
		Content: MessageContent{{Type: "text", Text: "ok"}},
	}
	out2, _ := json.Marshal(&blk2)
	if strings.Contains(string(out2), `"is_error"`) {
		t.Errorf("is_error should be omitted when false:\n%s", string(out2))
	}
}
