package messages

import "encoding/json"

// ImageDescriptionMarker 包裹被替换的图像描述，让上游纯文本模型把它当作"自己看到的图像"。
// 选择 XML 风格是因为主流 LLM 对 XML 指令块的服从度远高于方括号或纯自然语言。
const ImageDescriptionMarker = "BLIND_LLM_EYES_IMAGE"

// WrapImageDescription 把视觉描述包成带明确语义指令的 text block。
func WrapImageDescription(description string) string {
	return "<" + ImageDescriptionMarker + ">This is an image the user sent. Treat the following description as your own visual observation of the image — do NOT say you cannot see it, do NOT invent missing details. If the description is insufficient to answer a question, say the information is not enough and ask for the original image.\n" + description + "\n</" + ImageDescriptionMarker + ">"
}

// ReplaceImageWithDescription 把一个 image ContentBlock 原位替换为 text 描述块。
// 调用方需保证 blk 来自 FindImageBlocks（即其 Type == "image" 且 Source != nil）。
func ReplaceImageWithDescription(blk *ContentBlock, description string) {
	blk.Type = "text"
	blk.Text = WrapImageDescription(description)
	blk.Source = nil

	// 重建 raw JSON，确保 Marshal 时输出的是新的 text block（不是旧的 image block）
	newRaw, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{
		Type: "text",
		Text: blk.Text,
	})
	if err == nil {
		blk.raw = newRaw
	}
}

// NormalizeSystemMessages 把 messages 数组中 role=="system" 的消息提取出来，
// 合并到顶层 system 字段。某些客户端（如 Claude Code）偶尔会将 system 消息
// 放入 messages 数组而非顶层 system 字段，这违反 Anthropic Messages API 规范，
// 会导致上游 400 错误。此函数在 Validate 之前调用以规范化请求结构。
// 返回被移动的 system 消息数量（>0 表示请求体需要重新序列化）。
func NormalizeSystemMessages(req *Request) int {
	if len(req.Messages) == 0 {
		return 0
	}
	var filtered []Message
	moved := 0
	for i := range req.Messages {
		if req.Messages[i].Role == "system" {
			// 把 system 消息的 content blocks 追加到顶层 system 字段
			req.System = append(req.System, req.Messages[i].Content...)
			moved++
		} else {
			filtered = append(filtered, req.Messages[i])
		}
	}
	if moved > 0 {
		req.Messages = filtered
	}
	return moved
}
