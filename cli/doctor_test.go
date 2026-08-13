package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ROM4n2/blind-llm-eyes/config"
)

// writeConfigYAML helper not needed — doctor core takes a *config.Config directly.

func TestRunDoctor_AllPass(t *testing.T) {
	// Fake upstream (Anthropic-compatible /v1/messages) — returns 200.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstreamSrv.Close()

	// Fake vision (MiMo /v1/messages) — returns 200.
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer visionSrv.Close()

	cfg := &config.Config{
		Upstream: config.UpstreamCfg{BaseURL: upstreamSrv.URL, APIKey: "up-key"},
		Vision:   config.VisionCfg{BaseURL: visionSrv.URL, APIKey: "vis-key", Model: "mimo-v2.5"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit 0, got %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "upstream") || !strings.Contains(out, "PASS") {
		t.Errorf("expected upstream PASS in output, got: %s", out)
	}
	if !strings.Contains(out, "vision") || !strings.Contains(out, "PASS") {
		t.Errorf("expected vision PASS in output, got: %s", out)
	}
}

func TestRunDoctor_UpstreamFail(t *testing.T) {
	// Upstream returns 401.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstreamSrv.Close()

	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer visionSrv.Close()

	cfg := &config.Config{
		Upstream: config.UpstreamCfg{BaseURL: upstreamSrv.URL, APIKey: "bad"},
		Vision:   config.VisionCfg{BaseURL: visionSrv.URL, APIKey: "k", Model: "m"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "m", &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit on upstream failure")
	}
	if !strings.Contains(stdout.String(), "FAIL") {
		t.Errorf("expected FAIL in output, got: %s", stdout.String())
	}
}

func TestRunDoctor_VisionFail(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamSrv.Close()

	// Vision returns 403.
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer visionSrv.Close()

	cfg := &config.Config{
		Upstream: config.UpstreamCfg{BaseURL: upstreamSrv.URL, APIKey: "k"},
		Vision:   config.VisionCfg{BaseURL: visionSrv.URL, APIKey: "bad", Model: "m"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "m", &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit on vision failure")
	}
	if !strings.Contains(stdout.String(), "FAIL") {
		t.Errorf("expected FAIL in output, got: %s", stdout.String())
	}
}

func TestRunDoctor_VisionProviders(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamSrv.Close()

	// Two vision providers: one pass, one fail.
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer goodSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer badSrv.Close()

	cfg := &config.Config{
		Upstream: config.UpstreamCfg{BaseURL: upstreamSrv.URL, APIKey: "k"},
		VisionProviders: []config.ProviderCfg{
			{Name: "good", Type: "mimo", BaseURL: goodSrv.URL, APIKey: "k", Model: "m"},
			{Name: "bad", Type: "mimo", BaseURL: badSrv.URL, APIKey: "bad", Model: "m"},
		},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "m", &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit when one provider fails")
	}
	out := stdout.String()
	if !strings.Contains(out, "good") || !strings.Contains(out, "PASS") {
		t.Errorf("expected good PASS in output, got: %s", out)
	}
	if !strings.Contains(out, "bad") || !strings.Contains(out, "FAIL") {
		t.Errorf("expected bad FAIL in output, got: %s", out)
	}
}

func TestRunDoctor_UpstreamUnreachable(t *testing.T) {
	cfg := &config.Config{
		Upstream: config.UpstreamCfg{BaseURL: "http://127.0.0.1:1", APIKey: "k"},
		Vision:   config.VisionCfg{BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "m"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "m", &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit on unreachable endpoints")
	}
}

func TestRunDoctor_NoVisionConfigured(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamSrv.Close()

	cfg := &config.Config{
		Upstream:        config.UpstreamCfg{BaseURL: upstreamSrv.URL, APIKey: "k"},
		VisionProviders: nil, // no providers
		// Vision.BaseURL is empty → no single provider
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "m", &stdout, &stderr)
	// Upstream passes, no vision to check → should still exit 0
	if code != 0 {
		t.Errorf("expected exit 0 when upstream passes and no vision configured, got %d; stderr=%s", code, stderr.String())
	}
}

func TestRunDoctor_FlagParsing(t *testing.T) {
	// Verify -config flag is parsed correctly; we don't need a real config
	// file here — just verify the flag doesn't crash and produces a sensible
	// error for a missing file.
	var stdout, stderr bytes.Buffer
	code := runDoctor([]string{"-config", "nonexistent.yaml"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit for missing config file")
	}
	if !strings.Contains(stderr.String(), "nonexistent.yaml") {
		t.Errorf("expected error to mention config file, got: %s", stderr.String())
	}
}

func TestRunDoctor_JSONRequestNoImage(t *testing.T) {
	// Verify the upstream ping body is text-only (no image block).
	var seenBody map[string]any
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstreamSrv.Close()

	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer visionSrv.Close()

	cfg := &config.Config{
		Upstream: config.UpstreamCfg{BaseURL: upstreamSrv.URL, APIKey: "k"},
		Vision:   config.VisionCfg{BaseURL: visionSrv.URL, APIKey: "k", Model: "m"},
	}

	var stdout, stderr bytes.Buffer
	runDoctorCore(context.Background(), cfg, "deepseek-chat", &stdout, &stderr)

	msgs, _ := seenBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	msg, _ := msgs[0].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, b := range content {
		blk, _ := b.(map[string]any)
		if blk["type"] == "image" || blk["type"] == "image_url" {
			t.Errorf("upstream ping body must not contain an image block, got %v", blk["type"])
		}
	}
}
