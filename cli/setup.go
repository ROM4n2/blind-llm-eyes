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

// defaultListen is the proxy's default listen address, used as the single
// source of truth for self-referential URL detection in the setup wizard.
const defaultListen = "127.0.0.1:8790"

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
		allProviders, err := ImportFromCcSwitch(deps.ccSwitchPath, "")
		if err == nil && len(allProviders) > 0 {
			var providers []CcSwitchProvider
			var filteredCount int
			for _, p := range allProviders {
				if IsSelfReferentialURL(p.BaseURL, defaultListen) {
					filteredCount++
					continue
				}
				providers = append(providers, p)
			}
			fmt.Fprintf(stdout, "Found %d Claude Code providers in cc-switch", len(providers))
			if filteredCount > 0 {
				fmt.Fprintf(stdout, " (%d self-referential filtered out)", filteredCount)
			}
			fmt.Fprintln(stdout, ":")
			for i, p := range providers {
				fmt.Fprintf(stdout, "  %d. %s (base_url=%s, model=%s)\n", i+1, p.Name, p.BaseURL, p.Model)
			}
			if len(providers) == 0 {
				fmt.Fprintln(stdout, "  (no valid providers remaining)")
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
		if IsSelfReferentialURL(upstreamBaseURL, defaultListen) {
			fmt.Fprintln(stdout, "")
			fmt.Fprintf(stderr, "WARNING: upstream base_url %q points to the proxy itself (127.0.0.1:8790).\n", upstreamBaseURL)
			fmt.Fprintf(stderr, "  This will cause an infinite self-forwarding loop.\n")
			fmt.Fprintf(stderr, "  Please use a real upstream API endpoint instead.\n")
			upstreamBaseURL = prompt("Enter a valid upstream base URL: ")
			if upstreamBaseURL == "" || IsSelfReferentialURL(upstreamBaseURL, defaultListen) {
				fmt.Fprintln(stderr, "Invalid upstream URL. Setup cancelled.")
				return 1
			}
		}
	}
	if upstreamAPIKey == "" {
		upstreamAPIKey = prompt("Upstream API key: ")
	}

	// ── Step 2a: Vision provider preset (GLM/Qwen) or manual ──
	// presetType 非空表示用户选了预设（GLM/Qwen），将写出 vision_providers+type，
	// 走 BuildProvider -> OpenAIClient。为空表示手动，走 vision: 单块 -> MiMo Client。
	var presetType string
	if visionBaseURL == "" {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Vision provider options:")
		fmt.Fprintln(stdout, "  1. GLM-4V-Flash (FREE — zero cost, https://open.bigmodel.cn)")
		fmt.Fprintln(stdout, "  2. Qwen-VL (DashScope — China first, https://bailian.console.aliyun.com)")
		fmt.Fprintln(stdout, "  3. MiMo / other Anthropic-compatible (manual)")
		fmt.Fprintln(stdout, "  4. OpenAI-compatible (manual)")
		choice := prompt("Choose vision provider [1/2/3/4, default=1]: ")
		switch {
		case choice == "" || choice == "1":
			presetType = "glm_free"
			fmt.Fprintln(stdout, "")
			fmt.Fprintln(stdout, "Get a FREE API key from: https://open.bigmodel.cn")
			fmt.Fprintln(stdout, "(Register → API Keys → create. The free tier covers GLM-4V-Flash at no cost.)")
			visionAPIKey = prompt("GLM API key: ")
		case choice == "2":
			presetType = "qwen"
			fmt.Fprintln(stdout, "")
			fmt.Fprintln(stdout, "Get an API key at: https://bailian.console.aliyun.com")
			fmt.Fprintln(stdout, "(Bailian Console → API-KEY → create. qwen-vl-plus is the general vision model.)")
			visionAPIKey = prompt("DashScope API key: ")
		case choice == "3":
			// 手动 MiMo，落 vision: 块（presetType 保持空）
		case choice == "4":
			// 手动 OpenAI 兼容，落 vision: 块但需用户手改 type；setup 简化处理
		}
	}

	// 手动模式（presetType 为空）才提示 base_url/api_key/model 输入。
	// 预设模式的 base_url/model 由 loader 默认值填，api_key 已在上方 prompt。
	if presetType == "" {
		if visionBaseURL == "" {
			visionBaseURL = prompt("Vision base URL [https://api.xiaomimimo.com/anthropic]: ")
			if visionBaseURL == "" {
				visionBaseURL = "https://api.xiaomimimo.com/anthropic"
			}
			// Self-loop detection: vision base_url points to the proxy itself.
			// MiMo Client calls /v1/messages (same as proxy path) → infinite loop.
			if IsSelfReferentialURL(visionBaseURL, defaultListen) {
				fmt.Fprintln(stdout, "")
				fmt.Fprintf(stderr, "WARNING: vision base_url %q points to the proxy itself (127.0.0.1:8790).\n", visionBaseURL)
				fmt.Fprintf(stderr, "  This will cause an infinite self-forwarding loop (vision calls /v1/messages on the proxy).\n")
				fmt.Fprintf(stderr, "  Please use a real vision API endpoint instead.\n")
				visionBaseURL = prompt("Enter a valid vision base URL: ")
				if visionBaseURL == "" || IsSelfReferentialURL(visionBaseURL, defaultListen) {
					fmt.Fprintln(stderr, "Invalid vision URL. Setup cancelled.")
					return 1
				}
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
	}

	// ── Step 3: Doctor self-check ──
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Running connectivity self-check (doctor)...")
	cfg := &config.Config{
		Listen:   defaultListen,
		Upstream: config.UpstreamCfg{BaseURL: upstreamBaseURL, APIKey: upstreamAPIKey},
		Vision: config.VisionCfg{
			BaseURL: visionBaseURL,
			APIKey:  visionAPIKey,
			Model:   visionModel,
		},
	}
	// 预设模式：doctor 和最终配置都用 vision_providers + type（走 BuildProvider -> OpenAIClient，
	// 修正 GLM 走 MiMo Client 打 /v1/messages 的 404）。base_url/model 由 loader 默认值填。
	if presetType != "" {
		cfg.VisionProviders = []config.ProviderCfg{{
			Name:   presetType,
			Type:   presetType,
			APIKey: visionAPIKey,
		}}
		// 清空 vision: 避免两套并存
		cfg.Vision = config.VisionCfg{}
	}

	doctorCode := 1
	if deps.doctorFunc != nil {
		doctorCode = deps.doctorFunc(cfg, stdout, stderr)
	} else {
		doctorCode = runDoctorCore(context.Background(), cfg, "deepseek-chat", false, stdout, stderr)
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
		"log_level": "info",
	}
	if len(cfg.VisionProviders) > 0 {
		// 预设模式：写 vision_providers + type（base_url/model 留给 loader 默认值填）
		ps := make([]map[string]any, 0, len(cfg.VisionProviders))
		for _, p := range cfg.VisionProviders {
			m := map[string]any{
				"name":    p.Name,
				"type":    p.Type,
				"api_key": p.APIKey,
			}
			if p.BaseURL != "" {
				m["base_url"] = p.BaseURL
			}
			if p.Model != "" {
				m["model"] = p.Model
			}
			ps = append(ps, m)
		}
		out["vision_providers"] = ps
	} else {
		// 手动模式：写 vision: 单块
		out["vision"] = map[string]any{
			"base_url": cfg.Vision.BaseURL,
			"api_key":  cfg.Vision.APIKey,
			"model":    cfg.Vision.Model,
		}
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
