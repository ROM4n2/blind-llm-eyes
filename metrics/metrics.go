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

	// ── v1.3.0 P0: cache tiered observability + circuit breaker transitions ──
	// CacheHitsTotal counts per-tier hit/miss lookups so operators can compute
	// tier-specific hit ratios and detect cold-layer thrash (high cold miss →
	// LRU size too small for working set, or SQLite DB missing).
	// Labels:
	//   tier   = "hot"  (TwoTier in-memory LRU)
	//          | "cold" (TwoTier SQLite persistence)
	//          | "lru"  (single-tier LRU, no cold layer)
	//   result = "hit" | "miss"
	CacheHitsTotal *prometheus.CounterVec

	// CacheRowCount reports the number of rows in each durable cache backend.
	// Currently only backend="sqlite" emits a value; LRU-only deployments leave
	// this gauge absent (no labels set). Helps operators size SQLite vacuum
	// intervals and detect unbounded growth when TTL is disabled.
	CacheRowCount *prometheus.GaugeVec

	// CacheDriftPct is the drift between the in-memory SQLite row counter
	// (MemoryCount) and the true DB row count (ActualCount) expressed as a
	// percentage. 0% = perfect sync. Positive % = memory counter overcounts
	// vs reality (e.g. external DELETE or evict CAS racing). Values > ~5% for
	// sustained periods warrant investigation (trigger DB reconciliation in
	// a future release).
	// This gauge is ONLY set when a cache health check (or doctor) runs both
	// counts; it is not updated on the hot path to avoid O(N) queries.
	CacheDriftPct prometheus.Gauge

	// CBTransitions counts every circuit breaker state change with the full
	// (provider, from, to) triple. Complements CircuitBreakerState gauge which
	// only reports the current value per provider — this counter lets
	// dashboards show churning rate (CB flapping = upstream provider is
	// oscillating between healthy/dead, consider raising failure_threshold or
	// lowering provider priority).
	// Labels: provider, from ∈ {closed, open, half_open}, to ∈ same set.
	CBTransitions *prometheus.CounterVec

	// ── v1.3.0 P1: singleflight dedup observability ──
	// SFTotal splits singleflight.Do callers by the phase they experienced:
	//   phase="exec" — the one goroutine that actually ran the vision fn
	//                   (singleflight.Do returned shared=false)
	//   phase="wait" — all other goroutines deduplicated and waited on the
	//                   exec goroutine's result (shared=true)
	// Summing exec + wait gives the total caller count; exec/(exec+wait) is
	// the dedup density (1 = no concurrent duplicates → SF did nothing).
	SFTotal *prometheus.CounterVec

	// SFMerged counts how many duplicate in-flight callers were saved from
	// re-executing the vision call. Each shared=true caller increments this
	// once. SFMerged == sum of phase=wait in SFTotal; provided as a plain
	// counter so dashboards can write a simpler "dedup total" panel without
	// filtering a label.
	SFMerged prometheus.Counter

	// ── v1.3.0 P1: ingress bytes ──
	// ImagesBytesIn tracks cumulative decoded image input bytes per media
	// type (i.e. the raw byte length AFTER base64 decode). This is the
	// actual payload the vision provider receives / the image hash is
	// computed over. Operators use this to identify bursts of large images
	// (correlated with latency spikes) and to attribute cost across formats
	// (JPEG vs PNG vs WebP volume).
	ImagesBytesIn *prometheus.CounterVec

	// ── v1.3.0 P1: payload size histograms + SSE events ──
	// ReqBodyBytes is the HTTP request body size in bytes (after MaxBytes
	// cap; observed only on successful read). 8 buckets from 1KB to 20MB
	// cover the typical range: 1KB tiny text-only, 1–5MB multi-image,
	// 5–20MB extremely large. >20MB bucket catches the rare oversized.
	ReqBodyBytes prometheus.Histogram

	// RespBodyBytes is the total HTTP response body size written to the
	// client in bytes (SSE stream total or non-SSE response). Same buckets
	// as ReqBodyBytes. Paired with request content-length from upstream,
	// this lets operators detect proxy amplification/reshaping issues.
	RespBodyBytes prometheus.Histogram

	// SSEEvents counts Server-Sent Events parsed from the streamed
	// response body as it is written. Classification:
	//   event="message" — either no event: line (default SSE event name) or
	//                      explicit event:message.
	//   event="error"   — event:error lines (provider error streams).
	//   event="other"   — any other event: name.
	// Scanned line-by-line during streaming write; scans only the bytes
	// being written (no backtracking). Lines without an event: prefix are
	// counted as "message". Useful for spotting error-only streams,
	// truncation, or unexpected event types.
	SSEEvents *prometheus.CounterVec
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

		// ── v1.3.0 P0 metrics ──
		CacheHitsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_cache_hits_total",
				Help: "Per-tier cache lookups (hit/miss). tier=hot|cold|lru; allows dashboards to compute TwoTier hit ratios per layer.",
			},
			[]string{"tier", "result"},
		),

		CacheRowCount: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "blind_llm_eyes_cache_row_count",
				Help: "Current durable-cache row count by backend. Only 'sqlite' backend emits; LRU-only deployments have no samples.",
			},
			[]string{"backend"},
		),

		CacheDriftPct: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Name: "blind_llm_eyes_cache_drift_pct",
				Help: "SQLite in-memory counter drift vs actual DB count, as a percentage. Set only by cache/doctor health probes (not hot path).",
			},
		),

		CBTransitions: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_circuit_breaker_transitions_total",
				Help: "All circuit breaker state transitions, labeled by provider, from-state and to-state. Detect flapping providers via rate(this[5m]).",
			},
			[]string{"provider", "from", "to"},
		),

		// ── v1.3.0 P1 metrics ──
		SFTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_singleflight_total",
				Help: "Singleflight phases per vision dedup call. phase='exec' = the goroutine that ran the vision fn; phase='wait' = goroutines deduped and waiting on the exec result.",
			},
			[]string{"phase"},
		),
		SFMerged: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_singleflight_merged_requests_total",
				Help: "Total singleflight waiters merged into one exec call (i.e. duplicate in-flight calls saved). Equals sum of phase=wait in SFTotal.",
			},
		),
		ImagesBytesIn: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Name: "blind_llm_eyes_images_bytes_in_total",
				Help: "Cumulative bytes of decoded image input per media type, counted after base64 decode (this is the actual vision-provider payload size).",
			},
			[]string{"format"},
		),
	}

	// Payload size buckets chosen to cover the typical request/response
	// envelope: 1KB tiny text-only requests → 100KB average multi-image →
	// 5MB large request with several big images → 20MB extreme catch-all.
	sizeBuckets := []float64{1e3, 1e4, 5e4, 1e5, 5e5, 1e6, 5e6, 2e7}
	m.ReqBodyBytes = promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name:    "blind_llm_eyes_request_body_bytes",
		Help:    "HTTP request body size in bytes. Observed only after a successful MaxBytes-bounded ReadAll.",
		Buckets: sizeBuckets,
	})
	m.RespBodyBytes = promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name:    "blind_llm_eyes_response_body_bytes",
		Help:    "HTTP response body size in bytes written to client (SSE stream total, or non-SSE response).",
		Buckets: sizeBuckets,
	})
	m.SSEEvents = promauto.With(reg).NewCounterVec(
		prometheus.CounterOpts{
			Name: "blind_llm_eyes_sse_events_total",
			Help: "SSE events parsed from streamed response. event='message' covers default (no event:) or event:message; 'error' = event:error; 'other' = any other event name.",
		},
		[]string{"event"},
	)

	return m
}
