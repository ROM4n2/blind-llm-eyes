package messages

import (
	"testing"
)

// --- Request.Validate tests ---

func TestValidate_ValidRequest(t *testing.T) {
	req := &Request{
		Model: "deepseek-chat",
		Messages: []Message{
			{Role: RoleUser, Content: MessageContent{
				{Type: ContentTypeText, Text: "hello"},
			}},
		},
		MaxTokens: 100,
	}
	if err := req.Validate(); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

func TestValidate_ModelMissing(t *testing.T) {
	req := &Request{
		Messages: []Message{
			{Role: RoleUser, Content: MessageContent{
				{Type: ContentTypeText, Text: "hi"},
			}},
		},
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected error for missing model")
	}
	if err.Error() != "request.model is required and must be non-empty" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_MessagesEmpty(t *testing.T) {
	req := &Request{Model: "test-model"}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected error for empty messages")
	}
	if err.Error() != "request.messages is required and must contain at least one message" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidate_MaxTokensNegative(t *testing.T) {
	req := &Request{
		Model: "test",
		Messages: []Message{
			{Role: RoleUser, Content: MessageContent{
				{Type: ContentTypeText, Text: "hi"},
			}},
		},
		MaxTokens: -1,
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_tokens")
	}
}

// --- Message.Validate tests ---

func TestValidate_MessageRoleMissing(t *testing.T) {
	msg := &Message{
		Content: MessageContent{{Type: ContentTypeText, Text: "hi"}},
	}
	err := msg.Validate()
	if err == nil {
		t.Fatal("expected error for missing role")
	}
	if err.Error() != "message.role is required" {
		t.Errorf("unexpected: %v", err)
	}
}

func TestValidate_MessageRoleInvalid(t *testing.T) {
	msg := &Message{
		Role:    "system",
		Content: MessageContent{{Type: ContentTypeText, Text: "hi"}},
	}
	err := msg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid role")
	}
}

func TestValidate_MessageContentEmpty(t *testing.T) {
	msg := &Message{Role: RoleUser}
	err := msg.Validate()
	if err == nil {
		t.Fatal("expected error for empty content")
	}
}

// --- ContentBlock.Validate tests ---

func TestValidate_TextBlockMissingText(t *testing.T) {
	blk := &ContentBlock{Type: ContentTypeText}
	err := blk.Validate()
	if err == nil {
		t.Fatal("expected error for empty text")
	}
	if err.Error() != "text content block requires non-empty text field" {
		t.Errorf("unexpected: %v", err)
	}
}

func TestValidate_TextBlockValid(t *testing.T) {
	blk := &ContentBlock{Type: ContentTypeText, Text: "hello"}
	if err := blk.Validate(); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

func TestValidate_ImageBlockMissingSource(t *testing.T) {
	blk := &ContentBlock{Type: ContentTypeImage}
	err := blk.Validate()
	if err == nil {
		t.Fatal("expected error for missing source")
	}
	if err.Error() != "image content block requires source field" {
		t.Errorf("unexpected: %v", err)
	}
}

func TestValidate_ImageBlockValid(t *testing.T) {
	blk := &ContentBlock{
		Type: ContentTypeImage,
		Source: &ImageSource{
			Type:      ImageSourceTypeBase64,
			MediaType: "image/png",
			Data:      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
		},
	}
	if err := blk.Validate(); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

func TestValidate_UnknownTypePasses(t *testing.T) {
	// 未知类型（如 video、tool_use、tool_result）应放行：
	// 通用代理必须能 round-trip 不消费的块（保留其 raw JSON）
	for _, typ := range []string{"video", "tool_use", "tool_result"} {
		blk := &ContentBlock{Type: typ}
		if err := blk.Validate(); err != nil {
			t.Errorf("type %q should pass validation, got: %v", typ, err)
		}
	}
}

// --- ImageSource.Validate tests ---

func TestValidate_SourceTypeMissing(t *testing.T) {
	src := &ImageSource{MediaType: "image/png", Data: "abc"}
	err := src.Validate()
	if err == nil {
		t.Fatal("expected error for missing type")
	}
}

func TestValidate_SourceTypeInvalid(t *testing.T) {
	src := &ImageSource{Type: "url", MediaType: "image/png", Data: "abc"}
	err := src.Validate()
	if err == nil {
		t.Fatal("expected error for invalid source type")
	}
}

func TestValidate_MediaTypeInvalid(t *testing.T) {
	src := &ImageSource{Type: ImageSourceTypeBase64, MediaType: "image/bmp", Data: "abc"}
	err := src.Validate()
	if err == nil {
		t.Fatal("expected error for invalid media type")
	}
}

func TestValidate_MediaTypeValid(t *testing.T) {
	validTypes := []string{"image/png", "image/jpeg", "image/webp", "image/gif"}
	for _, mt := range validTypes {
		src := &ImageSource{Type: ImageSourceTypeBase64, MediaType: mt, Data: "abc"}
		if err := src.Validate(); err != nil {
			t.Errorf("media type %q should be valid, got: %v", mt, err)
		}
	}
}

func TestValidate_DataEmpty(t *testing.T) {
	src := &ImageSource{Type: ImageSourceTypeBase64, MediaType: "image/png"}
	err := src.Validate()
	if err == nil {
		t.Fatal("expected error for empty data")
	}
}

func TestValidate_DataWithDataPrefix(t *testing.T) {
	src := &ImageSource{
		Type:      ImageSourceTypeBase64,
		MediaType: "image/png",
		Data:      "data:image/png;base64,iVBORw0KGgo=",
	}
	err := src.Validate()
	if err == nil {
		t.Fatal("expected error for data: prefix")
	}
}

func TestValidate_ThinkingBlock(t *testing.T) {
	blk := &ContentBlock{Type: ContentTypeThinking}
	if err := blk.Validate(); err != nil {
		t.Errorf("thinking block should be valid, got: %v", err)
	}
}
