package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen              string                 `yaml:"listen"`
	Upstream            UpstreamCfg            `yaml:"upstream"`
	Vision              VisionCfg              `yaml:"vision"`
	VisionProviders     []ProviderCfg          `yaml:"vision_providers"` // 多 provider 池配置；非空时覆盖 vision 字段
	Cache               CacheCfg               `yaml:"cache"`
	FailOpen            bool                   `yaml:"fail_open"`
	LogLevel            string                 `yaml:"log_level"`         // debug|info|warn|error
	ConcurrencyLimit    int                    `yaml:"concurrency_limit"` // 单请求内并发 vision 调用上限
	MaxBodyBytes        int64                  `yaml:"max_body_bytes"`    // 请求体大小上限（字节），默认 20MB
	AdaptiveConcurrency AdaptiveConcurrencyCfg `yaml:"adaptive_concurrency"`
}

type AdaptiveConcurrencyCfg struct {
	Enabled         bool    `yaml:"enabled"`
	MinLimit        int     `yaml:"min_limit"`
	MaxLimit        int     `yaml:"max_limit"`
	FastThresholdMs int     `yaml:"fast_threshold_ms"`
	SlowThresholdMs int     `yaml:"slow_threshold_ms"`
	SampleWindow    int     `yaml:"sample_window"`
	CooldownMs      int     `yaml:"cooldown_ms"`
	IncreaseStep    int     `yaml:"increase_step"`
	DecreaseRatio   float64 `yaml:"decrease_ratio"`  // 0.0~1.0，乘以 limit
	ErrorThreshold  float64 `yaml:"error_threshold"` // 0.0~1.0
}

type UpstreamCfg struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"` // 可选：如果填了就用这个 key 转发，不依赖客户端传来的 Authorization
}

type VisionCfg struct {
	BaseURL             string        `yaml:"base_url"`
	APIKey              string        `yaml:"api_key"`
	Model               string        `yaml:"model"`
	TimeoutStr          string        `yaml:"timeout"`
	Timeout             time.Duration `yaml:"-"`
	LargeTimeoutStr     string        `yaml:"large_image_timeout"`
	LargeTimeout        time.Duration `yaml:"-"`
	LargeImageThreshold int64         `yaml:"large_image_threshold"` // bytes; images >= this use large timeout
	DescriptionCap      int           `yaml:"description_cap"`
	SupportedFormats    []string      `yaml:"supported_formats"`
	ContextRounds       int           `yaml:"context_rounds"`    // 最近 N 轮对话传给 vision；0 = 禁用上下文感知
	ContextMaxChars     int           `yaml:"context_max_chars"` // 上下文文本最大字符数；超出时整轮次截断早期历史
}

type CacheCfg struct {
	MaxEntries int `yaml:"max_entries"`
}

// ProviderCfg 是 vision_providers 列表中单个 provider 的配置。
type ProviderCfg struct {
	Name                string            `yaml:"name"`
	Type                string            `yaml:"type"` // "mimo" | "openai_compatible"
	Priority            int               `yaml:"priority"`
	BaseURL             string            `yaml:"base_url"`
	APIKey              string            `yaml:"api_key"`
	Model               string            `yaml:"model"`
	TimeoutStr          string            `yaml:"timeout"`
	Timeout             time.Duration     `yaml:"-"`
	LargeTimeoutStr     string            `yaml:"large_image_timeout"`
	LargeTimeout        time.Duration     `yaml:"-"`
	LargeImageThreshold int64             `yaml:"large_image_threshold"`
	DescriptionCap      int               `yaml:"description_cap"`
	SupportedFormats    []string          `yaml:"supported_formats"`
	CircuitBreaker      CircuitBreakerCfg `yaml:"circuit_breaker"`
}

// CircuitBreakerCfg 是单个 provider 的熔断器配置。
type CircuitBreakerCfg struct {
	FailureThreshold int           `yaml:"failure_threshold"`
	ResetTimeoutStr  string        `yaml:"reset_timeout"`
	ResetTimeout     time.Duration `yaml:"-"`
}

