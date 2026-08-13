package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics 包含所有可观测性指标。
// 通过构造函数 NewMetrics() 创建实例，注入到 HandlerDeps。
type Metrics struct {
	Registry *prometheus.Registry

	// HTTP 请求指标
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec

	// 图片处理指标
	ImagesProcessedTotal *prometheus.CounterVec
	CacheHitRatio        prometheus.Gauge

	// Vision 调用指标
	VisionCallsTotal   *prometheus.CounterVec
	VisionCallDuration *prometheus.HistogramVec

	// 上游转发指标
	UpstreamRequestsTotal   *prometheus.CounterVec
	UpstreamRequestDuration *prometheus.HistogramVec

	// 自适应限流指标
	AdaptiveConcurrencyCurrent     prometheus.Gauge
	AdaptiveConcurrencyAdjustments *prometheus.CounterVec
	AdaptiveVisionP90Seconds       prometheus.Gauge

	// 多 Provider 池指标
	ProviderCallsTotal  *prometheus.CounterVec
	ProviderDuration    *prometheus.HistogramVec
	CircuitBreakerState *prometheus.GaugeVec
	FailoverEventsTotal prometheus.Counter
}

// NewMetrics 创建并注册所有指标。
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		Registry: reg,

		HTTPRequestsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_http_requests_total",
				Help: "Total HTTP requests processed, labeled by method, route, and status code.",
			},
			[]string{"method", "route", "status"},
		),

		HTTPRequestDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "blind_llm_eyes_http_request_duration_seconds",
				Help:    "Duration of HTTP requests in seconds, end-to-end.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "route", "status"},
		),

		ImagesProcessedTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_images_processed_total",
				Help: "Total image blocks processed, labeled by outcome.",
			},
			[]string{"outcome"}, // "rewritten" | "cached" | "failed"
		),

		CacheHitRatio: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "blind_llm_eyes_cache_hit_ratio",
				Help: "Ratio of cache hits to total image lookups in the most recent window.",
			},
		),

		VisionCallsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_vision_calls_total",
				Help: "Total vision API calls, labeled by result.",
			},
			[]string{"result"}, // "success" | "error" | "fail_open"
		),

		VisionCallDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "blind_llm_eyes_vision_call_duration_seconds",
				Help:    "Duration of vision API calls in seconds.",
				Buckets: append(prometheus.DefBuckets, 30, 60, 120, 180),
			},
			[]string{"result"},
		),

		UpstreamRequestsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_upstream_requests_total",
				Help: "Total upstream LLM API requests, labeled by status code.",
			},
			[]string{"status"},
		),

		UpstreamRequestDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "blind_llm_eyes_upstream_request_duration_seconds",
				Help:    "Duration of upstream LLM API requests in seconds.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"status"},
		),

		AdaptiveConcurrencyCurrent: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "blind_llm_eyes_adaptive_concurrency_current",
				Help: "Current effective concurrency limit. Equals static value when adaptive disabled.",
			},
		),

		AdaptiveConcurrencyAdjustments: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_adaptive_concurrency_adjustments_total",
				Help: "Total adaptive concurrency adjustments, labeled by direction.",
			},
			[]string{"direction"},
		),

		AdaptiveVisionP90Seconds: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "blind_llm_eyes_adaptive_vision_p90_seconds",
				Help: "P90 vision latency (seconds) from the most recent evaluation window.",
			},
		),

		ProviderCallsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_provider_calls_total",
				Help: "Total vision provider calls, labeled by provider name and result.",
			},
			[]string{"provider", "result"}, // result: "success" | "error" | "skipped"
		),

		ProviderDuration: promauto.With(reg).NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "blind_llm_eyes_provider_duration_seconds",
				Help:    "Duration of vision provider calls in seconds, labeled by provider.",
				Buckets: append(prometheus.DefBuckets, 30, 60, 120, 180),
			},
			[]string{"provider"},
		),

		CircuitBreakerState: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "blind_llm_eyes_circuit_breaker_state",
				Help: "Circuit breaker state per provider: 0=closed, 1=open, 2=half_open.",
			},
			[]string{"provider"},
		),

		FailoverEventsTotal: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_failover_events_total",
				Help: "Total number of provider failover events (a provider failed and the next was tried).",
			},
		),
	}

	return m
}
