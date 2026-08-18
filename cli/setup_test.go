package cli

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ROM4n2/blind-llm-eyes/config"
	"gopkg.in/yaml.v3"
)

func TestRunSetupCore_ManualInput(t *testing.T) {
	var configName, configData string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error {
			configName = name
			configData = data
			return nil
		},
		connectFunc: func(proxyURL string) error { return nil },
		// Doctor passes
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int {
			return 0
		},
	}

	// Scripted stdin: no cc-switch, manual input, no connect
	stdin := strings.Join([]string{
		"n",                                      // import from cc-switch? no
		"https://api.deepseek.com/anthropic",     // upstream base_url
		"sk-deepseek-key",                        // upstream api_key
		"3",                                      // vision provider: manual (MiMo)
		"https://api.xiaomimimo.com/anthropic",   // vision base_url
		"sk-mimo-key",                            // vision api_key
		"mimo-v2.5",                              // vision model
		"n",                                      // connect now? no
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}

	// Verify config file was written
	if configName != "config.yaml" {
		t.Errorf("config name: got %q, want config.yaml", configName)
	}

	// Parse the generated YAML and verify fields
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(configData), &cfg); err != nil {
		t.Fatalf("parse generated config: %v\nconfig:\n%s", err, configData)
	}
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream.base_url: got %q", cfg.Upstream.BaseURL)
	}
	if cfg.Upstream.APIKey != "sk-deepseek-key" {
		t.Errorf("upstream.api_key: got %q", cfg.Upstream.APIKey)
	}
	if cfg.Vision.BaseURL != "https://api.xiaomimimo.com/anthropic" {
		t.Errorf("vision.base_url: got %q", cfg.Vision.BaseURL)
	}
	if cfg.Vision.APIKey != "sk-mimo-key" {
		t.Errorf("vision.api_key: got %q", cfg.Vision.APIKey)
	}
	if cfg.Vision.Model != "mimo-v2.5" {
		t.Errorf("vision.model: got %q", cfg.Vision.Model)
	}

	// Verify prompts contain key questions
	out := stdout.String()
	for _, want := range []string{"cc-switch", "upstream", "vision", "model", "connect"} {
		if !strings.Contains(strings.ToLower(out), want) {
			t.Errorf("stdout missing prompt about %q:\n%s", want, out)
		}
	}
}

func TestRunSetupCore_DoctorFail_SaveAnyway(t *testing.T) {
	var configData string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error {
			configData = data
			return nil
		},
		connectFunc: func(proxyURL string) error { return nil },
		// Doctor fails
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int {
			return 1
		},
	}

	// stdin: no cc-switch, manual input, doctor fails, save anyway, no connect
	stdin := strings.Join([]string{
		"n",
		"https://api.deepseek.com/anthropic",
		"sk-key",
		"3", // vision provider: manual (MiMo)
		"https://api.xiaomimimo.com/anthropic",
		"sk-vis",
		"mimo-v2.5",
		"y", // save anyway despite doctor failure
		"n", // no connect
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("expected exit 0 (saved anyway), got %d", code)
	}
	if configData == "" {
		t.Error("config should have been saved")
	}
	out := stdout.String()
	if !strings.Contains(strings.ToLower(out), "anyway") {
		t.Errorf("expected prompt about saving despite failure:\n%s", out)
	}
}

