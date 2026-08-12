package vision

import "context"

// VisionProvider 是视觉描述服务的抽象接口。
// 任何视觉后端（MiMo、GPT-4o、Claude Vision 等）只要实现此接口即可无缝替换。
type VisionProvider interface {
	// DescribeImage 将一张 base64 编码的图片转换为文字描述。
	// imageSize 是原始字节数（base64 解码后），用于调用方决定超时策略。
	DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error)
}