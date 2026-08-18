package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// VisionCapableModels lists upstream model names that natively support
	// image input (e.g. gpt-4o, claude-3-5-sonnet). When the request model
	// matches this list (case-insensitive, after sanitization), the proxy
	// skips image rewriting and forwards the body verbatim. Empty = always
	// rewrite (default).
	VisionCapableModels []string `yaml:"vision_capable_models"`
	// MetricsAuthToken, if set, requires /metrics requests to carry this token
	// via the "token" query parameter or "X-Metrics-Token" header. Empty = no auth.
	MetricsAuthToken string `yaml:"metrics_auth_token"`
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
	ContextRounds       *int          `yaml:"context_rounds"`    // nil = 默认 3 轮; 0 = 禁用; 正数 = N 轮
	ContextMaxChars     int           `yaml:"context_max_chars"` // 上下文文本最大字符数；超出时整轮次截断早期历史
}

type CacheCfg struct {
	MaxEntries       int           `yaml:"max_entries"`        // LRU hot-layer capacity, default 500
	Type             string        `yaml:"type"`               // "lru" (default/empty) | "twotier"
	DBPath           string        `yaml:"db_path"`            // SQLite path; empty + type=twotier -> default ./cache.db
	SqliteMaxEntries int           `yaml:"sqlite_max_entries"` // SQLite capacity cap, default 10000 (<=0 -> 10000)
	SqliteTTLStr     string        `yaml:"sqlite_ttl"`         // duration like "720h"; empty/0 = no TTL eviction
	SqliteTTL        time.Duration `yaml:"-"`
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
	//   - nil（yaml 无此字段）：兜底默认 3 轮
	//   - 0：显式禁用上下文感知
	//   - 正数：实际传入的对话轮数
	//   - 负数：handler 层规范化为 0（禁用）
	if c.Vision.ContextRounds == nil {
		defaultRounds := 3
		c.Vision.ContextRounds = &defaultRounds
	}
	if c.Vision.ContextMaxChars <= 0 {
		c.Vision.ContextMaxChars = 2000
	}
	if c.Cache.MaxEntries <= 0 {
		c.Cache.MaxEntries = 500
	}
	if c.Cache.Type == "" {
		c.Cache.Type = "lru"
	}
	if c.Cache.Type != "lru" && c.Cache.Type != "twotier" {
		return nil, fmt.Errorf("cache.type: must be \"lru\" or \"twotier\", got %q", c.Cache.Type)
	}
	if c.Cache.SqliteMaxEntries <= 0 {
		c.Cache.SqliteMaxEntries = 10000
	}
	if c.Cache.SqliteTTLStr != "" {
		d, err := time.ParseDuration(c.Cache.SqliteTTLStr)
		if err != nil {
			return nil, fmt.Errorf("cache.sqlite_ttl: %w", err)
		}
		c.Cache.SqliteTTL = d
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
			c.Vision.ContextRounds = &n
		}
	}
	if v := os.Getenv("BLIND_VISION_CONTEXT_MAX_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Vision.ContextMaxChars = n
		}
	}
	if v := os.Getenv("BLIND_METRICS_AUTH_TOKEN"); v != "" {
		c.MetricsAuthToken = v
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
			if p.Type != "mimo" && p.Type != "openai_compatible" && p.Type != "glm_free" && p.Type != "qwen" {
				return nil, fmt.Errorf("vision_providers[%d] %q: type must be \"mimo\", \"openai_compatible\", \"glm_free\", or \"qwen\", got %q",
					i, p.Name, p.Type)
			}
			// glm_free auto-fills base_url and model with GLM-4V-Flash
			// defaults; api_key is still required (free tier key from
			// https://open.bigmodel.cn).
			if p.Type == "glm_free" {
				if p.BaseURL == "" {
					p.BaseURL = "https://open.bigmodel.cn/api/paas/v4"
				}
				if p.Model == "" {
					p.Model = "glm-4v-flash"
				}
			}
			// qwen auto-fills base_url and model with DashScope Qwen-VL
			// defaults; api_key is still required (key from
			// https://bailian.console.aliyun.com).
			if p.Type == "qwen" {
				if p.BaseURL == "" {
					p.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
				}
				if p.Model == "" {
					p.Model = "qwen-vl-plus"
				}
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
	if err := validate(&c); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	return &c, nil
}

// validate checks Config invariants that apply both at startup and reload.
// Returns nil when valid. Non-nil causes Load (startup) or Reload (runtime)
// to abort with the descriptive message.
func validate(c *Config) error {
	if strings.TrimSpace(c.Listen) == "" {
		return fmt.Errorf("config.listen: must not be empty")
	}
	if c.ConcurrencyLimit < 1 {
		c.ConcurrencyLimit = 4
	}
	return nil
}

// ReloadableConfig holds the current *Config snapshot, atomically swap-able.
// Callers MUST call Load() once per request/tick scope into a local variable,
// then read all fields off that frozen snapshot — never call Load() multiple
// times in the same logical scope because fields could straddle two versions.
type ReloadableConfig struct {
	current atomic.Pointer[Config]
	mu      sync.Mutex // serializes concurrent Reload() calls (SIGHUP vs HTTP)
	path    string     // config.yaml source path (for reload file read)
}

// NewReloadableConfig wraps the given initial Config with atomic swap semantics.
// path is informational (for Reload() to know what yaml to re-read); empty OK.
func NewReloadableConfig(cfg *Config, path string) *ReloadableConfig {
	r := &ReloadableConfig{path: path}
	r.current.Store(cfg)
	return r
}

// Load returns the *Config snapshot. Never nil after NewReloadableConfig().
// The caller MUST NOT mutate the returned struct.
func (r *ReloadableConfig) Load() *Config {
	return r.current.Load()
}

// Reload re-reads r.path as yaml, validates it, then atomically swaps to the
// new Config. Returns (prev, next, err). On any validation error the swap is
// NOT performed — prev == next == old config, err is descriptive.
// Process-wide mutex serializes overlapping reloads from SIGHUP vs HTTP.
func (r *ReloadableConfig) Reload() (prev, next *Config, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev = r.current.Load()
	next, err = Load(r.path) // reuse existing loader + validate
	if err != nil {
		return prev, prev, err // ROLLBACK: keep old
	}
	// Non-reloadable field guard (spec §2.2): refuse changes to fields that
	// require restart. This prevents silent misconfiguration.
	if prev.Listen != next.Listen {
		return prev, prev, fmt.Errorf("field 'listen' is non-reloadable (restart required)")
	}
	if prev.Upstream.BaseURL != next.Upstream.BaseURL {
		return prev, prev, fmt.Errorf("field 'upstream.base_url' is non-reloadable (restart required)")
	}
	if prev.Upstream.APIKey != next.Upstream.APIKey {
		return prev, prev, fmt.Errorf("field 'upstream.api_key' is non-reloadable (restart required)")
	}
	if prev.MetricsAuthToken != next.MetricsAuthToken {
		return prev, prev, fmt.Errorf("field 'metrics_auth_token' is non-reloadable (restart required)")
	}
	if prev.Cache.DBPath != next.Cache.DBPath {
		return prev, prev, fmt.Errorf("field 'cache.db_path' is non-reloadable (restart required)")
	}
	r.current.Store(next)
	return prev, next, nil
}

// TestReloadFromConfig skips yaml parsing and swaps directly to `next` after
// validate(). Used only in unit tests to avoid writing temp yaml files for
// every test. NOT safe for production code because it bypasses the
// non-reloadable field boundary checks — those are handled by Reload() which
// wraps Load().
func (r *ReloadableConfig) TestReloadFromConfig(next *Config) (prev, after *Config, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev = r.current.Load()
	if err = validate(next); err != nil {
		return prev, prev, err
	}
	r.current.Store(next)
	return prev, next, nil
}

// VersionFingerprint returns a short deterministic hash of the current Config
// for log diffing (e.g. "applied reload v1→v2"). Uses the yaml-marshaled form
// so any semantic change → different hash. Non-cryptographic length (16 hex).
func (c *Config) VersionFingerprint() string {
	b, err := yaml.Marshal(c)
	if err != nil {
		return "err:" + err.Error()
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}
