package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen            string      `yaml:"listen"`
	Upstream          UpstreamCfg `yaml:"upstream"`
	Vision            VisionCfg   `yaml:"vision"`
	Cache             CacheCfg    `yaml:"cache"`
	FailOpen          bool        `yaml:"fail_open"`
	LogLevel          string      `yaml:"log_level"` // debug|info|warn|error
	ConcurrencyLimit  int         `yaml:"concurrency_limit"` // 单请求内并发 vision 调用上限
}

type UpstreamCfg struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"` // 可选：如果填了就用这个 key 转发，不依赖客户端传来的 Authorization
}

type VisionCfg struct {
	BaseURL            string        `yaml:"base_url"`
	APIKey             string        `yaml:"api_key"`
	Model              string        `yaml:"model"`
	TimeoutStr         string        `yaml:"timeout"`
	Timeout            time.Duration `yaml:"-"`
	LargeTimeoutStr    string        `yaml:"large_image_timeout"`
	LargeTimeout       time.Duration `yaml:"-"`
	LargeImageThreshold int64       `yaml:"large_image_threshold"` // bytes; images >= this use large timeout
	DescriptionCap     int           `yaml:"description_cap"`
	SupportedFormats   []string      `yaml:"supported_formats"`
}

type CacheCfg struct {
	MaxEntries int `yaml:"max_entries"`
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
	if c.Cache.MaxEntries <= 0 {
		c.Cache.MaxEntries = 500
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.ConcurrencyLimit <= 0 {
		c.ConcurrencyLimit = 4
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
	if c.Upstream.BaseURL == "" || c.Vision.BaseURL == "" {
		return nil, fmt.Errorf("upstream.base_url and vision.base_url are required")
	}
	return &c, nil
}
