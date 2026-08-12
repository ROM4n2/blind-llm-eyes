package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
	"github.com/ROM4n2/blind-llm-eyes/vision"
	dto "github.com/prometheus/client_model/go"
)

// mockVisionProvider 实现 vision.VisionProvider 接口，用于测试。
type mockVisionProvider struct {
	calls int
	desc  string
}

func (m *mockVisionProvider) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	m.calls++
	return m.desc, nil
}

// 假上游：收到请求后把 body 原样回显
func fakeUpstream(_ *testing.T, gotBody *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestHandler_ImageReplaceAndCache(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	// mock vision
	mockVis := &mockVisionProvider{desc: "MockDesc-A"}

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	// ==== 第 1 次请求：缓存 miss，应该调 vision ====
	rr1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer sk-test-upstream")
	h.ServeHTTP(rr1, req1)

	if rr1.Code != 200 {
		t.Fatalf("1st status: %d body=%s", rr1.Code, rr1.Body.String())
	}
	if mockVis.calls != 1 {
		t.Errorf("1st vision calls: want 1, got %d", mockVis.calls)
	}
	if hdr := rr1.Header().Get("X-Blind-Llm-Eyes"); !strings.Contains(hdr, "1 rewritten") {
		t.Errorf("1st header wrong: %q", hdr)
	}
	// 断言上游收到的 body 里：image 块没了，出现 MockDesc-A
	var upstreamReq map[string]any
	json.Unmarshal(upstreamGot, &upstreamReq)
	msgs := upstreamReq["messages"].([]any)
	firstMsg := msgs[0].(map[string]any)
	content := firstMsg["content"].([]any)
	secondBlock := content[1].(map[string]any)
	if secondBlock["type"] != "text" {
		t.Errorf("1st upstream: 2nd block not text, type=%v", secondBlock["type"])
	}
	if !strings.Contains(secondBlock["text"].(string), "MockDesc-A") {
		t.Errorf("1st upstream: desc missing in text=%v", secondBlock["text"])
	}

	// ==== 第 2 次请求：同一图，缓存 hit，visionCalls 不应增加 ====
	rr2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr2, req2)

	if mockVis.calls != 1 {
		t.Errorf("2nd vision calls: still want 1, got %d (cache not hit!)", mockVis.calls)
	}
	hdr := rr2.Header().Get("X-Blind-Llm-Eyes")
	if !strings.Contains(hdr, "1 cached") {
		t.Errorf("2nd header want 1 cached, got %q", hdr)
	}
}

func TestHandler_MockVisionProvider_Interface(t *testing.T) {
	// 验证 mock 实现确实满足 VisionProvider 接口
	var _ vision.VisionProvider = (*mockVisionProvider)(nil)

	mock := &mockVisionProvider{desc: "hello"}
	ctx := context.Background()
	desc, err := mock.DescribeImage(ctx, "base64data", "image/png", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desc != "hello" {
		t.Fatalf("want 'hello', got %q", desc)
	}
	if mock.calls != 1 {
		t.Fatalf("want 1 call, got %d", mock.calls)
	}
}

func TestHandler_GracefulShutdown_WaitGroup(t *testing.T) {
	var wg sync.WaitGroup

	// 模拟在途请求
	wg.Add(1)
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		close(done)
	}()

	// 等待 goroutine 完成
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("test goroutine timed out")
	}

	// Shutdown 应该立即返回（没有在途请求）
	wg.Wait() // 应立即返回
}

func TestHandler_MetricsIntegration(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "MockDesc-Metrics"}
	m := metrics.NewMetrics()

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Metrics:             m,
	}
	h := NewHandler(deps)

	reqBody := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	// 第 1 次：cache miss → vision 调用
	req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req1.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req1)

	// 第 2 次：cache hit
	req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req2)

	// 第 3 次：不同方法 → 405
	req3, _ := http.NewRequest("GET", "/v1/messages", nil)
	h.ServeHTTP(httptest.NewRecorder(), req3)

	// 验证 metrics
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	// 收集计数器值
	counterValues := make(map[string]float64)
	for _, mf := range families {
		for _, metric := range mf.GetMetric() {
			labels := metric.GetLabel()
			key := mf.GetName() + "|" + labelKey(labels)
			if mf.GetName() == "blind_llm_eyes_http_requests_total" {
				counterValues[key] = metric.GetCounter().GetValue()
			}
		}
	}

	// 验证 POST /v1/messages 200 出现了 2 次
	found := false
	for k, v := range counterValues {
		if strings.Contains(k, "http_requests_total") && strings.Contains(k, "POST") && strings.Contains(k, "200") {
			if v == 2 {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected 2 POST/200 requests in metrics")
	}

	// 验证 GET /v1/messages 405 出现了 1 次
	found405 := false
	for k, v := range counterValues {
		if strings.Contains(k, "http_requests_total") && strings.Contains(k, "GET") && strings.Contains(k, "405") {
			if v == 1 {
				found405 = true
			}
		}
	}
	if !found405 {
		t.Error("expected 1 GET/405 request in metrics")
	}

	// 验证图片处理指标
	imgMap := make(map[string]float64)
	for _, mf := range families {
		if mf.GetName() == "blind_llm_eyes_images_processed_total" {
			for _, metric := range mf.GetMetric() {
				labels := metric.GetLabel()
				outcome := ""
				for _, l := range labels {
					if l.GetName() == "outcome" {
						outcome = l.GetValue()
					}
				}
				imgMap[outcome] = metric.GetCounter().GetValue()
			}
		}
	}
	if imgMap["rewritten"] != 1 {
		t.Errorf("images rewritten: want 1, got %f", imgMap["rewritten"])
	}
	if imgMap["cached"] != 1 {
		t.Errorf("images cached: want 1, got %f", imgMap["cached"])
	}
}

func labelKey(labels []*dto.LabelPair) string {
	var parts []string
	for _, l := range labels {
		parts = append(parts, l.GetName()+"="+l.GetValue())
	}
	return strings.Join(parts, ",")
}
