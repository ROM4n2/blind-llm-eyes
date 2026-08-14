package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ROM4n2/blind-llm-eyes/cache"
)

// TestHandler_VisionWhitelist_Passthrough verifies that when the request model
// is in the vision_capable_models whitelist, the proxy skips image rewriting
// entirely: no vision calls are made, the request body is forwarded verbatim
// (image blocks preserved), and the response header reports passthrough=1.
func TestHandler_VisionWhitelist_Passthrough(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "ShouldNotBeCalled"}

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		VisionCapableModels: map[string]bool{"gpt-4o": true},
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	// Request uses model "gpt-4o" (whitelisted) and contains an image block.
	// The proxy must NOT rewrite the image — it should forward the body as-is.
	reqBody := `{"model":"gpt-4o","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	// No vision calls — the whole rewrite phase was skipped.
	if mockVis.calls != 0 {
		t.Errorf("vision calls: want 0 (whitelisted passthrough), got %d", mockVis.calls)
	}
	// The upstream must receive the image block unchanged.
	upstreamStr := string(upstreamGot)
	if !strings.Contains(upstreamStr, `"type":"image"`) {
		t.Errorf("upstream body missing image block (should be verbatim): %s", upstreamStr)
	}
	if strings.Contains(upstreamStr, "ShouldNotBeCalled") {
		t.Errorf("upstream body contains vision description (should not rewrite): %s", upstreamStr)
	}
	// Response header must report passthrough.
	hdr := rr.Header().Get("X-Blind-Llm-Eyes")
	if !strings.Contains(hdr, "passthrough") {
		t.Errorf("response header missing passthrough indicator: %q", hdr)
	}
}

// TestHandler_VisionWhitelist_NotListed verifies that when the model is NOT in
// the whitelist, the normal rewrite path runs (vision provider is called).
func TestHandler_VisionWhitelist_NotListed(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "MockDesc-Whitelist"}

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		VisionCapableModels: map[string]bool{"gpt-4o": true},
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	// Model "deepseek-chat" is NOT in the whitelist → normal rewrite.
	reqBody := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if mockVis.calls != 1 {
		t.Errorf("vision calls: want 1 (model not whitelisted), got %d", mockVis.calls)
	}
	hdr := rr.Header().Get("X-Blind-Llm-Eyes")
	if strings.Contains(hdr, "passthrough") {
		t.Errorf("response header should NOT contain passthrough for non-whitelisted model: %q", hdr)
	}
	if !strings.Contains(hdr, "1 rewritten") {
		t.Errorf("response header should report 1 rewritten: %q", hdr)
	}
}

// TestHandler_VisionWhitelist_CaseInsensitive verifies that the whitelist
// match is case-insensitive (model names from different sources may vary).
func TestHandler_VisionWhitelist_CaseInsensitive(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "NoCall"}

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		// Whitelist has lowercase; request uses mixed case.
		VisionCapableModels: map[string]bool{"gpt-4o": true},
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"GPT-4O","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if mockVis.calls != 0 {
		t.Errorf("vision calls: want 0 (case-insensitive whitelist match), got %d", mockVis.calls)
	}
	// Verify the upstream received the body with the image block intact.
	body, _ := io.ReadAll(rr.Result().Body)
	_ = body // response is SSE; we check upstreamGot instead
	if !strings.Contains(string(upstreamGot), `"type":"image"`) {
		t.Errorf("upstream body missing image block: %s", string(upstreamGot))
	}
}

// TestHandler_VisionWhitelist_SanitizedModel verifies that the whitelist check
// happens AFTER model sanitization, so "deepseek-chat[1m]" matches a whitelist
// entry of "deepseek-chat" IF deepseek-chat were whitelisted.
func TestHandler_VisionWhitelist_AfterSanitization(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "NoCall"}

	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		// Whitelist the sanitized name; request has a [1m] suffix.
		VisionCapableModels: map[string]bool{"gpt-4o": true},
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"gpt-4o[1m]","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if mockVis.calls != 0 {
		t.Errorf("vision calls: want 0 (sanitized model matches whitelist), got %d", mockVis.calls)
	}
}
