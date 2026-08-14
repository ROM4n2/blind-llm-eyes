package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ROM4n2/blind-llm-eyes/config"
	"gopkg.in/yaml.v3"
)

// setupDeps holds injectable dependencies for the setup wizard, enabling
// testing without real file I/O, network calls, or cc-switch databases.
type setupDeps struct {
	writeConfig  func(name, data string) error
	connectFunc  func(proxyURL string) error
	doctorFunc   func(cfg *config.Config, stdout, stderr io.Writer) int
	ccSwitchPath string
	configPath   string
}

// setupTestDeps is an alias used by tests (defined in setup_test.go).
type setupTestDeps = setupDeps

// runSetupCore drives the interactive setup wizard. It reads questions from
// stdin, writes prompts to stdout, and uses the injected dependencies for
// config writing, connecting, and doctor checks.
func runSetupCore(stdin io.Reader, stdout, stderr io.Writer, deps *setupDeps) int {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	reader := bufio.NewReader(stdin)
	prompt := func(question string) string {
		fmt.Fprint(stdout, question)
		line, _ := reader.ReadString('\n')
		return strings.TrimSpace(line)
	}
	askYesNo := func(question string, defaultYes bool) bool {
		if question == "" {
			return false
		}
		hint := "y/N"
		if defaultYes {
			hint = "Y/n"
		}
		ans := prompt(fmt.Sprintf("%s [%s]: ", question, hint))
		if ans == "" {
			return defaultYes
		}
		return strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes")
	}

	fmt.Fprintln(stdout, "=== blind-llm-eyes setup wizard ===")
	fmt.Fprintln(stdout, "")

	// ── Step 1: cc-switch import ──
	var upstreamBaseURL, upstreamAPIKey, visionBaseURL, visionAPIKey, visionModel string

	if askYesNo("Import from cc-switch?", false) && deps.ccSwitchPath != "" {
		providers, err := ImportFromCcSwitch(deps.ccSwitchPath)
		if err == nil && len(providers) > 0 {
			fmt.Fprintf(stdout, "Found %d Claude Code providers in cc-switch:\n", len(providers))
			for i, p := range providers {
				fmt.Fprintf(stdout, "  %d. %s (base_url=%s, model=%s)\n", i+1, p.Name, p.BaseURL, p.Model)
			}
			fmt.Fprintln(stdout, "")
			upIdx := prompt("Select upstream provider number (or Enter to type manually): ")
			if upIdx != "" {
				if idx := atoiSafe(upIdx); idx > 0 && idx <= len(providers) {
					p := providers[idx-1]
					upstreamBaseURL = p.BaseURL
					upstreamAPIKey = p.APIKey
				}
			}
			visIdx := prompt("Select vision provider number (or Enter to type manually): ")
			if visIdx != "" {
				if idx := atoiSafe(visIdx); idx > 0 && idx <= len(providers) {
					p := providers[idx-1]
					visionBaseURL = p.BaseURL
					visionAPIKey = p.APIKey
					visionModel = p.Model
				}
			}
		} else if err != nil {
			fmt.Fprintf(stdout, "cc-switch import skipped: %v\n", err)
		}
	}

	// ── Step 2: Manual input with defaults ──
	if upstreamBaseURL == "" {
		upstreamBaseURL = prompt("Upstream base URL [https://api.deepseek.com/anthropic]: ")
		if upstreamBaseURL == "" {
			upstreamBaseURL = "https://api.deepseek.com/anthropic"
		}
	}
	if upstreamAPIKey == "" {
		upstreamAPIKey = prompt("Upstream API key: ")
	}

	// ── Step 2a: Free GLM-4V-Flash preset ──
	// Offer the free default before asking for manual vision config. If the
	// user accepts, we fill in the GLM-4V-Flash base URL + model and only
	// ask for the (free) API key from https://open.bigmodel.cn.
	if visionBaseURL == "" {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Vision provider options:")
		fmt.Fprintln(stdout, "  1. GLM-4V-Flash (FREE — zero cost, get a key at https://open.bigmodel.cn)")
		fmt.Fprintln(stdout, "  2. MiMo / other Anthropic-compatible (manual)")
		fmt.Fprintln(stdout, "  3. OpenAI-compatible (manual)")
		choice := prompt("Choose vision provider [1/2/3, default=1]: ")
		if choice == "" || choice == "1" {
			visionBaseURL = "https://open.bigmodel.cn/api/paas/v4"
			visionModel = "glm-4v-flash"
			fmt.Fprintln(stdout, "")
			fmt.Fprintln(stdout, "Get a FREE API key from: https://open.bigmodel.cn")
			fmt.Fprintln(stdout, "(Register → API Keys → create. The free tier covers GLM-4V-Flash at no cost.)")
			visionAPIKey = prompt("GLM API key: ")
		}
	}

	if visionBaseURL == "" {
		visionBaseURL = prompt("Vision base URL [https://api.xiaomimimo.com/anthropic]: ")
		if visionBaseURL == "" {
			visionBaseURL = "https://api.xiaomimimo.com/anthropic"
		}
	}
	if visionAPIKey == "" {
		visionAPIKey = prompt("Vision API key: ")
	}
	if visionModel == "" {
		visionModel = prompt("Vision model [mimo-v2.5]: ")
		if visionModel == "" {
			visionModel = "mimo-v2.5"
		}
	}

	// ── Step 3: Doctor self-check ──
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Running connectivity self-check (doctor)...")
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: upstreamBaseURL, APIKey: upstreamAPIKey},
		Vision: config.VisionCfg{
			BaseURL: visionBaseURL,
			APIKey:  visionAPIKey,
			Model:   visionModel,
		},
	}

	doctorCode := 1
	if deps.doctorFunc != nil {
		doctorCode = deps.doctorFunc(cfg, stdout, stderr)
	} else {
		doctorCode = runDoctorCore(context.Background(), cfg, "deepseek-chat", stdout, stderr)
	}

	if doctorCode != 0 {
		fmt.Fprintln(stdout, "")
		if !askYesNo("One or more checks failed. Save config anyway?", false) {
			fmt.Fprintln(stdout, "Setup cancelled. Config not saved.")
			return 1
		}
	}

	// ── Step 4: Generate config YAML ──
	configYAML := generateConfigYAML(cfg)
	configPath := deps.configPath
	if configPath == "" {
		configPath = "config.yaml"
	}

	if err := deps.writeConfig(configPath, configYAML); err != nil {
		fmt.Fprintf(stderr, "write config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Config written to %s\n", configPath)

	// ── Step 5: Optional connect ──
	if askYesNo("Connect Claude Code to this proxy now?", false) {
		proxyURL := "http://" + cfg.Listen
		if deps.connectFunc != nil {
			if err := deps.connectFunc(proxyURL); err != nil {
				fmt.Fprintf(stderr, "connect: %v\n", err)
			}
		} else {
			sp, _ := defaultSettingsPath()
			bp, _ := defaultBackupPath()
			if err := connectSettings(sp, bp, proxyURL); err != nil {
				fmt.Fprintf(stderr, "connect: %v\n", err)
			} else {
				fmt.Fprintln(stdout, "Claude Code connected.")
			}
		}
	}

	// ── Step 6: Startup instructions ──
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "=== Setup complete ===")
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "To start the proxy:")
	fmt.Fprintln(stdout, "  blind-llm-eyes start")
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Or just run:")
	fmt.Fprintln(stdout, "  blind-llm-eyes")
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "The proxy will listen on http://"+cfg.Listen)
	fmt.Fprintln(stdout, "Claude Code should use ANTHROPIC_BASE_URL=http://"+cfg.Listen)

	return 0
}

