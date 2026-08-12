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
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/vision"
)

// 假上游：收到请求后把 body 原样回显
func fakeUpstream(t *testing.T, gotBody *[]byte) *httptest.Server {
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

	// mock vision：固定返回 "MockDesc-A"
	visionCalls := 0
	vis := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionCalls++
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "MockDesc-A"}}},
		})
	}))
	defer vis.Close()

	deps := HandlerDeps{
		UpstreamBaseURL: strings.TrimSuffix(up.URL, "/"),
		VisionClient: &vision.Client{
			BaseURL:        strings.TrimSuffix(vis.URL, "/"),
			APIKey:         "x",
			Model:          "mimo-v2.5",
			DescriptionCap: 300,
			Timeout:        5 * time.Second,
		},
		Cache:    cache.NewLRU(10),
		FailOpen: true,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
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
	if visionCalls != 1 {
		t.Errorf("1st vision calls: want 1, got %d", visionCalls)
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

	if visionCalls != 1 {
		t.Errorf("2nd vision calls: still want 1, got %d (cache not hit!)", visionCalls)
	}
	hdr := rr2.Header().Get("X-Blind-Llm-Eyes")
	if !strings.Contains(hdr, "1 cached") {
		t.Errorf("2nd header want 1 cached, got %q", hdr)
	}
}