func TestRunSetupCore_DoctorFail_DontSave(t *testing.T) {
	configWritten := false
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error {
			configWritten = true
			return nil
		},
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int {
			return 1
		},
	}

	stdin := strings.Join([]string{
		"n",
		"https://api.deepseek.com/anthropic",
		"sk-key",
		"3", // vision provider: manual (MiMo)
		"https://api.xiaomimimo.com/anthropic",
		"sk-vis",
		"mimo-v2.5",
		"n", // don't save
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code == 0 {
		t.Error("expected non-zero exit when user chooses not to save")
	}
	if configWritten {
		t.Error("config should NOT have been saved")
	}
}

func TestRunSetupCore_ConnectAfterSetup(t *testing.T) {
	connectCalled := false
	connectURL := ""
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { return nil },
		connectFunc: func(proxyURL string) error {
			connectCalled = true
			connectURL = proxyURL
			return nil
		},
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int {
			return 0
		},
	}

	stdin := strings.Join([]string{
		"n",
		"https://api.deepseek.com/anthropic",
		"sk-key",
		"3", // vision provider: manual (MiMo)
		"https://api.xiaomimimo.com/anthropic",
		"sk-vis",
		"mimo-v2.5",
		"y", // yes connect
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !connectCalled {
		t.Error("connect should have been called")
	}
	if !strings.Contains(connectURL, "127.0.0.1") {
		t.Errorf("connect URL should contain 127.0.0.1, got %q", connectURL)
	}
}

func TestRunSetupCore_DefaultValues(t *testing.T) {
	var configData string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error {
			configData = data
			return nil
		},
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int {
			return 0
		},
	}

	// stdin: no cc-switch, choose manual MiMo (option 3), all defaults, no connect
	stdin := strings.Join([]string{
		"n",
		"",     // upstream base_url = default
		"sk-key",
		"3",    // vision provider: manual (MiMo)
		"",     // vision base_url = default
		"sk-vis",
		"",     // vision model = default
		"n",
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}

	var cfg config.Config
	if err := yaml.Unmarshal([]byte(configData), &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// Defaults should be applied
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream base_url default: got %q", cfg.Upstream.BaseURL)
	}
	if cfg.Vision.BaseURL != "https://api.xiaomimimo.com/anthropic" {
		t.Errorf("vision base_url default: got %q", cfg.Vision.BaseURL)
	}
	if cfg.Vision.Model != "mimo-v2.5" {
		t.Errorf("vision model default: got %q", cfg.Vision.Model)
	}
}

func TestRunSetupCore_StartupInstructions(t *testing.T) {
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int {
			return 0
		},
	}

	stdin := strings.Join([]string{
		"n", "", "sk-key", "3", "", "sk-vis", "", "n",
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}

	out := stdout.String()
	// Should mention the start command
	if !strings.Contains(strings.ToLower(out), "start") {
		t.Errorf("output should mention 'start' command:\n%s", out)
	}
}

func TestRunSetupCore_GLMFreePreset(t *testing.T) {
	var configData string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error {
			configData = data
			return nil
		},
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int {
			return 0
		},
	}

	// stdin: no cc-switch, choose GLM-4V-Flash (option 1 = default), provide
	// GLM API key, no connect.
	stdin := strings.Join([]string{
		"n",              // import from cc-switch? no
		"",               // upstream base_url = default
		"sk-deepseek",    // upstream api_key
		"",               // vision provider: default = 1 (GLM-4V-Flash)
		"sk-free-glm",    // GLM API key
		"n",              // connect now? no
	}, "\n") + "\n"

	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%s)", code, stderr.String())
	}

	var cfg config.Config
	if err := yaml.Unmarshal([]byte(configData), &cfg); err != nil {
		t.Fatalf("parse config: %v\nconfig:\n%s", err, configData)
	}
	// 预设模式：应有 1 个 vision provider，type=glm_free
	if len(cfg.VisionProviders) != 1 {
		t.Fatalf("expected 1 vision provider, got %d", len(cfg.VisionProviders))
	}
	p := cfg.VisionProviders[0]
	if p.Type != "glm_free" {
		t.Errorf("provider type: want glm_free, got %q", p.Type)
	}
	if p.APIKey != "sk-free-glm" {
		t.Errorf("provider api_key: want sk-free-glm, got %q", p.APIKey)
	}
	// 预设模式应清空 vision: 块
	if cfg.Vision.BaseURL != "" {
		t.Errorf("vision: block should be empty in preset mode, got base_url=%q", cfg.Vision.BaseURL)
	}

	// Output should mention GLM and the free tier URL.
	out := stdout.String()
	if !strings.Contains(out, "GLM-4V-Flash") {
		t.Errorf("stdout should mention GLM-4V-Flash:\n%s", out)
	}
	if !strings.Contains(out, "open.bigmodel.cn") {
		t.Errorf("stdout should mention open.bigmodel.cn:\n%s", out)
	}
}

