package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/logging"
	"github.com/ROM4n2/blind-llm-eyes/messages"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
	"github.com/ROM4n2/blind-llm-eyes/modelutil"
	"github.com/ROM4n2/blind-llm-eyes/vision"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

// HandlerDeps 是 Handler 的依赖（用 struct 注入，方便测试替换 mock）。
type HandlerDeps struct {
	UpstreamBaseURL     string
	UpstreamAPIKey      string
	VisionProvider      vision.VisionProvider
	Cache               cache.Cache
	FailOpen            bool
	LargeImageThreshold int64
	MaxBodyBytes        int64                // 请求体上限字节，<=0 时 NewHandler 兜底 20MB
	ConcurrencyLimit    int                  // 单请求并发 vision 上限，<=0 时 NewHandler 兜底为 4
	AdaptiveConcurrency *AdaptiveConcurrency // 自适应限流控制器；nil 等价于 static 行为
	ContextRounds       int                  // P5：最近 N 轮对话，<=0 时禁用上下文感知（>0 才提取）
	ContextMaxChars     int                  // P5：上下文最大字符数，<=0 时 NewHandler 兜底 2000
	Log                 *slog.Logger
	WG                  *sync.WaitGroup
	Metrics             *metrics.Metrics // 可选：Prometheus 指标
	// VisionCapableModels lists upstream models that natively support image
	// input. When req.Model (after sanitization) matches this set, the proxy
	// skips image rewriting entirely and forwards the body verbatim — saving
	// a vision API call and latency. Match is case-insensitive. nil/empty =
	// never skip (always rewrite, the default behavior).
	VisionCapableModels map[string]bool
	// ListenAddr is the proxy's own listen address (e.g. "127.0.0.1:8790").
	// Used to detect and reject self-referential upstream URLs that would
	// cause infinite forwarding loops.
	ListenAddr string
}

// NewHandler 返回一个标准 http.Handler，处理 /v1/messages 所有请求。
func NewHandler(deps HandlerDeps) http.Handler {
	if deps.UpstreamBaseURL == "" {
		panic("NewHandler: UpstreamBaseURL must not be empty")
	}
	// Self-loop protection: reject upstream URLs that point back to the proxy
	// itself. If ListenAddr is set, validate at construction time.
	if deps.ListenAddr != "" && isSelfReferentialURL(deps.UpstreamBaseURL, deps.ListenAddr) {
		panic(fmt.Sprintf("NewHandler: UpstreamBaseURL (%s) points to the proxy's own listen address (%s). This causes infinite self-forwarding loops. Use a real upstream API endpoint.", deps.UpstreamBaseURL, deps.ListenAddr))
	}
	if deps.WG == nil {
		deps.WG = &sync.WaitGroup{}
	}
	if deps.ConcurrencyLimit <= 0 {
		deps.ConcurrencyLimit = 4
	}
	if deps.MaxBodyBytes <= 0 {
		deps.MaxBodyBytes = 20 << 20 // 20MB
	}
	if deps.LargeImageThreshold <= 0 {
		deps.LargeImageThreshold = 1 << 20 // 1MB
	}
	// ContextRounds：0 = 禁用上下文感知；正数 = N 轮；负数规范化为 0（兼容 yaml 写 -1）
	if deps.ContextRounds < 0 {
		deps.ContextRounds = 0 // 规范化：负数统一视为 0（禁用），避免 ExtractConversationContext 参数混淆
	}
	if deps.ContextMaxChars <= 0 {
		deps.ContextMaxChars = 2000
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.VisionProvider == nil {
		panic("NewHandler: VisionProvider must not be nil")
	}
	// Self-loop protection: reject vision provider base_urls that point back
	// to the proxy itself. MiMo Client calls /v1/messages (same as proxy path),
	// so a self-referential vision base_url causes infinite forwarding loops.
	// The check uses the optional BaseURLAware interface; providers that don't
	// implement it skip this check (falling back to doctor/setup detection).
	if deps.ListenAddr != "" {
		if ba, ok := deps.VisionProvider.(vision.BaseURLAware); ok {
			visionURL := ba.GetBaseURL()
			if isSelfReferentialURL(visionURL, deps.ListenAddr) {
				panic(fmt.Sprintf("NewHandler: VisionProvider base_url (%s) points to the proxy's own listen address (%s). This causes infinite self-forwarding loops (vision calls /v1/messages on the proxy). Use a real vision API endpoint.", visionURL, deps.ListenAddr))
			}
		}
	}
	if deps.Cache == nil {
		deps.Cache = cache.NewLRU(1000)
	}
	if deps.AdaptiveConcurrency == nil {
		deps.AdaptiveConcurrency = NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
			Enabled:      false,
			InitialLimit: deps.ConcurrencyLimit,
			MinLimit:     deps.ConcurrencyLimit,
			MaxLimit:     deps.ConcurrencyLimit,
		}, deps.Metrics, deps.Log)
	}
	// Normalize VisionCapableModels to lowercase keys for case-insensitive
	// matching. A nil/empty map means "never skip" (default behavior).
	if len(deps.VisionCapableModels) > 0 {
		normalized := make(map[string]bool, len(deps.VisionCapableModels))
		for k, v := range deps.VisionCapableModels {
			normalized[strings.ToLower(k)] = v
		}
		deps.VisionCapableModels = normalized
	}

	// 自定义 HTTP 客户端：连接超时 30s，连接池复用，空闲连接 90s 超时
	// 不设整体请求超时以支持 SSE 长流式响应，依赖 r.Context() 的取消语义
	upstreamClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}

	mux := http.NewServeMux()
	h := &requestHandler{deps: deps, client: upstreamClient}
	mux.HandleFunc("/v1/messages", h.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", h.handleCountTokens)
	return mux
}

