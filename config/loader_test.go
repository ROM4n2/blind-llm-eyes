package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLoad_ExampleYAML_Defaults(t *testing.T) {
	// Copy example.yaml → temp dir; ensure Load() succeeds without errors
	// and that the new v1.3.0 defaults (debug_pprof_enabled=true, context_rounds=3)
	// kick in for an unmodified config template.
	data, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Skipf("cannot read ../config.example.yaml (skipping; likely running from wrong cwd): %v", err)
	}
	tmp := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatalf("write tmp yaml: %v", err)
	}
	cfg, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load(config.example.yaml) err: %v", err)
	}
	// Defaults from *int / *bool pointer nil-modes
	if cfg.Vision.ContextRounds == nil || *cfg.Vision.ContextRounds != 3 {
		t.Errorf("context_rounds default: want *3, got %v", cfg.Vision.ContextRounds)
	}
	if cfg.DebugPprofEnabled == nil || *cfg.DebugPprofEnabled != true {
		t.Errorf("debug_pprof_enabled default: want *true, got %v", cfg.DebugPprofEnabled)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level: want info, got %s", cfg.LogLevel)
	}
}

func TestReloadableConfig_LoadAfterSwap(t *testing.T) {
	cfg1 := &Config{Listen: "127.0.0.1:8790", LogLevel: "info"}
	rcfg := NewReloadableConfig(cfg1, "config.yaml")
	if got := rcfg.Load(); got.Listen != cfg1.Listen {
		t.Fatalf("initial load mismatch: %v != %v", got.Listen, cfg1.Listen)
	}
	cfg2 := &Config{Listen: "127.0.0.1:8790", LogLevel: "debug"} // same non-reloadable fields
	_, _, err := rcfg.TestReloadFromConfig(cfg2) // test-internal helper bypasses yaml
	if err != nil {
		t.Fatalf("reload err: %v", err)
	}
	if got := rcfg.Load(); got.LogLevel != "debug" {
		t.Fatalf("after reload want debug, got %v", got.LogLevel)
	}
}

func TestReloadableConfig_ConcurrentLoadNoRace(t *testing.T) {
	rcfg := NewReloadableConfig(&Config{Listen: "127.0.0.1:8790"}, "config.yaml")
	// Atomic swap every ms, 100 goroutines doing Load
	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					_ = rcfg.Load()
					runtime.Gosched() // yield to prevent scheduler starvation
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		time.Sleep(time.Millisecond)
		_, _, _ = rcfg.TestReloadFromConfig(&Config{Listen: "127.0.0.1:8790", LogLevel: "info"})
		_, _, _ = rcfg.TestReloadFromConfig(&Config{Listen: "127.0.0.1:8790", LogLevel: "warn"})
	}
	close(done)
	// If we reach here with the -race detector clean, it passes.
}

func TestReloadableConfig_ReloadRollbackOnInvalidField(t *testing.T) {
	cfg1 := &Config{Listen: "127.0.0.1:8790", LogLevel: "info"}
	rcfg := NewReloadableConfig(cfg1, "config.yaml")
	bad := &Config{Listen: ""} // invalid: validate() must reject empty listen
	_, _, err := rcfg.TestReloadFromConfig(bad)
	if err == nil {
		t.Fatalf("expected validation error for empty listen")
	}
	// Old config kept — LogLevel must stay "info"
	if rcfg.Load().LogLevel != "info" {
		t.Fatalf("rollback failed — cfg mutated despite validation err")
	}
}

func TestReloadableConfig_VersionFingerprintChanges(t *testing.T) {
	cfg1 := &Config{Listen: "127.0.0.1:8790", LogLevel: "info"}
	cfg2 := &Config{Listen: "127.0.0.1:8790", LogLevel: "debug"}
	if cfg1.VersionFingerprint() == cfg2.VersionFingerprint() {
		t.Fatalf("different configs must have different fingerprints")
	}
}

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
