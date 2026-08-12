package messages

import (
	"encoding/json"
)

// SystemContent 兼容 system 字段的两种形态：
//  1. 新格式：[{"type":"text","text":"..."}, ...]
//  2. 旧格式："..." (纯字符串)
type SystemContent []ContentBlock

// UnmarshalJSON 让 SystemContent 同时接受 array 和 string。
func (s *SystemContent) UnmarshalJSON(b []byte) error {
	// 尝试按数组解析
	var blocks []ContentBlock
	if err := json.Unmarshal(b, &blocks); err == nil {
		*s = SystemContent(blocks)
		return nil
	}
	// 回退：按字符串解析
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		return err
	}
	if text == "" {
		*s = nil
		return nil
	}
	*s = SystemContent{{Type: "text", Text: text}}
	return nil
}

// Request 是 /v1/messages 的请求体（Claude Code 发来的格式）
type Request struct {
	Model     string        `json:"model"`
	Messages  []Message     `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	System    SystemContent `json:"system,omitempty"`
	Stream    bool          `json:"stream,omitempty"`
}

type Message struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content MessageContent `json:"content"`
}

// MessageContent 兼容 content 字段的两种形态：
//  1. 数组：[{"type":"text","text":"..."}, {"type":"image",...}]
//  2. 字符串："hello" (简写，等价于 [{"type":"text","text":"hello"}])
type MessageContent []ContentBlock

// UnmarshalJSON 让 MessageContent 同时接受 array 和 string。
func (m *MessageContent) UnmarshalJSON(b []byte) error {
	// 尝试按数组解析
	var blocks []ContentBlock
	if err := json.Unmarshal(b, &blocks); err == nil {
		*m = MessageContent(blocks)
		return nil
	}
	// 回退：按字符串解析
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		return err
	}
	if text == "" {
		*m = nil
		return nil
	}
	*m = MessageContent{{Type: "text", Text: text}}
	return nil
}

// ContentBlock 是请求/响应中的一个 content block。
// 注意：上游 CC Switch/Anthropic Messages API 会携带一些我们不关心的字段
// （例如 tool_use.id / input / name、thinking 块的 thinking 字段等）。
// 为避免 Unmarshal→Marshal 过程中丢失这些字段，我们用 raw 字段保存原始 JSON，
// 只在需要时解析关心的字段（Type/Text/Source）。
type ContentBlock struct {
	raw json.RawMessage // 原始 JSON，Marshal 时优先使用

	// 解析后的常用字段（非穷举）
	Type   string       `json:"-"`
	Text   string       `json:"-"`
	Source *ImageSource `json:"-"`
}

// UnmarshalJSON 保存原始 JSON，并解析常用字段。
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	b.raw = append(b.raw[:0], data...)

	// 解析常用字段到结构体字段，方便业务代码使用
	var aux struct {
		Type   string       `json:"type"`
		Text   string       `json:"text"`
		Source *ImageSource `json:"source"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	b.Type = aux.Type
	b.Text = aux.Text
	b.Source = aux.Source
	return nil
}

// MarshalJSON 输出保存的原始 JSON。
// 如果 raw 为空（极端情况），回退到结构体字段。
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.raw) > 0 {
		return b.raw, nil
	}
	type alias ContentBlock
	aux := struct {
		Type   string       `json:"type"`
		Text   string       `json:"text,omitempty"`
		Source *ImageSource `json:"source,omitempty"`
	}{
		Type:   b.Type,
		Text:   b.Text,
		Source: b.Source,
	}
	return json.Marshal(aux)
}

type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png" | "image/jpeg" | "image/webp"
	Data      string `json:"data"`       // base64 字符串，不带 data: 前缀
}
