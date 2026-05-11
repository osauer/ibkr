// setup.go hosts the `ibkr setup` subcommand: writes the MCP server entry
// into local-AI-client config files so the user doesn't have to paste JSON
// snippets and substitute their absolute binary path by hand.
//
// Currently supports `claude-desktop`. Adding more clients (cursor, zed,
// continue) is a matter of new locator + config-shape entries in the
// clients map — the read-merge-backup-write loop is shared.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func runSetup(args []string) int {
	target := "claude-desktop"
	if len(args) > 0 {
		if args[0] == "--help" || args[0] == "-h" {
			fmt.Println("ibkr setup — write the MCP server entry for a local AI client.")
			fmt.Println()
			fmt.Println("Usage: ibkr setup [client]")
			fmt.Println()
			fmt.Println("Supported clients:")
			fmt.Println("  claude-desktop  (default)")
			fmt.Println()
			fmt.Println("The command locates the client's config file, backs it up,")
			fmt.Println("merges an mcpServers.ibkr entry pointing at this binary, and")
			fmt.Println("writes the result back. Restart the client to pick up the change.")
			return 0
		}
		target = args[0]
	}

	switch target {
	case "claude-desktop":
		return setupClaudeDesktop()
	default:
		fmt.Fprintf(os.Stderr, "ibkr setup: unknown client %q\n", target)
		fmt.Fprintln(os.Stderr, "supported: claude-desktop")
		return 2
	}
}

// claudeDesktopConfigPath returns the platform-specific path to
// claude_desktop_config.json. macOS is the only supported target — Claude
// Desktop ships for macOS and Windows; we don't support Windows (the
// daemon uses Unix-only primitives), and there's no official Linux build.
func claudeDesktopConfigPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			return "", errors.New("%APPDATA% not set")
		}
		return filepath.Join(appdata, "Claude", "claude_desktop_config.json"), nil
	default:
		return "", fmt.Errorf("claude-desktop is not available on %s — try Cursor, Continue, or Zed (see the README for the JSON snippet)", runtime.GOOS)
	}
}

func setupClaudeDesktop() int {
	configPath, err := claudeDesktopConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr setup: %v\n", err)
		return 1
	}

	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr setup: locate own binary: %v\n", err)
		return 1
	}
	// Resolve symlinks so the config holds the canonical path. Without
	// this, `~/.local/bin/ibkr -> /opt/homebrew/Cellar/...` would record
	// the symlink, which works but is fragile across reinstalls.
	if resolved, err := filepath.EvalSymlinks(binPath); err == nil {
		binPath = resolved
	}

	// Read existing config (or start from an empty object). We round-trip
	// through a generic map so unrelated top-level keys the user may have
	// (theme, telemetry, etc.) survive untouched.
	cfg := map[string]any{}
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "ibkr setup: existing %s is not valid JSON: %v\n", configPath, err)
			fmt.Fprintln(os.Stderr, "  fix or delete the file and re-run, or restore from backup")
			return 1
		}
		// Backup so the user has a one-step rollback.
		stamp := time.Now().Format("20060102-150405")
		backup := configPath + ".bak-" + stamp
		if err := os.WriteFile(backup, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "ibkr setup: backup %s: %v\n", backup, err)
			return 1
		}
		fmt.Printf("Backed up existing config → %s\n", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ibkr setup: read %s: %v\n", configPath, err)
		return 1
	} else {
		// Config doesn't exist yet — make sure the parent dir does so the
		// write doesn't fail. Claude Desktop creates this dir on first
		// launch, so it should already exist if Claude Desktop was ever
		// opened. We mkdir defensively for the fresh-install case.
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "ibkr setup: create config dir: %v\n", err)
			return 1
		}
	}

	out, err := mergeIbkrMCPEntry(cfg, binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr setup: encode config: %v\n", err)
		return 1
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "ibkr setup: write %s: %v\n", configPath, err)
		return 1
	}

	fmt.Printf("Wired ibkr into %s\n", configPath)
	fmt.Println()
	fmt.Println("Next:")
	fmt.Println("  1. Fully quit Claude Desktop (⌘Q on macOS — not just close the window).")
	fmt.Println("  2. Reopen it.")
	fmt.Println("  3. Ask Claude something like \"what's in my IBKR account?\" and it'll call the ibkr_* tools.")
	fmt.Println()
	fmt.Println("Troubleshooting log: ~/Library/Logs/Claude/mcp-server-ibkr.log")
	return 0
}

// mergeIbkrMCPEntry takes a parsed config map and the binary's absolute
// path and returns the marshalled JSON bytes (with trailing newline) that
// claude_desktop_config.json should contain. Unrelated top-level keys are
// preserved; the mcpServers map is created if absent and any prior `ibkr`
// entry under it is overwritten. Pure — no I/O — so it carries the merge
// invariants for unit tests.
func mergeIbkrMCPEntry(cfg map[string]any, binPath string) ([]byte, error) {
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["ibkr"] = map[string]any{
		"command": binPath,
		"args":    []string{"mcp"},
	}
	cfg["mcpServers"] = servers

	// Two-space indent matches what Claude Desktop writes by default.
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	return out, nil
}
