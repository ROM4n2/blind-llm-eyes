package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log/slog"
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
	Log                  *slog.Logger
}

// NewClient 创建 Client，自动把 []string 格式列表转为 map。
func NewClient(baseURL, apiKey, model string, timeout, largeTimeout time.Duration, largeThreshold int64, descriptionCap int, supportedFormats []string, logger *slog.Logger) *Client {
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
		Log:                  logger,
	}
}

// DescribeImage 把一张 base64 图片变成文字描述。
// imageSize 是原始 base64 解码后的字节数，用于决定超时。
func (c *Client) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	if c.BaseURL == "" || c.APIKey == "" || c.Model == "" {
		return "", fmt.Errorf("vision client not configured")
	}

	start := time.Now()
	log := c.Log
	if log == nil {
		log = slog.Default()
	}

	// 0) 入口日志
	log.Debug("DescribeImage start",
		"media_type", mediaType,
		"image_size_bytes", imageSize,
		"base64_len", len(base64Data),
		"model", c.Model,
	)

	// 1) WebP → PNG 转换
	actualMediaType := mediaType
	actualData := base64Data
	if mediaType == "image/webp" {
		log.Debug("WebP detected, converting to PNG",
			"original_base64_len", len(base64Data),
		)
		convStart := time.Now()
		convertedData, err := convertWebPToPNG(base64Data)
		if err != nil {
			log.Error("WebP→PNG conversion failed",
				"err", err,
				"elapsed", time.Since(convStart).String(),
			)
			return "", fmt.Errorf("webp→png conversion failed: %w", err)
		}
		actualData = convertedData
		actualMediaType = "image/png"
		log.Info("WebP→PNG conversion done",
			"original_base64_len", len(base64Data),
			"converted_base64_len", len(convertedData),
			"compression_ratio", fmt.Sprintf("%.2f", float64(len(convertedData))/float64(len(base64Data))),
			"elapsed", time.Since(convStart).String(),
		)
	}

	// 2) 格式校验
	if len(c.SupportedFormats) > 0 && !c.SupportedFormats[actualMediaType] {
		log.Warn("unsupported media type rejected",
			"media_type", mediaType,
			"converted_type", actualMediaType,
			"supported", c.SupportedFormats,
		)
		return "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	// 3) 超时选择
	timeout := c.selectTimeout(imageSize)
	isLarge := imageSize >= c.LargeImageThreshold
	log.Debug("timeout selected",
		"image_size_bytes", imageSize,
		"threshold_bytes", c.LargeImageThreshold,
		"is_large_image", isLarge,
		"timeout", timeout.String(),
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 4) 构造请求
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
		log.Error("marshal vision request failed", "err", err)
		return "", fmt.Errorf("marshal vision req: %w", err)
	}
	log.Debug("vision request built",
		"payload_bytes", len(bodyBytes),
		"image_url_inlined", true,
		"max_tokens", c.DescriptionCap,
	)

	// 5) 发送 HTTP 请求
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		log.Error("build vision HTTP request failed", "err", err)
		return "", fmt.Errorf("build vision req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	log.Debug("sending vision HTTP request",
		"url", c.BaseURL+"/chat/completions",
		"timeout", timeout.String(),
		"payload_bytes", len(bodyBytes),
	)

	httpStart := time.Now()
	resp, err := httpClient.Do(req)
	httpElapsed := time.Since(httpStart)
	if err != nil {
		log.Error("vision HTTP request failed",
			"err", err,
			"elapsed", httpElapsed.String(),
			"context_deadline", ctx.Err() != nil,
		)
		return "", fmt.Errorf("vision http do: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("read vision response body failed",
			"err", err,
			"http_status", resp.StatusCode,
		)
		return "", fmt.Errorf("read vision resp: %w", err)
	}

	log.Debug("vision HTTP response received",
		"status_code", resp.StatusCode,
		"response_bytes", len(respBytes),
		"http_elapsed", httpElapsed.String(),
	)

	// 6) 处理错误响应
	if resp.StatusCode >= 400 {
		log.Error("vision API returned error",
			"status_code", resp.StatusCode,
			"response_body", truncate(string(respBytes), 500),
			"http_elapsed", httpElapsed.String(),
		)
		return "", fmt.Errorf("vision resp %d: %s", resp.StatusCode, truncate(string(respBytes), 500))
	}

	// 7) 解析响应
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
		log.Error("unmarshal vision response failed",
			"err", err,
			"raw_preview", truncate(string(respBytes), 300),
		)
		return "", fmt.Errorf("unmarshal vision resp: %w (raw=%s)", err, truncate(string(respBytes), 300))
	}
	if len(parsed.Choices) == 0 {
		log.Error("vision response has no choices",
			"raw_preview", truncate(string(respBytes), 300),
		)
		return "", fmt.Errorf("vision resp empty choices")
	}

	// 8) 提取描述（含 reasoning_content fallback）
	content := parsed.Choices[0].Message.Content
	finishReason := parsed.Choices[0].FinishReason
	usedReasoningFallback := false

	if content == "" {
		content = parsed.Choices[0].Message.ReasoningContent
		if content != "" {
			usedReasoningFallback = true
			content = truncate(content, c.DescriptionCap*2)
			log.Warn("vision content empty, using reasoning_content fallback",
				"reasoning_len", len(parsed.Choices[0].Message.ReasoningContent),
				"finish_reason", finishReason,
			)
		}
	}
	if content == "" {
		log.Error("vision response: both content and reasoning_content empty",
			"finish_reason", finishReason,
		)
		return "", fmt.Errorf("vision resp empty choices (finish_reason=%s)", finishReason)
	}

	log.Info("DescribeImage completed",
		"media_type", mediaType,
		"image_size_bytes", imageSize,
		"is_large_image", isLarge,
		"timeout_used", timeout.String(),
		"http_elapsed", httpElapsed.String(),
		"total_elapsed", time.Since(start).String(),
		"description_len", len(content),
		"finish_reason", finishReason,
		"used_reasoning_fallback", usedReasoningFallback,
		"description_preview", truncate(content, 80),
	)

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