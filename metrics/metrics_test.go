package metrics

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func TestNewMetrics_Registry(t *testing.T) {
	m := NewMetrics()
	if m.Registry == nil {
		t.Fatal("registry should not be nil")
	}

	// 触发计数器注册（promauto 惰性注册：只有 Inc() 后才会出现在 Gather 中）
	m.HTTPRequestsTotal.WithLabelValues("probe", "/probe", "0").Inc()
	m.HTTPRequestDuration.WithLabelValues("probe", "/probe", "0").Observe(0)
	m.ImagesProcessedTotal.WithLabelValues("probe").Inc()
	m.CacheHitRatio.Set(0)
	m.VisionCallsTotal.WithLabelValues("probe").Inc()
	m.VisionCallDuration.WithLabelValues("probe").Observe(0)
	m.UpstreamRequestsTotal.WithLabelValues("0").Inc()
	m.UpstreamRequestDuration.WithLabelValues("0").Observe(0)

	// 验证所有指标都已注册
	metricFamilies, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	names := make(map[string]bool)
	for _, mf := range metricFamilies {
		names[mf.GetName()] = true
	}

	expected := []string{
		"blind_llm_eyes_http_requests_total",
		"blind_llm_eyes_http_request_duration_seconds",
		"blind_llm_eyes_images_processed_total",
		"blind_llm_eyes_cache_hit_ratio",
		"blind_llm_eyes_vision_calls_total",
		"blind_llm_eyes_vision_call_duration_seconds",
		"blind_llm_eyes_upstream_requests_total",
		"blind_llm_eyes_upstream_request_duration_seconds",
	}

	for _, name := range expected {
		if !names[name] {
			t.Errorf("metric %q not found in registry", name)
		}
	}
}

func TestMetrics_CounterIncrement(t *testing.T) {
	m := NewMetrics()

	m.HTTPRequestsTotal.WithLabelValues("POST", "/v1/messages", "200").Inc()
	m.HTTPRequestsTotal.WithLabelValues("POST", "/v1/messages", "200").Inc()
	m.HTTPRequestsTotal.WithLabelValues("GET", "/healthz", "200").Inc()

	metricFamilies, _ := m.Registry.Gather()
	for _, mf := range metricFamilies {
		if mf.GetName() == "blind_llm_eyes_http_requests_total" {
			for _, metric := range mf.GetMetric() {
				labels := metric.GetLabel()
				method := getLabelValue(labels, "method")
				status := getLabelValue(labels, "status")
				count := metric.GetCounter().GetValue()

				if method == "POST" && status == "200" && count != 2 {
					t.Errorf("POST/200 count: want 2, got %f", count)
				}
				if method == "GET" && status == "200" && count != 1 {
					t.Errorf("GET/200 count: want 1, got %f", count)
				}
			}
		}
	}
}

func TestMetrics_GaugeSet(t *testing.T) {
	m := NewMetrics()
	m.CacheHitRatio.Set(0.75)

	metricFamilies, _ := m.Registry.Gather()
	for _, mf := range metricFamilies {
		if mf.GetName() == "blind_llm_eyes_cache_hit_ratio" {
			for _, metric := range mf.GetMetric() {
				val := metric.GetGauge().GetValue()
				if val != 0.75 {
					t.Errorf("gauge: want 0.75, got %f", val)
				}
			}
		}
	}
}

func TestMetrics_HistogramObserve(t *testing.T) {
	m := NewMetrics()
	m.VisionCallDuration.WithLabelValues("success").Observe(1.5)
	m.VisionCallDuration.WithLabelValues("success").Observe(2.5)

	metricFamilies, _ := m.Registry.Gather()
	for _, mf := range metricFamilies {
		if mf.GetName() == "blind_llm_eyes_vision_call_duration_seconds" {
			for _, metric := range mf.GetMetric() {
				count := metric.GetHistogram().GetSampleCount()
				if count != 2 {
					t.Errorf("histogram count: want 2, got %d", count)
				}
			}
		}
	}
}

