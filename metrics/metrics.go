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
	}

	return m
}
