package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCountTokens_Passthrough verifies that POST /v1/messages/count_tokens
// forwards the request body to upstream's /v1/messages/count_tokens unchanged
// and returns the upstream response verbatim (no image rewriting, no vision
// calls). This is critical because Claude Code calls this endpoint to display
// token counts in its UI; a 404 breaks the counter.
func TestCountTokens_Passthrough(t *testing.T) {
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer upstream.Close()

	handler := NewHandler(HandlerDeps{
		UpstreamBaseURL: upstream.URL,
		VisionProvider:  &mockVisionProvider{},
		Cache:           nil, // NewHandler defaults to LRU
	})

	body := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Fatalf("upstream path: want /v1/messages/count_tokens, got %s", gotPath)
	}
	if gotBody != body {
		t.Fatalf("body mismatch: want %q, got %q", body, gotBody)
	}
	if rr.Body.String() != `{"input_tokens":42}` {
		t.Fatalf("response: want {\"input_tokens\":42}, got %s", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: want application/json, got %q", ct)
	}
}

// TestCountTokens_ForwardsAuthHeader verifies that the upstream API key is
// injected into the Authorization header when configured.
func TestCountTokens_ForwardsAuthHeader(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"input_tokens":1}`))
	}))
	defer upstream.Close()

	handler := NewHandler(HandlerDeps{
		UpstreamBaseURL: upstream.URL,
		UpstreamAPIKey:  "sk-test-key",
		VisionProvider:  &mockVisionProvider{},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if gotAuth != "Bearer sk-test-key" {
		t.Fatalf("auth: want 'Bearer sk-test-key', got %q", gotAuth)
	}
}

// TestCountTokens_UpstreamError verifies that upstream errors are passed
// through to the client without modification.
func TestCountTokens_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","message":"invalid model"}`))
	}))
	defer upstream.Close()

	handler := NewHandler(HandlerDeps{
		UpstreamBaseURL: upstream.URL,
		VisionProvider:  &mockVisionProvider{},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"bad"}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rr.Code)
	}
}
