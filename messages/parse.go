package messages

// maxCollectDepth 限制 collectImageBlocks 的递归深度，防止恶意构造的
// 深层嵌套 tool_result 导致栈溢出。真实 Claude Code 请求嵌套通常不超过 2-3 层。
const maxCollectDepth = 16

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
			collectImageBlocks(&req.Messages[i].Content[j], &out, 0)
		}
	}
	return out
}

// collectImageBlocks 递归收集 image 块；只深入 tool_result（tool_use 极少内嵌图，不处理）。
// depth 限制递归深度，超过 maxCollectDepth 后停止下探，避免栈溢出。
func collectImageBlocks(blk *ContentBlock, out *[]*ContentBlock, depth int) {
	if blk.Type == "image" && blk.Source != nil && blk.Source.Data != "" {
		*out = append(*out, blk)
		return
	}
	if blk.Type == ContentTypeToolResult && depth < maxCollectDepth {
		for k := range blk.Content {
			collectImageBlocks(&blk.Content[k], out, depth+1)
		}
	}
}
