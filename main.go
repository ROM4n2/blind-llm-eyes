package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/admin"
	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/cli"
	"github.com/ROM4n2/blind-llm-eyes/config"
	"github.com/ROM4n2/blind-llm-eyes/logging"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
	"github.com/ROM4n2/blind-llm-eyes/proxy"
	"github.com/ROM4n2/blind-llm-eyes/vision"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	args := os.Args[1:]
	// Server path: no args (backward compat), "start", or any -flag (e.g. -config).
	// Everything else is a subcommand handled by cli.Run.
	if len(args) == 0 || args[0] == "start" || strings.HasPrefix(args[0], "-") {
		runServer(args)
		return
	}
	os.Exit(cli.Run(args, os.Stdin, os.Stdout, os.Stderr))
}

// runServer starts the proxy server in the foreground. It parses the -config
// flag (default config.yaml), stripping an optional leading "start" subcommand
// so both "blind-llm-eyes" and "blind-llm-eyes start [-config ...]" work.
func runServer(args []string) {
	flagArgs := args
	if len(flagArgs) > 0 && flagArgs[0] == "start" {
		flagArgs = flagArgs[1:]
	}
	fs := flag.NewFlagSet("blind-llm-eyes", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config yaml")
	fs.Parse(flagArgs)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	// 使用 JSON 结构化异步日志
	logger, logWriter := logging.NewLogger(cfg.LogLevel)
	defer logWriter.Close()

	logger.Info("blind-llm-eyes starting",
		"listen", cfg.Listen,
		"upstream", cfg.Upstream.BaseURL,
		"upstream_key_set", cfg.Upstream.APIKey != "",
		"vision_model", cfg.Vision.Model,
		"vision_timeout", cfg.Vision.Timeout,
		"vision_large_timeout", cfg.Vision.LargeTimeout,
		"large_image_threshold", cfg.Vision.LargeImageThreshold,
		"supported_formats", cfg.Vision.SupportedFormats,
		"fail_open", cfg.FailOpen,
		"cache_max", cfg.Cache.MaxEntries,
		"concurrency_limit", cfg.ConcurrencyLimit,
		"adaptive_enabled", cfg.AdaptiveConcurrency.Enabled,
		"adaptive_range", fmt.Sprintf("[%d, %d]", cfg.AdaptiveConcurrency.MinLimit, cfg.AdaptiveConcurrency.MaxLimit),
		"adaptive_threshold_ms", fmt.Sprintf("fast=%d slow=%d",
			cfg.AdaptiveConcurrency.FastThresholdMs, cfg.AdaptiveConcurrency.SlowThresholdMs),
		"context_rounds", cfg.Vision.ContextRounds,
		"context_max_chars", cfg.Vision.ContextMaxChars,
		"context_enabled", cfg.Vision.ContextRounds > 0,
	)

	// 初始化 Prometheus Metrics
	m := metrics.NewMetrics()
	logger.Info("prometheus metrics initialized",
		"endpoints", "/metrics",
	)

	// WaitGroup for graceful shutdown
	var wg sync.WaitGroup

	// 构造自适应并发控制器
	acCfg := proxy.AdaptiveConcurrencyCfg{
		Enabled:         cfg.AdaptiveConcurrency.Enabled,
		MinLimit:        cfg.AdaptiveConcurrency.MinLimit,
		MaxLimit:        cfg.AdaptiveConcurrency.MaxLimit,
		InitialLimit:    cfg.ConcurrencyLimit,
		FastThresholdMs: cfg.AdaptiveConcurrency.FastThresholdMs,
		SlowThresholdMs: cfg.AdaptiveConcurrency.SlowThresholdMs,
		SampleWindow:    cfg.AdaptiveConcurrency.SampleWindow,
		CooldownMs:      cfg.AdaptiveConcurrency.CooldownMs,
		IncreaseStep:    cfg.AdaptiveConcurrency.IncreaseStep,
		DecreaseRatio:   cfg.AdaptiveConcurrency.DecreaseRatio,
		ErrorThreshold:  cfg.AdaptiveConcurrency.ErrorThreshold,
	}
	ac := proxy.NewAdaptiveConcurrency(acCfg, m, logger)

	// 构造 VisionProvider：多 provider 池模式（vision_providers 非空）或单 provider 模式（向后兼容）
	var visionProvider vision.VisionProvider
	if len(cfg.VisionProviders) > 0 {
		pool, err := vision.BuildPool(cfg.VisionProviders, logger, m)
		if err != nil {
			fmt.Fprintf(os.Stderr, "build vision pool: %v\n", err)
			os.Exit(1)
		}
		visionProvider = pool
		logger.Info("multi-provider pool enabled",
			"providers", pool.ProviderNames(),
			"mode", "pool",
		)
	} else {
		p, err := vision.BuildSingleProvider(cfg.Vision, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "build vision provider: %v\n", err)
			os.Exit(1)
		}
		visionProvider = p
		logger.Info("single-provider mode",
			"mode", "single",
			"vision_model", cfg.Vision.Model,
		)
	}

	// Build cache backend: type=twotier uses LRU+SQLite; on SQLite open
	// failure, fall back to LRU-only (persistence is an enhancement, never
	// blocks startup). type=lru (default) stays in-memory only.
	var cacheBackend cache.Cache
	switch cfg.Cache.Type {
	case "twotier":
		sqlc, err := cache.OpenSQLite(cfg.Cache.DBPath, cfg.Cache.SqliteMaxEntries, cfg.Cache.SqliteTTL, logger)
		if err != nil {
			logger.Warn("sqlite open failed, falling back to LRU-only", "err", err)
			cacheBackend = cache.NewLRU(cfg.Cache.MaxEntries)
		} else {
			cacheBackend = cache.NewTwoTier(cfg.Cache.MaxEntries, sqlc, logger)
			logger.Info("persistent cache enabled",
				"db_path", cfg.Cache.DBPath,
				"sqlite_max_entries", cfg.Cache.SqliteMaxEntries,
				"sqlite_ttl", cfg.Cache.SqliteTTL,
			)
		}
	default:
		cacheBackend = cache.NewLRU(cfg.Cache.MaxEntries)
	}

	deps := proxy.HandlerDeps{
		UpstreamBaseURL:     strings.TrimRight(cfg.Upstream.BaseURL, "/"),
		UpstreamAPIKey:      cfg.Upstream.APIKey,
		VisionProvider:      visionProvider,
		Cache:               cacheBackend,
		FailOpen:            cfg.FailOpen,
		LargeImageThreshold: cfg.Vision.LargeImageThreshold,
		MaxBodyBytes:        cfg.MaxBodyBytes,
		ConcurrencyLimit:    cfg.ConcurrencyLimit,
		AdaptiveConcurrency: ac,
		ContextRounds:       cfg.Vision.ContextRounds,
		ContextMaxChars:     cfg.Vision.ContextMaxChars,
		Log:                 logger,
		WG:                  &wg,
		Metrics:             m,
	}
	// Build vision-capable model set for passthrough (skip rewrite when
	// upstream model natively supports images). NewHandler normalizes to
	// lowercase keys for case-insensitive matching.
	if len(cfg.VisionCapableModels) > 0 {
		deps.VisionCapableModels = make(map[string]bool, len(cfg.VisionCapableModels))
		for _, model := range cfg.VisionCapableModels {
			deps.VisionCapableModels[model] = true
		}
	}

	// 主路由：代理 + metrics
	mux := http.NewServeMux()
	mux.Handle("/v1/messages", proxy.NewHandler(deps))
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Admin shutdown endpoint + pidfile (used by the status/stop subcommands).
	// The token is generated per-start and written to the pidfile so "stop" can
	// authenticate. bind 127.0.0.1 (default listen) keeps the endpoint local.
	pidfilePath, err := cli.DefaultPidfilePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "locate pidfile path: %v\n", err)
		os.Exit(1)
	}
	adminToken := admin.MustGenerateToken(32)
	adminH := admin.NewShutdownHandler(adminToken)
	mux.Handle("/admin/shutdown", adminH)
	pid := os.Getpid()
	if err := cli.WritePidfile(pidfilePath, cli.PidfileData{
		PID:       pid,
		Addr:      cfg.Listen,
		Token:     adminH.Token(),
		StartedAt: time.Now(),
	}); err != nil {
		logger.Error("write pidfile", "err", err)
	}
	defer os.Remove(pidfilePath)

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      mux,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 10 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// gracefulShutdown stops accepting new requests, drains in-flight work, then
	// returns so the deferred pidfile removal + log flush run. Shared by the
	// signal and admin-requested shutdown paths.
	gracefulShutdown := func(reason string) {
		logger.Info("shutting down gracefully",
			"reason", reason,
			"waiting_for_inflight_requests", true,
		)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("server shutdown error", "err", err)
		}
		logger.Info("waiting for in-flight requests to complete...")
		wg.Wait()
		logger.Info("all in-flight requests completed, shutting down")
	}

	select {
	case err := <-errCh:
		logger.Error("server failed", "err", err)
		os.Remove(pidfilePath) // defers don't run before os.Exit
		logWriter.Close()
		os.Exit(1)
	case sig := <-sigCh:
		gracefulShutdown(fmt.Sprintf("signal %s", sig))
	case <-adminH.Done():
		gracefulShutdown("admin request")
	}
}