// generateConfigYAML produces the YAML config string from a Config struct.
func generateConfigYAML(cfg *config.Config) string {
	out := map[string]any{
		"listen": cfg.Listen,
		"upstream": map[string]any{
			"base_url": cfg.Upstream.BaseURL,
			"api_key":  cfg.Upstream.APIKey,
		},
		"vision": map[string]any{
			"base_url": cfg.Vision.BaseURL,
			"api_key":  cfg.Vision.APIKey,
			"model":    cfg.Vision.Model,
		},
		"log_level": "info",
	}
	b, _ := yaml.Marshal(out)
	return string(b)
}

// atoiSafe parses an integer, returning 0 on error.
func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// runSetup is the subcommand entry point for `setup`.
func runSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ccSwitchPath := fs.String("cc-switch-db", "", "path to cc-switch.db (default: auto-detect)")
	configPath := fs.String("config", "config.yaml", "output config file path")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dbPath := *ccSwitchPath
	if dbPath == "" {
		if p, err := defaultCcSwitchDBPath(); err == nil {
			if _, err := os.Stat(p); err == nil {
				dbPath = p
			}
		}
	}

	deps := &setupDeps{
		writeConfig:  func(name, data string) error { return os.WriteFile(name, []byte(data), 0644) },
		ccSwitchPath: dbPath,
		configPath:   *configPath,
	}

	return runSetupCore(stdin, stdout, stderr, deps)
}