func TestSetup_QwenPresetWritesVisionProviders(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}
	// 选 Qwen（第 2 选项）+ key + 不 connect
	stdin := strings.NewReader("n\nhttps://up.example\nup-key\n2\nds-key\nn\n")
	var stdout, stderr bytes.Buffer
	code := runSetupCore(stdin, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(cfgOut, "vision_providers") {
		t.Errorf("config missing vision_providers:\n%s", cfgOut)
	}
	if !strings.Contains(cfgOut, "type: qwen") {
		t.Errorf("config missing type: qwen:\n%s", cfgOut)
	}
	if strings.Contains(cfgOut, "vision:") {
		t.Errorf("preset should not write vision: block:\n%s", cfgOut)
	}
}

func TestSetup_GLMUnifiedToVisionProviders(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}
	// 选 GLM（默认，直接回车）+ key + 不 connect
	stdin := strings.NewReader("n\nhttps://up.example\nup-key\n\nglm-key\nn\n")
	var stdout, stderr bytes.Buffer
	code := runSetupCore(stdin, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}
	if !strings.Contains(cfgOut, "type: glm_free") {
		t.Errorf("GLM preset should write type: glm_free:\n%s", cfgOut)
	}
	if strings.Contains(cfgOut, "vision:") {
		t.Errorf("preset should not write vision: block:\n%s", cfgOut)
	}
}

// TestRunSetupCore_CcSwitchImport_FiltersSelfReferential verifies that the
// setup wizard filters self-referential providers during cc-switch import and
// shows the filtered-out count in the output.
func TestRunSetupCore_CcSwitchImport_FiltersSelfReferential(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "deepseek-1",
			appType:        "claude",
			name:           "DeepSeek",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-ds", "deepseek-chat"),
		},
		{
			id:             "glm-1",
			appType:        "claude",
			name:           "GLM",
			settingsConfig: makeClaudeSettings("https://open.bigmodel.cn/api/paas/v4", "sk-glm", "glm-4v"),
		},
		{
			id:             "self-1",
			appType:        "claude",
			name:           "SelfRef",
			settingsConfig: makeClaudeSettings("http://127.0.0.1:8790/anthropic", "sk-bad", "deepseek-chat"),
		},
	}
	createTestDB(t, dbPath, rows)

	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		ccSwitchPath: dbPath,
		configPath:   "config.yaml",
	}

	// stdin: y (import), 1 (upstream=DeepSeek), 2 (vision=GLM), n (no connect)
	stdin := strings.NewReader("y\n1\n2\nn\n")
	var stdout, stderr bytes.Buffer
	code := runSetupCore(stdin, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}

	out := stdout.String()
	// Should show "Found 2 ... (1 self-referential filtered out)".
	if !strings.Contains(out, "2 Claude Code providers") {
		t.Errorf("should show 2 providers after filtering, got:\n%s", out)
	}
	if !strings.Contains(out, "1 self-referential filtered out") {
		t.Errorf("should show '1 self-referential filtered out', got:\n%s", out)
	}
	// Should NOT list the self-referential provider.
	if strings.Contains(out, "SelfRef") {
		t.Errorf("self-referential provider should be filtered out, got:\n%s", out)
	}

	// Generated config should use DeepSeek (the selected non-self-referential).
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(cfgOut), &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, cfgOut)
	}
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream.base_url: got %q, want DeepSeek URL", cfg.Upstream.BaseURL)
	}
}

