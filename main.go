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
	"syscall"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/config"
	"github.com/ROM4n2/blind-llm-eyes/proxy"
	"github.com/ROM4n2/blind-llm-eyes/vision"
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
		"fail_open", cfg.FailOpen,
		"cache_max", cfg.Cache.MaxEntries,
	)

	deps := proxy.HandlerDeps{
		UpstreamBaseURL: strings.TrimRight(cfg.Upstream.BaseURL, "/"),
		UpstreamAPIKey:  cfg.Upstream.APIKey,
		VisionClient: &vision.Client{
			BaseURL:        strings.TrimRight(cfg.Vision.BaseURL, "/"),
			APIKey:         cfg.Vision.APIKey,
			Model:          cfg.Vision.Model,
			DescriptionCap: cfg.Vision.DescriptionCap,
			Timeout:        cfg.Vision.Timeout,
		},
		Cache:    cache.NewLRU(cfg.Cache.MaxEntries),
		FailOpen: cfg.FailOpen,
		Log:      logger,
	}

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      proxy.NewHandler(deps),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 10 * time.Minute, // 长 SSE 响应
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
		logger.Info("shutting down", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("shutdown error", "err", err)
		}
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