// Shutdown 等待所有在途请求完成。
func Shutdown(deps HandlerDeps) {
	if deps.WG != nil {
		deps.WG.Wait()
	}
}

// visionResult 封装 singleflight vision 调用的返回值，
// 将耗时数据（FnStart/FnEnd）与业务数据（Desc/Err）一起传递，
// 消除 singleflight executor 与 waiter 之间的数据竞争。
type visionResult struct {
	Desc    string
	Err     error
	FnStart time.Time
	FnEnd   time.Time
}

type requestHandler struct {
	deps   HandlerDeps
	sf     singleflight.Group // 进程级 in-flight 去重，跨请求合并同 hash vision 调用
	client *http.Client       // 上游 HTTP 客户端，带连接超时和连接池
}

// handleCountTokens forwards POST /v1/messages/count_tokens to upstream
// verbatim — no image rewriting, no vision calls, no caching. Claude Code
// calls this endpoint to display token counts in its UI; a 404 breaks the
// counter. The handler reads the body, forwards it with the same headers
// (plus upstream API key if configured), and copies the response back.
func (h *requestHandler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if h.deps.WG != nil {
		h.deps.WG.Add(1)
		defer h.deps.WG.Done()
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.deps.MaxBodyBytes))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	upstreamURL := h.deps.UpstreamBaseURL + "/v1/messages/count_tokens"
	// Runtime self-loop guard: if the constructor check was bypassed (e.g.
	// ListenAddr was empty at construction), catch self-referential URLs
	// before they cause infinite forwarding.
	if h.deps.ListenAddr != "" && isSelfReferentialURL(upstreamURL, h.deps.ListenAddr) {
		http.Error(w, "upstream URL points to the proxy itself (loop detected). Check your upstream.base_url config.", http.StatusLoopDetected)
		return
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Forward headers (strip sensitive ones, same as handleMessages).
	for k, vs := range r.Header {
		if shouldStripHeader(k, h.deps.UpstreamAPIKey != "") {
			continue
		}
		for _, v := range vs {
			upstreamReq.Header.Add(k, v)
		}
	}
	if h.deps.UpstreamAPIKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+h.deps.UpstreamAPIKey)
	}
	upstreamReq.ContentLength = int64(len(body))

	resp, err := h.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy upstream response headers and body verbatim.
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *requestHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	// Graceful shutdown: track in-flight request
	if h.deps.WG != nil {
		h.deps.WG.Add(1)
		defer h.deps.WG.Done()
	}

	requestStart := time.Now()
	requestID := logging.NewRequestID()
	log := h.deps.Log.With("node_name", "proxy", "request_id", requestID)
	route := "/v1/messages"

	// 用于记录最终状态码的闭包
	statusCode := http.StatusOK

	log.Info("request received",
		"stage", "request_start",
		"method", r.Method,
		"path", r.URL.Path,
		"content_length", r.ContentLength,
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
		"has_auth", r.Header.Get("Authorization") != "",
	)

	if r.Method != http.MethodPost {
		log.Warn("method not allowed",
			"method", r.Method,
			"path", r.URL.Path,
		)
		statusCode = http.StatusMethodNotAllowed
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
		return
	}

	// 1) 读原始 body（加上限保护）
	readStart := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, h.deps.MaxBodyBytes)
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			log.Error("request body too large",
				"err", err,
				"max_bytes", h.deps.MaxBodyBytes,
			)
			statusCode = http.StatusRequestEntityTooLarge
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
			return
		}
		log.Error("read request body failed",
			"err", err,
			"read_elapsed", time.Since(readStart).String(),
		)
		statusCode = http.StatusBadRequest
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
		return
	}
	log.Info("request body read",
		"stage", "body_read_complete",
		"body_bytes", len(rawBody),
		"max_body_bytes", h.deps.MaxBodyBytes,
		"read_elapsed_ms", time.Since(readStart).Milliseconds(),
	)

	// 2) 解析 JSON
	var req messages.Request
	parseErr := json.Unmarshal(rawBody, &req)

	var rewritten atomic.Int64
	var cached atomic.Int64
	var failed atomic.Int64

	// 缓存命中率跟踪
	var totalLookups atomic.Int64
	var cacheHits atomic.Int64

	// passthrough = true 当上游模型原生支持图片输入（在 vision_capable_models
	// 白名单中），跳过整个图片改写阶段，直接透传请求体。
	passthrough := false

	if parseErr == nil {
		// ── Model sanitization: strip trailing [1m]/[1M]/[1K] suffixes ──
		// DeepSeek and other vendors may append context-length markers to the
		// model name; these must not be forwarded to the upstream endpoint.
		origModel := req.Model
		req.Model = modelutil.SanitizeModel(req.Model)
		modelSanitized := req.Model != origModel

		// 统计各角色消息数、content block 类型分布、tool_result 块数量
		// 合并为一次遍历，避免后续重复迭代 req.Messages
		roleCounts := map[string]int{}
		blockTypeCounts := map[string]int{}
		toolResultCount := 0
		for i := range req.Messages {
			roleCounts[req.Messages[i].Role]++
			for j := range req.Messages[i].Content {
				t := req.Messages[i].Content[j].Type
				blockTypeCounts[t]++
				if t == messages.ContentTypeToolResult {
					toolResultCount++
				}
			}
		}
		log.Info("request JSON parsed",
			"stage", "json_parse_complete",
			"model", req.Model,
			"model_sanitized", modelSanitized,
			"messages", len(req.Messages),
			"system_blocks", len(req.System),
			"max_tokens", req.MaxTokens,
			"stream", req.Stream,
			"role_counts", roleCounts,
			"block_type_counts", blockTypeCounts,
			"tool_result_blocks", toolResultCount,
			"body_bytes", len(rawBody),
		)

		// ── Vision-capable model whitelist (passthrough) ──
		// If the upstream model natively supports image input, skip the
		// entire rewrite phase and forward the body verbatim (only
		// re-marshal if the model name was sanitized). Saves a vision API
		// call + ~8s latency for models like gpt-4o.
		if len(h.deps.VisionCapableModels) > 0 && h.deps.VisionCapableModels[strings.ToLower(req.Model)] {
			passthrough = true
			log.Info("vision-capable model, skipping image rewrite (passthrough)",
				"stage", "whitelist_passthrough",
				"status", "passthrough",
				"model", req.Model,
			)
		}

		// 2.4) 规范化：把 messages 数组中 role=="system" 的消息提取到顶层 system 字段
		systemMoved := 0
		if !passthrough {
			systemMoved = messages.NormalizeSystemMessages(&req)
			if systemMoved > 0 {
				log.Info("system messages normalized",
					"stage", "system_normalize_complete",
					"status", "moved",
					"moved_count", systemMoved,
					"messages_after", len(req.Messages),
					"system_blocks_after", len(req.System),
				)
			} else {
				log.Info("system messages normalized",
					"stage", "system_normalize_complete",
					"status", "noop",
					"moved_count", 0,
					"messages", len(req.Messages),
					"system_blocks", len(req.System),
				)
			}

			// 2.5) 校验请求结构
			if verr := req.Validate(); verr != nil {
				log.Warn("request validation failed",
					"stage", "validate_complete",
					"status", "failed",
					"err", verr,
					"body_bytes", len(rawBody),
				)
				statusCode = http.StatusBadRequest
				http.Error(w, "validation failed: "+verr.Error(), http.StatusBadRequest)
				h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
				return
			}
			log.Info("request validation passed",
				"stage", "validate_complete",
				"status", "ok",
				"messages", len(req.Messages),
			)

			// 3) 找图
			imgs := messages.FindImageBlocks(&req)
			var totalImageBytes int64
			// 预解码所有图片的 base64 数据，后续 hash 和 goroutine 复用，避免重复解码
			decodedImages := make([][]byte, len(imgs))
			decodeErrors := make([]error, len(imgs))
			for idx, blk := range imgs {
				decodedImages[idx], decodeErrors[idx] = base64.StdEncoding.DecodeString(blk.Source.Data)
				totalImageBytes += int64(len(decodedImages[idx]))
			}
			// toolResultCount 已在 json_parse_complete 阶段统计，无需重复遍历
			log.Info("image blocks found in request",
				"stage", "find_images_complete",
				"count", len(imgs),
				"total_image_bytes", totalImageBytes,
				"is_large_request", totalImageBytes >= h.deps.LargeImageThreshold,
				"tool_result_blocks", toolResultCount,
				"has_nested_source", toolResultCount > 0,
			)

			// 3.5) P5：预提取对话上下文（所有图片共享同一份上下文，只读不写无并发安全问题）
			var contextText string
			if h.deps.ContextRounds > 0 && len(req.Messages) > 0 {
				ctxStart := time.Now()
				contextText = messages.ExtractConversationContext(&req, h.deps.ContextRounds, h.deps.ContextMaxChars)
				if len(contextText) > 0 {
					log.Info("conversation context extracted",
						"stage", "context_extract_complete",
						"status", "ok",
						"context_rounds_config", h.deps.ContextRounds,
						"context_max_chars_config", h.deps.ContextMaxChars,
						"context_chars", len(contextText),
						"message_count", len(req.Messages),
						"duration_ms", time.Since(ctxStart).Milliseconds(),
					)
				} else {
					log.Warn("conversation context extracted but empty",
						"stage", "context_extract_complete",
						"status", "empty",
						"context_rounds_config", h.deps.ContextRounds,
						"context_max_chars_config", h.deps.ContextMaxChars,
						"message_count", len(req.Messages),
						"reason", "no text content found in recent messages (only images/tool results)",
						"duration_ms", time.Since(ctxStart).Milliseconds(),
					)
				}
			} else {
				disableReason := "no messages in request"
				if h.deps.ContextRounds <= 0 {
					disableReason = "context_rounds<=0 (explicitly disabled)"
				}
				log.Info("context-aware description disabled",
					"stage", "context_extract_complete",
					"status", "disabled",
					"context_rounds_config", h.deps.ContextRounds,
					"reason", disableReason,
					"message_count", len(req.Messages),
				)
			}

			// 4) 并行处理图片：查缓存 → 未命中调视觉
			// 用 errgroup 并发执行。singleflight fn 内部用独立 ctx，不依赖 gctx，
			// 所以用普通 errgroup.Group 即可（无需 WithContext 的 cancel 语义）。
			g := new(errgroup.Group)
			effectiveLimit := h.deps.AdaptiveConcurrency.CurrentLimit()
			g.SetLimit(effectiveLimit) // 限制并发，避免一次请求里大量图片打爆 MiMo

			parallelStart := time.Now()
			log.Info("parallel image processing started",
				"stage", "parallel_images_start",
				"status", "info",
				"image_count", len(imgs),
				"concurrency_limit", h.deps.ConcurrencyLimit, // 静态配置值（参考）
				"effective_limit", effectiveLimit, // 实际生效值（自适应）
				"total_image_bytes", totalImageBytes,
			)

			for i, blk := range imgs {
				i, blk := i, blk // 闭包捕获
				g.Go(func() error {
					imgStart := time.Now()
					// outcome / retErr 由 defer 读取，用于在每个 return 点统一记录 goroutine 结束日志
					var outcome string
					var retErr error
					defer func() {
						log.Info("image goroutine finished",
							"stage", "image_goroutine_complete",
							"index", i,
							"outcome", outcome,
							"duration_ms", time.Since(imgStart).Milliseconds(),
							"err", retErr,
						)
					}()

					log.Info("image goroutine started",
						"stage", "image_goroutine_start",
						"index", i,
						"total_images", len(imgs),
					)

					rawBytes := decodedImages[i]
					herr := decodeErrors[i]
					var hash string
					if herr == nil {
						hash = cache.HashFromRawBytes(rawBytes)
					}
					imageSize := int64(len(rawBytes))
					isLarge := imageSize >= h.deps.LargeImageThreshold

					log.Debug("processing image block",
						"index", i,
						"data_len", len(blk.Source.Data),
						"image_size_bytes", imageSize,
						"media_type", blk.Source.MediaType,
						"is_large", isLarge,
						"hash", hash,
						"hash_err", fmt.Sprintf("%v", herr),
					)

					totalLookups.Add(1)

					if herr != nil {
						log.Warn("hash computation failed, skipping cache and vision",
							"stage", "image_hash_failed",
							"index", i,
							"err", herr,
							"data_len", len(blk.Source.Data),
						)
						if h.deps.FailOpen {
							if err := messages.ReplaceImageWithDescription(blk, "[Image hash failed, unable to compute cache key]"); err != nil {
								log.Error("replace image failed after hash failure",
									"stage", "image_replace_error",
									"index", i,
									"err", err,
								)
								retErr = fmt.Errorf("replace image after hash failure (index=%d): %w", i, err)
								outcome = "hash_fail_error"
								return retErr
							}
							rewritten.Add(1)
							h.recordImageMetric("rewritten")
							outcome = "hash_fail_open"
							return nil
						}
						retErr = fmt.Errorf("hash computation failed (index=%d): %w", i, herr)
						outcome = "hash_fail_error"
						return retErr
					}

					if desc, ok := h.deps.Cache.Get(hash); ok {
						if err := messages.ReplaceImageWithDescription(blk, desc); err != nil {
							log.Error("replace image failed on cache hit",
								"stage", "image_replace_error",
								"index", i,
								"err", err,
							)
							retErr = fmt.Errorf("replace image on cache hit (index=%d): %w", i, err)
							outcome = "cache_hit_error"
							return retErr
						}
						rewritten.Add(1)
						cached.Add(1)
						cacheHits.Add(1)
						h.recordImageMetric("cached")
						log.Info("cache hit for image",
							"stage", "image_cache_hit",
							"index", i,
							"hash", hash,
							"image_size_bytes", imageSize,
							"desc_len", len(desc),
							"cache_elapsed_ms", time.Since(imgStart).Milliseconds(),
						)
						outcome = "cache_hit"
						return nil
					}

					// 缓存 miss → 调视觉
					log.Info("cache miss, calling vision",
						"stage", "image_cache_miss",
						"index", i,
						"hash", hash,
						"hash_err", fmt.Sprintf("%v", herr),
						"image_size_bytes", imageSize,
						"is_large", isLarge,
						"timeout_override", isLarge,
					)

					// Runtime self-loop guard: if the vision provider's base_url
					// points back to the proxy itself (e.g. ListenAddr was empty
					// at construction or config was hot-reloaded), reject the
					// vision call with 508 instead of causing an infinite loop.
					// MiMo Client calls /v1/messages (same as proxy path).
					if h.deps.ListenAddr != "" {
						if ba, ok := h.deps.VisionProvider.(vision.BaseURLAware); ok {
							if isSelfReferentialURL(ba.GetBaseURL(), h.deps.ListenAddr) {
								log.Error("vision provider base_url points to proxy itself (loop detected)",
									"stage", "vision_self_loop_detected",
									"status", "error",
									"vision_base_url", ba.GetBaseURL(),
									"listen_addr", h.deps.ListenAddr,
								)
								return fmt.Errorf("vision provider base_url (%s) points to the proxy itself (loop detected). Check your vision.base_url config", ba.GetBaseURL())
							}
						}
					}

					vStart := time.Now()
					// singleflight：同 hash 并发调用合并为一次 vision 请求
					// fn 内部用独立 ctx（context.Background + WithCancel），避免某个调用者
					// 取消请求导致其他等待者也失败。
					// 注意：这里不再设共享硬截止 deadline —— per-provider 的 timeout 由 Pool 层
					// 用独立 WithTimeout 子 ctx 管理，否则共享父 deadline 会饿死 fallback
					// （第一个 provider 耗满预算后，后续 provider 拿到已过期 ctx 瞬间失败）。
					// 耗时数据（FnStart/FnEnd）封装在 visionResult 中返回，
					// 消除 executor 与 waiter goroutine 之间的数据竞争
					v, verr, shared := h.sf.Do(hash, func() (any, error) {
						res := &visionResult{}
						res.FnStart = time.Now()
						dedupCtx, cancel := context.WithCancel(context.Background())
						defer cancel()
						dedupCtx = logging.WithRequestID(dedupCtx, requestID)
						// P5：优先调用带上下文的方法；provider 未实现时回退到无上下文的 DescribeImage
						if cvp, ok := h.deps.VisionProvider.(vision.ContextualVisionProvider); ok {
							log.Info("dispatching vision call with context",
								"stage", "vision_call_dispatch",
								"index", i,
								"hash", hash,
								"method", "DescribeImageWithContext",
								"has_context", contextText != "",
								"context_chars", len(contextText),
								"context_preview", truncatePreview(contextText, 80),
								"image_size_bytes", imageSize,
								"is_large", isLarge,
							)
							res.Desc, res.Err = cvp.DescribeImageWithContext(dedupCtx, blk.Source.Data, blk.Source.MediaType, imageSize, contextText)
						} else {
							log.Info("dispatching vision call without context (fallback)",
								"stage", "vision_call_dispatch",
								"index", i,
								"hash", hash,
								"method", "DescribeImage",
								"reason", "provider does not implement ContextualVisionProvider",
								"image_size_bytes", imageSize,
								"is_large", isLarge,
							)
							res.Desc, res.Err = h.deps.VisionProvider.DescribeImage(dedupCtx, blk.Source.Data, blk.Source.MediaType, imageSize)
						}
						res.FnEnd = time.Now()
						// fn 内部写缓存：确保等待者从 SF.Do 返回时缓存已就绪，
						// 避免 errgroup 释放 semaphore 后下一个 goroutine 查缓存 miss
						if res.Err == nil {
							h.deps.Cache.Put(hash, res.Desc)
						}
						return res, res.Err
					})
					res, _ := v.(*visionResult)
					desc := res.Desc
					fnStart := res.FnStart
					fnEnd := res.FnEnd
					visionElapsed := time.Since(vStart)
					visionMs := visionElapsed.Milliseconds()

					// singleflight 耗时分解：
					// - fn_exec_ms: fn 实际执行 vision 调用的耗时（对所有 goroutine 可见，作为参考）
					// - sf_wait_ms: 该 goroutine 在 SF.Do 内的等待时间（执行者≈0，等待者=sf_total）
					fnExecMs := fnEnd.Sub(fnStart).Milliseconds()
					sfWaitMs := int64(0)
					if shared {
						sfWaitMs = visionMs // 等待者全程在等待
					}

					log.Info("singleflight Do completed",
						"stage", "singleflight_complete",
						"index", i,
						"hash", hash,
						"deduplicated", shared,
						"sf_total_ms", visionMs,
						"fn_exec_ms", fnExecMs,
						"sf_wait_ms", sfWaitMs,
						"has_context", contextText != "",
					)

					// 只让 singleflight executor（非等待者）上报 fn_exec_ms 样本，
					// 避免 1 次真实 vision 调用被 N 个等待者重复放大
					if !shared {
						isVisionErr := verr != nil
						h.deps.AdaptiveConcurrency.RecordSample(fnExecMs, isVisionErr)
					}

					if verr != nil {
						if !h.deps.FailOpen {
							log.Error("vision call failed, fail_open=false, returning 502",
								"index", i,
								"err", verr,
								"vision_elapsed", visionElapsed.String(),
								"vision_duration_ms", visionMs,
								"deduplicated", shared,
								"fn_exec_ms", fnExecMs,
								"sf_wait_ms", sfWaitMs,
							)
							h.recordVisionMetric("error", visionElapsed)
							failed.Add(1)
							h.recordImageMetric("failed")
							outcome = "vision_fail"
							retErr = fmt.Errorf("vision call failed (index=%d): %w", i, verr)
							return retErr
						}
						log.Warn("vision call failed, replacing with placeholder",
							"index", i,
							"err", verr,
							"vision_elapsed", visionElapsed.String(),
							"vision_duration_ms", visionMs,
							"image_size_bytes", imageSize,
							"deduplicated", shared,
							"fn_exec_ms", fnExecMs,
							"sf_wait_ms", sfWaitMs,
						)
						if err := messages.ReplaceImageWithDescription(blk, "[Image could not be described by vision model]"); err != nil {
							log.Error("replace image failed after vision failure",
								"stage", "image_replace_error",
								"index", i,
								"err", err,
							)
							retErr = fmt.Errorf("replace image after vision failure (index=%d): %w", i, err)
							outcome = "vision_fail_error"
							return retErr
						}
						rewritten.Add(1)
						h.recordVisionMetric("fail_open", visionElapsed)
						h.recordImageMetric("rewritten")
						outcome = "vision_fail_open"
						return nil
					}

					// 成功：缓存已在 singleflight fn 内部写入，无需重复写
					if err := messages.ReplaceImageWithDescription(blk, desc); err != nil {
						log.Error("replace image failed after vision success",
							"stage", "image_replace_error",
							"index", i,
							"err", err,
						)
						retErr = fmt.Errorf("replace image after vision success (index=%d): %w", i, err)
						outcome = "vision_replace_error"
						return retErr
					}
					rewritten.Add(1)
					h.recordVisionMetric("success", visionElapsed)
					h.recordImageMetric("rewritten")
					log.Info("image block processed successfully",
						"index", i,
						"image_size_bytes", imageSize,
						"is_large", isLarge,
						"vision_elapsed", visionElapsed.String(),
						"vision_duration_ms", visionMs,
						"fn_exec_ms", fnExecMs,
						"sf_wait_ms", sfWaitMs,
						"desc_len", len(desc),
						"desc_preview", truncate(desc, 80),
					)
					outcome = "vision_success"
					return nil
				})
			}

			if err := g.Wait(); err != nil {
				log.Error("parallel image processing failed, returning 502",
					"stage", "parallel_images_complete",
					"status", "error",
					"duration_ms", time.Since(parallelStart).Milliseconds(),
					"err", err,
					"rewritten", rewritten.Load(),
					"cached", cached.Load(),
					"failed", failed.Load(),
				)
				statusCode = http.StatusBadGateway
				http.Error(w, "vision call failed: "+err.Error(), http.StatusBadGateway)
				h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
				return
			}

			log.Info("parallel image processing completed",
				"stage", "parallel_images_complete",
				"status", "ok",
				"duration_ms", time.Since(parallelStart).Milliseconds(),
				"image_count", len(imgs),
				"rewritten", rewritten.Load(),
				"cached", cached.Load(),
				"failed", failed.Load(),
			)

			// 计算缓存命中率
			if totalLookups.Load() > 0 && h.deps.Metrics != nil {
				ratio := float64(cacheHits.Load()) / float64(totalLookups.Load())
				h.deps.Metrics.CacheHitRatio.Set(ratio)
			}

			// 5) 如果做过改写，追加 system 指令
			if rewritten.Load() > 0 {
				postfix := messages.ContentBlock{
					Type: "text",
					Text: "IMPORTANT: Some image blocks in this conversation have been replaced with <BLIND_LLM_EYES_IMAGE>...</BLIND_LLM_EYES_IMAGE> text blocks. Treat the content inside those tags as if you saw the image directly — never reply that you cannot see the image, never guess what might be missing. If the description is insufficient, ask the user to share the original image.",
				}
				req.System = append(req.System, postfix)
			}
		} // end if !passthrough

		// 6) 改写了图片、规范化了 system 消息、或 sanitize 了 model → 重新序列化请求体
		if rewritten.Load() > 0 || systemMoved > 0 || modelSanitized {
			newBody, merr := json.Marshal(&req)
			if merr != nil {
				log.Error("re-marshal request failed",
					"stage", "remarshal_complete",
					"status", "error",
					"err", merr,
					"rewritten_count", rewritten.Load(),
					"system_moved", systemMoved,
				)
			} else {
				oldBytes := len(rawBody)
				rawBody = newBody
				log.Info("request re-marshaled for upstream",
					"stage", "remarshal_complete",
					"status", "ok",
					"original_bytes", oldBytes,
					"new_bytes", len(newBody),
					"delta_bytes", len(newBody)-oldBytes,
					"rewritten_count", rewritten.Load(),
					"system_moved", systemMoved,
				)
			}
		}
	} else {
		log.Warn("request JSON parse failed, passthrough raw",
			"stage", "json_parse_complete",
			"status", "failed_passthrough",
			"err", parseErr,
			"body_bytes", len(rawBody),
			"body_preview", truncateBytes(rawBody, 200),
		)
	}

	// 6) 转发给上游 (DeepSeek)
	upstreamURL := h.deps.UpstreamBaseURL + "/v1/messages"

	// Runtime self-loop guard: if the constructor check was bypassed (e.g.
	// ListenAddr was empty at construction), catch self-referential URLs
	// before they cause infinite forwarding.
	if h.deps.ListenAddr != "" && isSelfReferentialURL(upstreamURL, h.deps.ListenAddr) {
		log.Error("upstream URL points to proxy itself, rejecting to prevent infinite loop",
			"stage", "upstream_request_start",
			"status", "error",
			"upstream_url", upstreamURL,
			"listen_addr", h.deps.ListenAddr,
		)
		statusCode = http.StatusLoopDetected
		http.Error(w, "upstream URL points to the proxy itself (loop detected). Check your upstream.base_url config.", http.StatusLoopDetected)
		h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
		return
	}

	// ── stage: upstream_request_start ──
	log.Info("forwarding request to upstream",
		"stage", "upstream_request_start",
		"status", "info",
		"url", upstreamURL,
		"body_bytes", len(rawBody),
		"rewritten", rewritten.Load(),
		"cached", cached.Load(),
		"failed", failed.Load(),
		"passthrough", passthrough,
	)

	// ── stage: upstream_request_build ──
	reqBuildStart := time.Now()
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(rawBody))
	if err != nil {
		log.Error("build upstream request failed",
			"stage", "upstream_request_build",
			"status", "error",
			"error_code", "upstream_req_build_failed",
			"error_message", err.Error(),
			"url", upstreamURL,
			"stack", logging.StackTrace(500),
		)
		statusCode = http.StatusInternalServerError
		http.Error(w, "build upstream req: "+err.Error(), http.StatusInternalServerError)
		h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
		return
	}

	// Header 处理：显式过滤安全头，防止敏感信息泄露
	for k, vs := range r.Header {
		if shouldStripHeader(k, h.deps.UpstreamAPIKey != "") {
			if k != "Host" {
				log.Debug("stripping sensitive header before forwarding",
					"header", k,
				)
			}
			continue
		}
		for _, v := range vs {
			upstreamReq.Header.Add(k, v)
		}
	}
	if h.deps.UpstreamAPIKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+h.deps.UpstreamAPIKey)
	}
	upstreamReq.ContentLength = int64(len(rawBody))
	reqBuildMs := time.Since(reqBuildStart).Milliseconds()
	log.Debug("upstream request built",
		"stage", "upstream_request_build",
		"status", "ok",
		"duration_ms", reqBuildMs,
	)

	// ── httptrace: 捕获 DeepSeek 连接各阶段耗时 ──
	var dnsStart, connectStart, tlsStart, gotConnTime, wroteReqTime time.Time

	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStart = time.Now()
			log.Debug("upstream DNS lookup started",
				"stage", "upstream_dns_lookup",
				"host", info.Host,
			)
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			var ms int64
			if !dnsStart.IsZero() {
				ms = time.Since(dnsStart).Milliseconds()
			}
			log.Info("upstream DNS lookup completed",
				"stage", "upstream_dns_lookup",
				"status", "ok",
				"addrs", len(info.Addrs),
				"duration_ms", ms,
			)
		},
		ConnectStart: func(network, addr string) {
			connectStart = time.Now()
			log.Debug("upstream TCP connect started",
				"stage", "upstream_tcp_connect",
				"addr", addr,
			)
		},
		ConnectDone: func(network, addr string, err error) {
			var ms int64
			if !connectStart.IsZero() {
				ms = time.Since(connectStart).Milliseconds()
			}
			log.Info("upstream TCP connect completed",
				"stage", "upstream_tcp_connect",
				"status", "ok",
				"addr", addr,
				"duration_ms", ms,
			)
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
			log.Debug("upstream TLS handshake started",
				"stage", "upstream_tls_handshake",
			)
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			var ms int64
			if !tlsStart.IsZero() {
				ms = time.Since(tlsStart).Milliseconds()
			}
			log.Info("upstream TLS handshake completed",
				"stage", "upstream_tls_handshake",
				"status", "ok",
				"duration_ms", ms,
				"tls_version", state.Version,
			)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			gotConnTime = time.Now()
			log.Info("upstream connection established",
				"stage", "upstream_got_conn",
				"status", "ok",
				"reused", info.Reused,
				"was_idle", info.WasIdle,
				"remote_addr", info.Conn.RemoteAddr().String(),
			)
		},
		WroteRequest: func(wr httptrace.WroteRequestInfo) {
			wroteReqTime = time.Now()
			log.Debug("upstream request body fully sent",
				"stage", "upstream_wrote_request",
				"err", wr.Err,
			)
		},
		GotFirstResponseByte: func() {
			firstByte := time.Now()
			var ttfbMs int64
			if !wroteReqTime.IsZero() {
				ttfbMs = firstByte.Sub(wroteReqTime).Milliseconds()
			}
			log.Info("upstream first response byte received",
				"stage", "upstream_first_byte",
				"status", "ok",
				"ttfb_ms", ttfbMs,
			)
		},
	}

	traceCtx := httptrace.WithClientTrace(upstreamReq.Context(), trace)
	upstreamReq = upstreamReq.WithContext(traceCtx)

	upstreamStart := time.Now()
	upstreamResp, err := h.client.Do(upstreamReq)
	upstreamElapsed := time.Since(upstreamStart)
	upstreamMs := upstreamElapsed.Milliseconds()
	if err != nil {
		log.Error("upstream request failed",
			"stage", "upstream_error",
			"status", "error",
			"error_code", "upstream_request_failed",
			"error_message", err.Error(),
			"url", upstreamURL,
			"headers_duration_ms", upstreamMs,
			"stack", logging.StackTrace(500),
		)
		statusCode = http.StatusBadGateway
		h.recordUpstreamMetric(strconv.Itoa(statusCode), upstreamElapsed)
		http.Error(w, "upstream do: "+err.Error(), http.StatusBadGateway)
		h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
		return
	}
	defer upstreamResp.Body.Close()

	statusCode = upstreamResp.StatusCode
	h.recordUpstreamMetric(strconv.Itoa(statusCode), upstreamElapsed)

	var connReadyMs int64
	if !gotConnTime.IsZero() {
		connReadyMs = gotConnTime.Sub(upstreamStart).Milliseconds()
	}

	// ── stage: upstream_response_received ──
	respContentLength := upstreamResp.ContentLength
	log.Info("upstream response received",
		"stage", "upstream_response_received",
		"status", "ok",
		"http_status_code", upstreamResp.StatusCode,
		"response_content_length", respContentLength,
		"req_build_duration_ms", reqBuildMs,
		"conn_ready_ms", connReadyMs,
		"headers_duration_ms", upstreamMs,
	)

	// 7) 加改写结果头
	hdr := formatCountHeader(rewritten.Load(), cached.Load())
	if passthrough {
		hdr += ", passthrough=1"
	}
	upstreamResp.Header.Set("X-Blind-Llm-Eyes", hdr)

	// 8) SSE 原样透传
	streamStart := time.Now()
	if err := CopyResponse(w, upstreamResp); err != nil {
		streamMs := time.Since(streamStart).Milliseconds()
		log.Error("copy response to client failed",
			"stage", "upstream_error",
			"status", "error",
			"error_code", "stream_copy_failed",
			"error_message", err.Error(),
			"stream_duration_ms", streamMs,
			"total_duration_ms", time.Since(requestStart).Milliseconds(),
		)
	} else {
		streamMs := time.Since(streamStart).Milliseconds()
		totalMs := time.Since(requestStart).Milliseconds()
		// ── stage: upstream_complete ──
		log.Info("upstream response streamed to client",
			"stage", "upstream_complete",
			"status", "ok",
			"http_status_code", statusCode,
			"req_build_duration_ms", reqBuildMs,
			"conn_ready_ms", connReadyMs,
			"headers_duration_ms", upstreamMs,
			"stream_duration_ms", streamMs,
			"total_http_duration_ms", upstreamMs+streamMs,
			"total_duration_ms", totalMs,
			"rewritten", rewritten.Load(),
			"cached", cached.Load(),
			"failed", failed.Load(),
		)
	}

	h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
}

