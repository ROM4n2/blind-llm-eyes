package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/ROM4n2/blind-llm-eyes/buildinfo"
)

func TestRun_Version_PrintsVersionAndGoRuntime(t *testing.T) {
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = "1.2.3"

	var stdout, stderr bytes.Buffer
	code := Run([]string{"version"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "blind-llm-eyes 1.2.3") {
		t.Errorf("output missing version string: %q", out)
	}
	if !strings.Contains(out, runtime.Version()) {
		t.Errorf("output missing go runtime version %q: %q", runtime.Version(), out)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRun_Version_UsesDevDefault(t *testing.T) {
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = "dev"

	var stdout bytes.Buffer
	code := Run([]string{"version"}, nil, &stdout, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "blind-llm-eyes dev") {
		t.Errorf("expected dev version in output: %q", stdout.String())
	}
}

func TestRun_NoArgs_ExitsNonZeroAndPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(nil, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit for no args")
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("expected usage in stderr: %q", stderr.String())
	}
}

// TestRun_Routing is a table-driven test verifying that each known command is
// recognised (never reported as "unknown command") and that unknown commands
// are. Subcommands not yet implemented report "not implemented" (exit 2); as
// later tasks implement them they replace the stubs and add focused tests.
func TestRun_Routing(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantCode    int
		wantInStderr string
		notWant      string // substring that must NOT appear in stderr
	}{
		{"unknown command", []string{"bogus"}, 2, "unknown command", ""},
		{"start advisory", []string{"start"}, 0, "server", "unknown command"},
		{"setup not implemented", []string{"setup"}, 2, "not implemented", "unknown command"},
		{"doctor missing config", []string{"doctor"}, 1, "config.yaml", "unknown command"},
		{"connect not implemented", []string{"connect"}, 2, "not implemented", "unknown command"},
		{"disconnect not implemented", []string{"disconnect"}, 2, "not implemented", "unknown command"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(c.args, nil, &stdout, &stderr)
			if code != c.wantCode {
				t.Errorf("exit code: got %d, want %d (stderr=%q)", code, c.wantCode, stderr.String())
			}
			if c.wantInStderr != "" && !strings.Contains(stderr.String(), c.wantInStderr) {
				t.Errorf("stderr missing %q: %q", c.wantInStderr, stderr.String())
			}
			if c.notWant != "" && strings.Contains(stderr.String(), c.notWant) {
				t.Errorf("stderr should not contain %q: %q", c.notWant, stderr.String())
			}
		})
	}
}

// TestRun_ArgPassThrough verifies that flags after a subcommand are forwarded
// to the handler (here: setup -config foo.yaml reaches runSetup with rest).
func TestRun_ArgPassThrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// setup is still a stub, but it must not be treated as unknown.
	code := Run([]string{"setup", "-config", "foo.yaml"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero for not-implemented setup")
	}
	if strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("setup with flags must not be 'unknown command': %q", stderr.String())
	}
}
