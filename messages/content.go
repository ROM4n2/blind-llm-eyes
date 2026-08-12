package messages

import (
	"encoding/json"
)

// SystemContent 兼容 system 字段的两种形态：
//   1) 新格式：[{"type":"text","text":"..."}, ...]
//   2) 旧格式："..." (纯字符串)
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
	Content []ContentBlock `json:"content"`
}

type ContentBlock struct {
	Type   string       `json:"type"` // "text" | "image" | "tool_use" | "tool_result"
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"` // image 块专用
}

type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png" | "image/jpeg" | "image/webp"
	Data      string `json:"data"`       // base64 字符串，不带 data: 前缀
}