// TestRunSetupCore_CcSwitchImport_AllSelfReferential_NoProviders verifies
// that when every cc-switch provider is self-referential, the wizard shows
// "no valid providers remaining" and falls through to manual input.
func TestRunSetupCore_CcSwitchImport_AllSelfReferential_NoProviders(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "self-1",
			appType:        "claude",
			name:           "SelfRef1",
			settingsConfig: makeClaudeSettings("http://127.0.0.1:8790/anthropic", "sk-bad", "deepseek-chat"),
		},
		{
			id:             "self-2",
			appType:        "claude",
			name:           "SelfRef2",
			settingsConfig: makeClaudeSettings("http://localhost:8790/anthropic", "sk-bad", "deepseek-chat"),
		},
	}
	createTestDB(t, dbPath, rows)

	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		ccSwitchPath: dbPath,
		configPath:   "config.yaml",
	}

	// stdin: y (import), Enter (manual upstream), Enter (manual vision),
	// then manual input for everything.
	stdin := strings.Join([]string{
		"y",                                  // import from cc-switch
		"",                                   // upstream: Enter (manual)
		"",                                   // vision: Enter (manual)
		"https://api.deepseek.com/anthropic", // upstream base URL
		"sk-key",                             // upstream api key
		"3",                                  // vision: manual MiMo
		"https://api.xiaomimimo.com/anthropic",
		"sk-vis",
		"mimo-v2.5",
		"n", // no connect
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "no valid providers remaining") {
		t.Errorf("should show 'no valid providers remaining', got:\n%s", out)
	}
	if !strings.Contains(out, "2 self-referential filtered out") {
		t.Errorf("should show '2 self-referential filtered out', got:\n%s", out)
	}

	// Config should still be written with manual input.
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(cfgOut), &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, cfgOut)
	}
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream.base_url: got %q", cfg.Upstream.BaseURL)
	}
}

// TestRunSetupCore_ManualInput_SelfReferential_WarnsAndReprompts verifies
// that manually entering a self-referential upstream URL triggers a WARNING
// and a re-prompt; entering a valid URL on the second try proceeds normally.
func TestRunSetupCore_ManualInput_SelfReferential_WarnsAndReprompts(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}

	// stdin: no cc-switch; first URL is self-referential; second URL is valid.
	stdin := strings.Join([]string{
		"n",                                  // no cc-switch import
		"http://127.0.0.1:8790",              // self-referential (triggers warning)
		"https://api.deepseek.com/anthropic", // valid (accepted)
		"sk-key",                             // upstream api key
		"3",                                  // vision: manual MiMo
		"https://api.xiaomimimo.com/anthropic",
		"sk-vis",
		"mimo-v2.5",
		"n", // no connect
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "WARNING") {
		t.Errorf("stderr should contain WARNING, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "self-forwarding loop") {
		t.Errorf("stderr should mention self-forwarding loop, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "127.0.0.1:8790") {
		t.Errorf("stderr should mention the offending URL, got:\n%s", errOut)
	}

	// Config should use the corrected URL, not the self-referential one.
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(cfgOut), &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, cfgOut)
	}
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream.base_url: got %q, want corrected URL", cfg.Upstream.BaseURL)
	}
}

