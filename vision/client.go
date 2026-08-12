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

	"github.com/ROM4n2/blind-llm-eyes/logging"
	"golang.org/x/image/webp"
)

// Client 调视觉后端（OpenAI 兼容 /v1/chat/completions）。
type Client struct {
	BaseURL             string
	APIKey              string
	Model               string
	DescriptionCap      int
	Timeout             time.Duration
	LargeTimeout        time.Duration
	LargeImageThreshold int64
	SupportedFormats    map[string]bool
	HTTPClient          *http.Client
	Log                 *slog.Logger
}

// NewClient 创建 Client，自动把 []string 格式列表转为 map。
func NewClient(baseURL, apiKey, model string, timeout, largeTimeout time.Duration, largeThreshold int64, descriptionCap int, supportedFormats []string, logger *slog.Logger) *Client {
	fmtMap := make(map[string]bool, len(supportedFormats))
	for _, f := range supportedFormats {
		fmtMap[f] = true
	}
	return &Client{
		BaseURL:             baseURL,
		APIKey:              apiKey,
		Model:               model,
		DescriptionCap:      descriptionCap,
		Timeout:             timeout,
		LargeTimeout:        largeTimeout,
		LargeImageThreshold: largeThreshold,
		SupportedFormats:    fmtMap,
		Log:                 logger,
	}
}

