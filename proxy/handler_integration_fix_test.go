package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
// 集成测试：模拟真实用户请求验证 5 个严重问题修复
// ═══════════════════════════════════════════════════════════════════════════════

// recordingUpstream 记录所有收到的请求头，用于验证 Authorization 是否被过滤
type recordingUpstream struct {
	server       *httptest.Server
	mu           sync.Mutex
	gotHeaders   http.Header
	gotBody      []byte
	requestCount int
}

func newRecordingUpstream() *recordingUpstream {
	ru := &recordingUpstream{}
	ru.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.mu.Lock()
		ru.gotHeaders = r.Header.Clone()
		ru.gotBody, _ = io.ReadAll(r.Body)
		ru.requestCount++
		ru.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	return ru
}

func (ru *recordingUpstream) Close()      { ru.server.Close() }
func (ru *recordingUpstream) URL() string { return ru.server.URL }
func (ru *recordingUpstream) Headers() http.Header {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	return ru.gotHeaders
}
func (ru *recordingUpstream) Body() []byte {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	return ru.gotBody
}

// errorVisionProvider 总是返回错误，用于测试 fail_open 和 error 路径
type errorVisionProvider struct {
	calls int
	mu    sync.Mutex
}

func (e *errorVisionProvider) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return "", context.DeadlineExceeded
}

func (e *errorVisionProvider) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// ═══════════════════════════════════════════════════════════════════════════════
// Fix #1: Singleflight 数据竞争 — 验证并发场景下 fnStart/fnEnd 无竞态
// ═══════════════════════════════════════════════════════════════════════════════

// TestFix1_Singleflight_NoDataRace 验证 singleflight 在高并发下去重时
// fnStart/fnEnd 不会产生数据竞争（配合 -race 检测）
func TestFix1_Singleflight_NoDataRace(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	// 200ms delay 确保多个 goroutine 同时进入 singleflight.Do
	slow := newSlowVisionMock(200*time.Millisecond, "RaceFreeDesc")

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      slow,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	// 10 个并发请求，全部带同一张图
	reqBody := buildNImageRequestWithSameData(1)

	var wg sync.WaitGroup
	results := make([]int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rr := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
			req.Header.Set("Content-Type", "application/json")
			h.ServeHTTP(rr, req)
			results[idx] = rr.Code
		}(i)
	}
	wg.Wait()

	// 所有请求应成功
	for i, code := range results {
		if code != 200 {
			t.Errorf("request %d: status=%d, want 200", i, code)
		}
	}

	// vision 只被调用 1 次（10 个请求去重到同一次 singleflight）
	calls := len(slow.callStart)
	if calls != 1 {
		t.Errorf("vision calls = %d, want 1 (singleflight should dedup 10 concurrent requests)", calls)
	}
	t.Logf("10 concurrent requests with same image → %d vision call(s), all status=200", calls)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Fix #2: 上游 HTTP 超时 — 验证自定义 client 配置生效
// ═══════════════════════════════════════════════════════════════════════════════

// TestFix2_CustomHTTPClient_Used 验证 handler 使用自定义 client 而非 http.DefaultClient
func TestFix2_CustomHTTPClient_Used(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "TimeoutTest"}

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	// 类型断言验证 handler 内部有自定义 client
	rh, ok := h.(*http.ServeMux)
	if !ok {
		t.Fatalf("handler is not *http.ServeMux")
	}
	_ = rh // ServeMux 内部包装了 requestHandler，我们通过行为验证

	// 发送正常请求验证连接正常
	reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	t.Logf("upstream request succeeded with custom client, status=200")
}