// TestRunSetupCore_ManualInput_SelfReferential_Twice_Cancelled verifies
// that if the user enters a self-referential URL twice in a row, setup is
// cancelled with exit code 1 and no config is written.
func TestRunSetupCore_ManualInput_SelfReferential_Twice_Cancelled(t *testing.T) {
	configWritten := false
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error {
			configWritten = true
			return nil
		},
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}

	// stdin: no cc-switch; both URL entries are self-referential (different
	// aliases to confirm the second check also catches them).
	stdin := strings.Join([]string{
		"n",                     // no cc-switch import
		"http://127.0.0.1:8790", // self-referential (first attempt)
		"http://localhost:8790", // self-referential (second attempt, alias)
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 1 {
		t.Fatalf("expected exit 1 on double self-referential, got %d", code)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "Invalid upstream URL") {
		t.Errorf("stderr should say 'Invalid upstream URL', got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "Setup cancelled") {
		t.Errorf("stderr should say 'Setup cancelled', got:\n%s", errOut)
	}
	if configWritten {
		t.Error("config should NOT be written when setup is cancelled")
	}
}

// P1-17: manually entering a 0.0.0.0 alias of the proxy's listen address
// should trigger the self-referential WARNING and re-prompt.
func TestRunSetupCore_ManualInput_ZeroBind_Warns(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}

	// stdin: no cc-switch; first URL is 0.0.0.0:8790 (alias of 127.0.0.1:8790);
	// second URL is valid.
	stdin := strings.Join([]string{
		"n",                                  // no cc-switch import
		"http://0.0.0.0:8790",                // self-referential (0.0.0.0 alias)
		"https://api.deepseek.com/anthropic", // valid (accepted)
		"sk-key",                             // upstream api key
		"3",                                  // vision: manual MiMo
		"https://api.xiaomimimo.com/anthropic",
		"sk-vis",
		"mimo-v2.5",
		"n", // no connect
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "WARNING") {
		t.Errorf("stderr should contain WARNING, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "0.0.0.0:8790") {
		t.Errorf("stderr should mention the offending URL, got:\n%s", errOut)
	}

	// Config should use the corrected URL.
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(cfgOut), &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, cfgOut)
	}
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream.base_url: got %q, want corrected URL", cfg.Upstream.BaseURL)
	}
}

// P1-18: manually entering an ::1 IPv6 loopback alias should trigger the
// self-referential WARNING and re-prompt.
func TestRunSetupCore_ManualInput_IPv6_Warns(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}

	// stdin: no cc-switch; first URL is [::1]:8790 (IPv6 alias of 127.0.0.1:8790);
	// second URL is valid.
	stdin := strings.Join([]string{
		"n",                                  // no cc-switch import
		"http://[::1]:8790",                  // self-referential (IPv6 alias)
		"https://api.deepseek.com/anthropic", // valid (accepted)
		"sk-key",                             // upstream api key
		"3",                                  // vision: manual MiMo
		"https://api.xiaomimimo.com/anthropic",
		"sk-vis",
		"mimo-v2.5",
		"n", // no connect
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "WARNING") {
		t.Errorf("stderr should contain WARNING, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "[::1]:8790") {
		t.Errorf("stderr should mention the offending URL, got:\n%s", errOut)
	}

	var cfg config.Config
	if err := yaml.Unmarshal([]byte(cfgOut), &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, cfgOut)
	}
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream.base_url: got %q, want corrected URL", cfg.Upstream.BaseURL)
	}
}

// P1-19: a self-referential URL with a trailing path component should still
// trigger the WARNING — the self-loop check must not be fooled by /anthropic.
func TestRunSetupCore_ManualInput_URLWithPath_Warns(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}

	// stdin: no cc-switch; first URL is 127.0.0.1:8790/anthropic (with path);
	// second URL is valid.
	stdin := strings.Join([]string{
		"n",                                  // no cc-switch import
		"http://127.0.0.1:8790/anthropic",    // self-referential (with path)
		"https://api.deepseek.com/anthropic", // valid (accepted)
		"sk-key",                             // upstream api key
		"3",                                  // vision: manual MiMo
		"https://api.xiaomimimo.com/anthropic",
		"sk-vis",
		"mimo-v2.5",
		"n", // no connect
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "WARNING") {
		t.Errorf("stderr should contain WARNING, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "127.0.0.1:8790") {
		t.Errorf("stderr should mention the offending URL, got:\n%s", errOut)
	}

	var cfg config.Config
	if err := yaml.Unmarshal([]byte(cfgOut), &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, cfgOut)
	}
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream.base_url: got %q, want corrected URL", cfg.Upstream.BaseURL)
	}
}

