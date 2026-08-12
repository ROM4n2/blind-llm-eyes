package messages

// Request 是 /v1/messages 的请求体（Claude Code 发来的格式）
type Request struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Stream    bool      `json:"stream,omitempty"`
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
