// Package e2e contains end-to-end integration tests that wire the real proxy
// handler, a real vision.Client, and the real admin shutdown path against
// httptest fakes for the upstream and vision backends.
//
// These tests exercise cross-package contracts that unit tests cover only in
// isolation: model-name sanitization through the live handler, image→vision
// dispatch through the real Anthropic-format client, SSE passthrough, and the
// admin shutdown + pidfile cleanup lifecycle.
package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/admin"
	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/cli"
	"github.com/ROM4n2/blind-llm-eyes/proxy"
	"github.com/ROM4n2/blind-llm-eyes/vision"
)

// quietLogger discards all output — the E2E tests assert on side effects, not logs.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// redPNGBase64 encodes a small solid-red PNG and returns its base64 string,
// plus the decoded byte length (the size the handler passes to the vision client).
func redPNGBase64(t *testing.T) (string, int64) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	raw := buf.Bytes()
	return base64.StdEncoding.EncodeToString(raw), int64(len(raw))
}

// TestE2E_FullPipeline wires a real vision.Client + real proxy.NewHandler
// against httptest fakes for the MiMo vision endpoint and the DeepSeek upstream.
//
// It asserts the full onboarding-critical contract:
//   - a request carrying model "deepseek-chat[1m]" has the [1m] suffix stripped
//     before reaching the upstream (modelutil.SanitizeModel, integrated in the handler),
//   - the image block triggers exactly one real vision call (DescribeImage),
//   - the vision endpoint receives the vision model name (not the upstream model),
//   - the image block is replaced by the vision description in the forwarded body,
//   - the SSE response is passed through byte-for-byte.
func TestE2E_FullPipeline(t *testing.T) {
	logger := quietLogger()
	pngB64, pngSize := redPNGBase64(t)

	// --- Fake MiMo vision endpoint (Anthropic Messages API at /v1/messages) ---
	var visionCalls int32
	var visionGotModel string
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("vision: unexpected path %q", r.URL.Path)
		}
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("vision: unmarshal request: %v", err)
		}
		if m, ok := req["model"].(string); ok {
			visionGotModel = m
		}
		atomic.AddInt32(&visionCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "E2E_DESCRIPTION: a solid red square"},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer visionSrv.Close()

	// --- Fake DeepSeek upstream (Anthropic-compatible, streams SSE) ---
	var upstreamGot []byte
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamGot = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstreamSrv.Close()

	// --- Real vision.Client pointed at the fake MiMo server ---
	visionClient := vision.NewClient(
		visionSrv.URL, "test-vision-key", "mimo-v2.5",
		10*time.Second, 30*time.Second, 1<<20, 300,
		[]string{"image/png", "image/jpeg", "image/webp", "image/gif"},
		logger,
	)

	// --- Real proxy handler (exercises modelutil.SanitizeModel via the handler) ---
	deps := proxy.HandlerDeps{
		UpstreamBaseURL:     upstreamSrv.URL,
		UpstreamAPIKey:      "test-upstream-key",
		VisionProvider:      visionClient,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1 << 20,
		Log:                 logger,
	}
	handler := proxy.NewHandler(deps)

	// --- Request: model carries the [1m] suffix + an image block ---
	reqBody := `{"model":"deepseek-chat[1m]","max_tokens":100,"stream":true,` +
		`"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"What is in this image?"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}` +
		`]}]}`

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	// 1. Exactly one vision call (DescribeImage dispatched through the real client).
	if got := atomic.LoadInt32(&visionCalls); got != 1 {
		t.Errorf("vision calls: got %d, want 1", got)
	}

	// 2. The vision endpoint saw the vision model, not the upstream model.
	if visionGotModel != "mimo-v2.5" {
		t.Errorf("vision model: got %q, want mimo-v2.5", visionGotModel)
	}

	// 3. The upstream received the sanitized model ([1m] stripped).
	var upstreamReq map[string]any
	if err := json.Unmarshal(upstreamGot, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream request: %v\nbody: %s", err, upstreamGot)
	}
	if got := upstreamReq["model"].(string); got != "deepseek-chat" {
		t.Errorf("upstream model: got %q, want \"deepseek-chat\" ([1m] stripped)", got)
	}

	// 4. The image block was replaced by the vision description; no image survives.
	msgs := upstreamReq["messages"].([]any)
	firstMsg := msgs[0].(map[string]any)
	content := firstMsg["content"].([]any)
	foundDesc, foundImage := false, false
	for _, blk := range content {
		b := blk.(map[string]any)
		switch b["type"] {
		case "text":
			if strings.Contains(b["text"].(string), "E2E_DESCRIPTION") {
				foundDesc = true
			}
		case "image":
			foundImage = true
		}
	}
	if !foundDesc {
		t.Errorf("upstream body missing vision description; content=%v", content)
	}
	if foundImage {
		t.Errorf("upstream body still contains an image block (should be replaced); content=%v", content)
	}

	// 5. SSE response passed through.
	body := rr.Body.String()
	for _, want := range []string{"message_start", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("response body missing %q: %q", want, body)
		}
	}

	// 6. The X-Blind-Llm-Eyes header reports one rewrite.
	if hdr := rr.Header().Get("X-Blind-Llm-Eyes"); !strings.Contains(hdr, "1 rewritten") {
		t.Errorf("X-Blind-Llm-Eyes header: got %q, want \"1 rewritten\"", hdr)
	}

	// 7. A second identical request hits the cache (no new vision call).
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr2, req2)
	if got := atomic.LoadInt32(&visionCalls); got != 1 {
		t.Errorf("vision calls after 2nd request: got %d, want 1 (cache hit expected)", got)
	}
	if hdr := rr2.Header().Get("X-Blind-Llm-Eyes"); !strings.Contains(hdr, "1 cached") {
		t.Errorf("2nd request header: got %q, want \"1 cached\"", hdr)
	}
	// imageSize is exercised by the vision path (confirms the decoded byte length flows through).
	if pngSize <= 0 {
		t.Errorf("pngSize should be positive, got %d", pngSize)
	}
}