// TestRunSetupCore_ManualInput_VisionSelfReferential_WarnsAndReprompts
// verifies that manually entering a self-referential vision base_url triggers
// a WARNING and a re-prompt; entering a valid vision URL on the second try
// proceeds normally. MiMo Client calls /v1/messages (the proxy's own path), so
// a self-referential vision.base_url causes an infinite self-forwarding loop.
func TestRunSetupCore_ManualInput_VisionSelfReferential_WarnsAndReprompts(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}

	// stdin: no cc-switch; valid upstream; vision=manual MiMo; FIRST vision
	// base_url is self-referential (triggers warning); SECOND is valid.
	stdin := strings.Join([]string{
		"n",                                    // no cc-switch import
		"https://api.deepseek.com/anthropic",   // valid upstream
		"sk-upstream",                          // upstream api key
		"3",                                    // vision: manual MiMo
		"http://127.0.0.1:8790/anthropic",      // self-referential vision URL (warning)
		"https://api.xiaomimimo.com/anthropic", // valid vision URL (accepted)
		"sk-vis",                               // vision api key
		"mimo-v2.5",                            // vision model
		"n",                                    // no connect
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("setup exited %d (stderr=%s)", code, stderr.String())
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "WARNING") {
		t.Errorf("stderr should contain WARNING, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "vision base_url") {
		t.Errorf("stderr should mention 'vision base_url', got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "self-forwarding loop") {
		t.Errorf("stderr should mention self-forwarding loop, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "127.0.0.1:8790") {
		t.Errorf("stderr should mention the offending URL, got:\n%s", errOut)
	}

	// Config should use the corrected vision URL, not the self-referential one.
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(cfgOut), &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, cfgOut)
	}
	if cfg.Vision.BaseURL != "https://api.xiaomimimo.com/anthropic" {
		t.Errorf("vision.base_url: got %q, want corrected URL", cfg.Vision.BaseURL)
	}
	// Upstream must be untouched by the vision check.
	if cfg.Upstream.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("upstream.base_url: got %q, want untouched", cfg.Upstream.BaseURL)
	}
}

// TestRunSetupCore_ManualInput_VisionSelfReferential_Twice_Cancelled verifies
// that if the user enters a self-referential vision base_url twice in a row,
// setup is cancelled with exit code 1 and no config is written.
func TestRunSetupCore_ManualInput_VisionSelfReferential_Twice_Cancelled(t *testing.T) {
	configWritten := false
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error {
			configWritten = true
			return nil
		},
		connectFunc: func(proxyURL string) error { return nil },
		doctorFunc:  func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath:  "config.yaml",
	}

	// stdin: no cc-switch; valid upstream; vision=manual MiMo; BOTH vision
	// base_url entries are self-referential (different aliases to confirm the
	// second check also catches them).
	stdin := strings.Join([]string{
		"n",                                  // no cc-switch import
		"https://api.deepseek.com/anthropic", // valid upstream
		"sk-upstream",                        // upstream api key
		"3",                                  // vision: manual MiMo
		"http://127.0.0.1:8790/anthropic",    // self-referential (first attempt)
		"http://localhost:8790/anthropic",    // self-referential (second attempt, alias)
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	code := runSetupCore(strings.NewReader(stdin), &stdout, &stderr, deps)
	if code != 1 {
		t.Fatalf("expected exit 1 on double self-referential vision URL, got %d", code)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "Invalid vision URL") {
		t.Errorf("stderr should say 'Invalid vision URL', got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "Setup cancelled") {
		t.Errorf("stderr should say 'Setup cancelled', got:\n%s", errOut)
	}
	if configWritten {
		t.Error("config should NOT be written when setup is cancelled")
	}
}
