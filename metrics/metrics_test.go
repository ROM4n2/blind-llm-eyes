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
