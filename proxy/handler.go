package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/logging"
	"github.com/ROM4n2/blind-llm-eyes/messages"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
	"github.com/ROM4n2/blind-llm-eyes/vision"
)

// HandlerDeps 是 Handler 的依赖（用 struct 注入，方便测试替换 mock）。
type HandlerDeps struct {
	UpstreamBaseURL      string
	UpstreamAPIKey       string
	VisionProvider       vision.VisionProvider
	Cache                *cache.LRU
	FailOpen             bool
	LargeImageThreshold  int64
	Log                  *slog.Logger
	WG                   *sync.WaitGroup
	Metrics              *metrics.Metrics // 可选：Prometheus 指标
}

// NewHandler 返回一个标准 http.Handler，处理 /v1/messages 所有请求。
func NewHandler(deps HandlerDeps) http.Handler {
	if deps.WG == nil {
		deps.WG = &sync.WaitGroup{}
	}
	mux := http.NewServeMux()
	h := &requestHandler{deps: deps}
	mux.HandleFunc("/v1/messages", h.handleMessages)
	return mux
}

// Shutdown 等待所有在途请求完成。
func Shutdown(deps HandlerDeps) {
	if deps.WG != nil {
		deps.WG.Wait()
	}
}

type requestHandler struct {
	deps HandlerDeps
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

	// 1) 读原始 body
	readStart := time.Now()
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error("read request body failed",
			"err", err,
			"read_elapsed", time.Since(readStart).String(),
		)
		statusCode = http.StatusBadRequest
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
		return
	}
	log.Debug("request body read",
		"body_bytes", len(rawBody),
		"read_elapsed", time.Since(readStart).String(),
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

	if parseErr == nil {
		log.Debug("request JSON parsed",
			"messages", len(req.Messages),
			"body_bytes", len(rawBody),
		)

		// 2.5) 校验请求结构
		if verr := req.Validate(); verr != nil {
			log.Warn("request validation failed",
				"err", verr,
				"body_bytes", len(rawBody),
			)
			statusCode = http.StatusBadRequest
			http.Error(w, "validation failed: "+verr.Error(), http.StatusBadRequest)
			h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
			return
		}

		// 3) 找图
		imgs := messages.FindImageBlocks(&req)
		var totalImageBytes int64
		for _, blk := range imgs {
			raw, _ := base64.StdEncoding.DecodeString(blk.Source.Data)
			totalImageBytes += int64(len(raw))
		}
		log.Info("image blocks found in request",
			"count", len(imgs),
			"total_image_bytes", totalImageBytes,
			"is_large_request", totalImageBytes >= h.deps.LargeImageThreshold,
		)

		// 4) 逐个图：查缓存 → 未命中调视觉
		for i, blk := range imgs {
			imgStart := time.Now()
			hash, herr := cache.HashFromBase64Data(blk.Source.Data)
			rawBytes, _ := base64.StdEncoding.DecodeString(blk.Source.Data)
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

			if herr == nil {
				if desc, ok := h.deps.Cache.Get(hash); ok {
					messages.ReplaceImageWithDescription(blk, desc)
					rewritten.Add(1)
					cached.Add(1)
					cacheHits.Add(1)
					h.recordImageMetric("cached")
					log.Debug("cache hit for image",
						"index", i,
						"hash", hash,
						"desc_len", len(desc),
						"cache_elapsed", time.Since(imgStart).String(),
					)
					continue
				}
			}

			// 缓存 miss / hash 失败 → 调视觉
			log.Debug("cache miss, calling vision",
				"index", i,
				"hash", hash,
				"image_size_bytes", imageSize,
				"timeout_override", isLarge,
			)

			vStart := time.Now()
			visionCtx := logging.WithRequestID(r.Context(), requestID)
			desc, verr := h.deps.VisionProvider.DescribeImage(visionCtx, blk.Source.Data, blk.Source.MediaType, imageSize)
			visionElapsed := time.Since(vStart)

			if verr != nil {
				if !h.deps.FailOpen {
					log.Error("vision call failed, fail_open=false, returning 502",
						"index", i,
						"err", verr,
						"vision_elapsed", visionElapsed.String(),
					)
					statusCode = http.StatusBadGateway
					h.recordVisionMetric("error", visionElapsed)
					failed.Add(1)
					h.recordImageMetric("failed")
					http.Error(w, "vision call failed: "+verr.Error(), http.StatusBadGateway)
					h.recordRequestMetrics(r.Method, route, statusCode, requestStart)
					return
				}
				log.Warn("vision call failed, replacing with placeholder",
					"index", i,
					"err", verr,
					"vision_elapsed", visionElapsed.String(),
					"image_size_bytes", imageSize,
				)
				messages.ReplaceImageWithDescription(blk, "[Image could not be described by vision model]")
				rewritten.Add(1)
				h.recordVisionMetric("fail_open", visionElapsed)
				h.recordImageMetric("rewritten")
				continue
			}

			// 成功
			if herr == nil {
				h.deps.Cache.Put(hash, desc)
				log.Debug("cache populated for image",
					"index", i,
					"hash", hash,
					"desc_len", len(desc),
				)
			}
			messages.ReplaceImageWithDescription(blk, desc)
			rewritten.Add(1)
			h.recordVisionMetric("success", visionElapsed)
			h.recordImageMetric("rewritten")
			log.Info("image block processed successfully",
				"index", i,
				"image_size_bytes", imageSize,
				"is_large", isLarge,
				"vision_elapsed", visionElapsed.String(),
				"desc_len", len(desc),
				"desc_preview", truncate(desc, 80),
			)
		}

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

			newBody, merr := json.Marshal(&req)
			if merr != nil {
				log.Error("re-marshal request failed",
					"err", merr,
					"rewritten_count", rewritten.Load(),
				)
			} else {
				rawBody = newBody
				log.Debug("request re-marshaled for upstream",
					"original_bytes", len(rawBody),
					"new_bytes", len(newBody),
					"rewritten_count", rewritten.Load(),
				)
			}
		}
	} else {
		log.Warn("request JSON parse failed, passthrough raw",
			"err", parseErr,
			"body_bytes", len(rawBody),
		)
	}

	// 6) 转发给上游 (DeepSeek)
	upstreamURL := h.deps.UpstreamBaseURL + "/v1/messages"

	// ── stage: upstream_request_start ──
	log.Info("forwarding request to upstream",
		"stage", "upstream_request_start",
		"status", "info",
		"url", upstreamURL,
		"body_bytes", len(rawBody),
		"rewritten", rewritten.Load(),
		"cached", cached.Load(),
		"failed", failed.Load(),
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

	// Header 处理
	for k, vs := range r.Header {
		if k == "Host" {
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
	upstreamResp, err := http.DefaultClient.Do(upstreamReq)
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
	upstreamResp.Header.Set("X-Blind-Llm-Eyes",
		formatCountHeader(rewritten.Load(), cached.Load()))

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
