package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"time"

	_ "image/gif"
	_ "image/jpeg"

	"golang.org/x/image/webp"
)

// Client 调视觉后端（OpenAI 兼容 /v1/chat/completions）。
type Client struct {
	BaseURL              string
	APIKey               string
	Model                string
	DescriptionCap       int
	Timeout              time.Duration
	LargeTimeout         time.Duration
	LargeImageThreshold  int64
	SupportedFormats     map[string]bool
	HTTPClient           *http.Client
}

// NewClient 创建 Client，自动把 []string 格式列表转为 map。
func NewClient(baseURL, apiKey, model string, timeout, largeTimeout time.Duration, largeThreshold int64, descriptionCap int, supportedFormats []string) *Client {
	fmtMap := make(map[string]bool, len(supportedFormats))
	for _, f := range supportedFormats {
		fmtMap[f] = true
	}
	return &Client{
		BaseURL:              baseURL,
		APIKey:               apiKey,
		Model:                model,
		DescriptionCap:       descriptionCap,
		Timeout:              timeout,
		LargeTimeout:         largeTimeout,
		LargeImageThreshold:  largeThreshold,
		SupportedFormats:     fmtMap,
	}
}

// DescribeImage 把一张 base64 图片变成文字描述。
// imageSize 是原始 base64 解码后的字节数，用于决定超时。
func (c *Client) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	if c.BaseURL == "" || c.APIKey == "" || c.Model == "" {
		return "", fmt.Errorf("vision client not configured")
	}

	// 1) WebP → PNG 转换（MiMo 可能不完全支持 WebP 原生输入）
	actualMediaType := mediaType
	actualData := base64Data
	if mediaType == "image/webp" {
		convertedData, err := convertWebPToPNG(base64Data)
		if err != nil {
			return "", fmt.Errorf("webp→png conversion failed: %w", err)
		}
		actualData = convertedData
		actualMediaType = "image/png"
	}

	// 2) 格式校验（nil map 表示不过滤，向后兼容）
	if len(c.SupportedFormats) > 0 && !c.SupportedFormats[actualMediaType] {
		return "", fmt.Errorf("unsupported media type: %s (supported: png, jpeg, webp, gif)", mediaType)
	}

	// 3) 根据图片大小选择超时
	timeout := c.selectTimeout(imageSize)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	systemPrompt := "You are a visual description assistant. Describe the provided image in detail in Chinese. Focus on objects, layout, colors, text visible in the image, and any code snippets or UI elements. Keep the description under 400 words but be precise."

	reqBody := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": systemPrompt,
			},
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "image_url",
						"image_url": map[string]string{
							"url": fmt.Sprintf("data:%s;base64,%s", actualMediaType, actualData),
						},
					},
				},
			},
		},
		"max_tokens":  c.DescriptionCap,
		"temperature": 0.0,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal vision req: %w", err)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("build vision req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("vision http do: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read vision resp: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("vision resp %d: %s", resp.StatusCode, truncate(string(respBytes), 500))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("unmarshal vision resp: %w (raw=%s)", err, truncate(string(respBytes), 300))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("vision resp empty choices")
	}

	content := parsed.Choices[0].Message.Content
	if content == "" {
		content = parsed.Choices[0].Message.ReasoningContent
		if content != "" {
			content = truncate(content, c.DescriptionCap*2)
		}
	}
	if content == "" {
		return "", fmt.Errorf("vision resp empty choices (finish_reason=%s)", parsed.Choices[0].FinishReason)
	}
	return content, nil
}

// selectTimeout 根据图片大小选择合适的超时。
func (c *Client) selectTimeout(imageSize int64) time.Duration {
	if imageSize >= c.LargeImageThreshold && c.LargeTimeout > 0 {
		return c.LargeTimeout
	}
	return c.Timeout
}

// convertWebPToPNG 把 base64-encoded WebP 解码后编码为 PNG，返回 base64-encoded PNG。
func convertWebPToPNG(base64Data string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}

	img, err := webp.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode webp: %w", err)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode png: %w", err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}