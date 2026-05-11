package main

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// TestMergeIbkrMCPEntryFreshConfig covers the case where no existing config
// is present — the resulting JSON should contain only mcpServers.ibkr.
func TestMergeIbkrMCPEntryFreshConfig(t *testing.T) {
	t.Parallel()
	out, err := mergeIbkrMCPEntry(map[string]any{}, "/usr/local/bin/ibkr")
	if err != nil {
		t.Fatalf("mergeIbkrMCPEntry: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong shape: %#v", got)
	}
	ibkr, ok := servers["ibkr"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers.ibkr missing or wrong shape: %#v", servers)
	}
	if ibkr["command"] != "/usr/local/bin/ibkr" {
		t.Errorf("command: got %v, want /usr/local/bin/ibkr", ibkr["command"])
	}
	args, ok := ibkr["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "mcp" {
		t.Errorf("args: got %v, want [\"mcp\"]", ibkr["args"])
	}
	if !strings.HasSuffix(string(out), "\n") {
		t.Errorf("output should end in newline")
	}
}

// TestMergeIbkrMCPEntryPreservesUnrelatedKeys is the load-bearing invariant
// — a user's other settings (theme, telemetry, unrelated mcpServers) must
// survive the merge unchanged.
func TestMergeIbkrMCPEntryPreservesUnrelatedKeys(t *testing.T) {
	t.Parallel()
	existing := map[string]any{
		"theme":     "dark",
		"telemetry": false,
		"mcpServers": map[string]any{
			"filesystem": map[string]any{
				"command": "npx",
				"args":    []any{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
			},
		},
	}
	out, err := mergeIbkrMCPEntry(existing, "/opt/bin/ibkr")
	if err != nil {
		t.Fatalf("mergeIbkrMCPEntry: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["theme"] != "dark" {
		t.Errorf("theme lost or corrupted: %v", got["theme"])
	}
	if got["telemetry"] != false {
		t.Errorf("telemetry lost or corrupted: %v", got["telemetry"])
	}
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["filesystem"]; !ok {
		t.Errorf("filesystem mcpServer was clobbered: %#v", servers)
	}
	ibkr := servers["ibkr"].(map[string]any)
	if ibkr["command"] != "/opt/bin/ibkr" {
		t.Errorf("ibkr.command: got %v", ibkr["command"])
	}
}

// TestMergeIbkrMCPEntryOverwritesPriorEntry — re-running `ibkr setup` after
// reinstalling to a new path must update the command, not duplicate.
func TestMergeIbkrMCPEntryOverwritesPriorEntry(t *testing.T) {
	t.Parallel()
	existing := map[string]any{
		"mcpServers": map[string]any{
			"ibkr": map[string]any{
				"command": "/old/path/ibkr",
				"args":    []any{"mcp"},
			},
		},
	}
	out, err := mergeIbkrMCPEntry(existing, "/new/path/ibkr")
	if err != nil {
		t.Fatalf("mergeIbkrMCPEntry: %v", err)
	}

	var got map[string]any
	_ = json.Unmarshal(out, &got)
	ibkr := got["mcpServers"].(map[string]any)["ibkr"].(map[string]any)
	if ibkr["command"] != "/new/path/ibkr" {
		t.Errorf("expected command to be overwritten to /new/path/ibkr, got %v", ibkr["command"])
	}
	if servers := got["mcpServers"].(map[string]any); len(servers) != 1 {
		t.Errorf("expected exactly one mcpServer entry after overwrite, got %d: %#v", len(servers), servers)
	}
}

// TestClaudeDesktopConfigPathPlatforms covers the path-resolution edges. We
// can't easily set runtime.GOOS, so this test asserts the current platform
// returns a sensible path on darwin and an explanatory error elsewhere.
func TestClaudeDesktopConfigPathPlatforms(t *testing.T) {
	t.Parallel()
	path, err := claudeDesktopConfigPath()
	switch runtime.GOOS {
	case "darwin":
		if err != nil {
			t.Fatalf("darwin: unexpected error: %v", err)
		}
		if !strings.HasSuffix(path, "Library/Application Support/Claude/claude_desktop_config.json") {
			t.Errorf("darwin: path suffix wrong: %s", path)
		}
	case "windows":
		// Don't actively ship Windows, but the code branch exists. Skip
		// rather than assert APPDATA shape — CI doesn't run on Windows.
		t.Skip("not exercised by CI")
	default:
		if err == nil {
			t.Errorf("expected error on %s, got path %q", runtime.GOOS, path)
		}
		if !strings.Contains(err.Error(), "not available on") {
			t.Errorf("error message should explain platform unavailability, got: %v", err)
		}
	}
}