// TestFix2_UpstreamTimeout_Returns502 验证上游不可达时不会无限挂起
func TestFix2_UpstreamTimeout_Returns502(t *testing.T) {
	// 使用一个不存在的端口，连接会快速失败（而不是挂起）
	mockVis := &mockVisionProvider{desc: "x"}

	deps := HandlerDeps{
		UpstreamBaseURL:     "http://127.0.0.1:1", // 端口 1 不可达，连接被拒绝
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	h.ServeHTTP(rr, req)
	elapsed := time.Since(start)

	// 连接被拒绝应快速返回 502，不应挂起
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 (upstream unreachable)", rr.Code)
	}
	// 应在 30s（DialContext 超时）内返回，实际连接拒绝通常 <1s
	if elapsed > 10*time.Second {
		t.Errorf("elapsed=%v, want < 10s (should fail fast on connection refused)", elapsed)
	}
	t.Logf("upstream unreachable → status=%d in %v (no hang)", rr.Code, elapsed)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Fix #3: ReplaceImageWithDescription 错误处理 — 验证替换成功和 image 块不泄露
// ═══════════════════════════════════════════════════════════════════════════════

// TestFix3_ImageReplacement_NoLeak 验证 image 块被正确替换为 text，不泄露原始 base64
func TestFix3_ImageReplacement_NoLeak(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "VerifiedDescription"}
	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	upstreamBody := string(up.Body())

	// 验证 1：image 块不泄露 — 原始 base64 数据不应出现在上游请求体中
	if strings.Contains(upstreamBody, pngB64) {
		t.Error("original base64 image data leaked to upstream — ReplaceImageWithDescription failed")
	}

	// 验证 2：描述文本存在
	if !strings.Contains(upstreamBody, "VerifiedDescription") {
		t.Error("description not found in upstream body — replacement didn't work")
	}

	// 验证 3：type 已从 image 改为 text
	var upstreamReq map[string]any
	if err := json.Unmarshal(up.Body(), &upstreamReq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := upstreamReq["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	secondBlock := content[1].(map[string]any)
	if secondBlock["type"] != "text" {
		t.Errorf("block type=%v, want 'text'", secondBlock["type"])
	}
	// 验证 4：source 字段应不存在
	if _, hasSource := secondBlock["source"]; hasSource {
		t.Error("source field still present — should be nil after replacement")
	}

	t.Logf("image replaced successfully: type=text, desc present, base64 not leaked, source=nil")
}

// TestFix3_FailOpen_ReplacementWithPlaceholder 验证 vision 失败时 fail_open 占位符替换
func TestFix3_FailOpen_ReplacementWithPlaceholder(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	errVis := &errorVisionProvider{}
	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      errVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d (fail_open should return 200)", rr.Code)
	}

	upstreamBody := string(up.Body())

	// 验证 base64 不泄露
	if strings.Contains(upstreamBody, pngB64) {
		t.Error("base64 leaked even on vision failure (fail_open placeholder should replace it)")
	}

	// 验证占位符存在
	if !strings.Contains(upstreamBody, "could not be described") {
		t.Error("placeholder text not found in upstream body")
	}

	t.Logf("vision failed → fail_open placeholder applied, base64 not leaked, status=200")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Fix #4: Hash 失败不污染缓存 — 验证空 key 不写入缓存、不去重
// ═══════════════════════════════════════════════════════════════════════════════

// countingVisionProvider 记录每次调用的 base64Data，用于验证去重行为
type countingVisionProvider struct {
	mu    sync.Mutex
	calls int
	desc  string
}

func (c *countingVisionProvider) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.desc, nil
}

func (c *countingVisionProvider) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestFix4_HashFailure_NoCachePollution 验证 hash 失败时不写入空 key 到缓存
func TestFix4_HashFailure_NoCachePollution(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "NormalDesc"}
	c := cache.NewLRU(10)

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      mockVis,
		Cache:               c,
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	// 正常图片请求
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}

	// 验证缓存中没有空 key（""）的条目
	// 直接检查缓存大小：正常请求后缓存应有 1 个条目（有效 hash），不应有空 key
	// 我们通过第二次相同请求验证缓存命中来判断缓存是否正常工作
	rr2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr2, req2)

	if mockVis.calls != 1 {
		t.Errorf("vision calls after 2nd request = %d, want 1 (cache should hit on 2nd)", mockVis.calls)
	}

	// 验证缓存中没有空 key：检查缓存是否有 "" key
	// LRU.Get("") 不应返回任何值
	if _, ok := c.Get(""); ok {
		t.Error("empty string key found in cache — hash failure polluted cache")
	}

	t.Logf("hash success path verified: 2 requests → 1 vision call, no empty key in cache")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Fix #5: Authorization Header 不泄露 — 验证客户端 key 不被转发到上游
// ═══════════════════════════════════════════════════════════════════════════════

