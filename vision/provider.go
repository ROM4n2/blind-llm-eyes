package vision

import "context"

// VisionProvider 是视觉描述服务的抽象接口。
// 任何视觉后端（MiMo、GPT-4o、Claude Vision 等）只要实现此接口即可无缝替换。
type VisionProvider interface {
	// DescribeImage 将一张 base64 编码的图片转换为文字描述。
	// imageSize 是原始字节数（base64 解码后），用于调用方决定超时策略。
	DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error)
}

// ContextualVisionProvider 是 VisionProvider 的可选扩展：支持带对话上下文描述图片。
// handler 和 pool 层会先断言此接口，成功则调用 DescribeImageWithContext，
// 失败则回退到 VisionProvider.DescribeImage（无上下文行为）。
// 这样 VisionProvider 接口零改动，所有现有 provider/mock 自动兼容。
type ContextualVisionProvider interface {
	// DescribeImageWithContext 带对话上下文描述图片。
	// contextText 是最近 N 轮对话的纯文本摘要，可为空（等价于 DescribeImage）。
	DescribeImageWithContext(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error)
}