// recordRequestMetrics 记录 HTTP 请求全链路指标。
func (h *requestHandler) recordRequestMetrics(method, route string, status int, start time.Time) {
	if h.deps.Metrics == nil {
		return
	}
	s := strconv.Itoa(status)
	duration := time.Since(start).Seconds()
	h.deps.Metrics.HTTPRequestsTotal.WithLabelValues(method, route, s).Inc()
	h.deps.Metrics.HTTPRequestDuration.WithLabelValues(method, route, s).Observe(duration)
}

// recordImageMetric 记录图片处理结果。
func (h *requestHandler) recordImageMetric(outcome string) {
	if h.deps.Metrics == nil {
		return
	}
	h.deps.Metrics.ImagesProcessedTotal.WithLabelValues(outcome).Inc()
}

// recordVisionMetric 记录视觉调用结果和耗时。
func (h *requestHandler) recordVisionMetric(result string, duration time.Duration) {
	if h.deps.Metrics == nil {
		return
	}
	h.deps.Metrics.VisionCallsTotal.WithLabelValues(result).Inc()
	h.deps.Metrics.VisionCallDuration.WithLabelValues(result).Observe(duration.Seconds())
}

// recordUpstreamMetric 记录上游调用结果和耗时。
func (h *requestHandler) recordUpstreamMetric(status string, duration time.Duration) {
	if h.deps.Metrics == nil {
		return
	}
	h.deps.Metrics.UpstreamRequestsTotal.WithLabelValues(status).Inc()
	h.deps.Metrics.UpstreamRequestDuration.WithLabelValues(status).Observe(duration.Seconds())
}