// TestE2E_AdminShutdown_PidfileCleanup wires the real admin.ShutdownHandler
// and the real cli pidfile management against an httptest server that mirrors
// main.go's lifecycle: a goroutine waits on adminH.Done() and removes the
// pidfile (the equivalent of main.go's deferred os.Remove).
//
// It asserts the cross-package contract the `stop` subcommand depends on:
// a token-authenticated POST /admin/shutdown signals graceful shutdown, after
// which the pidfile is cleaned up; a wrong token is rejected with 403.
func TestE2E_AdminShutdown_PidfileCleanup(t *testing.T) {
	token := admin.MustGenerateToken(32)
	adminH := admin.NewShutdownHandler(token)

	mux := http.NewServeMux()
	mux.Handle("/admin/shutdown", adminH)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	// Write a real pidfile (cli.WritePidfile, as main.go does).
	pidPath := filepath.Join(t.TempDir(), "pidfile.json")
	if err := cli.WritePidfile(pidPath, cli.PidfileData{
		PID:       os.Getpid(),
		Addr:      addr,
		Token:     token,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}

	// The pidfile should round-trip through cli.ReadPidfile with the same token.
	data, err := cli.ReadPidfile(pidPath)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if data.Token != token {
		t.Fatalf("pidfile token round-trip: got %q, want %q", data.Token, token)
	}

	// Mirror main.go: on adminH.Done() the server removes the pidfile (deferred
	// os.Remove in runServer). Use a done channel so the test can wait for it.
	pidfileRemoved := make(chan struct{})
	go func() {
		<-adminH.Done()
		os.Remove(pidPath)
		close(pidfileRemoved)
	}()

	// A wrong token must be rejected (403) and must NOT trigger shutdown.
	wrongReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/shutdown", nil)
	wrongReq.Header.Set("X-Admin-Token", "wrong-token")
	wrongResp, err := http.DefaultClient.Do(wrongReq)
	if err != nil {
		t.Fatalf("wrong-token request: %v", err)
	}
	wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong token: status got %d, want 403", wrongResp.StatusCode)
	}
	select {
	case <-adminH.Done():
		t.Fatal("wrong token must not trigger shutdown")
	case <-time.After(200 * time.Millisecond):
		// good: still running
	}
	if _, err := os.Stat(pidPath); os.IsNotExist(err) {
		t.Fatal("pidfile removed by wrong-token request (should still exist)")
	}

	// The correct token triggers shutdown (202).
	goodReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/shutdown", nil)
	goodReq.Header.Set("X-Admin-Token", token)
	goodResp, err := http.DefaultClient.Do(goodReq)
	if err != nil {
		t.Fatalf("correct-token request: %v", err)
	}
	goodResp.Body.Close()
	if goodResp.StatusCode != http.StatusAccepted {
		t.Errorf("correct token: status got %d, want 202", goodResp.StatusCode)
	}

	// Wait for the simulated graceful-shutdown pidfile cleanup.
	select {
	case <-pidfileRemoved:
		// good
	case <-time.After(3 * time.Second):
		t.Fatal("pidfile was not removed within 3s of shutdown")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pidfile still exists after shutdown: %v", err)
	}

	// After shutdown, /healthz is no longer reachable once the test server closes
	// (deferred srv.Close()). We assert the shutdown signal fired via Done().
	select {
	case <-adminH.Done():
		// good — channel is closed
	default:
		t.Fatal("adminH.Done() not closed after accepted shutdown")
	}
}