func getLabelValue(labels []*dto.LabelPair, name string) string {
	for _, l := range labels {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// ── v1.3.0 P0 metrics: cache tiered hits/misses + SQLite gauges + CB transitions ──

func TestNewMetrics_P0MetricsRegistered(t *testing.T) {
	m := NewMetrics()
	// Trigger lazy registration for counters/vecs:
	m.CacheHitsTotal.WithLabelValues("hot", "hit").Inc()
	m.CacheHitsTotal.WithLabelValues("cold", "miss").Inc()
	m.CacheHitsTotal.WithLabelValues("lru", "hit").Inc()
	m.CacheRowCount.WithLabelValues("sqlite").Set(0)
	m.CacheDriftPct.Set(0)
	m.CBTransitions.WithLabelValues("primary", "closed", "open").Inc()
	// ProviderCallsTotal is the project's existing provider call counter; it
	// already matches the planned "ProviderCallsV2" spec {provider, result}.
	m.ProviderCallsTotal.WithLabelValues("mimo", "success").Inc()

	mf, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool)
	for _, f := range mf {
		names[f.GetName()] = true
	}
	want := []string{
		"blind_llm_eyes_cache_hits_total",
		"blind_llm_eyes_cache_row_count",
		"blind_llm_eyes_cache_drift_pct",
		"blind_llm_eyes_circuit_breaker_transitions_total",
		"blind_llm_eyes_provider_calls_total",
	}
	for _, n := range want {
		if !names[n] {
			t.Errorf("expected P0 metric %q not registered", n)
		}
	}
}

func TestMetrics_CacheHitsTotal_TierLabels(t *testing.T) {
	m := NewMetrics()
	for i := 0; i < 7; i++ {
		m.CacheHitsTotal.WithLabelValues("hot", "hit").Inc()
	}
	for i := 0; i < 3; i++ {
		m.CacheHitsTotal.WithLabelValues("hot", "miss").Inc()
	}
	m.CacheHitsTotal.WithLabelValues("cold", "hit").Inc()
	m.CacheHitsTotal.WithLabelValues("cold", "miss").Inc()
	m.CacheHitsTotal.WithLabelValues("lru", "hit").Inc()

	mf, _ := m.Registry.Gather()
	counts := map[[2]string]float64{}
	for _, f := range mf {
		if f.GetName() != "blind_llm_eyes_cache_hits_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			labels := metric.GetLabel()
			key := [2]string{getLabelValue(labels, "tier"), getLabelValue(labels, "result")}
			counts[key] = metric.GetCounter().GetValue()
		}
	}
	cases := []struct {
		tier, result string
		want         float64
	}{
		{"hot", "hit", 7},
		{"hot", "miss", 3},
		{"cold", "hit", 1},
		{"cold", "miss", 1},
		{"lru", "hit", 1},
	}
	for _, c := range cases {
		got := counts[[2]string{c.tier, c.result}]
		if got != c.want {
			t.Errorf("cache_hits{tier=%q,result=%q}: want %v, got %v", c.tier, c.result, c.want, got)
		}
	}
}

func TestMetrics_CacheRowCount_And_DriftPct(t *testing.T) {
	m := NewMetrics()
	m.CacheRowCount.WithLabelValues("sqlite").Set(1234)
	m.CacheDriftPct.Set(3.7) // 3.7% drift: memory counter drifted up by 3.7% vs real DB

	mf, _ := m.Registry.Gather()
	found := map[string]float64{}
	for _, f := range mf {
		for _, metric := range f.GetMetric() {
			switch f.GetName() {
			case "blind_llm_eyes_cache_row_count":
				if getLabelValue(metric.GetLabel(), "backend") == "sqlite" {
					found["rowcount"] = metric.GetGauge().GetValue()
				}
			case "blind_llm_eyes_cache_drift_pct":
				found["drift"] = metric.GetGauge().GetValue()
			}
		}
	}
	if got, ok := found["rowcount"]; !ok || got != 1234 {
		t.Errorf("cache_row_count sqlite: want 1234, got %v (ok=%v)", got, ok)
	}
	if got, ok := found["drift"]; !ok || got != 3.7 {
		t.Errorf("cache_drift_pct: want 3.7, got %v (ok=%v)", got, ok)
	}
}

func TestMetrics_CircuitBreakerTransitions(t *testing.T) {
	m := NewMetrics()
	// 2 closed→open on providers "a" and "b"; 1 open→half_open on "a"; 1 half_open→closed on "a"
	m.CBTransitions.WithLabelValues("a", "closed", "open").Inc()
	m.CBTransitions.WithLabelValues("a", "closed", "open").Inc()
	m.CBTransitions.WithLabelValues("b", "closed", "open").Inc()
	m.CBTransitions.WithLabelValues("a", "open", "half_open").Inc()
	m.CBTransitions.WithLabelValues("a", "half_open", "closed").Inc()

	mf, _ := m.Registry.Gather()
	counts := map[[3]string]float64{}
	for _, f := range mf {
		if f.GetName() != "blind_llm_eyes_circuit_breaker_transitions_total" {
			continue
		}
		for _, metric := range f.GetMetric() {
			labels := metric.GetLabel()
			key := [3]string{
				getLabelValue(labels, "provider"),
				getLabelValue(labels, "from"),
				getLabelValue(labels, "to"),
			}
			counts[key] = metric.GetCounter().GetValue()
		}
	}
	cases := []struct {
		prov, from, to string
		want           float64
	}{
		{"a", "closed", "open", 2},
		{"b", "closed", "open", 1},
		{"a", "open", "half_open", 1},
		{"a", "half_open", "closed", 1},
	}
	for _, c := range cases {
		got := counts[[3]string{c.prov, c.from, c.to}]
		if got != c.want {
			t.Errorf("cb_transitions{prov=%q,from=%q,to=%q}: want %v, got %v",
				c.prov, c.from, c.to, c.want, got)
		}
	}
}

