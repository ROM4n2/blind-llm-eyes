package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
)

// silentLogger 返回一个丢弃所有输出的 logger，用于测试。
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildNImageRequestWithSameData 构造包含 n 张相同 base64 data 的图片请求，
// 用于触发 cache stampede 场景验证 singleflight 去重效果。
func buildNImageRequestWithSameData(n int) string {
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	content := []map[string]any{
		{"type": "text", "text": "describe these images"},
	}
	for i := 0; i < n; i++ {
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]string{
				"type":       "base64",
				"media_type": "image/png",
				"data":       pngB64,
			},
		})
	}
	body := map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": content},
		},
		"stream": true,
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// TestHandler_Singleflight_SameImageInOneRequest 验证单请求内 cache stampede 防护：
// 5 张相同图的请求，singleflight 应合并为 1 次 vision 调用。
func TestHandler_Singleflight_SameImageInOneRequest(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	// 500ms delay 足以让 5 个 goroutine 同时进入 singleflight.Do
	slow := newSlowVisionMock(500*time.Millisecond, "DedupDesc")

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      slow,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	// 5 张相同 data 的图片
	reqBody := buildNImageRequestWithSameData(5)

	start := time.Now()
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	// 调试：检查缓存状态
	const dbgB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	dbgHash, dbgErr := cache.HashFromBase64Data(dbgB64)
	dbgDesc, dbgOK := deps.Cache.Get(dbgHash)
	t.Logf("debug: hash=%q herr=%v cache_ok=%v cache_desc=%q", dbgHash, dbgErr, dbgOK, dbgDesc)

	// 核心断言：vision 只被调用 1 次（singleflight 合并了 5 个相同 hash 的请求）
	calls := len(slow.callStart)
	if calls != 1 {
		t.Errorf("vision calls = %d, want 1 (singleflight should dedup 5 identical images)", calls)
	}
	t.Logf("5 identical images → %d vision call(s), elapsed=%v", calls, elapsed)

	// X-Blind-Llm-Eyes 应报告 5 rewritten
	hdr := rr.Header().Get("X-Blind-Llm-Eyes")
	if !strings.Contains(hdr, "5 rewritten") {
		t.Errorf("X-Blind-Llm-Eyes = %q, want contains '5 rewritten'", hdr)
	}
}

// TestHandler_Singleflight_CrossRequest 验证跨请求去重：
// 两个并发请求带同一张图，singleflight 应合并为 1 次 vision 调用。
func TestHandler_Singleflight_CrossRequest(t *testing.T) {
	// 用内联的线程安全 upstream mock，避免 fakeUpstream 的 upstreamGot 并发写
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	// 1s delay 确保两个请求的 vision 调用时间窗口重叠
	slow := newSlowVisionMock(1*time.Second, "SharedDesc")

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      slow,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	// 两个请求都带同一张图（相同 base64 data）
	reqBody := buildNImageRequestWithSameData(1)

	var (
		wg    sync.WaitGroup
		code1 int
		code2 int
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		code1 = rec.Code
	}()
	go func() {
		defer wg.Done()
		// 错开 50ms 确保第一个请求先进入 singleflight.Do
		time.Sleep(50 * time.Millisecond)
		rec := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		code2 = rec.Code
	}()
	wg.Wait()

	if code1 != 200 || code2 != 200 {
		t.Fatalf("status: req1=%d req2=%d", code1, code2)
	}

	// 核心：两个请求带同一张图，vision 只被调用 1 次
	calls := len(slow.callStart)
	if calls != 1 {
		t.Errorf("vision calls = %d, want 1 (cross-request singleflight dedup)", calls)
	}
	t.Logf("2 concurrent requests with same image → %d vision call(s)", calls)
}

// TestHandler_Singleflight_CtxCancellationIsolation 验证 ctx 取消隔离：
// 请求 A 发起 vision 后立即取消，请求 B 应仍能拿到结果（fn 用独立 ctx）。
func TestHandler_Singleflight_CtxCancellationIsolation(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	// 800ms delay，足够让请求 A 先进入 singleflight.Do 再被取消
	slow := newSlowVisionMock(800*time.Millisecond, "IsolationDesc")

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      slow,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	reqBody := buildNImageRequestWithSameData(1)

	// 请求 A：发出后 100ms 取消（vision 还在跑）
	ctxA, cancelA := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancelA()
	}()
	rrA := httptest.NewRecorder()
	reqA, _ := http.NewRequestWithContext(ctxA, "POST", "/v1/messages", bytes.NewBufferString(reqBody))
	reqA.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rrA, reqA)
	// 请求 A 被取消，可能返回非 200，这是正常的

	// 请求 B：正常等待，应在 fn 完成后拿到结果（缓存命中或 singleflight 等待）
	rrB := httptest.NewRecorder()
	reqB, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	reqB.Header.Set("Content-Type", "application/json")
	startB := time.Now()
	h.ServeHTTP(rrB, reqB)
	elapsedB := time.Since(startB)

	// 请求 B 应成功（fn 用独立 ctx，不受 A 取消影响）
	if rrB.Code != 200 {
		t.Errorf("request B status = %d, want 200 (should not be affected by A's cancellation)", rrB.Code)
	}
	t.Logf("request A cancelled at 100ms, request B completed in %v with status %d", elapsedB, rrB.Code)

	// 验证 vision 调用次数：A 的 fn 启动后不受 A 取消影响，B 应复用结果
	calls := len(slow.callStart)
	t.Logf("vision calls = %d (A cancelled, B should reuse result)", calls)
	if calls > 2 {
		t.Errorf("vision calls = %d, want <= 2", calls)
	}
}