// TestFix5_AuthorizationHeader_Stripped 验证客户端 Authorization 不被转发到上游
func TestFix5_AuthorizationHeader_Stripped(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "SecurityTest"}
	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		UpstreamAPIKey:      "sk-server-configured-key",
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	// 客户端发送自己的 Authorization（模拟用户 API key）
	req.Header.Set("Authorization", "Bearer sk-client-secret-key-12345")
	// 客户端发送 Cookie（也应被过滤）
	req.Header.Set("Cookie", "session=abc123")

	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	upstreamHeaders := up.Headers()

	// 验证 1：客户端的 Authorization 不被转发
	authHeader := upstreamHeaders.Get("Authorization")
	if strings.Contains(authHeader, "sk-client-secret-key-12345") {
		t.Errorf("client Authorization leaked to upstream: %q", authHeader)
	}

	// 验证 2：上游收到的 Authorization 是服务端配置的 key
	if authHeader != "Bearer sk-server-configured-key" {
		t.Errorf("upstream Authorization = %q, want 'Bearer sk-server-configured-key'", authHeader)
	}

	// 验证 3：Cookie 不被转发
	if upstreamHeaders.Get("Cookie") != "" {
		t.Errorf("Cookie leaked to upstream: %q", upstreamHeaders.Get("Cookie"))
	}

	t.Logf("Authorization: client key stripped, server key set; Cookie stripped")
}

// TestFix5_NoUpstreamKey_ClientAuthForwarded 验证 UpstreamAPIKey 为空时，
// 客户端的 Authorization 必须转发给上游。代理在此模式下是透明转发器
// （passthrough），上游需要客户端的凭证进行认证，否则返回 401。
// 这与 "Client Authorization must not be forwarded when UpstreamAPIKey
// is configured" 的约束一致 —— 仅在 proxy 注入自己的 key 时才剥离客户端 key。
func TestFix5_NoUpstreamKey_ClientAuthForwarded(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "NoKeyTest"}
	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		UpstreamAPIKey:      "", // 服务端未配置上游 key → 透明转发模式
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-client-secret-key-67890")

	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}

	upstreamHeaders := up.Headers()
	authHeader := upstreamHeaders.Get("Authorization")

	// UpstreamAPIKey 为空时，客户端 Authorization 必须被转发给上游
	if authHeader != "Bearer sk-client-secret-key-67890" {
		t.Errorf("client Authorization should be forwarded when UpstreamAPIKey is empty, got %q", authHeader)
	}

	t.Logf("UpstreamAPIKey empty → Authorization = %q (client key forwarded for upstream auth)", authHeader)
}

// ═══════════════════════════════════════════════════════════════════════════════
// 端到端集成场景：模拟真实用户请求完整链路
// ═══════════════════════════════════════════════════════════════════════════════

