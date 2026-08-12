package messages

// ReplaceImageWithDescription 把一个 image ContentBlock 原位替换为 text 描述块。
// 调用方需保证 blk 来自 FindImageBlocks（即其 Type == "image" 且 Source != nil）。
func ReplaceImageWithDescription(blk *ContentBlock, description string) {
	blk.Type = "text"
	blk.Text = "[Image Description: " + description + "]"
	blk.Source = nil
}
