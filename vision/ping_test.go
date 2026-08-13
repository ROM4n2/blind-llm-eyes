package vision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// decodePingBody parses the recorded request body and asserts it is text-only.
func decodePingBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode ping body: %v", err)
	}
	if mt, ok := body["max_tokens"].(float64); !ok || int(mt) != 1 {
		t.Errorf("expected max_tokens=1, got %v", body["max_tokens"])
	}
	msgs, _ := body["messages"].([]any)
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
	return body
}

func TestClient_Ping_Success(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = decodePingBody(t, r)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected /v1/messages, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "mimo-v2.5", Timeout: 5 * time.Second}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if seen["model"] != "mimo-v2.5" {
		t.Errorf("expected model in body, got %v", seen["model"])
	}
}

func TestClient_Ping_AuthFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "bad", Model: "m", Timeout: 5 * time.Second}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestClient_Ping_ModelLevel400_NotFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"max_tokens too small"}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: 5 * time.Second}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("model-level 400 must not fail ping: %v", err)
	}
}

func TestClient_Ping_NetworkError(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "m", Timeout: 2 * time.Second}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error on network failure")
	}
}

func TestOpenAIClient_Ping_Success(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = decodePingBody(t, r)
		if r.URL.Path != "/chat/completions" {
			t.Errorf("expected /chat/completions, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := &OpenAIClient{BaseURL: srv.URL, APIKey: "k", Model: "gpt-4o", Timeout: 5 * time.Second}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if seen["model"] != "gpt-4o" {
		t.Errorf("expected model in body, got %v", seen["model"])
	}
}

func TestOpenAIClient_Ping_AuthFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := &OpenAIClient{BaseURL: srv.URL, APIKey: "bad", Model: "m", Timeout: 5 * time.Second}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestOpenAIClient_Ping_NetworkError(t *testing.T) {
	c := &OpenAIClient{BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "m", Timeout: 2 * time.Second}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected error on network failure")
	}
}
