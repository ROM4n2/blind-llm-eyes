package messages

import (
	"fmt"
	"strings"
)

// 合法值白名单
const (
	RoleUser      = "user"
	RoleAssistant  = "assistant"

	ContentTypeText      = "text"
	ContentTypeImage     = "image"
	ContentTypeThinking  = "thinking"
	ContentTypeToolResult = "tool_result"

	ImageSourceTypeBase64 = "base64"
)

// validMediaTypes 允许的图片媒体类型
var validMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// validContentTypes 允许的 content block 类型
var validContentTypes = map[string]bool{
	ContentTypeText:    true,
	ContentTypeImage:   true,
	ContentTypeThinking: true,
}

// validRoles 允许的消息角色
var validRoles = map[string]bool{
	RoleUser:     true,
	RoleAssistant: true,
}

// Validate 校验 Request 结构的完整性和合法性。
// 返回第一个发现的错误（如有）。
func (r *Request) Validate() error {
	if r.Model == "" {
		return fmt.Errorf("request.model is required and must be non-empty")
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("request.messages is required and must contain at least one message")
	}
	for i, msg := range r.Messages {
		if err := msg.Validate(); err != nil {
			return fmt.Errorf("request.messages[%d]: %w", i, err)
		}
	}
	if r.MaxTokens < 0 {
		return fmt.Errorf("request.max_tokens must be non-negative, got %d", r.MaxTokens)
	}
	return nil
}

// Validate 校验 Message 结构。
func (m *Message) Validate() error {
	if m.Role == "" {
		return fmt.Errorf("message.role is required")
	}
	if !validRoles[m.Role] {
		return fmt.Errorf("message.role must be %q or %q, got %q", RoleUser, RoleAssistant, m.Role)
	}
	if len(m.Content) == 0 {
		return fmt.Errorf("message.content is required and must contain at least one content block")
	}
	for j, blk := range m.Content {
		if err := blk.Validate(); err != nil {
			return fmt.Errorf("message.content[%d]: %w", j, err)
		}
	}
	return nil
}

// Validate 校验 ContentBlock 结构。
func (b *ContentBlock) Validate() error {
	if b.Type == "" {
		return fmt.Errorf("content_block.type is required")
	}
	if !validContentTypes[b.Type] {
		allowed := make([]string, 0, len(validContentTypes))
		for t := range validContentTypes {
			allowed = append(allowed, t)
		}
		return fmt.Errorf("content_block.type must be one of %v, got %q", allowed, b.Type)
	}

	switch b.Type {
	case ContentTypeText:
		if b.Text == "" {
			return fmt.Errorf("text content block requires non-empty text field")
		}
	case ContentTypeImage:
		if b.Source == nil {
			return fmt.Errorf("image content block requires source field")
		}
		if err := b.Source.Validate(); err != nil {
			return fmt.Errorf("image source: %w", err)
		}
	}
	return nil
}

// Validate 校验 ImageSource 结构。
func (s *ImageSource) Validate() error {
	if s.Type == "" {
		return fmt.Errorf("source.type is required")
	}
	if s.Type != ImageSourceTypeBase64 {
		return fmt.Errorf("source.type must be %q, got %q", ImageSourceTypeBase64, s.Type)
	}
	if s.MediaType == "" {
		return fmt.Errorf("source.media_type is required")
	}
	if !validMediaTypes[s.MediaType] {
		allowed := make([]string, 0, len(validMediaTypes))
		for t := range validMediaTypes {
			allowed = append(allowed, t)
		}
		return fmt.Errorf("source.media_type must be one of %v, got %q", allowed, s.MediaType)
	}
	if s.Data == "" {
		return fmt.Errorf("source.data is required and must be non-empty base64 string")
	}
	// 简单检查：base64 数据不应包含空白或非法前缀
	data := strings.TrimSpace(s.Data)
	if data == "" {
		return fmt.Errorf("source.data contains only whitespace")
	}
	if strings.HasPrefix(data, "data:") {
		prefix := s.Data
		if len(prefix) > 50 {
			prefix = prefix[:50]
		}
		return fmt.Errorf("source.data should not include data: prefix, got %q", prefix)
	}
	return nil
}