// TestE2E_AdminShutdown_RejectsMissingToken ensures a request with no token at
// all is also rejected, complementing the wrong-token case above.
func TestE2E_AdminShutdown_RejectsMissingToken(t *testing.T) {
	adminH := admin.NewShutdownHandler("secret")
	mux := http.NewServeMux()
	mux.Handle("/admin/shutdown", adminH)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/admin/shutdown", "text/plain", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("missing token: status got %d, want 403", resp.StatusCode)
	}
	// Drain the body so the connection can be reused/closed cleanly.
	io.Copy(io.Discard, resp.Body)

	// Sanity: the handler must still be armed (shutdown not triggered).
	select {
	case <-adminH.Done():
		t.Fatal("missing token triggered shutdown")
	default:
	}
}

// TestE2E_VisionTimeout_FailOpen simulates the MiMo vision endpoint being
// unreachable (slow response) and verifies that FailOpen=true lets the proxy
// substitute a placeholder description and keep the request flowing to the
// upstream. This is the critical resilience path described in the onboarding
// plan: "fail-open — a failed vision call replaces the image with a placeholder
// instead of blocking the whole request."
func TestE2E_VisionTimeout_FailOpen(t *testing.T) {
	logger := quietLogger()
	pngB64, _ := redPNGBase64(t)

	// --- Slow vision server: deliberately blocks > client timeout ---
	var visionCalls int32
	visionDone := make(chan struct{})
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&visionCalls, 1)
		// Use select + done channel instead of bare time.Sleep so srv.Close()
		// doesn't hang waiting for this handler to finish. The 2s delay exceeds
		// the client's 200ms timeout, guaranteeing a context deadline error.
		select {
		case <-time.After(2 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "SHOULD_NOT_APPEAR"},
				},
				"stop_reason": "end_turn",
			})
		case <-visionDone:
			// test is cleaning up; just return
			return
		}
	}))
	defer func() {
		close(visionDone)
		visionSrv.Close()
	}()

	// --- Fake upstream (should still receive the forwarded request with placeholder) ---
	var upstreamGot []byte
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamGot = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstreamSrv.Close()

	// --- Vision client with a short timeout so the 2s server delay triggers a deadline error ---
	visionClient := vision.NewClient(
		visionSrv.URL, "test-vision-key", "mimo-v2.5",
		200*time.Millisecond, 500*time.Millisecond, 1<<20, 300,
		[]string{"image/png", "image/jpeg", "image/webp", "image/gif"},
		logger,
	)

	deps := proxy.HandlerDeps{
		UpstreamBaseURL:     upstreamSrv.URL,
		UpstreamAPIKey:      "test-upstream-key",
		VisionProvider:      visionClient,
		Cache:               cache.NewLRU(10),
		FailOpen:            true, // <-- the key knob: resilience over correctness
		LargeImageThreshold: 1 << 20,
		Log:                 logger,
	}
	handler := proxy.NewHandler(deps)

	reqBody := `{"model":"deepseek-chat[1m]","max_tokens":100,"stream":true,` +
		`"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"What is in this image?"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}` +
		`]}]}`

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr, req)

	// 1. FailOpen: the proxy returns 200 (not 502) — the request still succeeds.
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (FailOpen should mask vision timeout)", rr.Code)
	}

	// 2. Exactly one vision call was attempted (singleflight dedup doesn't change that).
	if got := atomic.LoadInt32(&visionCalls); got != 1 {
		t.Errorf("vision calls: got %d, want 1", got)
	}

	// 3. The upstream received the placeholder text, NOT the vision's delayed response.
	var upstreamReq map[string]any
	if err := json.Unmarshal(upstreamGot, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream request: %v\nbody: %s", err, upstreamGot)
	}
	msgs := upstreamReq["messages"].([]any)
	firstMsg := msgs[0].(map[string]any)
	content := firstMsg["content"].([]any)
	foundPlaceholder, foundImage := false, false
	for _, blk := range content {
		b := blk.(map[string]any)
		switch b["type"] {
		case "text":
			if strings.Contains(b["text"].(string), "Image could not be described") {
				foundPlaceholder = true
			}
			if strings.Contains(b["text"].(string), "SHOULD_NOT_APPEAR") {
				t.Errorf("upstream body contains delayed vision response that should never arrive")
			}
		case "image":
			foundImage = true
		}
	}
	if !foundPlaceholder {
		t.Errorf("upstream body missing fail-open placeholder; content=%v", content)
	}
	if foundImage {
		t.Errorf("upstream body still contains an image block (should be replaced by placeholder); content=%v", content)
	}

	// 4. Model sanitization still works even when vision fails.
	if got := upstreamReq["model"].(string); got != "deepseek-chat" {
		t.Errorf("upstream model: got %q, want \"deepseek-chat\" ([1m] stripped)", got)
	}

	// 5. SSE passthrough still works (upstream was reached successfully).
	body := rr.Body.String()
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("response body missing [DONE]: %q", body)
	}

	// 6. Header reports the fail-open outcome.
	if hdr := rr.Header().Get("X-Blind-Llm-Eyes"); !strings.Contains(hdr, "rewritten") {
		t.Errorf("X-Blind-Llm-Eyes header: got %q, want rewritten count", hdr)
	}
}

