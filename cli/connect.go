package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/ROM4n2/blind-llm-eyes/config"
)

// runConnect implements the `connect` subcommand: load config to get the
// listen address, then wire Claude Code's settings.json to point at this
// proxy by setting env.ANTHROPIC_BASE_URL.
func runConnect(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to config file")
	settingsPath := fs.String("settings", "", "path to ~/.claude/settings.json (default: auto-detect)")
	backupPath := fs.String("backup", "", "path to backup file (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config %q: %v\n", *configPath, err)
		return 1
	}

	sp := *settingsPath
	if sp == "" {
		sp, err = defaultSettingsPath()
		if err != nil {
			fmt.Fprintf(stderr, "locate settings.json: %v\n", err)
			return 1
		}
	}
	bp := *backupPath
	if bp == "" {
		bp, err = defaultBackupPath()
		if err != nil {
			fmt.Fprintf(stderr, "locate backup path: %v\n", err)
			return 1
		}
	}

	proxyURL := "http://" + cfg.Listen
	if err := connectSettings(sp, bp, proxyURL); err != nil {
		fmt.Fprintf(stderr, "connect: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Claude Code settings.json wired to proxy:\n")
	fmt.Fprintf(stdout, "  %s → env.ANTHROPIC_BASE_URL = %s\n", sp, proxyURL)
	fmt.Fprintf(stdout, "  backup: %s\n", bp)
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "IMPORTANT: restart Claude Code for the change to take effect.")
	fmt.Fprintln(stdout, "During connect, do not use cc-switch to switch providers — it will")
	fmt.Fprintln(stdout, "overwrite settings.json and break the proxy wiring.")
	return 0
}

// runDisconnect implements the `disconnect` subcommand: restore Claude Code's
// settings.json from the backup created by connect.
func runDisconnect(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("disconnect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	settingsPath := fs.String("settings", "", "path to ~/.claude/settings.json (default: auto-detect)")
	backupPath := fs.String("backup", "", "path to backup file (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sp := *settingsPath
	if sp == "" {
		var err error
		sp, err = defaultSettingsPath()
		if err != nil {
			fmt.Fprintf(stderr, "locate settings.json: %v\n", err)
			return 1
		}
	}
	bp := *backupPath
	if bp == "" {
		var err error
		bp, err = defaultBackupPath()
		if err != nil {
			fmt.Fprintf(stderr, "locate backup path: %v\n", err)
			return 1
		}
	}

	if err := disconnectSettings(sp, bp); err != nil {
		fmt.Fprintf(stderr, "disconnect: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Claude Code settings.json restored from backup:\n")
	fmt.Fprintf(stdout, "  %s ← %s\n", sp, bp)
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Restart Claude Code for the change to take effect.")
	return 0
}
