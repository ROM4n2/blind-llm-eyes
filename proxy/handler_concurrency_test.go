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
