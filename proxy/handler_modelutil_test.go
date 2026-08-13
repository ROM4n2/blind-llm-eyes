package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ROM4n2/blind-llm-eyes/cache"
)

// TestHandler_ModelSanitization_NoImages verifies that the [1m] suffix is
// stripped from the model name even when no images are present in the request
// (no re-marshal would normally happen).
func TestHandler_ModelSanitization_NoImages(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "desc"}
	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	// Request with [1m] suffix, no images
	reqBody := `{"model":"deepseek-chat[1m]","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	// Verify upstream received stripped model name
	var upstreamReq map[string]any
	if err := json.Unmarshal(upstreamGot, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	gotModel, _ := upstreamReq["model"].(string)
	if gotModel != "deepseek-chat" {
		t.Errorf("upstream model: want %q, got %q", "deepseek-chat", gotModel)
	}
}

// TestHandler_ModelSanitization_WithImages verifies that the [1m] suffix is
// stripped when images are also being rewritten (re-marshal happens anyway).
func TestHandler_ModelSanitization_WithImages(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "ImageDesc"}
	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"deepseek-v4-flash[1M]","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	var upstreamReq map[string]any
	if err := json.Unmarshal(upstreamGot, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	gotModel, _ := upstreamReq["model"].(string)
	if gotModel != "deepseek-v4-flash" {
		t.Errorf("upstream model: want %q, got %q", "deepseek-v4-flash", gotModel)
	}
}

// TestHandler_ModelSanitization_NoSuffix verifies that a model without a
// bracket suffix is passed through unchanged.
func TestHandler_ModelSanitization_NoSuffix(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "desc"}
	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	var upstreamReq map[string]any
	if err := json.Unmarshal(upstreamGot, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	gotModel, _ := upstreamReq["model"].(string)
	if gotModel != "deepseek-chat" {
		t.Errorf("upstream model: want %q, got %q", "deepseek-chat", gotModel)
	}
}

// TestHandler_ModelSanitization_OtherFieldsUnchanged verifies that sanitizing
// the model doesn't alter any other fields in the forwarded request.
func TestHandler_ModelSanitization_OtherFieldsUnchanged(t *testing.T) {
	var upstreamGot []byte
	up := fakeUpstream(t, &upstreamGot)
	defer up.Close()

	mockVis := &mockVisionProvider{desc: "desc"}
	deps := HandlerDeps{
		UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
		VisionProvider:      mockVis,
		Cache:               cache.NewLRU(10),
		FailOpen:            true,
		LargeImageThreshold: 1_000_000,
		Log:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h := NewHandler(deps)

	reqBody := `{"model":"deepseek-chat[1m]","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}

	var upstreamReq map[string]any
	if err := json.Unmarshal(upstreamGot, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	// Verify other fields are preserved
	if mt, ok := upstreamReq["max_tokens"].(float64); !ok || int(mt) != 100 {
		t.Errorf("max_tokens: want 100, got %v", upstreamReq["max_tokens"])
	}
	msgs, _ := upstreamReq["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages: want 1, got %d", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role: want user, got %v", msg["role"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content blocks: want 1, got %d", len(content))
	}
	blk, _ := content[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != "hi" {
		t.Errorf("content block: want text/hi, got %v/%v", blk["type"], blk["text"])
	}
	if st, ok := upstreamReq["stream"].(bool); !ok || !st {
		t.Errorf("stream: want true, got %v", upstreamReq["stream"])
	}
}

// fakeUpstreamNoStream is a variant that records the body without SSE.
func fakeUpstreamNoStream(t *testing.T, gotBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = b
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
}
