package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/config"
	"github.com/ROM4n2/blind-llm-eyes/vision"
)

// deepTestPNG is a 1x1 transparent PNG (67 bytes decoded), used by the
// --deep flag to send a real image through the vision pipeline.
const deepTestPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// runDoctor implements the `doctor` subcommand: load config, then probe the
// upstream endpoint and every configured vision provider with a lightweight
// text-only ping. Exit 0 if all checks pass, 1 if any fails.
//
// The --deep flag adds an end-to-end image test: after the ping passes, a
// real 1x1 PNG is sent through DescribeImage to verify the full vision
// pipeline (base64 decode, API call, response parsing).
func runDoctor(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to config file")
	upstreamModel := fs.String("upstream-model", "deepseek-chat", "model name for upstream ping")
	deep := fs.Bool("deep", false, "send a real 1x1 PNG to each vision provider (end-to-end image test)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config %q: %v\n", *configPath, err)
		return 1
	}

	return runDoctorCore(context.Background(), cfg, *upstreamModel, *deep, stdout, stderr)
}

// runDoctorCore is the testable core of the doctor subcommand. It takes a
// loaded config, pings the upstream and each vision provider, and writes
// results to stdout/stderr. Returns 0 if all checks pass, 1 if any fails.
//
// If deep is true, each vision provider that passes the ping also receives
// a real 1x1 PNG via DescribeImage to verify the full image pipeline.
func runDoctorCore(ctx context.Context, cfg *config.Config, upstreamModel string, deep bool, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	failed := false

	// ── Upstream ping ──
	fmt.Fprintf(stdout, "checking upstream (%s) ... ", cfg.Upstream.BaseURL)
	switch {
	case cfg.Upstream.BaseURL == "":
		fmt.Fprintln(stdout, "SKIP (no upstream.base_url configured)")
	case IsSelfReferentialURL(cfg.Upstream.BaseURL, cfg.Listen):
		// Self-loop detection: upstream.base_url points to the proxy itself,
		// which would cause infinite self-forwarding loops. Report FAIL once
		// here instead of pinging our own endpoint.
		fmt.Fprintln(stdout, "FAIL")
		fmt.Fprintf(stderr, "  upstream.base_url points to proxy's own listen address (%s) — this causes infinite self-forwarding loops\n", cfg.Listen)
		fmt.Fprintf(stderr, "  Fix: set upstream.base_url to a real upstream API endpoint\n")
		failed = true
	default:
		if err := PingUpstream(ctx, cfg.Upstream.BaseURL, cfg.Upstream.APIKey, upstreamModel); err != nil {
			fmt.Fprintln(stdout, "FAIL")
			fmt.Fprintf(stderr, "  %v\n", err)
			failed = true
		} else {
			fmt.Fprintln(stdout, "PASS")
		}
	}

	// ── Vision providers ping ──
	if len(cfg.VisionProviders) > 0 {
		for _, pc := range cfg.VisionProviders {
			fmt.Fprintf(stdout, "checking vision provider %q (%s) ... ", pc.Name, pc.BaseURL)
			// Self-loop detection: vision provider base_url points to the proxy itself.
			// MiMo Client calls /v1/messages (same as proxy path) → infinite loop.
			if IsSelfReferentialURL(pc.BaseURL, cfg.Listen) {
				fmt.Fprintln(stdout, "FAIL")
				fmt.Fprintf(stderr, "  vision provider %q base_url points to proxy's own listen address (%s) — this causes infinite self-forwarding loops\n", pc.Name, cfg.Listen)
				fmt.Fprintf(stderr, "  Fix: set vision_providers[].base_url to a real vision API endpoint\n")
				failed = true
				continue
			}
			p, err := vision.BuildProvider(pc, logger)
			if err != nil {
				fmt.Fprintln(stdout, "FAIL")
				fmt.Fprintf(stderr, "  build: %v\n", err)
				failed = true
				continue
			}
			if err := pingVisionProvider(ctx, p); err != nil {
				fmt.Fprintln(stdout, "FAIL")
				fmt.Fprintf(stderr, "  %v\n", err)
				failed = true
				continue
			}
			if deep {
				if err := deepCheckVision(ctx, p); err != nil {
					fmt.Fprintln(stdout, "FAIL (deep)")
					fmt.Fprintf(stderr, "  deep: %v\n", err)
					failed = true
				} else {
					fmt.Fprintln(stdout, "PASS (deep)")
				}
			} else {
				fmt.Fprintln(stdout, "PASS")
			}
		}
	} else if cfg.Vision.BaseURL != "" {
		fmt.Fprintf(stdout, "checking vision (%s) ... ", cfg.Vision.BaseURL)
		// Self-loop detection: vision base_url points to the proxy itself.
		// MiMo Client calls /v1/messages (same as proxy path) → infinite loop.
		if IsSelfReferentialURL(cfg.Vision.BaseURL, cfg.Listen) {
			fmt.Fprintln(stdout, "FAIL")
			fmt.Fprintf(stderr, "  vision.base_url points to proxy's own listen address (%s) — this causes infinite self-forwarding loops\n", cfg.Listen)
			fmt.Fprintf(stderr, "  Fix: set vision.base_url to a real vision API endpoint\n")
			failed = true
		} else {
			p, err := vision.BuildSingleProvider(cfg.Vision, logger)
			if err != nil {
				fmt.Fprintln(stdout, "FAIL")
				fmt.Fprintf(stderr, "  build: %v\n", err)
				failed = true
			} else {
				if err := pingVisionProvider(ctx, p); err != nil {
					fmt.Fprintln(stdout, "FAIL")
					fmt.Fprintf(stderr, "  %v\n", err)
					failed = true
				} else if deep {
					if err := deepCheckVision(ctx, p); err != nil {
						fmt.Fprintln(stdout, "FAIL (deep)")
						fmt.Fprintf(stderr, "  deep: %v\n", err)
						failed = true
					} else {
						fmt.Fprintln(stdout, "PASS (deep)")
					}
				} else {
					fmt.Fprintln(stdout, "PASS")
				}
			}
		}
	}

	// ── P1 D1: DB writable (twotier only; lru=SKIP) ──
	if r := runDoctorCheckDBWritable(stderr, cfg); r == DoctorFail {
		failed = true
	}
	// ── P1 D2: Upstream reachable (lightweight HEAD) ──
	if r := runDoctorCheckUpstreamReachable(stderr, cfg); r == DoctorFail {
		failed = true
	}
	// ── P1 D3: Vision model exists (ping + /models WARN-only) ──
	if r := runDoctorCheckVisionModelExists(stderr, cfg); r == DoctorFail {
		failed = true
	}

	if failed {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "one or more checks failed")
		return 1
	}
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "all checks passed")
	return 0
}

