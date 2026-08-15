package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfigYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

func TestLoad_CacheDefaults(t *testing.T) {
	path := writeConfigYAML(t, "upstream: {base_url: \"http://x\"}\nvision: {base_url: \"http://v\", api_key: \"k\", model: \"m\"}\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.Type != "lru" {
		t.Fatalf("default type want lru, got %q", cfg.Cache.Type)
	}
	if cfg.Cache.SqliteMaxEntries != 10000 {
		t.Fatalf("sqlite_max default want 10000, got %d", cfg.Cache.SqliteMaxEntries)
	}
	if cfg.Cache.SqliteTTL != 0 {
		t.Fatalf("ttl default want 0, got %v", cfg.Cache.SqliteTTL)
	}
}

func TestLoad_CacheTwoTier(t *testing.T) {
	path := writeConfigYAML(t, "upstream: {base_url: \"http://x\"}\nvision: {base_url: \"http://v\", api_key: \"k\", model: \"m\"}\ncache:\n  type: twotier\n  sqlite_max_entries: 5000\n  sqlite_ttl: \"24h\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.Type != "twotier" {
		t.Fatalf("type want twotier, got %q", cfg.Cache.Type)
	}
	if cfg.Cache.SqliteMaxEntries != 5000 {
		t.Fatalf("max want 5000, got %d", cfg.Cache.SqliteMaxEntries)
	}
	if cfg.Cache.SqliteTTL != 24*time.Hour {
		t.Fatalf("ttl want 24h, got %v", cfg.Cache.SqliteTTL)
	}
}

func TestLoad_CacheBadType(t *testing.T) {
	path := writeConfigYAML(t, "upstream: {base_url: \"http://x\"}\nvision: {base_url: \"http://v\", api_key: \"k\", model: \"m\"}\ncache: {type: \"bogus\"}\n")
	if _, err := Load(path); err == nil {
		t.Fatal("want error for bad cache.type")
	}
}
