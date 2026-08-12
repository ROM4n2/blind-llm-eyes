package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
)

// slowVisionMock 模拟长耗时的 vision 调用，每次 DescribeImage 阻塞 delay，
// 并记录每次调用的开始时间（相对于构造时记录的 base），用于断言并发行为。
type slowVisionMock struct {
	mu        sync.Mutex
	delay     time.Duration
	desc      string
	callStart []time.Time // 每次调用进入时的绝对时间
	base      time.Time   // 构造时记录的基准时间
}

func newSlowVisionMock(delay time.Duration, desc string) *slowVisionMock {
	return &slowVisionMock{
		delay: delay,
		desc:  desc,
		base:  time.Now(),
	}
}

func (m *slowVisionMock) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	m.mu.Lock()
	m.callStart = append(m.callStart, time.Now())
	m.mu.Unlock()

	select {
	case <-time.After(m.delay):
		return m.desc, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// offsets 返回每次调用开始时间相对于 base 的偏移（毫秒）。
func (m *slowVisionMock) offsets() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int64, len(m.callStart))
	for i, t := range m.callStart {
		out[i] = t.Sub(m.base).Milliseconds()
	}
	return out
}

// buildNImageRequest 构造包含 n 张图片的请求。每张图用不同的 base64 data，
// 确保产生不同的 cache hash，避免 cache stampede 导致后续图命中缓存。
func buildNImageRequest(n int) string {
	content := []map[string]any{
		{"type": "text", "text": "describe these images"},
	}
	for i := 0; i < n; i++ {
		// 每张图用不同的原始字节，编码成不同的 base64 data
		raw := []byte{byte('A' + i), byte('A' + i), byte('A' + i)}
		data := base64.StdEncoding.EncodeToString(raw)
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]string{
				"type":       "base64",
				"media_type": "image/png",
				"data":       data,
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

// TestHandler_ParallelImageProcessing_5Images 验证 5 张图的并行处理：
//   - concurrency_limit=4，所以前 4 张应同时启动，第 5 张等前 4 张完成后再启动
//   - 每张图 mock delay=2s，串行预期 10s，并行预期 ~4s（2 批）
//   - 日志应包含 5 个 image_goroutine_start / image_goroutine_complete 事件
//   - parallel_images_start / parallel_images_complete 阶段日志应出现
func TestHandler_ParallelImageProcessing_5Images(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	// mock vision: 每张图 2 秒
	slow := newSlowVisionMock(2*time.Second, "SlowMockDesc")

	// 日志写到缓冲区，便于断言
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      slow,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 logger,
	}
	h := NewHandler(deps)

	reqBody := buildNImageRequest(5)

	start := time.Now()
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	// 串行 5×2s=10s，并行 limit=4 应该 ~4s（2 批：4 并行 + 1 等待）
	// 留 2s 余量，避免偶发调度抖动误报
	if elapsed > 6*time.Second {
		t.Errorf("elapsed = %v, want <6s (parallel not working?)", elapsed)
	}
	t.Logf("total elapsed: %v (5 images × 2s, concurrency_limit=4, expected ~4s)", elapsed)

	// header 应报告 5 rewritten
	hdr := rr.Header().Get("X-Blind-Llm-Eyes")
	if !strings.Contains(hdr, "5 rewritten") {
		t.Errorf("X-Blind-Llm-Eyes = %q, want contains '5 rewritten'", hdr)
	}

	// ── 日志断言 ──
	logs := logBuf.String()
	_ = logs // 也输出到 stderr 方便人工查看

	// 5 个 goroutine start / complete
	startCount := strings.Count(logs, "image_goroutine_start")
	completeCount := strings.Count(logs, "image_goroutine_complete")
	if startCount != 5 {
		t.Errorf("image_goroutine_start count = %d, want 5", startCount)
	}
	if completeCount != 5 {
		t.Errorf("image_goroutine_complete count = %d, want 5", completeCount)
	}

	// errgroup 阶段日志
	if !strings.Contains(logs, "parallel_images_start") {
		t.Errorf("missing parallel_images_start log")
	}
	if !strings.Contains(logs, "parallel_images_complete") {
		t.Errorf("missing parallel_images_complete log")
	}
	if !strings.Contains(logs, "outcome=vision_success") {
		t.Errorf("missing outcome=vision_success in logs")
	}

	// ── 并发行为断言：检查 vision 调用的实际开始时间 ──
	offsets := slow.offsets()
	if len(offsets) != 5 {
		t.Fatalf("vision calls = %d, want 5", len(offsets))
	}
	t.Logf("vision call start offsets (ms): %v", offsets)

	// 前 4 个调用应几乎同时开始（< 200ms 偏移）
	for i := 0; i < 4; i++ {
		if offsets[i] > 200 {
			t.Errorf("vision call %d started at %dms, want <200ms (should be concurrent)", i, offsets[i])
		}
	}
	// 第 5 个调用应该在前 4 个完成后才开始（>= 1900ms）
	if offsets[4] < 1900 {
		t.Errorf("vision call 4 started at %dms, want >=1900ms (concurrency_limit=4 should block)", offsets[4])
	}
	// 第 5 个调用不应该超过 2×delay + 余量（即应该在第二批 2s 后开始）
	if offsets[4] > 2500 {
		t.Errorf("vision call 4 started too late: %dms, want <2500ms", offsets[4])
	}

	// 把日志也输出到 stderr，方便 go test -v 时人工查看
	fmt.Fprintln(os.Stderr, "=== captured logs ===")
	fmt.Fprintln(os.Stderr, logs)
}

// TestHandler_ConcurrencyLimit_CustomValue 验证 HandlerDeps.ConcurrencyLimit
// 真实驱动 errgroup.SetLimit：设 limit=2 + 3 张图 × 1s，第 3 张应在第 1 批
// 完成后才开始（offset >= 900ms）。
func TestHandler_ConcurrencyLimit_CustomValue(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	// mock vision: 每张图 1 秒
	slow := newSlowVisionMock(1*time.Second, "SlowMockDesc")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      slow,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		ConcurrencyLimit:    2, // 关键：覆盖默认 4
		Log:                 logger,
	}
	h := NewHandler(deps)

	reqBody := buildNImageRequest(3)

	start := time.Now()
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	// 3 张图 × 1s，limit=2 → 2 批：预期 ~2s（串行 3s）
	if elapsed > 3500*time.Millisecond {
		t.Errorf("elapsed = %v, want <3.5s (limit=2 not working?)", elapsed)
	}
	t.Logf("total elapsed: %v (3 images × 1s, concurrency_limit=2, expected ~2s)", elapsed)

	offsets := slow.offsets()
	if len(offsets) != 3 {
		t.Fatalf("vision calls = %d, want 3", len(offsets))
	}
	t.Logf("vision call start offsets (ms): %v", offsets)

	// 前 2 个并发启动
	for i := 0; i < 2; i++ {
		if offsets[i] > 200 {
			t.Errorf("vision call %d started at %dms, want <200ms", i, offsets[i])
		}
	}
	// 第 3 个应等第 1 批完成（>= 900ms）
	if offsets[2] < 900 {
		t.Errorf("vision call 2 started at %dms, want >=900ms (limit=2 should block)", offsets[2])
	}
	if offsets[2] > 1300 {
		t.Errorf("vision call 2 started too late: %dms, want <1300ms", offsets[2])
	}
}

