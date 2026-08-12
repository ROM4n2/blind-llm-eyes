package vision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDescribeImage_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth header missing: %s", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "mimo-v2.5" {
			t.Errorf("model wrong: %v", req["model"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "一张蓝色背景的测试图片，左上角有 Logo"}},
			},
		})
	}))
	defer srv.Close()

	c := &Client{
		BaseURL:        srv.URL,
		APIKey:         "test-key",
		Model:          "mimo-v2.5",
		DescriptionCap: 300,
		Timeout:        5 * time.Second,
	}

	desc, err := c.DescribeImage(context.Background(), "iVBORw0K...", "image/png")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if desc != "一张蓝色背景的测试图片，左上角有 Logo" {
		t.Errorf("desc mismatch: %q", desc)
	}
}

func TestDescribeImage_500Fail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, APIKey: "x", Model: "m", DescriptionCap: 100, Timeout: 2 * time.Second}
	_, err := c.DescribeImage(context.Background(), "abc", "image/png")
	if err == nil {
		t.Errorf("expected error on 500")
	}
}
