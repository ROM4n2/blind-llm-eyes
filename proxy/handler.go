package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/messages"
	"github.com/ROM4n2/blind-llm-eyes/vision"
)

// HandlerDeps 是 Handler 的依赖（用 struct 注入，方便测试替换 mock）。
type HandlerDeps struct {
	UpstreamBaseURL string
	UpstreamAPIKey  string // 可选：填了就用这个 key 覆盖 Authorization 头
	VisionClient    *vision.Client
	Cache           *cache.LRU
	FailOpen        bool
	Log             *slog.Logger
}

// NewHandler 返回一个标准 http.Handler，处理 /v1/messages 所有请求。
func NewHandler(deps HandlerDeps) http.Handler {
	mux := http.NewServeMux()
	h := &requestHandler{deps: deps}
	mux.HandleFunc("/v1/messages", h.handleMessages)
	return mux
}

type requestHandler struct {
	deps HandlerDeps
}

func (h *requestHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1) 读原始 body
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 2) 解析 JSON
	var req messages.Request
	parseErr := json.Unmarshal(rawBody, &req)

	var rewritten atomic.Int64
	var cached atomic.Int64

	if parseErr == nil {
		h.deps.Log.Debug("parsed request",
			"messages", len(req.Messages),
			"body_bytes", len(rawBody),
		)
		// 3) 找图
		imgs := messages.FindImageBlocks(&req)
		h.deps.Log.Debug("found image blocks", "count", len(imgs))

		// 4) 逐个图：查缓存 → 未命中调视觉
		for _, blk := range imgs {
			hash, herr := cache.HashFromBase64Data(blk.Source.Data)
			h.deps.Log.Debug("processing image block",
				"data_len", len(blk.Source.Data),
				"media_type", blk.Source.MediaType,
				"hash_err", fmt.Sprintf("%v", herr),
			)
			if herr == nil {
				if desc, ok := h.deps.Cache.Get(hash); ok {
					messages.ReplaceImageWithDescription(blk, desc)
					rewritten.Add(1)
					cached.Add(1)
					h.deps.Log.Debug("cache hit", "hash", hash)
					continue
				}
			}

			// 缓存 miss / hash 失败 → 调视觉
			desc, verr := h.deps.VisionClient.DescribeImage(r.Context(), blk.Source.Data, blk.Source.MediaType)
			if verr != nil {
				// 视觉失败：fail-open 则不替换
				if !h.deps.FailOpen {
					http.Error(w, "vision call failed: "+verr.Error(), http.StatusBadGateway)
					return
				}
				h.deps.Log.Warn("vision call failed, fail-open keeping original image", "err", verr)
				continue
			}

			// 成功：写缓存 + 替换
			if herr == nil {
				h.deps.Cache.Put(hash, desc)
			}
			messages.ReplaceImageWithDescription(blk, desc)
			rewritten.Add(1)
		}

		// 5) 如果做过改写，在请求的 system 段追加一条强指令，防止上游模型
		//    声称"看不到图"或凭空编造。
		if rewritten.Load() > 0 {
			postfix := &messages.ContentBlock{
				Type: "text",
				Text: "IMPORTANT: Some image blocks in this conversation have been replaced with <BLIND_LLM_EYES_IMAGE>...</BLIND_LLM_EYES_IMAGE> text blocks. Treat the content inside those tags as if you saw the image directly — never reply that you cannot see the image, never guess what might be missing. If the description is insufficient, ask the user to share the original image.",
			}
			req.System = append(req.System, *postfix)

			newBody, merr := json.Marshal(&req)
			if merr != nil {
				h.deps.Log.Error("re-marshal request failed", "err", merr)
			} else {
				rawBody = newBody
			}
		}
	} else {
		h.deps.Log.Warn("parse anthropic request failed, passthrough raw", "err", parseErr)
	}

	// 6) 转发给上游
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.deps.UpstreamBaseURL+"/v1/messages", bytes.NewReader(rawBody))
	if err != nil {
		http.Error(w, "build upstream req: "+err.Error(), http.StatusInternalServerError)
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
	// 如果配置了上游 API key，覆盖 Authorization 头（最常用场景）
	if h.deps.UpstreamAPIKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+h.deps.UpstreamAPIKey)
	}
	upstreamReq.ContentLength = int64(len(rawBody))

	upstreamResp, err := http.DefaultClient.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream do: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstreamResp.Body.Close()

	// 7) 加改写结果头
	upstreamResp.Header.Set("X-Blind-Llm-Eyes",
		formatCountHeader(rewritten.Load(), cached.Load()))

	// 8) SSE 原样透传
	if err := CopyResponse(w, upstreamResp); err != nil {
		h.deps.Log.Error("copy response", "err", err)
	}
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
