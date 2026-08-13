package messages

// FindImageBlocks 返回请求体中所有 user message 里的 image 块指针，
// 含 tool_result 内嵌的（真实 Claude Code 截图多藏在此）。指针直接指向切片元素，
// 修改 *block 即原位替换。
func FindImageBlocks(req *Request) []*ContentBlock {
	var out []*ContentBlock
	for i := range req.Messages {
		if req.Messages[i].Role != "user" {
			continue
		}
		for j := range req.Messages[i].Content {
			collectImageBlocks(&req.Messages[i].Content[j], &out)
		}
	}
	return out
}

// collectImageBlocks 递归收集 image 块；只深入 tool_result（tool_use 极少内嵌图，不处理）。
func collectImageBlocks(blk *ContentBlock, out *[]*ContentBlock) {
	if blk.Type == "image" && blk.Source != nil && blk.Source.Data != "" {
		*out = append(*out, blk)
		return
	}
	if blk.Type == ContentTypeToolResult {
		for k := range blk.Content {
			collectImageBlocks(&blk.Content[k], out)
		}
	}
}
