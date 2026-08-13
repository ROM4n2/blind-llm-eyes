package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/ROM4n2/blind-llm-eyes/config"
	"github.com/ROM4n2/blind-llm-eyes/vision"
)

// runDoctor implements the `doctor` subcommand: load config, then probe the
// upstream endpoint and every configured vision provider with a lightweight
// text-only ping. Exit 0 if all checks pass, 1 if any fails.
func runDoctor(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to config file")
	upstreamModel := fs.String("upstream-model", "deepseek-chat", "model name for upstream ping")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config %q: %v\n", *configPath, err)
		return 1
	}

	return runDoctorCore(context.Background(), cfg, *upstreamModel, stdout, stderr)
}

// runDoctorCore is the testable core of the doctor subcommand. It takes a
// loaded config, pings the upstream and each vision provider, and writes
// results to stdout/stderr. Returns 0 if all checks pass, 1 if any fails.
func runDoctorCore(ctx context.Context, cfg *config.Config, upstreamModel string, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	failed := false

	// ── Upstream ping ──
	fmt.Fprintf(stdout, "checking upstream (%s) ... ", cfg.Upstream.BaseURL)
	if cfg.Upstream.BaseURL == "" {
		fmt.Fprintln(stdout, "SKIP (no upstream.base_url configured)")
	} else {
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
			} else {
				fmt.Fprintln(stdout, "PASS")
			}
		}
	} else if cfg.Vision.BaseURL != "" {
		fmt.Fprintf(stdout, "checking vision (%s) ... ", cfg.Vision.BaseURL)
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
			} else {
				fmt.Fprintln(stdout, "PASS")
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
