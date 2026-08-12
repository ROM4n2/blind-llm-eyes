package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/config"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
	"github.com/ROM4n2/blind-llm-eyes/proxy"
	"github.com/ROM4n2/blind-llm-eyes/vision"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config yaml")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	logger := buildLogger(cfg.LogLevel)
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
	)

	// 初始化 Prometheus Metrics
	m := metrics.NewMetrics()
	logger.Info("prometheus metrics initialized",
		"endpoints", "/metrics",
	)

	// WaitGroup for graceful shutdown
	var wg sync.WaitGroup

	deps := proxy.HandlerDeps{
		UpstreamBaseURL:     strings.TrimRight(cfg.Upstream.BaseURL, "/"),
		UpstreamAPIKey:      cfg.Upstream.APIKey,
		VisionProvider: vision.NewClient(
			strings.TrimRight(cfg.Vision.BaseURL, "/"),
			cfg.Vision.APIKey,
			cfg.Vision.Model,
			cfg.Vision.Timeout,
			cfg.Vision.LargeTimeout,
			cfg.Vision.LargeImageThreshold,
			cfg.Vision.DescriptionCap,
			cfg.Vision.SupportedFormats,
			logger,
		),
		Cache:               cache.NewLRU(cfg.Cache.MaxEntries),
		FailOpen:            cfg.FailOpen,
		LargeImageThreshold: cfg.Vision.LargeImageThreshold,
		Log:                 logger,
		WG:                  &wg,
		Metrics:             m,
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

	select {
	case err := <-errCh:
		logger.Error("server failed", "err", err)
		os.Exit(1)
	case sig := <-sigCh:
		logger.Info("shutting down gracefully",
			"signal", sig,
			"waiting_for_inflight_requests", true,
		)

		// 1) 停止接受新请求（给在途请求 15s 时间完成）
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("server shutdown error", "err", err)
		}

		// 2) 等待所有在途请求完成
		logger.Info("waiting for in-flight requests to complete...")
		wg.Wait()
		logger.Info("all in-flight requests completed, shutting down")
	}
}

func buildLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
