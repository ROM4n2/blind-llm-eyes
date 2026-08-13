package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
)

// ═══════════════════════════════════════════════════════════════════════════════
// 跨请求并发压力测试：用 -race 检测跨请求共享状态的数据竞争
//   覆盖：singleflight 跨请求去重、LRU 缓存并发读写、AdaptiveConcurrency 并发采样、
//         Pool/CircuitBreaker 并发调用、errgroup 内 goroutine 并发改写 req
// ═══════════════════════════════════════════════════════════════════════════════

// TestRaceStress_CrossRequestConcurrentShares 跨请求并发压力测试
// 多个请求共享同一个 handler（singleflight.Group / Cache / AdaptiveConcurrency / Pool），
// 混合相同和不同图片，验证 -race 不报错。
func TestRaceStress_CrossRequestConcurrentShared(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	// 共享的 slow vision mock：50ms 延迟确保 singleflight 有时间合并跨请求调用
	slow := newSlowVisionMock(50*time.Millisecond, "StressDesc")

	// 共享的 handler：所有并发请求共用同一个 singleflight.Group、Cache、AdaptiveConcurrency
	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        8,
		InitialLimit:    4,
		FastThresholdMs: 10,
		SlowThresholdMs: 200,
		SampleWindow:    5,
		CooldownMs:      1,
		DecreaseRatio:   0.75,
		ErrorThreshold:  0.5,
	}, nil, silentLogger())

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      slow,
		Cache:               cache.NewLRU(50),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		ConcurrencyLimit:    4,
		AdaptiveConcurrency: ac,
		UpstreamAPIKey:      "sk-stress-server-key",
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	// 构造 3 组不同的图片数据
	imgA := base64.StdEncoding.EncodeToString([]byte("AAA"))
	imgB := base64.StdEncoding.EncodeToString([]byte("BBB"))
	imgC := base64.StdEncoding.EncodeToString([]byte("CCC"))

	// buildMixedRequest 构造包含顶层 image + tool_result 嵌套 image 的请求
	buildMixedRequest := func(topImg, nestedImg string) string {
		body := map[string]any{
			"model":      "stress-test",
			"max_tokens": 50,
			"messages": []map[string]any{
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "text", "text": "analyze screenshots"},
						{"type": "image", "source": map[string]string{
							"type": "base64", "media_type": "image/png", "data": topImg,
						}},
						{"type": "tool_result", "tool_use_id": "toolu_stress", "content": []map[string]any{
							{"type": "text", "text": "nested screenshot"},
							{"type": "image", "source": map[string]string{
								"type": "base64", "media_type": "image/png", "data": nestedImg,
							}},
						}},
					},
				},
			},
			"stream": true,
		}
		b, _ := json.Marshal(body)
		return string(b)
	}

	// 20 个并发请求，混合 3 种图片组合
	// 组合 1: (imgA, imgB) — 跨请求共享
	// 组合 2: (imgA, imgC) — imgA 跨请求共享，imgC 独有
	// 组合 3: (imgB, imgA) — 与组合 1 的图片相同但位置交换
	requests := []string{
		buildMixedRequest(imgA, imgB),
		buildMixedRequest(imgA, imgC),
		buildMixedRequest(imgB, imgA),
	}

	const numGoroutines = 20
	var wg sync.WaitGroup
	var successCount atomic.Int64
	var errorCount atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			reqBody := requests[idx%len(requests)]
			rr := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
			req.Header.Set("Content-Type", "application/json")
			// 每个请求带不同的客户端 Authorization，验证不会被并发写入上游
			req.Header.Set("Authorization", fmt.Sprintf("Bearer sk-client-%d", idx))
			h.ServeHTTP(rr, req)
			if rr.Code == 200 {
				successCount.Add(1)
			} else {
				errorCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	t.Logf("cross-request stress: %d goroutines, success=%d, error=%d",
		numGoroutines, successCount.Load(), errorCount.Load())

	if successCount.Load() != numGoroutines {
		t.Errorf("success=%d, want %d (all should succeed with fail_open=true)", successCount.Load(), numGoroutines)
	}

	// 验证上游收到的所有请求都不含客户端 key
	// (recordingUpstream 只记录最后一次请求的 header，所以这里只验证最后一次)
	authHeader := up.Headers().Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer sk-client-") {
		t.Errorf("client Authorization leaked to upstream: %q", authHeader)
	}
	if authHeader != "Bearer sk-stress-server-key" {
		t.Errorf("upstream Authorization=%q, want server key", authHeader)
	}

	// 验证 vision 调用数 < 请求数×2（singleflight + cache 去重生效）
	// 3 种唯一图片 (imgA, imgB, imgC)，每请求 2 张图
	// 理论上最少 3 次 vision 调用（每种图片首次 miss）
	// 但由于并发时序，可能有少量重复，上限应远低于 40（20×2）
	visionCalls := len(slow.callStart)
	if visionCalls > 10 {
		t.Errorf("vision calls=%d, expected ≤10 (singleflight+cache dedup should reduce from %d)", visionCalls, numGoroutines*2)
	}
	t.Logf("vision calls=%d (deduped from potential %d via singleflight+cache)", visionCalls, numGoroutines*2)
}

// TestRaceStress_AdaptiveConcurrencyConcurrentSamples 验证 AdaptiveConcurrency
// 在高并发 RecordSample 下不产生数据竞争
func TestRaceStress_AdaptiveConcurrencyConcurrentSamples(t *testing.T) {
	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        8,
		InitialLimit:    4,
		FastThresholdMs: 10,
		SlowThresholdMs: 200,
		SampleWindow:    5,
		CooldownMs:      1,
		DecreaseRatio:   0.75,
		ErrorThreshold:  0.5,
	}, nil, silentLogger())

	var wg sync.WaitGroup
	// 50 个 goroutine 并发调用 RecordSample 和 CurrentLimit
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ac.RecordSample(int64(idx*10), idx%3 == 0)
			_ = ac.CurrentLimit()
			ac.RecordSample(int64(idx*20), idx%5 == 0)
			_ = ac.CurrentLimit()
		}(i)
	}
	wg.Wait()

	// 最终 limit 应在 [min, max] 范围内
	limit := ac.CurrentLimit()
	if limit < 1 || limit > 8 {
		t.Errorf("final limit=%d, want in [1, 8]", limit)
	}
	t.Logf("adaptive concurrency: 100 concurrent RecordSample calls, final limit=%d (no race)", limit)
}
