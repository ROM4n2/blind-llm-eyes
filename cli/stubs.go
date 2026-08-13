package cli

import "io"

// Stubs for subcommands implemented in later tasks. Each is replaced by a real
// implementation in its own file when that task lands. Keeping them in one
// place makes the incremental replacement obvious.

func runSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return notImplemented("setup", stderr)
}

func runDoctor(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return notImplemented("doctor", stderr)
}

func runConnect(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return notImplemented("connect", stderr)
}

func runDisconnect(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return notImplemented("disconnect", stderr)
}
