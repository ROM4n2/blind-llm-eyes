package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/logging"
)

// OpenAIClient 是通用 OpenAI 兼容视觉后端客户端。
// 调用 /chat/completions 端点，使用 image_url 格式发送图片。
// 兼容 GPT-4o、GLM-4V、Qwen-VL 等任何遵循 OpenAI Chat Completions API 的视觉模型。
type OpenAIClient struct {
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

// NewOpenAIClient 创建 OpenAI 兼容客户端，自动把 []string 格式列表转为 map。
func NewOpenAIClient(baseURL, apiKey, model string, timeout, largeTimeout time.Duration, largeThreshold int64, descriptionCap int, supportedFormats []string, logger *slog.Logger) *OpenAIClient {
	fmtMap := make(map[string]bool, len(supportedFormats))
	for _, f := range supportedFormats {
		fmtMap[f] = true
	}
	return &OpenAIClient{
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

// DescribeImage 把一张 base64 图片变成文字描述（无上下文）。
// imageSize 是原始 base64 解码后的字节数，用于决定超时。
// 等价于 DescribeImageWithContext(ctx, base64Data, mediaType, imageSize, "").
func (c *OpenAIClient) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	return c.describeImageInternal(ctx, base64Data, mediaType, imageSize, "")
}

// GetBaseURL returns the configured BaseURL, implementing the BaseURLAware
// interface. The proxy's runtime self-loop guard uses this to reject vision
// calls that would loop back to the proxy itself.
func (c *OpenAIClient) GetBaseURL() string { return c.BaseURL }

// DescribeImageWithContext 带对话上下文描述图片，实现 ContextualVisionProvider 接口。
// contextText 为 "" 时行为完全等价于 DescribeImage。
func (c *OpenAIClient) DescribeImageWithContext(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error) {
	return c.describeImageInternal(ctx, base64Data, mediaType, imageSize, contextText)
}

// describeImageInternal 是描述图片的内部实现，被两个公开方法共用。
// 当 contextText != "" 时会在 user content 前插入上下文提示 text block。
func (c *OpenAIClient) describeImageInternal(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error) {
	if c.BaseURL == "" || c.APIKey == "" || c.Model == "" {
		return "", fmt.Errorf("openai vision client not configured")
	}

	nodeStart := time.Now()
	requestID := logging.GetRequestID(ctx)
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("node_name", "openai_vision", "request_id", requestID)

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
		"context_chars", len(contextText),
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

	// ── stage: 构造 OpenAI Chat Completions 请求 ──
	systemPrompt := "You are a visual description assistant. Describe the provided image in detail in Chinese. Focus on objects, layout, colors, text visible in the image, and any code snippets or UI elements. Keep the description under 400 words but be precise."

	// 构造 user content：如有上下文，先插入上下文 text block；再插入图片块；最后指令 text block
	userContent := make([]map[string]any, 0, 3)
	var contextBlockText string
	if contextText != "" {
		contextBlockText = "对话上下文（请结合以下对话内容更精准地描述图片，聚焦与上下文相关的细节，尤其是报错信息和代码片段）：\n" + contextText
		userContent = append(userContent, map[string]any{
			"type": "text",
			"text": contextBlockText,
		})
	}
	userContent = append(userContent, map[string]any{
		"type": "image_url",
		"image_url": map[string]string{
			"url": fmt.Sprintf("data:%s;base64,%s", actualMediaType, actualData),
		},
	})
	descInstruction := "Describe this image in detail."
	if contextText != "" {
		descInstruction = "Describe this image in detail, focusing on details relevant to the conversation context provided above (especially error messages, code snippets, and UI elements mentioned in the context)."
	}
	userContent = append(userContent, map[string]any{
		"type": "text",
		"text": descInstruction,
	})

	// 日志：打印完整文本 prompt（不含图片 base64），便于排查上下文拼接
	if c.Log != nil {
		c.Log.Info("vision prompt constructed",
			"stage", "prompt_built",
			"system_prompt", systemPrompt,
			"has_context_block", contextBlockText != "",
			"context_block_text", contextBlockText,
			"desc_instruction", descInstruction,
		)
	}

	reqBody := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": systemPrompt,
			},
			{
				"role":    "user",
				"content": userContent,
			},
		},
		"max_tokens":  c.DescriptionCap,
		"temperature": 0.0,
	}

	marshalStart := time.Now()
	bodyBytes, err := json.Marshal(reqBody)
	marshalMs := time.Since(marshalStart).Milliseconds()
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
	log.Debug("request body marshaled",
		"stage", "request_build_marshal",
		"status", "ok",
		"payload_bytes", len(bodyBytes),
		"duration_ms", marshalMs,
		"context_chars", len(contextText),
		"user_content_blocks", len(userContent),
	)

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
			"stage", "request_build_httpreq",
			"status", "error",
			"error_code", "http_req_build_failed",
			"error_message", err.Error(),
			"stack", logging.StackTrace(500),
		)
		return "", fmt.Errorf("build vision req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	// ── stage: HTTP 调用 ──
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

	bodyReadStart := time.Now()
	respBytes, err := io.ReadAll(resp.Body)
	bodyReadMs := time.Since(bodyReadStart).Milliseconds()
	if err != nil {
		log.Error("read vision response body failed",
			"stage", "http_response_received",
			"status", "error",
			"error_code", "read_body_failed",
			"error_message", err.Error(),
			"http_status", resp.StatusCode,
			"headers_duration_ms", httpMs,
			"body_read_duration_ms", bodyReadMs,
		)
		return "", fmt.Errorf("read vision resp: %w", err)
	}

	// ── stage: http_response_received ──
	totalHttpMs := httpMs + bodyReadMs
	log.Info("vision HTTP response received",
		"stage", "http_response_received",
		"status", "ok",
		"http_status_code", resp.StatusCode,
		"response_bytes", len(respBytes),
		"headers_duration_ms", httpMs,
		"body_read_duration_ms", bodyReadMs,
		"total_http_duration_ms", totalHttpMs,
	)

	// 处理错误响应
	if resp.StatusCode >= 400 {
		log.Error("vision API returned error",
			"stage", "http_response_received",
			"status", "error",
			"error_code", "api_error",
			"error_message", truncate(string(respBytes), 300),
			"http_status_code", resp.StatusCode,
			"headers_duration_ms", httpMs,
			"body_read_duration_ms", bodyReadMs,
		)
		return "", fmt.Errorf("vision resp %d: %s", resp.StatusCode, truncate(string(respBytes), 500))
	}

	// ── stage: response_parsing ──
	parseStart := time.Now()
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
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

	content := parsed.Choices[0].Message.Content
	if content == "" {
		log.Error("vision response content is empty",
			"stage", "response_parsing",
			"status", "error",
			"error_code", "empty_content",
			"error_message", "choices[0].message.content is empty",
			"duration_ms", time.Since(parseStart).Milliseconds(),
		)
		return "", fmt.Errorf("vision resp empty content")
	}

	parseMs := time.Since(parseStart).Milliseconds()

	// ── stage: node_complete ──
	totalMs := time.Since(nodeStart).Milliseconds()
	log.Info("DescribeImage completed",
		"stage", "node_complete",
		"status", "ok",
		"media_type", mediaType,
		"image_size_bytes", imageSize,
		"is_large_image", isLarge,
		"timeout_used", timeout.String(),
		"marshal_duration_ms", marshalMs,
		"headers_duration_ms", httpMs,
		"body_read_duration_ms", bodyReadMs,
		"total_http_duration_ms", httpMs+bodyReadMs,
		"parse_duration_ms", parseMs,
		"total_duration_ms", totalMs,
		"description_len", len(content),
		"description_preview", truncate(content, 80),
	)

	return content, nil
}

// selectTimeout 根据图片大小选择合适的超时。
func (c *OpenAIClient) selectTimeout(imageSize int64) time.Duration {
	if imageSize >= c.LargeImageThreshold && c.LargeTimeout > 0 {
		return c.LargeTimeout
	}
	return c.Timeout
}