// pingVisionProvider type-asserts to vision.Pinger and calls Ping. Providers
// that don't implement Pinger are reported as SKIP.
func pingVisionProvider(ctx context.Context, p vision.VisionProvider) error {
	pinger, ok := p.(vision.Pinger)
	if !ok {
		return fmt.Errorf("provider does not support ping (not a Pinger)")
	}
	return pinger.Ping(ctx)
}

// deepCheckVision sends a real 1x1 PNG through DescribeImage to verify the
// full vision pipeline: base64 handling, API call, response parsing. The
// description must be non-empty to pass.
func deepCheckVision(ctx context.Context, p vision.VisionProvider) error {
	pngBytes, err := base64.StdEncoding.DecodeString(deepTestPNG)
	if err != nil {
		return fmt.Errorf("decode test PNG: %w", err)
	}
	desc, err := p.DescribeImage(ctx, deepTestPNG, "image/png", int64(len(pngBytes)))
	if err != nil {
		return fmt.Errorf("describe image: %w", err)
	}
	if desc == "" {
		return fmt.Errorf("vision provider returned empty description")
	}
	return nil
}

// DoctorResult represents the outcome of a single doctor check.
type DoctorResult int

const (
	DoctorPass DoctorResult = iota // check succeeded (or SKIP without problem)
	DoctorWarn                     // non-fatal issue (doesn't change exit code)
	DoctorFail                     // fatal issue (forces exit 1)
)

// runDoctorCheckDBWritable opens the SQLite DB, runs PRAGMA quick_check, and
// writes+deletes a probe row. For LRU-only configs it prints SKIP and returns
// DoctorPass. Any failure at any step → DoctorFail with a diagnostic line.
func runDoctorCheckDBWritable(stderr io.Writer, cfg *config.Config) DoctorResult {
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintf(stderr, "%-32s SKIP  (cache.type=%s, no persistent store)\n", "db_writable", cfg.Cache.Type)
		return DoctorPass
	}
	dbPath := resolveDBPath(cfg)
	maxEntries := cfg.Cache.SqliteMaxEntries
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	s, err := cache.OpenSQLite(dbPath, maxEntries, cfg.Cache.SqliteTTL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		fmt.Fprintf(stderr, "%-32s FAIL  open: %v\n", "db_writable", err)
		return DoctorFail
	}
	defer s.Close()
	if err := s.PragmaQuickCheck(); err != nil {
		fmt.Fprintf(stderr, "%-32s FAIL  quick_check: %v\n", "db_writable", err)
		return DoctorFail
	}
	probeHash := fmt.Sprintf("doctor_write_probe_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := s.WriteProbe(probeHash); err != nil {
		fmt.Fprintf(stderr, "%-32s FAIL  probe write: %v\n", "db_writable", err)
		return DoctorFail
	}
	fmt.Fprintf(stderr, "%-32s PASS  (%s)\n", "db_writable", dbPath)
	return DoctorPass
}

