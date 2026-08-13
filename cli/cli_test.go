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

func TestRun_UnknownCommand_ExitsNonZeroAndPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"bogus"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit for unknown command")
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("expected 'unknown command' in stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("expected usage in stderr: %q", stderr.String())
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