// DescribeImage 把一张 base64 图片变成文字描述。
// imageSize 是原始 base64 解码后的字节数，用于决定超时。
func (c *Client) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	if c.BaseURL == "" || c.APIKey == "" || c.Model == "" {
		return "", fmt.Errorf("vision client not configured")
	}

	nodeStart := time.Now()
	requestID := logging.GetRequestID(ctx)
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("node_name", "mimo_vision", "request_id", requestID)

	// ── stage: node_start ──
	log.Info("DescribeImage started",
		"stage", "node_start",
		"status", "info",
		"media_type", mediaType,
		"image_size_bytes", imageSize,
		"base64_len", len(base64Data),
		"model", c.Model,
		"timeout", c.Timeout.String(),
		"large_timeout", c.LargeTimeout.String(),
		"large_image_threshold", c.LargeImageThreshold,
	)

	// ── stage: preprocessing (WebP → PNG) ──
	actualMediaType := mediaType
	actualData := base64Data
	if mediaType == "image/webp" {
		convStart := time.Now()
		log.Info("WebP conversion started",
			"stage", "preprocessing",
			"status", "info",
			"original_base64_len", len(base64Data),
		)
		convertedData, err := convertWebPToPNG(base64Data)
		convMs := time.Since(convStart).Milliseconds()
		if err != nil {
			log.Error("WebP→PNG conversion failed",
				"stage", "preprocessing",
				"status", "error",
				"error_code", "webp_convert_failed",
				"error_message", err.Error(),
				"duration_ms", convMs,
				"stack", logging.StackTrace(500),
			)
			return "", fmt.Errorf("webp→png conversion failed: %w", err)
		}
		actualData = convertedData
		actualMediaType = "image/png"
		log.Info("WebP→PNG conversion complete",
			"stage", "preprocessing_complete",
			"status", "ok",
			"original_base64_len", len(base64Data),
			"converted_base64_len", len(convertedData),
			"compression_ratio", fmt.Sprintf("%.2f", float64(len(convertedData))/float64(len(base64Data))),
			"duration_ms", convMs,
		)
	}

	// 格式校验
	if len(c.SupportedFormats) > 0 && !c.SupportedFormats[actualMediaType] {
		log.Error("unsupported media type rejected",
			"stage", "preprocessing",
			"status", "error",
			"error_code", "unsupported_media_type",
			"error_message", fmt.Sprintf("unsupported media type: %s", mediaType),
			"media_type", mediaType,
			"converted_type", actualMediaType,
		)
		return "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	// ── 超时选择 ──
	timeout := c.selectTimeout(imageSize)
	isLarge := imageSize >= c.LargeImageThreshold

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// ── stage: 构造请求 ──
	systemPrompt := "You are a visual description assistant. Describe the provided image in detail in Chinese. Focus on objects, layout, colors, text visible in the image, and any code snippets or UI elements. Keep the description under 400 words but be precise."

	reqBody := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": []map[string]any{
				{
					"type": "image_url",
					"image_url": map[string]string{
						"url": fmt.Sprintf("data:%s;base64,%s", actualMediaType, actualData),
					},
				},
			}},
		},
		"max_tokens":  c.DescriptionCap,
		"temperature": 0.0,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		log.Error("marshal vision request failed",
			"stage", "request_build",
			"status", "error",
			"error_code", "marshal_failed",
			"error_message", err.Error(),
			"stack", logging.StackTrace(500),
		)
		return "", fmt.Errorf("marshal vision req: %w", err)
	}

	// ── stage: http_request_start ──
	log.Info("vision HTTP request sending",
		"stage", "http_request_start",
		"status", "info",
		"url", c.BaseURL+"/chat/completions",
		"timeout", timeout.String(),
		"payload_bytes", len(bodyBytes),
		"max_tokens", c.DescriptionCap,
	)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		log.Error("build vision HTTP request failed",
			"stage", "http_request_start",
			"status", "error",
			"error_code", "http_req_build_failed",
			"error_message", err.Error(),
			"stack", logging.StackTrace(500),
		)
		return "", fmt.Errorf("build vision req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	httpStart := time.Now()
	resp, err := httpClient.Do(req)
	httpElapsed := time.Since(httpStart)
	httpMs := httpElapsed.Milliseconds()
	if err != nil {
		log.Error("vision HTTP request failed",
			"stage", "http_response_received",
			"status", "error",
			"error_code", "http_request_failed",
			"error_message", err.Error(),
			"duration_ms", httpMs,
			"context_deadline", ctx.Err() != nil,
			"stack", logging.StackTrace(500),
		)
		return "", fmt.Errorf("vision http do: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("read vision response body failed",
			"stage", "http_response_received",
			"status", "error",
			"error_code", "read_body_failed",
			"error_message", err.Error(),
			"http_status", resp.StatusCode,
			"duration_ms", httpMs,
		)
		return "", fmt.Errorf("read vision resp: %w", err)
	}

	// ── stage: http_response_received ──
	log.Info("vision HTTP response received",
		"stage", "http_response_received",
		"status", "ok",
		"http_status_code", resp.StatusCode,
		"response_bytes", len(respBytes),
		"duration_ms", httpMs,
	)

	// 处理错误响应
	if resp.StatusCode >= 400 {
		log.Error("vision API returned error",
			"stage", "http_response_received",
			"status", "error",
			"error_code", "api_error",
			"error_message", truncate(string(respBytes), 300),
			"http_status_code", resp.StatusCode,
			"duration_ms", httpMs,
		)
		return "", fmt.Errorf("vision resp %d: %s", resp.StatusCode, truncate(string(respBytes), 500))
	}

	// ── stage: response_parsing ──
	parseStart := time.Now()
	log.Debug("response parsing started",
		"stage", "response_parsing",
		"status", "info",
		"response_bytes", len(respBytes),
	)

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
		parseMs := time.Since(parseStart).Milliseconds()
		log.Error("unmarshal vision response failed",
			"stage", "response_parsing",
			"status", "error",
			"error_code", "unmarshal_failed",
			"error_message", err.Error(),
			"raw_preview", truncate(string(respBytes), 200),
			"duration_ms", parseMs,
			"stack", logging.StackTrace(500),
		)
		return "", fmt.Errorf("unmarshal vision resp: %w (raw=%s)", err, truncate(string(respBytes), 300))
	}
	if len(parsed.Choices) == 0 {
		parseMs := time.Since(parseStart).Milliseconds()
		log.Error("vision response has no choices",
			"stage", "response_parsing",
			"status", "error",
			"error_code", "empty_choices",
			"error_message", "vision response returned 0 choices",
			"raw_preview", truncate(string(respBytes), 200),
			"duration_ms", parseMs,
		)
		return "", fmt.Errorf("vision resp empty choices")
	}

	parseMs := time.Since(parseStart).Milliseconds()

	// 提取描述（含 reasoning_content fallback）
	content := parsed.Choices[0].Message.Content
	finishReason := parsed.Choices[0].FinishReason
	usedReasoningFallback := false

	if content == "" {
		content = parsed.Choices[0].Message.ReasoningContent
		if content != "" {
			usedReasoningFallback = true
			content = truncate(content, c.DescriptionCap*2)
			log.Warn("vision content empty, using reasoning_content fallback",
				"stage", "response_parsing",
				"status", "warning",
				"reasoning_len", len(parsed.Choices[0].Message.ReasoningContent),
				"finish_reason", finishReason,
				"duration_ms", parseMs,
			)
		}
	}
	if content == "" {
		log.Error("vision response: both content and reasoning_content empty",
			"stage", "response_parsing",
			"status", "error",
			"error_code", "empty_content",
			"error_message", "both content and reasoning_content are empty",
			"finish_reason", finishReason,
			"duration_ms", parseMs,
		)
		return "", fmt.Errorf("vision resp empty choices (finish_reason=%s)", finishReason)
	}

	// ── stage: response_parsed ──
	log.Info("response parsed successfully",
		"stage", "response_parsed",
		"status", "ok",
		"description_len", len(content),
		"finish_reason", finishReason,
		"used_reasoning_fallback", usedReasoningFallback,
		"duration_ms", parseMs,
	)

	// ── stage: node_complete ──
	totalMs := time.Since(nodeStart).Milliseconds()
	log.Info("DescribeImage completed",
		"stage", "node_complete",
		"status", "ok",
		"media_type", mediaType,
		"image_size_bytes", imageSize,
		"is_large_image", isLarge,
		"timeout_used", timeout.String(),
		"http_duration_ms", httpMs,
		"parse_duration_ms", parseMs,
		"total_duration_ms", totalMs,
		"description_len", len(content),
		"finish_reason", finishReason,
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