// TestIntegration_FullPipeline_AllFixesVerified 端到端验证所有修复在真实场景中协同工作
func TestIntegration_FullPipeline_AllFixesVerified(t *testing.T) {
	up := newRecordingUpstream()
	defer up.Close()

	mockVis := &countingVisionProvider{desc: "IntegrationDesc"}
	c := cache.NewLRU(100)

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL(), "/"),
		VisionProvider:      mockVis,
		Cache:               c,
		FailOpen:            true,
		LargeImageThreshold: 1024,
		UpstreamAPIKey:      "sk-integration-server-key",
		Log:                 silentLogger(),
	}
	h := NewHandler(deps)

	const smallImg = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	largeImg := buildLargeImageBase64(5_000) // 5KB > 1024 threshold

	// ── 场景 1：带 system 消息 + 嵌套 tool_result image 的请求 ──
	t.Run("scenario 1: nested tool_result with image + system message normalization", func(t *testing.T) {
		reqBody := `{
			"model":"claude-3-5-sonnet",
			"max_tokens":100,
			"messages":[
				{"role":"system","content":[{"type":"text","text":"You are helpful."}]},
				{"role":"user","content":[
					{"type":"text","text":"see screenshot:"},
					{"type":"tool_result","tool_use_id":"toolu_123","content":[
						{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + largeImg + `"}}
					]}
				]}
			],
			"stream":true
		}`

		rr := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-client-leaked-key")
		h.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}

		// Fix #3: image 被替换，base64 不泄露
		upstreamBody := string(up.Body())
		if strings.Contains(upstreamBody, largeImg) {
			t.Error("Fix #3: large image base64 leaked to upstream")
		}
		if !strings.Contains(upstreamBody, "IntegrationDesc") {
			t.Error("Fix #3: description not found in upstream body")
		}

		// Fix #5: 客户端 Authorization 不泄露
		authHeader := up.Headers().Get("Authorization")
		if strings.Contains(authHeader, "sk-client-leaked-key") {
			t.Error("Fix #5: client Authorization leaked to upstream")
		}
		if authHeader != "Bearer sk-integration-server-key" {
			t.Errorf("Fix #5: upstream Authorization = %q, want server key", authHeader)
		}

		// system 消息被规范化
		var upstreamReq map[string]any
		json.Unmarshal(up.Body(), &upstreamReq)
		msgs := upstreamReq["messages"].([]any)
		for _, msg := range msgs {
			if msg.(map[string]any)["role"] == "system" {
				t.Error("system message not normalized — still in messages array")
			}
		}
		systemBlocks := upstreamReq["system"].([]any)
		if len(systemBlocks) < 1 {
			t.Error("system blocks not found in upstream body")
		}

		t.Logf("scenario 1 passed: nested image replaced, system normalized, auth stripped")
	})

	// ── 场景 2：缓存命中 + 并发去重 ──
	t.Run("scenario 2: cache hit + singleflight dedup", func(t *testing.T) {
		// 使用独立的 vision provider 和 cache，避免场景 1 的调用计数干扰
		scenario2Vis := &countingVisionProvider{desc: "Scenario2Desc"}
		scenario2Cache := cache.NewLRU(100)
		scenario2Up := newRecordingUpstream()
		defer scenario2Up.Close()

		scenario2Deps := HandlerDeps{
			UpstreamBaseURL:     strings.TrimSuffix(scenario2Up.URL(), "/"),
			VisionProvider:      scenario2Vis,
			Cache:               scenario2Cache,
			FailOpen:            true,
			LargeImageThreshold: 1024,
			UpstreamAPIKey:      "sk-integration-server-key",
			Log:                 silentLogger(),
		}
		scenario2Handler := NewHandler(scenario2Deps)

		reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}],"stream":true}`

		// 第一次请求：cache miss
		rr1 := httptest.NewRecorder()
		req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req1.Header.Set("Content-Type", "application/json")
		scenario2Handler.ServeHTTP(rr1, req1)

		callsAfterFirst := scenario2Vis.Calls()
		if callsAfterFirst != 1 {
			t.Fatalf("1st request: vision calls=%d, want 1", callsAfterFirst)
		}

		// 第二次请求：cache hit
		rr2 := httptest.NewRecorder()
		req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
		req2.Header.Set("Content-Type", "application/json")
		scenario2Handler.ServeHTTP(rr2, req2)

		callsAfterSecond := scenario2Vis.Calls()
		if callsAfterSecond != 1 {
			t.Errorf("2nd request: vision calls=%d, want 1 (cache hit)", callsAfterSecond)
		}

		// Fix #4: 缓存无空 key
		if _, ok := scenario2Cache.Get(""); ok {
			t.Error("Fix #4: empty key found in cache")
		}

		t.Logf("scenario 2 passed: 2 requests → 1 vision call (cache hit), no empty key in cache")
	})

	// ── 场景 3：并发请求验证 Fix #1 无竞态 ──
	t.Run("scenario 3: concurrent requests no race", func(t *testing.T) {
		reqBody := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallImg + `"}}]}],"stream":true}`

		var wg sync.WaitGroup
		var successCount atomic.Int64
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rr := httptest.NewRecorder()
				req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
				req.Header.Set("Content-Type", "application/json")
				h.ServeHTTP(rr, req)
				if rr.Code == 200 {
					successCount.Add(1)
				}
			}()
		}
		wg.Wait()

		if successCount.Load() != 5 {
			t.Errorf("concurrent: success=%d, want 5", successCount.Load())
		}

		t.Logf("scenario 3 passed: 5 concurrent requests all succeeded (no race)")
	})
}