func formatCountHeader(rewritten, cached int64) string {
	return formatInt(rewritten) + " rewritten, " + formatInt(cached) + " cached"
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// truncateBytes 直接操作字节切片，只把需要的前 n 字节转换为字符串，
// 避免把整个 rawBody（可能数 MB）一次性转为字符串造成大块内存分配。
func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// isSelfReferentialURL checks if urlStr points to the proxy's own listen
// address. This prevents infinite self-forwarding loops.
//
// Comparison normalizes host aliases: localhost, 127.0.0.1, 0.0.0.0, ::1
// are all treated as equivalent. Returns false for unparseable URLs (safe default).
func isSelfReferentialURL(urlStr, proxyListenAddr string) bool {
	if urlStr == "" || proxyListenAddr == "" {
		return false
	}
	parsed, err := url.Parse(urlStr)
	if err != nil || parsed.Host == "" {
		return false
	}
	proxyHost, proxyPort, err := net.SplitHostPort(proxyListenAddr)
	if err != nil {
		return false
	}
	urlHost, urlPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		urlPort = defaultPort(parsed.Scheme)
		urlHost = parsed.Host
	}
	return normalizeHost(urlHost) == normalizeHost(proxyHost) && urlPort == proxyPort
}

// normalizeHost normalizes host aliases for self-reference detection.
// Uses net.ParseIP to cover the entire 127.0.0.0/8 loopback range, all IPv6
// loopback forms (::1, 0:0:0:0:0:0:0:1, [::1]), and unspecified addresses
// (0.0.0.0, ::). Also strips trailing dots (DNS FQDN form, e.g. "127.0.0.1.")
// so that such URLs are still recognized as self-referential.
func normalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip IPv6 brackets
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	// Strip trailing dot (DNS FQDN form like "127.0.0.1.")
	h = strings.TrimRight(h, ".")

	// Check if it's an IP address
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsLoopback() {
			return "127.0.0.1"
		}
		if ip.IsUnspecified() { // 0.0.0.0 or ::
			return "127.0.0.1"
		}
		return ip.String() // canonical form
	}

	// Hostname aliases
	switch h {
	case "localhost":
		return "127.0.0.1"
	}
	return h
}

