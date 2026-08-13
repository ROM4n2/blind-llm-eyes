package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPingUpstream_Success(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected /v1/messages, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth header missing: %s", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	err := PingUpstream(context.Background(), srv.URL, "test-key", "deepseek-chat")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	// Verify text-only body with max_tokens=1
	if seen["model"] != "deepseek-chat" {
		t.Errorf("expected model deepseek-chat, got %v", seen["model"])
	}
	if mt, ok := seen["max_tokens"].(float64); !ok || int(mt) != 1 {
		t.Errorf("expected max_tokens=1, got %v", seen["max_tokens"])
	}
	msgs, _ := seen["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	msg, _ := msgs[0].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, b := range content {
		blk, _ := b.(map[string]any)
		if blk["type"] == "image" || blk["type"] == "image_url" {
			t.Errorf("ping body must not contain an image block, got %v", blk["type"])
		}
	}
}

func TestPingUpstream_AuthFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	err := PingUpstream(context.Background(), srv.URL, "bad-key", "m")
	if err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestPingUpstream_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := PingUpstream(context.Background(), srv.URL, "bad-key", "m")
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestPingUpstream_ModelLevel400_NotFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"max_tokens too small"}`))
	}))
	defer srv.Close()

	err := PingUpstream(context.Background(), srv.URL, "k", "m")
	if err != nil {
		t.Fatalf("model-level 400 must not fail ping: %v", err)
	}
}

func TestPingUpstream_NetworkError(t *testing.T) {
	err := PingUpstream(context.Background(), "http://127.0.0.1:1", "k", "m")
	if err == nil {
		t.Fatal("expected error on network failure")
	}
}

func TestPingUpstream_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := PingUpstream(ctx, srv.URL, "k", "m")
	if err == nil {
		t.Fatal("expected error on timeout")
	}
}
