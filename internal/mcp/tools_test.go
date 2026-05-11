package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/cli"
)

// TestParity is the binding drift gate: every CLI subcommand from
// internal/cli must either have a matching MCP tool (`ibkr_<name>`) or be
// listed in ExcludedCLI with a justification. Adding a new CLI command
// without an MCP tool — or vice versa — fails `make check`.
func TestParity(t *testing.T) {
	t.Parallel()

	cliNames := map[string]bool{}
	for _, c := range cli.Commands() {
		cliNames[c.Name] = true
	}

	mcpNames := map[string]bool{}
	for _, tool := range Tools {
		name := strings.TrimPrefix(tool.Name, "ibkr_")
		if name == tool.Name {
			t.Errorf("MCP tool %q must use the ibkr_ prefix", tool.Name)
			continue
		}
		mcpNames[name] = true
	}

	for name := range cliNames {
		if _, excluded := ExcludedCLI[name]; excluded {
			if mcpNames[name] {
				t.Errorf("CLI command %q is in ExcludedCLI but also has an MCP tool — pick one", name)
			}
			continue
		}
		if !mcpNames[name] {
			t.Errorf("CLI command %q has no MCP tool (ibkr_%s); add a Tool entry to internal/mcp/tools.go or list it in ExcludedCLI with a reason", name, name)
		}
	}
	for name := range mcpNames {
		if !cliNames[name] {
			t.Errorf("MCP tool ibkr_%s has no CLI counterpart (cli.Commands has no command named %q) — drop the tool or add the CLI command", name, name)
		}
	}
	for name := range ExcludedCLI {
		if !cliNames[name] {
			t.Errorf("ExcludedCLI entry %q is not in cli.Commands; remove the stale exclusion", name)
		}
	}
}

// TestSchemasAreValidJSON guards against typos in the hand-built JSON
// Schemas. Every tool's InputSchema must be a well-formed object schema.
func TestSchemasAreValidJSON(t *testing.T) {
	t.Parallel()
	for _, tool := range Tools {
		var got map[string]any
		if err := json.Unmarshal(tool.JSONSchema, &got); err != nil {
			t.Errorf("tool %s: invalid JSONSchema: %v", tool.Name, err)
			continue
		}
		if got["type"] != "object" {
			t.Errorf("tool %s: schema must declare type=object (got %v)", tool.Name, got["type"])
		}
	}
}

// TestNoTradingTools is the safety counterpart to the build-tag stub and the
// PreToolUse hook. Even if a contributor adds a Tool entry by mistake,
// `make check` refuses to compile a binary that exposes trading verbs.
func TestNoTradingTools(t *testing.T) {
	t.Parallel()
	for _, tool := range Tools {
		for _, banned := range []string{"order", "trade", "cancel", "submit", "place"} {
			if strings.Contains(strings.ToLower(tool.Name), banned) {
				t.Errorf("tool %s name contains banned trading verb %q — ibkr is read-only", tool.Name, banned)
			}
		}
	}
}

// TestInitializeAndToolsList walks the MCP handshake without touching the
// daemon: send initialize, expect serverInfo + tools capability; send
// tools/list, expect every Tool we registered.
func TestInitializeAndToolsList(t *testing.T) {
	t.Parallel()

	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")

	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := bytes.Split(bytes.TrimRight(out.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d:\n%s", len(lines), out.String())
	}

	var initResp struct {
		Result struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[0], &initResp); err != nil {
		t.Fatalf("initialize response: %v", err)
	}
	if initResp.Result.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocolVersion: got %q want %q", initResp.Result.ProtocolVersion, ProtocolVersion)
	}
	if initResp.Result.ServerInfo.Name != "ibkr" {
		t.Errorf("serverInfo.name: got %q want %q", initResp.Result.ServerInfo.Name, "ibkr")
	}
	if _, ok := initResp.Result.Capabilities["tools"]; !ok {
		t.Errorf("capabilities missing tools key: %v", initResp.Result.Capabilities)
	}

	var listResp struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[1], &listResp); err != nil {
		t.Fatalf("tools/list response: %v", err)
	}
	if len(listResp.Result.Tools) != len(Tools) {
		t.Errorf("tools/list returned %d tools, expected %d", len(listResp.Result.Tools), len(Tools))
	}
	for i, want := range Tools {
		if listResp.Result.Tools[i].Name != want.Name {
			t.Errorf("tool[%d].name: got %q want %q", i, listResp.Result.Tools[i].Name, want.Name)
		}
	}
}

// TestUnknownToolReturnsMethodNotFound verifies the protocol-error branch
// for tools/call with a bogus name.
func TestUnknownToolReturnsMethodNotFound(t *testing.T) {
	t.Parallel()
	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}` + "\n")
	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error response, got: %s", out.String())
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Errorf("code: got %d want %d", resp.Error.Code, codeMethodNotFound)
	}
}