// runDoctorCheckUpstreamReachable does a lightweight HEAD request to
// <base_url>/v1/models with 5s timeout. Network-level failures → FAIL;
// any HTTP response (even 404/401) → PASS since the network path works.
// If cfg.Listen points to the same host:port (self-loop) we SKIP the probe
// to match the existing upstream ping guard.
func runDoctorCheckUpstreamReachable(stderr io.Writer, cfg *config.Config) DoctorResult {
	if cfg.Upstream.BaseURL == "" {
		fmt.Fprintf(stderr, "%-32s SKIP  (no upstream.base_url)\n", "upstream_reachable")
		return DoctorPass
	}
	if cfg.Listen != "" && IsSelfReferentialURL(cfg.Upstream.BaseURL, cfg.Listen) {
		fmt.Fprintf(stderr, "%-32s SKIP  (self-referential; covered by upstream check above)\n", "upstream_reachable")
		return DoctorPass
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := strings.TrimRight(cfg.Upstream.BaseURL, "/") + "/v1/models"
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if cfg.Upstream.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Upstream.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "%-32s FAIL  %v\n", "upstream_reachable", err)
		return DoctorFail
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	fmt.Fprintf(stderr, "%-32s PASS  (HTTP %d)\n", "upstream_reachable", resp.StatusCode)
	return DoctorPass
}

// doctorProviderList flattens the config's vision provider sources into a
// single ordered list. Uses cfg.VisionProviders if non-empty; otherwise falls
// back to cfg.Vision (legacy single mode) as a synthetic "legacy" entry.
// Empty BaseURL entries are skipped so the caller doesn't probe nothing.
func doctorProviderList(cfg *config.Config) []config.ProviderCfg {
	var out []config.ProviderCfg
	if len(cfg.VisionProviders) > 0 {
		for _, p := range cfg.VisionProviders {
			if p.BaseURL == "" {
				continue
			}
			out = append(out, p)
		}
		return out
	}
	if cfg.Vision.BaseURL != "" {
		out = append(out, config.ProviderCfg{
			Name:    "legacy",
			Type:    "mimo",
			BaseURL: cfg.Vision.BaseURL,
			APIKey:  cfg.Vision.APIKey,
			Model:   cfg.Vision.Model,
			Timeout: cfg.Vision.Timeout,
		})
	}
	return out
}

// listModelsHTTP does a raw GET <base>/v1/models with auth header. Returns
// the list of model IDs as reported by the server, or an error. Used by D3
// vision_model_exists as the optional secondary (WARN-only) model-list check.
func listModelsHTTP(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode /models response: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// containsModel reports whether model is present in models (case-sensitive).
func containsModel(models []string, model string) bool {
	for _, m := range models {
		if m == model {
			return true
		}
	}
	return false
}

// runDoctorCheckVisionModelExists runs for each configured vision provider:
//   - Ping the provider (uses existing Pinger interface, POST /v1/messages or
//     /chat/completions). 401 = explicit auth failure → FAIL. Any network
//     error → FAIL. Other HTTP → PASS (endpoint reachable).
//   - Then, as a WARN-only secondary step, call GET /v1/models and check if
//     the configured model appears in the list. 404 / parse error / model
//     missing → WARN. Provider ping failure → skip /models probe (already
//     in FAIL state).
func runDoctorCheckVisionModelExists(stderr io.Writer, cfg *config.Config) DoctorResult {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	providers := doctorProviderList(cfg)
	if len(providers) == 0 {
		fmt.Fprintf(stderr, "%-32s SKIP  (no vision providers configured)\n", "vision_model_exists")
		return DoctorPass
	}
	worst := DoctorPass
	for _, p := range providers {
		// ── Step 1: Ping (FAIL on connect/401) ──
		provider, err := vision.BuildProvider(p, logger)
		if err != nil {
			fmt.Fprintf(stderr, "%-32s FAIL  provider=%s build: %v\n", "vision_model_exists", p.Name, err)
			worst = DoctorFail
			continue
		}
		pinger, ok := provider.(interface{ Ping(context.Context) error })
		var pingErr error
		if ok {
			pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			pingErr = pinger.Ping(pingCtx)
			cancel()
		}
		if pingErr != nil {
			fmt.Fprintf(stderr, "%-32s FAIL  provider=%s ping: %v\n", "vision_model_exists", p.Name, pingErr)
			worst = DoctorFail
			continue
		}
		// ── Step 2: /models probe (WARN-only) ──
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		models, err := listModelsHTTP(ctx, p.BaseURL, p.APIKey)
		cancel()
		if err != nil {
			fmt.Fprintf(stderr, "%-32s WARN  provider=%s /models unavailable (%v); ping OK\n", "vision_model_exists", p.Name, err)
			if worst != DoctorFail {
				worst = DoctorWarn
			}
			continue
		}
		if p.Model != "" && !containsModel(models, p.Model) {
			fmt.Fprintf(stderr, "%-32s WARN  provider=%s model %q not in returned list (len=%d)\n", "vision_model_exists", p.Name, p.Model, len(models))
			if worst != DoctorFail {
				worst = DoctorWarn
			}
			continue
		}
		fmt.Fprintf(stderr, "%-32s PASS  provider=%s model=%s\n", "vision_model_exists", p.Name, p.Model)
	}
	return worst
}
