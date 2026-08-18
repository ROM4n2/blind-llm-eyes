package vision

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
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

// DescribeImage 把一张 base64 图片变成文字描述（无上下文）。
// imageSize 是原始 base64 解码后的字节数，用于决定超时。
// 等价于 DescribeImageWithContext(ctx, base64Data, mediaType, imageSize, "").
func (c *Client) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	return c.describeImageInternal(ctx, base64Data, mediaType, imageSize, "")
}

// GetBaseURL returns the configured BaseURL, implementing the BaseURLAware
// interface. The proxy's runtime self-loop guard uses this to reject vision
// calls that would loop back to the proxy itself.
func (c *Client) GetBaseURL() string { return c.BaseURL }

// DescribeImageWithContext 带对话上下文描述图片，实现 ContextualVisionProvider 接口。
// contextText 为 "" 时行为完全等价于 DescribeImage。
func (c *Client) DescribeImageWithContext(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error) {
	return c.describeImageInternal(ctx, base64Data, mediaType, imageSize, contextText)
}

// describeImageInternal 是描述图片的内部实现，被两个公开方法共用。
// 当 contextText != "" 时会在 user content 前插入上下文提示 text block，
// 让视觉模型聚焦于与对话相关的细节。
func (c *Client) describeImageInternal(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error) {
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

	// ── stage: 构造请求 (Anthropic Messages API 格式) ──
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
	userContent = append(userContent,
		map[string]any{
			"type": "image",
			"source": map[string]string{
				"type":       "base64",
				"media_type": actualMediaType,
				"data":       actualData,
			},
		},
	)
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
		"model":  c.Model,
		"system": systemPrompt,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": userContent,
			},
		},
		"max_tokens":  c.DescriptionCap,
		"temperature": 0.0,
		"thinking": map[string]string{
			"type": "disabled", // 关闭 MiMo 思考模式，跳过 reasoning_content 生成
		},
	}

	// ── stage: request_build (marshal) ──
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
		"url", c.BaseURL+"/v1/messages",
		"timeout", timeout.String(),
		"payload_bytes", len(bodyBytes),
		"max_tokens", c.DescriptionCap,
	)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	// ── stage: request_build (http.Request) ──
	reqBuildStart := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(bodyBytes))
	reqBuildMs := time.Since(reqBuildStart).Milliseconds()
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
	log.Debug("http.Request object built",
		"stage", "request_build_httpreq",
		"status", "ok",
		"duration_ms", reqBuildMs,
	)

	// ── httptrace: 捕获连接各阶段耗时 ──
	var dnsStart, connectStart, tlsStart, gotConnTime, wroteReqTime time.Time

	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStart = time.Now()
			log.Debug("DNS lookup started",
				"stage", "dns_lookup",
				"host", info.Host,
			)
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			var ms int64
			if !dnsStart.IsZero() {
				ms = time.Since(dnsStart).Milliseconds()
			}
			log.Info("DNS lookup completed",
				"stage", "dns_lookup",
				"status", "ok",
				"addrs", len(info.Addrs),
				"duration_ms", ms,
			)
		},
		ConnectStart: func(network, addr string) {
			connectStart = time.Now()
			log.Debug("TCP connect started",
				"stage", "tcp_connect",
				"addr", addr,
			)
		},
		ConnectDone: func(network, addr string, err error) {
			var ms int64
			if !connectStart.IsZero() {
				ms = time.Since(connectStart).Milliseconds()
			}
			log.Info("TCP connect completed",
				"stage", "tcp_connect",
				"status", "ok",
				"addr", addr,
				"duration_ms", ms,
			)
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
			log.Debug("TLS handshake started",
				"stage", "tls_handshake",
			)
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			var ms int64
			if !tlsStart.IsZero() {
				ms = time.Since(tlsStart).Milliseconds()
			}
			log.Info("TLS handshake completed",
				"stage", "tls_handshake",
				"status", "ok",
				"duration_ms", ms,
				"tls_version", state.Version,
			)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			gotConnTime = time.Now()
			log.Info("connection established",
				"stage", "got_conn",
				"status", "ok",
				"reused", info.Reused,
				"was_idle", info.WasIdle,
				"remote_addr", info.Conn.RemoteAddr().String(),
			)
		},
		WroteRequest: func(wr httptrace.WroteRequestInfo) {
			wroteReqTime = time.Now()
			log.Debug("request body fully sent",
				"stage", "wrote_request",
				"err", wr.Err,
			)
		},
		GotFirstResponseByte: func() {
			firstByte := time.Now()
			var ttfbMs int64
			if !wroteReqTime.IsZero() {
				ttfbMs = firstByte.Sub(wroteReqTime).Milliseconds()
			}
			log.Info("first response byte received",
				"stage", "first_byte",
				"status", "ok",
				"ttfb_ms", ttfbMs,
			)
		},
	}

	traceCtx := httptrace.WithClientTrace(req.Context(), trace)
	req = req.WithContext(traceCtx)

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
	log.Debug("response parsing started",
		"stage", "response_parsing",
		"status", "info",
		"response_bytes", len(respBytes),
	)

	var parsed struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
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
	if len(parsed.Content) == 0 {
		parseMs := time.Since(parseStart).Milliseconds()
		log.Error("vision response has no content blocks",
			"stage", "response_parsing",
			"status", "error",
			"error_code", "empty_content",
			"error_message", "vision response returned 0 content blocks",
			"raw_preview", truncate(string(respBytes), 200),
			"duration_ms", parseMs,
		)
		return "", fmt.Errorf("vision resp empty content")
	}

	parseMs := time.Since(parseStart).Milliseconds()

	// 提取描述（从 Anthropic content blocks 中聚合 text 和 thinking）
	var content, reasoningContent string
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			content += block.Text
		case "thinking":
			reasoningContent += block.Thinking
		}
	}
	finishReason := parsed.StopReason
	usedReasoningFallback := false

	log.Debug("response fields extracted",
		"stage", "response_parsed",
		"content_len", len(content),
		"reasoning_content_len", len(reasoningContent),
		"finish_reason", finishReason,
	)

	if content == "" && reasoningContent != "" {
		content = reasoningContent
		usedReasoningFallback = true
		content = truncate(content, c.DescriptionCap*2)
		log.Warn("vision text empty, using thinking fallback",
			"stage", "response_parsing",
			"status", "warning",
			"reasoning_len", len(reasoningContent),
			"finish_reason", finishReason,
			"duration_ms", parseMs,
		)
	}
	if content == "" {
		log.Error("vision response: both text and thinking empty",
			"stage", "response_parsing",
			"status", "error",
			"error_code", "empty_content",
			"error_message", "both text and thinking blocks are empty",
			"finish_reason", finishReason,
			"duration_ms", parseMs,
		)
		return "", fmt.Errorf("vision resp empty content (stop_reason=%s)", finishReason)
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
	var connReadyMs int64
	if !gotConnTime.IsZero() {
		connReadyMs = gotConnTime.Sub(httpStart).Milliseconds()
	}
	log.Info("DescribeImage completed",
		"stage", "node_complete",
		"status", "ok",
		"media_type", mediaType,
		"image_size_bytes", imageSize,
		"is_large_image", isLarge,
		"timeout_used", timeout.String(),
		"marshal_duration_ms", marshalMs,
		"req_build_duration_ms", reqBuildMs,
		"conn_ready_ms", connReadyMs,
		"headers_duration_ms", httpMs,
		"body_read_duration_ms", bodyReadMs,
		"total_http_duration_ms", httpMs+bodyReadMs,
		"parse_duration_ms", parseMs,
		"total_duration_ms", totalMs,
		"description_len", len(content),
		"reasoning_content_len", len(reasoningContent),
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
