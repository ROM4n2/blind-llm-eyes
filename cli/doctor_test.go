package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)
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
	code := runDoctorCore(context.Background(), cfg, "m", false, &stdout, &stderr)
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
	code := runDoctorCore(context.Background(), cfg, "m", false, &stdout, &stderr)
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
	code := runDoctorCore(context.Background(), cfg, "m", false, &stdout, &stderr)
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
	code := runDoctorCore(context.Background(), cfg, "m", false, &stdout, &stderr)
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
	code := runDoctorCore(context.Background(), cfg, "m", false, &stdout, &stderr)
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
	runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)

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

func TestRunDoctor_Deep_Pass(t *testing.T) {
	// Fake upstream — returns 200.
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstreamSrv.Close()

	// Fake vision (MiMo /v1/messages) — returns 200 with a text description.
	// The deep check sends a real image and expects a non-empty description.
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"a tiny transparent pixel"}]}`))
	}))
	defer visionSrv.Close()

	// NOTE: these tests construct config.Config directly (bypassing config.Load,
	// which applies default timeouts). Set an explicit Timeout so the vision
	// client's context.WithTimeout doesn't get a zero-duration (immediately
	// expired) deadline.
	cfg := &config.Config{
		Upstream: config.UpstreamCfg{BaseURL: upstreamSrv.URL, APIKey: "up-key"},
		Vision: config.VisionCfg{
			BaseURL:  visionSrv.URL,
			APIKey:   "vis-key",
			Model:    "mimo-v2.5",
			Timeout:  30 * time.Second,
		},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "deep") {
		t.Errorf("expected 'deep' in output, got: %s", out)
	}
	if !strings.Contains(out, "PASS") {
		t.Errorf("expected PASS in output, got: %s", out)
	}
}

func TestRunDoctor_Deep_VisionFail(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamSrv.Close()

	// Vision server returns 500 — the ping might pass (any HTTP response is
	// a pass for ping), but the deep image call should fail.
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First call (ping) returns 200; subsequent calls (deep) return 500.
		body, _ := io.ReadAll(r.Body)
		if len(body) > 100 {
			// Likely the deep image request (larger body)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer visionSrv.Close()

	cfg := &config.Config{
		Upstream: config.UpstreamCfg{BaseURL: upstreamSrv.URL, APIKey: "k"},
		Vision: config.VisionCfg{
			BaseURL: visionSrv.URL,
			APIKey:  "k",
			Model:   "m",
			Timeout: 30 * time.Second,
		},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "m", true, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit when deep check fails")
	}
	out := stdout.String()
	if !strings.Contains(out, "FAIL") {
		t.Errorf("expected FAIL in output, got: %s", out)
	}
}

// TestRunDoctor_SelfReferential_ReportsFailOnce verifies that when
// upstream.base_url points to the proxy's own listen address, doctor reports
// FAIL exactly once. The pre-fix bug printed the upstream check line twice:
// once as FAIL (from the standalone self-loop block) and once as SKIP (from
// the ping block). After the fix, the self-loop check is merged into the ping
// block, so only one "checking upstream" line appears.
func TestRunDoctor_SelfReferential_ReportsFailOnce(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "http://127.0.0.1:8790", APIKey: "k"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit on self-referential upstream")
	}

	out := stdout.String()
	// "checking upstream" should appear exactly once (not twice).
	if c := strings.Count(out, "checking upstream"); c != 1 {
		t.Errorf("expected 'checking upstream' once, got %d times:\n%s", c, out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("expected FAIL in output, got: %s", out)
	}
	// Should NOT contain the old duplicate SKIP message.
	if strings.Contains(out, "SKIP (self-referential") {
		t.Errorf("should not contain 'SKIP (self-referential...' (old duplicate output):\n%s", out)
	}

	// stderr should explain the self-loop problem and mention the listen addr.
	errOut := stderr.String()
	if !strings.Contains(errOut, "self-forwarding loops") {
		t.Errorf("stderr should explain self-forwarding loops, got: %s", errOut)
	}
	if !strings.Contains(errOut, "127.0.0.1:8790") {
		t.Errorf("stderr should mention listen address, got: %s", errOut)
	}
}

// TestRunDoctor_SelfReferential_DoesNotPing verifies that when upstream is
// self-referential, doctor does NOT actually ping the upstream endpoint
// (which would be pinging itself — wasteful and potentially looping).
func TestRunDoctor_SelfReferential_DoesNotPing(t *testing.T) {
	// Fake upstream that records if it was called. Point cfg.Upstream.BaseURL
	// at it AND set cfg.Listen to its address, making it self-referential.
	// Doctor should detect this and skip the ping entirely.
	var pingCount int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pingCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	// Extract host:port from up.URL (http://127.0.0.1:54321 → 127.0.0.1:54321).
	listenAddr := strings.TrimPrefix(strings.TrimPrefix(up.URL, "http://"), "https://")

	cfg := &config.Config{
		Listen:   listenAddr,
		Upstream: config.UpstreamCfg{BaseURL: up.URL, APIKey: "k"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit on self-referential upstream")
	}

	if got := atomic.LoadInt32(&pingCount); got != 0 {
		t.Errorf("upstream should NOT be pinged on self-referential config, got %d calls", got)
	}
}

// TestRunDoctor_SelfReferential_LocalhostVariant verifies that host aliases
// (localhost, 0.0.0.0, ::1) are all detected as self-referential when the
// proxy listens on 127.0.0.1 — because normalizeHost collapses them.
func TestRunDoctor_SelfReferential_LocalhostVariant(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		listen  string
	}{
		{"localhost-variant", "http://localhost:8790", "127.0.0.1:8790"},
		{"zero-bind-variant", "http://127.0.0.1:8790", "0.0.0.0:8790"},
		{"ipv6-loopback", "http://[::1]:8790", "127.0.0.1:8790"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{
				Listen:   c.listen,
				Upstream: config.UpstreamCfg{BaseURL: c.baseURL, APIKey: "k"},
			}
			var stdout, stderr bytes.Buffer
			code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)
			if code == 0 {
				t.Errorf("expected non-zero exit (self-referential should FAIL)")
			}
			if !strings.Contains(stdout.String(), "FAIL") {
				t.Errorf("expected FAIL for %q vs %q, got: %s", c.baseURL, c.listen, stdout.String())
			}
		})
	}
}

// P1-14: when upstream is self-referential, doctor should still check vision
// providers — the upstream failure must not short-circuit vision checks,
// because the user needs full diagnostic output to fix all issues at once.
func TestRunDoctor_SelfReferential_VisionStillChecked(t *testing.T) {
	// Fake vision server that returns 200 — doctor should ping it.
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer visionSrv.Close()

	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "http://127.0.0.1:8790", APIKey: "k"},
		Vision:   config.VisionCfg{BaseURL: visionSrv.URL, APIKey: "k", Model: "m"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit (upstream self-referential should FAIL)")
	}

	out := stdout.String()
	// Upstream should FAIL (self-referential).
	if !strings.Contains(out, "checking upstream") || !strings.Contains(out, "FAIL") {
		t.Errorf("upstream should be checked and FAIL, got: %s", out)
	}
	// Vision should still be checked and PASS.
	if !strings.Contains(out, "checking vision") {
		t.Errorf("vision should still be checked, got: %s", out)
	}
	if !strings.Contains(out, "PASS") {
		t.Errorf("vision should PASS (independent of upstream), got: %s", out)
	}
}

// P1-15: doctor self-referential error message should contain the listen
// address (for context) and an actionable fix hint pointing to upstream.base_url.
func TestRunDoctor_SelfReferential_ErrorMessageFormat(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "http://localhost:8790/anthropic", APIKey: "k"},
	}

	var stdout, stderr bytes.Buffer
	_ = runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)

	errOut := stderr.String()
	// Must mention the listen address for context.
	if !strings.Contains(errOut, "127.0.0.1:8790") {
		t.Errorf("error should mention listen address, got: %s", errOut)
	}
	// Must contain an actionable fix hint.
	if !strings.Contains(errOut, "Fix:") {
		t.Errorf("error should contain 'Fix:' hint, got: %s", errOut)
	}
	if !strings.Contains(errOut, "upstream.base_url") {
		t.Errorf("error should mention 'upstream.base_url' config key, got: %s", errOut)
	}
}

// P1-16: doctor should return exit code 1 (not 0 or 2) when upstream is
// self-referential — signaling failure to the caller (e.g. setup wizard).
func TestRunDoctor_SelfReferential_ExitCode(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "http://127.0.0.1:8790", APIKey: "k"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 for self-referential upstream, got %d", code)
	}
}

// P2-9: the --deep flag (which sends a real image to vision providers) should
// not affect self-loop detection — the upstream check happens before any deep
// vision test, so self-referential configs still FAIL.
func TestRunDoctor_SelfReferential_DeepFlag(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "http://127.0.0.1:8790", APIKey: "k"},
	}

	var stdout, stderr bytes.Buffer
	// deep=true — should still detect self-referential upstream.
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", true, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit 1 with --deep on self-referential upstream, got %d", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "FAIL") {
		t.Errorf("should FAIL with --deep, got: %s", out)
	}
	if !strings.Contains(stderr.String(), "self-forwarding loops") {
		t.Errorf("stderr should explain self-forwarding loops, got: %s", stderr.String())
	}
}

// P2-10: self-referential detection should fire even when API key is empty —
// the check is URL-based, not auth-based, so missing credentials must not
// mask the more fundamental self-loop problem.
func TestRunDoctor_SelfReferential_NoAPIKey(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "http://127.0.0.1:8790", APIKey: ""}, // no API key
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit 1 even without API key, got %d", code)
	}

	// The self-loop error should take precedence over any auth error.
	errOut := stderr.String()
	if !strings.Contains(errOut, "self-forwarding loops") {
		t.Errorf("stderr should explain self-forwarding loops (not auth error), got: %s", errOut)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// P3: Doctor false-positive prevention (must NOT report self-loop on valid configs)
// ──────────────────────────────────────────────────────────────────────────

// P3-7: when upstream.base_url is empty, doctor should NOT report a self-loop
// — it should SKIP the upstream check entirely (no base_url = no upstream).
// This prevents false positives when users haven't configured upstream yet.
func TestRunDoctor_EmptyBaseURL_NoSelfLoop(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "", APIKey: ""}, // no upstream configured
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)

	out := stdout.String()
	// Should SKIP (no base_url), not FAIL (self-loop).
	if !strings.Contains(out, "SKIP") {
		t.Errorf("empty base_url should SKIP, got: %s", out)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("empty base_url should NOT FAIL (no self-loop possible): %s", out)
	}
	if strings.Contains(stderr.String(), "self-forwarding loops") {
		t.Errorf("empty base_url should NOT mention self-forwarding loops: %s", stderr.String())
	}
	// Exit code should be 0 (SKIP is not a failure).
	if code != 0 {
		t.Errorf("expected exit 0 for empty base_url (SKIP), got %d", code)
	}
}

// P3-8: a normal external upstream URL should NOT be flagged as self-loop —
// this is the happy path, and ensures the self-loop check doesn't have
// false positives that would block valid configs.
func TestRunDoctor_NormalUpstream_NoSelfLoop(t *testing.T) {
	// Fake upstream that returns 200 — doctor should ping it and PASS.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_123","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer up.Close()

	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: up.URL, APIKey: "k"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)

	out := stdout.String()
	if !strings.Contains(out, "PASS") {
		t.Errorf("normal upstream should PASS, got: %s", out)
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("normal upstream should NOT FAIL: %s", out)
	}
	if strings.Contains(stderr.String(), "self-forwarding loops") {
		t.Errorf("normal upstream should NOT mention self-forwarding loops: %s", stderr.String())
	}
	if code != 0 {
		t.Errorf("expected exit 0 for normal upstream, got %d", code)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// P0: Vision base_url self-loop detection (NEW — previously uncovered)
// ──────────────────────────────────────────────────────────────────────────

// P0-VD1: doctor should detect self-referential vision.base_url (single
// provider mode) and report FAIL without pinging the loop endpoint.
func TestRunDoctor_VisionSelfReferential_SingleProvider(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "https://api.deepseek.com/anthropic", APIKey: "k"},
		Vision:   config.VisionCfg{BaseURL: "http://127.0.0.1:8790/anthropic", APIKey: "k", Model: "m"},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)

	// Upstream will FAIL (no real server), but vision should also FAIL with
	// self-loop error. Exit code must be 1.
	if code != 1 {
		t.Errorf("expected exit 1 (vision self-loop), got %d", code)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "vision.base_url points to proxy's own listen address") {
		t.Errorf("stderr should mention vision.base_url self-loop, got: %s", errOut)
	}
	if !strings.Contains(errOut, "vision.base_url") {
		t.Errorf("stderr should mention 'vision.base_url' config key, got: %s", errOut)
	}
}

// P0-VD2: doctor should detect self-referential vision_providers[].base_url
// (pool mode) and report FAIL without pinging the loop endpoint.
func TestRunDoctor_VisionSelfReferential_PoolProvider(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: "https://api.deepseek.com/anthropic", APIKey: "k"},
		VisionProviders: []config.ProviderCfg{{
			Name:    "self-loop-provider",
			Type:    "mimo",
			BaseURL: "http://127.0.0.1:8790/anthropic",
			APIKey:  "k",
			Model:   "m",
		}},
	}

	var stdout, stderr bytes.Buffer
	code := runDoctorCore(context.Background(), cfg, "deepseek-chat", false, &stdout, &stderr)

	if code != 1 {
		t.Errorf("expected exit 1 (vision_providers self-loop), got %d", code)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "vision provider") {
		t.Errorf("stderr should mention vision provider, got: %s", errOut)
	}
	if !strings.Contains(errOut, "self-forwarding loops") {
		t.Errorf("stderr should mention self-forwarding loops, got: %s", errOut)
	}
	// Should reference the provider name for actionable diagnostics.
	if !strings.Contains(errOut, "self-loop-provider") {
		t.Errorf("stderr should mention provider name, got: %s", errOut)
	}
}
