package messages

// FindImageBlocks 返回请求体中所有顶层 user message 里的 image 块指针。
// 返回的指针直接指向 req.Messages[i].Content[j]，修改 *block 即可原位替换。
func FindImageBlocks(req *Request) []*ContentBlock {
	var out []*ContentBlock
	for i := range req.Messages {
		if req.Messages[i].Role != "user" {
			continue
		}
		for j := range req.Messages[i].Content {
			blk := &req.Messages[i].Content[j]
			if blk.Type == "image" && blk.Source != nil && blk.Source.Data != "" {
				out = append(out, blk)
			}
		}
	}
	return out
}
