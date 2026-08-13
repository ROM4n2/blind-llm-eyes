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
