package cli

import (
	"bytes"
	"io"
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

	// stdin: no cc-switch, all defaults (empty lines), no connect
	stdin := strings.Join([]string{
		"n",
		"", // upstream base_url = default
		"sk-key",
		"", // vision base_url = default
		"sk-vis",
		"", // vision model = default
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
		"n", "", "sk-key", "", "sk-vis", "", "n",
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