// shouldStripHeader returns true if the header must not be forwarded to the
// upstream API. Sensitive headers (Authorization, Proxy-Authorization, Cookie)
// are stripped only when the proxy injects its own Authorization (i.e.,
// UpstreamAPIKey is configured). When UpstreamAPIKey is empty, the proxy
// acts as a transparent forwarder and MUST pass the client's Authorization
// to the upstream — otherwise the upstream returns 401 and passthrough mode
// (vision_capable_models) breaks. Host is always stripped (set by
// http.NewRequestWithContext).
func shouldStripHeader(k string, hasUpstreamKey bool) bool {
	switch k {
	case "Host":
		return true
	case "Authorization", "Proxy-Authorization", "Cookie":
		return hasUpstreamKey
	}
	return false
}

// defaultPort returns the default port for a URL scheme.
func defaultPort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// truncatePreview returns the first maxLen bytes of s, appending "..." if
// truncation occurred. Used for logging context text without leaking full
// conversation history. Truncation aligns to UTF-8 rune boundaries: if maxLen
// would split a multi-byte rune (common in Chinese text), it is backed off
// to the preceding rune start so the result is always valid UTF-8.
func truncatePreview(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// Back off to the previous rune boundary so we don't split a multi-byte
	// UTF-8 sequence (e.g. a 3-byte Chinese character).
	for maxLen > 0 && !utf8.RuneStart(s[maxLen]) {
		maxLen--
	}
	return s[:maxLen] + "..."
}
