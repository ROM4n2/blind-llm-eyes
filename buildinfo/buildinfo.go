// Package buildinfo holds version metadata injected at link time.
//
// Version defaults to "dev" for local builds. Release builds override it via
// goreleaser ldflags:
//
//	-X github.com/ROM4n2/blind-llm-eyes/buildinfo.Version={{.Version}}
package buildinfo

// Version is the application version. Default "dev"; overridden at link time.
var Version = "dev"
