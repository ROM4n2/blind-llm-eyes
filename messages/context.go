package messages

import (
	"strings"
)

// ExtractConversationContext 从请求中提取最近 recentRounds 轮对话的纯文本。
// 跳过 image 块和 tool_result 中的 image 嵌套块，只收集 text 块。
// 按时间顺序拼接为 "[user] ...\n[assistant] ...\n[user] ..." 格式。
// maxChars 限制总长度，超出时以整轮次为单位截断早期对话（保留最新的）。
// recentRounds <= 0 或 messages 无对话历史时返回空字符串。
// 最后一条 user message 如果含有 image 块则整体跳过（避免把图片所在轮次的问题文本重复注入）。
func ExtractConversationContext(req *Request, recentRounds int, maxChars int) string {
	if req == nil || recentRounds <= 0 || maxChars <= 0 || len(req.Messages) == 0 {
		return ""
	}

	msgs := req.Messages
	n := len(msgs)

	// ── 判断最后一条是否是含 image 的 user，若是则它不参与上下文收集 ──
	lastIdx := n - 1
	excludeLast := false
	if msgs[lastIdx].Role == "user" && messageHasImage(&msgs[lastIdx]) {
		excludeLast = true
	}

	// ── 从尾部向前遍历，收集最近 recentRounds 轮的纯文本 ──
	// 一轮 = 若干条消息，但通常 user/assistant 交替。我们按"条"向前回溯，
	// 记录遇到的 user role 次数作为轮数计数器（每遇到一个 user 算一轮结束或开始）。
	// 简化实现：从尾向前，收集到的消息按顺序存入反转切片，直到 recentRounds 个 user 或遍历完。
	type collectedMsg struct {
		role string
		text string
	}
	var collected []collectedMsg
	userCount := 0

	startIdx := n - 1
	if excludeLast {
		startIdx = n - 2 // 跳过最后那条 user
	}
	for i := startIdx; i >= 0; i-- {
		msg := &msgs[i]
		role := strings.ToLower(msg.Role)
		if role != "user" && role != "assistant" {
			continue // 跳过 system 等其他 role
		}
		text := extractMessageText(msg)
		if text == "" {
			continue // 纯图片消息不贡献文本
		}
		collected = append(collected, collectedMsg{role: role, text: text})
		if role == "user" {
			userCount++
			if userCount >= recentRounds {
				break // 收集够了 recentRounds 个 user（及其之前的 assistant）
			}
		}
	}
	if len(collected) == 0 {
		return ""
	}

	// ── 反转回时间正序 ──
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}

	// ── 按 maxChars 从尾部开始累加，跳过超出的早期轮次（整轮次为单位） ──
	// 先把每条格式化为字符串
	formatted := make([]string, len(collected))
	for i, c := range collected {
		formatted[i] = "[" + c.role + "] " + c.text
	}

	// 从后向前累加长度
	total := 0
	cutFrom := 0 // 从 formatted[cutFrom:] 保留
	for i := len(formatted) - 1; i >= 0; i-- {
		lineLen := len(formatted[i]) + 1 // +1 for \n
		if total+lineLen > maxChars && i > 0 {
			// 再加入这条就超了（且不是最后一条，保证至少保留最新一条）
			cutFrom = i + 1
			break
		}
		total += lineLen
	}
	if cutFrom >= len(formatted) {
		cutFrom = len(formatted) - 1
	}

	// 拼接最终结果
	var sb strings.Builder
	for i := cutFrom; i < len(formatted); i++ {
		sb.WriteString(formatted[i])
		sb.WriteByte('\n')
	}
	result := sb.String()
	// 去掉末尾多余 \n
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	return result
}

// messageHasImage 检查一条消息是否包含 image 块（含 tool_result 嵌套）。
func messageHasImage(msg *Message) bool {
	for j := range msg.Content {
		if hasImageRecursive(&msg.Content[j], 0) {
			return true
		}
	}
	return false
}

func hasImageRecursive(blk *ContentBlock, depth int) bool {
	if blk == nil || depth >= maxCollectDepth {
		return false
	}
	if blk.Type == ContentTypeImage && blk.Source != nil && blk.Source.Data != "" {
		return true
	}
	if blk.Type == ContentTypeToolResult {
		for k := range blk.Content {
			if hasImageRecursive(&blk.Content[k], depth+1) {
				return true
			}
		}
	}
	return false
}

// extractMessageText 提取一条消息中所有 text 块（含 tool_result 内嵌套的 text），跳过 image。
func extractMessageText(msg *Message) string {
	var parts []string
	for j := range msg.Content {
		collectTextRecursive(&msg.Content[j], 0, &parts)
	}
	return strings.Join(parts, " ")
}

func collectTextRecursive(blk *ContentBlock, depth int, out *[]string) {
	if blk == nil || depth >= maxCollectDepth {
		return
	}
	if blk.Type == ContentTypeText && blk.Text != "" {
		*out = append(*out, blk.Text)
		return
	}
	// image 直接跳过
	if blk.Type == ContentTypeImage {
		return
	}
	// tool_result 深入收集 text（跳过嵌套 image）
	if blk.Type == ContentTypeToolResult {
		for k := range blk.Content {
			collectTextRecursive(&blk.Content[k], depth+1, out)
		}
	}
}
