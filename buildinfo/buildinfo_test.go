package buildinfo

import "testing"

// TestVersion_DefaultIsDev verifies the package-level default when no ldflags
// injection occurred (i.e. a plain `go test`/`go build` run).
func TestVersion_DefaultIsDev(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("expected default Version %q, got %q", "dev", Version)
	}
}

// TestVersion_Injectable verifies the variable is mutable so that ldflags
// injection (and test-time overrides) take effect.
func TestVersion_Injectable(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "1.2.3"
	if Version != "1.2.3" {
		t.Fatalf("expected injected Version %q, got %q", "1.2.3", Version)
	}
}
