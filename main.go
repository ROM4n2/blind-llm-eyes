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

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/config"
	"github.com/ROM4n2/blind-llm-eyes/logging"
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
		entries := make([]vision.PoolEntry, 0, len(cfg.VisionProviders))
		for _, pc := range cfg.VisionProviders {
			var p vision.VisionProvider
			switch pc.Type {
			case "mimo":
				p = vision.NewClient(
					strings.TrimRight(pc.BaseURL, "/"),
					pc.APIKey,
					pc.Model,
					pc.Timeout,
					pc.LargeTimeout,
					pc.LargeImageThreshold,
					pc.DescriptionCap,
					pc.SupportedFormats,
					logger,
				)
			case "openai_compatible":
				p = vision.NewOpenAIClient(
					strings.TrimRight(pc.BaseURL, "/"),
					pc.APIKey,
					pc.Model,
					pc.Timeout,
					pc.LargeTimeout,
					pc.LargeImageThreshold,
					pc.DescriptionCap,
					pc.SupportedFormats,
					logger,
				)
			default:
				fmt.Fprintf(os.Stderr, "unknown provider type %q for %q\n", pc.Type, pc.Name)
				os.Exit(1)
			}
			entries = append(entries, vision.PoolEntry{
				Name:                pc.Name,
				Provider:            p,
				Priority:            pc.Priority,
				CB:                  vision.NewCircuitBreaker(pc.CircuitBreaker.FailureThreshold, pc.CircuitBreaker.ResetTimeout),
				Timeout:             pc.Timeout,
				LargeTimeout:        pc.LargeTimeout,
				LargeImageThreshold: pc.LargeImageThreshold,
			})
		}
		pool, err := vision.NewPool(entries, logger, m)
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
		visionProvider = vision.NewClient(
			strings.TrimRight(cfg.Vision.BaseURL, "/"),
			cfg.Vision.APIKey,
			cfg.Vision.Model,
			cfg.Vision.Timeout,
			cfg.Vision.LargeTimeout,
			cfg.Vision.LargeImageThreshold,
			cfg.Vision.DescriptionCap,
			cfg.Vision.SupportedFormats,
			logger,
		)
		logger.Info("single-provider mode",
			"mode", "single",
			"vision_model", cfg.Vision.Model,
		)
	}

	deps := proxy.HandlerDeps{
		UpstreamBaseURL:     strings.TrimRight(cfg.Upstream.BaseURL, "/"),
		UpstreamAPIKey:      cfg.Upstream.APIKey,
		VisionProvider:      visionProvider,
		Cache:               cache.NewLRU(cfg.Cache.MaxEntries),
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
		logWriter.Close()
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

		// 3) 刷新日志缓冲
		logWriter.Close()
	}
}