// ── v1.3.0 P1 metrics: singleflight + ingress bytes + payload histograms + SSE events ──

func TestSingleflightCounters(t *testing.T) {
	m := NewMetrics()
	// phase=exec: the one goroutine that runs the vision call inside sf.Do
	m.SFTotal.WithLabelValues("exec").Inc()
	// phase=wait: goroutines that were deduped and waited on the exec goroutine
	m.SFTotal.WithLabelValues("wait").Inc()
	m.SFTotal.WithLabelValues("wait").Inc()
	// SFMerged counts how many duplicate in-flight calls were merged total
	m.SFMerged.Inc()
	m.SFMerged.Inc()

	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool)
	for _, mf := range fams {
		names[mf.GetName()] = true
	}
	for _, need := range []string{
		"blind_llm_eyes_singleflight_total",
		"blind_llm_eyes_singleflight_merged_requests_total",
	} {
		if !names[need] {
			t.Fatalf("expected P1 metric %q not registered in Gather output", need)
		}
	}
}

func TestImagesBytesInCounter(t *testing.T) {
	m := NewMetrics()
	// Per-format decoded image bytes (after base64 decode, the actual payload
	// size the vision provider will receive / that hash is computed over).
	m.ImagesBytesIn.WithLabelValues("image/png").Add(123456)
	m.ImagesBytesIn.WithLabelValues("image/jpeg").Add(1_234_567)

	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range fams {
		if mf.GetName() == "blind_llm_eyes_images_bytes_in_total" {
			// Verify both label sets produced distinct counters.
			found := map[string]float64{}
			for _, metric := range mf.GetMetric() {
				fmt := getLabelValue(metric.GetLabel(), "format")
				found[fmt] = metric.GetCounter().GetValue()
			}
			if found["image/png"] != 123456 {
				t.Errorf("images_bytes_in{format=image/png}: want 123456, got %v", found["image/png"])
			}
			if found["image/jpeg"] != 1_234_567 {
				t.Errorf("images_bytes_in{format=image/jpeg}: want 1234567, got %v", found["image/jpeg"])
			}
			return
		}
	}
	t.Fatalf("metric blind_llm_eyes_images_bytes_in_total not registered")
}

func TestPayloadHistograms(t *testing.T) {
	m := NewMetrics()
	// Size buckets: [1e3, 1e4, 5e4, 1e5, 5e5, 1e6, 5e6, 2e7]
	m.ReqBodyBytes.Observe(250_000)    // lands in the 1e5–5e5 bucket
	m.ReqBodyBytes.Observe(500)        // below smallest bucket (Inf side)
	m.RespBodyBytes.Observe(5_000_000) // lands in the 1e6–5e6 bucket
	m.RespBodyBytes.Observe(25_000_000) // above largest bucket

	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool)
	sampleCounts := map[string]uint64{}
	for _, mf := range fams {
		names[mf.GetName()] = true
		for _, metric := range mf.GetMetric() {
			if h := metric.GetHistogram(); h != nil {
				sampleCounts[mf.GetName()] = h.GetSampleCount()
			}
		}
	}
	for _, need := range []string{
		"blind_llm_eyes_request_body_bytes",
		"blind_llm_eyes_response_body_bytes",
	} {
		if !names[need] {
			t.Fatalf("expected P1 metric %q not registered", need)
		}
	}
	if got := sampleCounts["blind_llm_eyes_request_body_bytes"]; got != 2 {
		t.Errorf("req_body_bytes sample count: want 2, got %d", got)
	}
	if got := sampleCounts["blind_llm_eyes_response_body_bytes"]; got != 2 {
		t.Errorf("resp_body_bytes sample count: want 2, got %d", got)
	}
}

func TestSSEEventsCounter(t *testing.T) {
	m := NewMetrics()
	// event=message covers both the no-event default and explicit event:message
	m.SSEEvents.WithLabelValues("message").Add(100)
	// event=error is surfaced in SSE error streams
	m.SSEEvents.WithLabelValues("error").Inc()
	// event=other captures every other event: name the provider emits
	m.SSEEvents.WithLabelValues("other").Add(2)

	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range fams {
		if mf.GetName() == "blind_llm_eyes_sse_events_total" {
			found := map[string]float64{}
			for _, metric := range mf.GetMetric() {
				ev := getLabelValue(metric.GetLabel(), "event")
				found[ev] = metric.GetCounter().GetValue()
			}
			if found["message"] != 100 {
				t.Errorf("sse_events{event=message}: want 100, got %v", found["message"])
			}
			if found["error"] != 1 {
				t.Errorf("sse_events{event=error}: want 1, got %v", found["error"])
			}
			if found["other"] != 2 {
				t.Errorf("sse_events{event=other}: want 2, got %v", found["other"])
			}
			return
		}
	}
	t.Fatalf("metric blind_llm_eyes_sse_events_total not registered")
}
