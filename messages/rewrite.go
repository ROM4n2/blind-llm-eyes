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