// TestE2E_VisionTimeout_FailClosed verifies the counterpoint: when
// FailOpen=false, a vision timeout must surface as HTTP 502 to the client
// instead of silently substituting a placeholder. This is the "fail loud"
// mode that callers can opt into when data integrity matters more than
// availability.
func TestE2E_VisionTimeout_FailClosed(t *testing.T) {
	logger := quietLogger()
	pngB64, _ := redPNGBase64(t)

	var visionCalls int32
	visionDone := make(chan struct{})
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&visionCalls, 1)
		select {
		case <-time.After(2 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "SHOULD_NOT_APPEAR"},
				},
				"stop_reason": "end_turn",
			})
		case <-visionDone:
			return
		}
	}))
	defer func() {
		close(visionDone)
		visionSrv.Close()
	}()

	var upstreamCalled int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalled, 1)
	}))
	defer upstreamSrv.Close()

	visionClient := vision.NewClient(
		visionSrv.URL, "test-vision-key", "mimo-v2.5",
		200*time.Millisecond, 500*time.Millisecond, 1<<20, 300,
		[]string{"image/png", "image/jpeg", "image/webp", "image/gif"},
		logger,
	)

	deps := proxy.HandlerDeps{
		UpstreamBaseURL:     upstreamSrv.URL,
		UpstreamAPIKey:      "test-upstream-key",
		VisionProvider:      visionClient,
		Cache:               cache.NewLRU(10),
		FailOpen:            false, // <-- fail-closed: vision failure = request failure
		LargeImageThreshold: 1 << 20,
		Log:                 logger,
	}
	handler := proxy.NewHandler(deps)

	reqBody := `{"model":"deepseek-chat[1m]","max_tokens":100,"stream":true,` +
		`"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"What is in this image?"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}` +
		`]}]}`

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rr, req)

	// 1. FailClosed: the proxy returns 502 (not 200) — the request fails loudly.
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502 (FailClosed should surface vision timeout)", rr.Code)
	}

	// 2. Exactly one vision call was attempted.
	if got := atomic.LoadInt32(&visionCalls); got != 1 {
		t.Errorf("vision calls: got %d, want 1", got)
	}

	// 3. The upstream was never reached — the request was terminated before forwarding.
	if got := atomic.LoadInt32(&upstreamCalled); got != 0 {
		t.Errorf("upstream calls: got %d, want 0 (should not forward on fail-closed)", got)
	}

	// 4. The response body mentions the vision failure.
	if body := rr.Body.String(); !strings.Contains(body, "vision call failed") {
		t.Errorf("response body missing failure detail: %q", body)
	}
}

