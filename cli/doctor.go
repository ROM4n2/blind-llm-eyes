package cli

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log/slog"

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
