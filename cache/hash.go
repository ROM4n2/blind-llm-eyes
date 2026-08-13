package cache

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// HashFromBase64Data 接收 base64 字符串（不带前缀），解码后 sha256，返回 URL-safe base64 前 16 字节作为 key。
func HashFromBase64Data(base64Data string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	return HashFromRawBytes(raw), nil
}

// HashFromRawBytes 接收已解码的原始字节，计算 sha256 并返回 URL-safe base64 前 16 字节作为 key。
// 用于调用方已经解码过 base64 数据的场景，避免重复解码。
func HashFromRawBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(sum[:16])
}