// TestHandler_AdaptiveConcurrency_DecreasesAcrossRequests 验证自适应限流
// 跨请求真实生效：请求 1 产生 4 个「慢 vision」样本填满窗口，触发 MD 下降
// limit: 4 → 3；请求 2 验证 effective_limit=3 真实驱动 errgroup 批次行为
// （第 4 张图在请求 2 中应等待第一批 3 张完成后才开始）
func TestHandler_AdaptiveConcurrency_DecreasesAcrossRequests(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	// mock vision: 每张图 300ms（故意 > slow_threshold=100ms，稳定触发下降）
	mockDelay := 300 * time.Millisecond
	slow := newSlowVisionMock(mockDelay, "AdaptiveMockDesc")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 自适应配置：initial=4, window=4, cooldown=1ms, slow=100ms(<300ms mock)
	// 注意 max_limit=4 不影响下降（下降只看 ratio）
	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        4,
		InitialLimit:    4,
		FastThresholdMs: 50,
		SlowThresholdMs: 100, // < mock 300ms
		SampleWindow:    4,   // 请求 1 刚好 4 张图填满
		CooldownMs:      1,   // 小值：请求间 sleep 保证超过
		DecreaseRatio:   0.75,
		ErrorThreshold:  0.5,
	}, nil, logger)

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      slow,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		ConcurrencyLimit:    4, // 静态初值
		AdaptiveConcurrency: ac,
		Log:                 logger,
	}
	h := NewHandler(deps)

	// ── 请求 1：4 张不同图（4 个 SF executor，各上报 1 个样本，共 4 个填满窗口）
	//     请求本身 limit=4（还没发生下降）→ 4 张同时开始
	req1Body := buildNImageRequest(4)
	rr1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(req1Body))
	req1.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr1, req1)
	if rr1.Code != 200 {
		t.Fatalf("request 1 status: %d body=%s", rr1.Code, rr1.Body.String())
	}

	offsetsAfterReq1 := slow.offsets()
	t.Logf("after request 1: vision call offsets=%v", offsetsAfterReq1)
	if len(offsetsAfterReq1) != 4 {
		t.Fatalf("request 1: vision calls=%d, want 4", len(offsetsAfterReq1))
	}

	// 请求 1 limit=4 应全部并发启动（<200ms 偏移）
	for i, off := range offsetsAfterReq1 {
		if off > 200 {
			t.Errorf("request 1 call %d started at %dms, want <200ms (concurrent, limit still 4)", i, off)
		}
	}

	// 关键断言：自适应 limit 应已下降（初始 4 → MD floor(4×0.75)=3）
	limitAfterReq1 := ac.CurrentLimit()
	t.Logf("after request 1, adaptive limit=%d (want 3, decreased from initial 4)", limitAfterReq1)
	if limitAfterReq1 != 3 {
		t.Fatalf("after request 1 adaptive limit=%d, want 3 (should have decreased via MD)", limitAfterReq1)
	}

	// 请求间 sleep，保证 cooldown 清零（下一批数据如果有决策可以触发）
	time.Sleep(5 * time.Millisecond)

	// ── 请求 2：4 张不同图，effective_limit=3 应生效 → 前 3 张并发，第 4 张等
	//     注意：用全新的图字节（每张不同，用 i+100 偏移构造，避免与 request1 hash 重叠）
	req2Body := buildNImageRequestOffset(4, 100)
	rr2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(req2Body))
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("request 2 status: %d body=%s", rr2.Code, rr2.Body.String())
	}

	allOffsets := slow.offsets()
	req2Offsets := allOffsets[len(offsetsAfterReq1):] // 取请求 2 产生的 4 个偏移
	t.Logf("after request 2: req2-specific vision call offsets=%v (absolute)", req2Offsets)
	if len(req2Offsets) != 4 {
		t.Fatalf("request 2: new vision calls=%d, want 4", len(req2Offsets))
	}

	// 把 req2Offsets 归一化为 req2 开始时间（相对于请求 2 的第一个调用的起点）
	// （因为 slowVisionMock 的 base 是构造时就设定的，请求 1 已耗费 300ms+）
	first := req2Offsets[0]
	rel := make([]int64, 4)
	for i, v := range req2Offsets {
		rel[i] = v - first
	}
	t.Logf("request 2 vision call offsets (relative to req2-first-call start, ms): %v", rel)

	// limit=3：前 3 张应 < 200ms（并发启动）
	for i := 0; i < 3; i++ {
		if rel[i] > 200 {
			t.Errorf("request 2 call %d relative start=%dms, want <200ms (concurrent batch 3)", i, rel[i])
		}
	}
	// 第 4 张应在第 1 批完成后才开始：mockDelay=300ms，留 20ms 余量
	relMin := int64(280)
	relMax := int64(400) // 正常调度下 300~350ms，给 100ms 余量
	if rel[3] < relMin {
		t.Errorf("request 2 call 3 relative start=%dms, want >=%dms (limit=3 should block 4th until batch complete)", rel[3], relMin)
	}
	if rel[3] > relMax {
		t.Errorf("request 2 call 3 relative start=%dms, want <=%dms (started too late, mock delay is 300ms)", rel[3], relMax)
	}
}

// buildNImageRequestOffset 是 buildNImageRequest 的带偏移版本：
// offset 让两张请求即使张数相同也得到不同的 base64 data（从而不同 hash），避免互相命中缓存。
func buildNImageRequestOffset(n, offset int) string {
	content := []map[string]any{
		{"type": "text", "text": "describe these images"},
	}
	for i := 0; i < n; i++ {
		raw := []byte{byte('A' + i + offset), byte('A' + i + offset), byte('A' + i + offset)}
		data := base64.StdEncoding.EncodeToString(raw)
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]string{
				"type":       "base64",
				"media_type": "image/png",
				"data":       data,
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