// Load 从路径加载 YAML；env 覆盖对应字段（BLIND_ 前缀）。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8790"
	}
	if c.Vision.TimeoutStr == "" {
		c.Vision.TimeoutStr = "30s"
	}
	d, err := time.ParseDuration(c.Vision.TimeoutStr)
	if err != nil {
		return nil, fmt.Errorf("vision.timeout: %w", err)
	}
	c.Vision.Timeout = d

	if c.Vision.LargeTimeoutStr == "" {
		c.Vision.LargeTimeoutStr = "120s"
	}
	ld, err := time.ParseDuration(c.Vision.LargeTimeoutStr)
	if err != nil {
		return nil, fmt.Errorf("vision.large_image_timeout: %w", err)
	}
	c.Vision.LargeTimeout = ld

	if c.Vision.LargeImageThreshold <= 0 {
		c.Vision.LargeImageThreshold = 1_000_000 // 1MB default
	}
	if len(c.Vision.SupportedFormats) == 0 {
		c.Vision.SupportedFormats = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}
	}
	if c.Vision.DescriptionCap <= 0 {
		c.Vision.DescriptionCap = 1000
	}
	// ContextRounds 处理：
	//   - 用户写正数：实际传入的对话轮数
	//   - 未设置（yaml 无此字段，Go 零值 0）：兜底默认 3 轮
	//   - 用户写 0：按上述处理也是兜底 3 轮，无法区分未设置和显式 0
	//   - 用户写负数（推荐 -1）：handler 层视为禁用
	if c.Vision.ContextRounds == 0 {
		c.Vision.ContextRounds = 3
	}
	if c.Vision.ContextMaxChars <= 0 {
		c.Vision.ContextMaxChars = 2000
	}
	if c.Cache.MaxEntries <= 0 {
		c.Cache.MaxEntries = 500
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.ConcurrencyLimit <= 0 {
		c.ConcurrencyLimit = 4
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 20 << 20 // 20MB
	}
	if v := os.Getenv("BLIND_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("BLIND_VISION_API_KEY"); v != "" {
		c.Vision.APIKey = v
	}
	if v := os.Getenv("BLIND_UPSTREAM_BASE_URL"); v != "" {
		c.Upstream.BaseURL = v
	}
	if v := os.Getenv("BLIND_UPSTREAM_API_KEY"); v != "" {
		c.Upstream.APIKey = v
	}
	if v := os.Getenv("BLIND_VISION_CONTEXT_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Vision.ContextRounds = n
		}
	}
	if v := os.Getenv("BLIND_VISION_CONTEXT_MAX_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Vision.ContextMaxChars = n
		}
	}

	// ── adaptive_concurrency 默认值（即使用户只写 enabled: true 也能跑） ──
	if c.AdaptiveConcurrency.MinLimit <= 0 {
		c.AdaptiveConcurrency.MinLimit = 1
	}
	if c.AdaptiveConcurrency.MaxLimit <= 0 {
		c.AdaptiveConcurrency.MaxLimit = 12
	}
	if c.AdaptiveConcurrency.FastThresholdMs <= 0 {
		c.AdaptiveConcurrency.FastThresholdMs = 8000
	}
	if c.AdaptiveConcurrency.SlowThresholdMs <= 0 {
		c.AdaptiveConcurrency.SlowThresholdMs = 15000
	}
	if c.AdaptiveConcurrency.SampleWindow <= 0 {
		c.AdaptiveConcurrency.SampleWindow = 10
	}
	if c.AdaptiveConcurrency.CooldownMs <= 0 {
		c.AdaptiveConcurrency.CooldownMs = 2000
	}
	if c.AdaptiveConcurrency.IncreaseStep <= 0 {
		c.AdaptiveConcurrency.IncreaseStep = 1
	}
	if c.AdaptiveConcurrency.DecreaseRatio <= 0 || c.AdaptiveConcurrency.DecreaseRatio >= 1 {
		c.AdaptiveConcurrency.DecreaseRatio = 0.75
	}
	if c.AdaptiveConcurrency.ErrorThreshold <= 0 || c.AdaptiveConcurrency.ErrorThreshold >= 1 {
		c.AdaptiveConcurrency.ErrorThreshold = 0.10
	}
	// 参数合理性校验（防止用户配错）
	if c.AdaptiveConcurrency.MinLimit > c.AdaptiveConcurrency.MaxLimit {
		return nil, fmt.Errorf("adaptive_concurrency: min_limit(%d) > max_limit(%d)",
			c.AdaptiveConcurrency.MinLimit, c.AdaptiveConcurrency.MaxLimit)
	}
	if c.AdaptiveConcurrency.FastThresholdMs >= c.AdaptiveConcurrency.SlowThresholdMs {
		return nil, fmt.Errorf("adaptive_concurrency: fast_threshold_ms(%d) must be < slow_threshold_ms(%d)",
			c.AdaptiveConcurrency.FastThresholdMs, c.AdaptiveConcurrency.SlowThresholdMs)
	}

	// ── vision_providers 多 provider 配置解析与默认值 ──
	if len(c.VisionProviders) > 0 {
		names := make(map[string]bool, len(c.VisionProviders))
		for i := range c.VisionProviders {
			p := &c.VisionProviders[i]

			// 必填字段校验
			if p.Name == "" {
				return nil, fmt.Errorf("vision_providers[%d]: name is required", i)
			}
			if names[p.Name] {
				return nil, fmt.Errorf("vision_providers[%d]: duplicate name %q", i, p.Name)
			}
			names[p.Name] = true

			if p.Type == "" {
				p.Type = "mimo"
			}
			if p.Type != "mimo" && p.Type != "openai_compatible" {
				return nil, fmt.Errorf("vision_providers[%d] %q: type must be \"mimo\" or \"openai_compatible\", got %q",
					i, p.Name, p.Type)
			}
			if p.BaseURL == "" {
				return nil, fmt.Errorf("vision_providers[%d] %q: base_url is required", i, p.Name)
			}
			if p.APIKey == "" {
				return nil, fmt.Errorf("vision_providers[%d] %q: api_key is required", i, p.Name)
			}
			if p.Model == "" {
				return nil, fmt.Errorf("vision_providers[%d] %q: model is required", i, p.Name)
			}

			// 默认值（与 VisionCfg 一致）
			if p.TimeoutStr == "" {
				p.TimeoutStr = "60s"
			}
			d, err := time.ParseDuration(p.TimeoutStr)
			if err != nil {
				return nil, fmt.Errorf("vision_providers[%d] %q: timeout: %w", i, p.Name, err)
			}
			p.Timeout = d

			if p.LargeTimeoutStr == "" {
				p.LargeTimeoutStr = "120s"
			}
			ld, err := time.ParseDuration(p.LargeTimeoutStr)
			if err != nil {
				return nil, fmt.Errorf("vision_providers[%d] %q: large_image_timeout: %w", i, p.Name, err)
			}
			p.LargeTimeout = ld

			if p.LargeImageThreshold <= 0 {
				p.LargeImageThreshold = 1_000_000
			}
			if p.DescriptionCap <= 0 {
				p.DescriptionCap = 1000
			}
			if len(p.SupportedFormats) == 0 {
				p.SupportedFormats = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}
			}

			// 熔断器默认值
			if p.CircuitBreaker.FailureThreshold <= 0 {
				p.CircuitBreaker.FailureThreshold = 5
			}
			if p.CircuitBreaker.ResetTimeoutStr == "" {
				p.CircuitBreaker.ResetTimeoutStr = "30s"
			}
			rd, err := time.ParseDuration(p.CircuitBreaker.ResetTimeoutStr)
			if err != nil {
				return nil, fmt.Errorf("vision_providers[%d] %q: circuit_breaker.reset_timeout: %w", i, p.Name, err)
			}
			p.CircuitBreaker.ResetTimeout = rd
		}
	}

	// upstream.base_url 始终必填；vision 配置在多 provider 模式下不要求
	if c.Upstream.BaseURL == "" {
		return nil, fmt.Errorf("upstream.base_url is required")
	}
	if len(c.VisionProviders) == 0 && c.Vision.BaseURL == "" {
		return nil, fmt.Errorf("vision.base_url or vision_providers is required")
	}
	return &c, nil
}
