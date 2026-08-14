package vision

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/config"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
)

func testBuildLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validProviderCfg(name, ptype string) config.ProviderCfg {
	return config.ProviderCfg{
		Name:     name,
		Type:     ptype,
		Priority: 1,
		BaseURL:  "https://api.example.com/anthropic",
		APIKey:   "k",
		Model:    "mimo-v2.5",
		Timeout:  30 * time.Second, LargeTimeout: 120 * time.Second,
		LargeImageThreshold: 1_000_000, DescriptionCap: 1000,
		SupportedFormats: []string{"image/png"},
		CircuitBreaker:    config.CircuitBreakerCfg{FailureThreshold: 5, ResetTimeout: 30 * time.Second},
	}
}

func TestBuildProvider_Mimo(t *testing.T) {
	p, err := BuildProvider(validProviderCfg("mimo", "mimo"), testBuildLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*Client); !ok {
		t.Fatalf("expected *Client, got %T", p)
	}
}

func TestBuildProvider_OpenAICompatible(t *testing.T) {
	p, err := BuildProvider(validProviderCfg("oai", "openai_compatible"), testBuildLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*OpenAIClient); !ok {
		t.Fatalf("expected *OpenAIClient, got %T", p)
	}
}

func TestBuildProvider_TwiceNoPanic(t *testing.T) {
	pc := validProviderCfg("m", "mimo")
	p1, err1 := BuildProvider(pc, testBuildLogger())
	p2, err2 := BuildProvider(pc, testBuildLogger())
	if err1 != nil || err2 != nil || p1 == nil || p2 == nil {
		t.Fatalf("expected two successful builds, got err1=%v err2=%v p1=%v p2=%v", err1, err2, p1, p2)
	}
	if p1 == p2 {
		t.Fatal("expected distinct provider instances")
	}
}

func TestBuildProvider_EmptyFields(t *testing.T) {
	cases := []struct {
		name string
		pc   config.ProviderCfg
	}{
		{"empty base_url", config.ProviderCfg{Name: "n", Type: "mimo", APIKey: "k", Model: "m"}},
		{"empty api_key", config.ProviderCfg{Name: "n", Type: "mimo", BaseURL: "u", Model: "m"}},
		{"empty model", config.ProviderCfg{Name: "n", Type: "mimo", BaseURL: "u", APIKey: "k"}},
	}
	for _, c := range cases {
		if _, err := BuildProvider(c.pc, testBuildLogger()); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

func TestBuildProvider_UnknownType(t *testing.T) {
	pc := validProviderCfg("n", "bogus")
	if _, err := BuildProvider(pc, testBuildLogger()); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestBuildProvider_GLMFree(t *testing.T) {
	// glm_free with only api_key: base_url and model should be auto-filled.
	pc := config.ProviderCfg{
		Name:     "glm-free",
		Type:     "glm_free",
		Priority: 1,
		APIKey:   "free-glm-key",
		Timeout:  30 * time.Second, LargeTimeout: 120 * time.Second,
		LargeImageThreshold: 1_000_000, DescriptionCap: 1000,
		SupportedFormats: []string{"image/png"},
		CircuitBreaker:    config.CircuitBreakerCfg{FailureThreshold: 5, ResetTimeout: 30 * time.Second},
	}
	p, err := BuildProvider(pc, testBuildLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	client, ok := p.(*OpenAIClient)
	if !ok {
		t.Fatalf("expected *OpenAIClient, got %T", p)
	}
	if client.BaseURL != GLMFreeBaseURL {
		t.Errorf("base_url: want %q, got %q", GLMFreeBaseURL, client.BaseURL)
	}
	if client.Model != GLMFreeModel {
		t.Errorf("model: want %q, got %q", GLMFreeModel, client.Model)
	}
}

func TestBuildProvider_GLMFree_OverrideDefaults(t *testing.T) {
	// User can override the auto-filled base_url and model.
	pc := config.ProviderCfg{
		Name:     "glm-custom",
		Type:     "glm_free",
		Priority: 1,
		BaseURL:  "https://custom.example.com/v4",
		APIKey:   "k",
		Model:    "glm-4v-plus",
		Timeout:  30 * time.Second, LargeTimeout: 120 * time.Second,
		LargeImageThreshold: 1_000_000, DescriptionCap: 1000,
		SupportedFormats: []string{"image/png"},
		CircuitBreaker:    config.CircuitBreakerCfg{FailureThreshold: 5, ResetTimeout: 30 * time.Second},
	}
	p, err := BuildProvider(pc, testBuildLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	client := p.(*OpenAIClient)
	if client.BaseURL != "https://custom.example.com/v4" {
		t.Errorf("base_url override: want custom, got %q", client.BaseURL)
	}
	if client.Model != "glm-4v-plus" {
		t.Errorf("model override: want glm-4v-plus, got %q", client.Model)
	}
}

func TestBuildProvider_GLMFree_EmptyAPIKey(t *testing.T) {
	// api_key is still required for glm_free.
	pc := config.ProviderCfg{
		Name:    "glm-no-key",
		Type:    "glm_free",
		BaseURL: "https://open.bigmodel.cn/api/paas/v4",
		Model:   "glm-4v-flash",
	}
	if _, err := BuildProvider(pc, testBuildLogger()); err == nil {
		t.Fatal("expected error for empty api_key on glm_free")
	}
}

func TestBuildSingleProvider(t *testing.T) {
	vc := config.VisionCfg{
		BaseURL: "https://api.example.com/anthropic", APIKey: "k", Model: "mimo-v2.5",
		Timeout: 30 * time.Second, LargeTimeout: 120 * time.Second,
		LargeImageThreshold: 1_000_000, DescriptionCap: 1000,
		SupportedFormats:    []string{"image/png"},
	}
	p, err := BuildSingleProvider(vc, testBuildLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*Client); !ok {
		t.Fatalf("expected *Client, got %T", p)
	}
}

func TestBuildSingleProvider_EmptyFields(t *testing.T) {
	if _, err := BuildSingleProvider(config.VisionCfg{APIKey: "k", Model: "m"}, testBuildLogger()); err == nil {
		t.Error("expected error for empty base_url")
	}
	if _, err := BuildSingleProvider(config.VisionCfg{BaseURL: "u", Model: "m"}, testBuildLogger()); err == nil {
		t.Error("expected error for empty api_key")
	}
	if _, err := BuildSingleProvider(config.VisionCfg{BaseURL: "u", APIKey: "k"}, testBuildLogger()); err == nil {
		t.Error("expected error for empty model")
	}
}

func TestBuildPool(t *testing.T) {
	providers := []config.ProviderCfg{
		validProviderCfg("mimo", "mimo"),
		validProviderCfg("oai", "openai_compatible"),
	}
	m := metrics.NewMetrics()
	pool, err := BuildPool(providers, testBuildLogger(), m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(pool.ProviderNames()); got != 2 {
		t.Fatalf("expected 2 providers, got %d", got)
	}
}

func TestBuildPool_Empty(t *testing.T) {
	if _, err := BuildPool(nil, testBuildLogger(), metrics.NewMetrics()); err == nil {
		t.Fatal("expected error for empty providers")
	}
}