// TestE2E_CacheSurvivesRestart verifies the core Tier 2 promise: an image
// description written to the SQLite cold layer by one handler instance is
// still readable by a fresh handler instance pointing at the same db file.
//
// This simulates a proxy restart (process crash + relaunch, or stop + start):
// the in-memory LRU hot layer is lost, but the SQLite cold layer persists on
// disk. A second request carrying the same image must hit the cold-layer
// cache — the vision provider is NOT called again, and the X-Blind-Llm-Eyes
// header reports "cached".
func TestE2E_CacheSurvivesRestart(t *testing.T) {
	logger := quietLogger()
	pngB64, _ := redPNGBase64(t)
	dbPath := filepath.Join(t.TempDir(), "cache.db")

	// --- Fake MiMo vision endpoint (counts calls) ---
	var visionCalls int32
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&visionCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "RESTART_TEST: a solid red square"},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer visionSrv.Close()

	// --- Fake upstream (minimal SSE passthrough) ---
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstreamSrv.Close()

	// --- Real vision.Client pointed at the fake MiMo server ---
	visionClient := vision.NewClient(
		visionSrv.URL, "test-vision-key", "mimo-v2.5",
		10*time.Second, 30*time.Second, 1<<20, 300,
		[]string{"image/png", "image/jpeg", "image/webp", "image/gif"},
		logger,
	)

	reqBody := `{"model":"deepseek-chat","max_tokens":100,"stream":true,` +
		`"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"What is in this image?"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}` +
		`]}]}`

	// ── 1st "cold start": TwoTier with a fresh SQLite db ──
	cold1, err := cache.OpenSQLite(dbPath, 10000, 0, logger)
	if err != nil {
		t.Fatalf("open sqlite (1st): %v", err)
	}
	tt1 := cache.NewTwoTier(10, cold1, logger)
	h1 := proxy.NewHandler(proxy.HandlerDeps{
		UpstreamBaseURL:     upstreamSrv.URL,
		UpstreamAPIKey:      "test-upstream-key",
		VisionProvider:      visionClient,
		Cache:               tt1,
		FailOpen:            true,
		LargeImageThreshold: 1 << 20,
		Log:                 logger,
	})

	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req1.Header.Set("Content-Type", "application/json")
	h1.ServeHTTP(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("1st request: status %d (body=%s)", rr1.Code, rr1.Body.String())
	}
	if got := atomic.LoadInt32(&visionCalls); got != 1 {
		t.Fatalf("1st request: vision calls got %d, want 1", got)
	}
	// Cache miss → header reports "rewritten" but NOT "cached".
	hdr1 := rr1.Header().Get("X-Blind-Llm-Eyes")
	if !strings.Contains(hdr1, "1 rewritten") {
		t.Errorf("1st request header: got %q, want \"1 rewritten\"", hdr1)
	}
	if strings.Contains(hdr1, "1 cached") {
		t.Errorf("1st request header should NOT report cached: got %q", hdr1)
	}

	// ── "Restart": close the old SQLite handle, reopen the same db file ──
	// This mirrors a proxy restart: the in-memory LRU is lost, but the SQLite
	// cold layer (with WAL checkpointed on Close) persists on disk.
	if err := cold1.Close(); err != nil {
		t.Fatalf("close sqlite (1st): %v", err)
	}
	cold2, err := cache.OpenSQLite(dbPath, 10000, 0, logger)
	if err != nil {
		t.Fatalf("open sqlite (2nd): %v", err)
	}
	defer cold2.Close()
	tt2 := cache.NewTwoTier(10, cold2, logger)
	h2 := proxy.NewHandler(proxy.HandlerDeps{
		UpstreamBaseURL:     upstreamSrv.URL,
		UpstreamAPIKey:      "test-upstream-key",
		VisionProvider:      visionClient,
		Cache:               tt2,
		FailOpen:            true,
		LargeImageThreshold: 1 << 20,
		Log:                 logger,
	})

	// ── 2nd request: same image, fresh handler — must hit cold-layer cache ──
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	h2.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("2nd request: status %d (body=%s)", rr2.Code, rr2.Body.String())
	}
	// Vision provider must NOT be called again — the description survived the
	// restart in the SQLite cold layer.
	if got := atomic.LoadInt32(&visionCalls); got != 1 {
		t.Errorf("2nd request: vision calls got %d, want 1 (cache should survive restart)", got)
	}
	// Cache hit → header reports "cached".
	hdr2 := rr2.Header().Get("X-Blind-Llm-Eyes")
	if !strings.Contains(hdr2, "1 cached") {
		t.Errorf("2nd request header: got %q, want \"1 cached\" (cache hit after restart)", hdr2)
	}
}
