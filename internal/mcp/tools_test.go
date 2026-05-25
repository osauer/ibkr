package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/cli"
	"github.com/osauer/ibkr/internal/rpc"
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
		if cliNames[name] {
			continue
		}
		// MCP tools can expose CLI subverbs as their own tool (e.g.
		// `ibkr scan params` → `ibkr_scan_params`). Accept the tool if
		// the prefix up to the first underscore is itself a CLI command,
		// since MCP clients use a flat tool surface and a focused tool
		// per subverb is easier for agents than one mega-tool with a
		// mode discriminator.
		if i := strings.Index(name, "_"); i > 0 && cliNames[name[:i]] {
			continue
		}
		t.Errorf("MCP tool ibkr_%s has no CLI counterpart (cli.Commands has no command named %q nor a parent command for a subverb) — drop the tool or add the CLI command", name, name)
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

// TestIbkrGammaSchemaHasScopeParam pins the P1 wire-parity gate: the
// ibkr_gamma tool's input schema must declare a `scope` property whose
// enum carries the three CLI-equivalent values ("spy", "spx",
// "spy+spx"). Drops the param without updating the schema and this
// test fails — clients reading tools/list never see the new arg.
func TestIbkrGammaSchemaHasScopeParam(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_gamma")
	if !ok {
		t.Fatalf("ibkr_gamma tool not registered")
	}
	var schema struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.JSONSchema, &schema); err != nil {
		t.Fatalf("decode ibkr_gamma schema: %v", err)
	}
	scope, ok := schema.Properties["scope"]
	if !ok {
		t.Fatalf("ibkr_gamma schema missing 'scope' property")
	}
	if scope.Type != "string" {
		t.Errorf("scope.type: got %q want %q", scope.Type, "string")
	}
	want := map[string]bool{"spy": true, "spx": true, "spy+spx": true}
	for _, v := range scope.Enum {
		delete(want, v)
	}
	if len(want) > 0 {
		t.Errorf("scope.enum missing values: %v (got %v)", want, scope.Enum)
	}
}

// TestIbkrGammaRejectsUnknownScope pins the validation edge: a bogus
// scope value surfaces as a tool error (isError=true) rather than
// hitting the daemon with garbage and getting a generic wire error.
// MCP-side normalisation keeps the contract close to the CLI's
// behaviour for --only.
func TestIbkrGammaRejectsUnknownScope(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_gamma")
	if !ok {
		t.Fatalf("ibkr_gamma tool not registered")
	}
	_, err := tool.Handler(context.Background(), nil, json.RawMessage(`{"scope":"nope"}`))
	if err == nil {
		t.Fatalf("expected error on unknown scope, got nil")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention 'scope', got: %v", err)
	}
}

func TestIbkrWatchSchemaHasQuoteParams(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_watch")
	if !ok {
		t.Fatalf("ibkr_watch tool not registered")
	}
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
			Minimum     int    `json:"minimum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.JSONSchema, &schema); err != nil {
		t.Fatalf("decode ibkr_watch schema: %v", err)
	}
	for _, name := range []string{"include_quotes", "include_positions", "timeout_ms"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("ibkr_watch schema missing %q", name)
		}
	}
	if schema.Properties["include_quotes"].Type != "boolean" {
		t.Fatalf("include_quotes.type = %q, want boolean", schema.Properties["include_quotes"].Type)
	}
	if schema.Properties["include_positions"].Type != "boolean" {
		t.Fatalf("include_positions.type = %q, want boolean", schema.Properties["include_positions"].Type)
	}
	if schema.Properties["timeout_ms"].Type != "integer" || schema.Properties["timeout_ms"].Minimum != 100 {
		t.Fatalf("timeout_ms schema = %+v, want integer minimum 100", schema.Properties["timeout_ms"])
	}
	if !strings.Contains(tool.Description, "decision-making monitor") {
		t.Fatalf("ibkr_watch description should explain the enriched quote use case:\n%s", tool.Description)
	}
	if !strings.Contains(schema.Properties["include_quotes"].Description, "default true") {
		t.Fatalf("include_quotes description should document the quote-default behavior: %q", schema.Properties["include_quotes"].Description)
	}
}

func TestWatchlistQuoteContractUsesHoldingRoute(t *testing.T) {
	t.Parallel()
	got := watchlistQuoteContract("MBG", &rpc.WatchlistHolding{Currency: "EUR", Exchange: "IBIS"})
	if got.Currency != "EUR" || got.Market != "de" || got.Exchange != "" {
		t.Fatalf("watchlistQuoteContract route = %+v, want market=de EUR", got)
	}
}

func TestIbkrCalendarSchemaHasSupportedMarkets(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_calendar")
	if !ok {
		t.Fatalf("ibkr_calendar tool not registered")
	}
	var schema struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.JSONSchema, &schema); err != nil {
		t.Fatalf("decode ibkr_calendar schema: %v", err)
	}
	market, ok := schema.Properties["market"]
	if !ok {
		t.Fatalf("ibkr_calendar schema missing market property")
	}
	if market.Type != "string" {
		t.Fatalf("market.type = %q, want string", market.Type)
	}
	want := map[string]bool{"us": true, "us-equity": true, "us-options": true, "de": true, "de-xetra": true}
	for _, v := range market.Enum {
		delete(want, v)
	}
	if len(want) > 0 {
		t.Fatalf("market enum missing values: %v (got %v)", want, market.Enum)
	}
}

// TestIbkrRegimeResponseHasCompositeStreaksQuality is the P2 wire-
// parity gate: every RegimeSnapshotResult field the CLI consumes must
// be reachable via the MCP envelope so an MCP-only client sees the
// same surface. Marshals the rpc shape with all the new optional
// fields populated, then re-decodes and checks each load-bearing
// JSON key landed.
func TestIbkrRegimeResponseHasCompositeStreaksQuality(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	ratio := 0.91
	vix := 14.5
	res := rpc.RegimeSnapshotResult{
		AsOf: now,
		VIXTermStructure: rpc.RegimeVIXTerm{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "green",
				BandReason: "<0.92 contango",
				Thresholds: &rpc.RegimeThresholds{
					Label: "vix_term_structure_v1", Green: "VIX/VIX3M < 0.92", Yellow: "0.92-1.00", Red: ">=1.00",
					Heuristic: true, PendingBacktest: true,
				},
				AsOf: &rpc.RegimeAsOfSummary{Label: "live", Time: now, Freshness: rpc.FreshnessLive, Source: "VIX tick"},
			},
			Status: rpc.RegimeStatusOK,
			VIX:    &vix,
			Ratio:  &ratio,
			VIXQuality: &rpc.Quality{
				AsOf: now, FreshnessClass: rpc.FreshnessLive,
				Confidence: rpc.ConfidenceFirm, Source: "VIX tick",
			},
			Streak: &rpc.StreakInfo{Band: "green", Sessions: 3, Since: "2026-05-20"},
		},
		Composite: rpc.RegimeComposite{
			Verdict:              "Normal regime",
			GreenCount:           3,
			YellowCount:          0,
			RedCount:             0,
			RankedCount:          3,
			ClusterGreenCount:    3,
			ClusterRankedCount:   3,
			ClusterUnrankedCount: 4,
		},
		Summary: rpc.RegimeSummary{
			Label: "Normal regime", Evidence: "3 green clusters / 4 unranked clusters", IndicatorEvidence: "3 green",
			PunchLine:  "volatility term structure is constructive.",
			Confidence: "high", NotAdvice: "Regime read only; not investment advice or a trade recommendation.",
		},
		WarningDetails: []rpc.RegimeWarning{{
			Code: "gamma_zero_computing", Scope: "gamma_zero", Severity: "warning",
			Message: "dealer gamma is still computing", Impact: "gamma is unranked", Action: "retry later",
		}},
		SpecDoc: "docs/specs/risk-regime-dashboard.md",
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	// Top-level composite key + nested verdict — agents read this
	// path to display the headline.
	comp, ok := wire["composite"].(map[string]any)
	if !ok {
		t.Fatalf("composite missing from envelope: %s", b)
	}
	if comp["verdict"] != "Normal regime" {
		t.Errorf("composite.verdict: got %v want %q", comp["verdict"], "Normal regime")
	}
	if _, ok := comp["cluster_green_count"]; !ok {
		t.Errorf("composite should expose cluster counts for agent scoring: %#v", comp)
	}
	summary, ok := wire["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing from envelope: %s", b)
	}
	if summary["punch_line"] == "" || summary["not_advice"] == "" {
		t.Errorf("summary should carry punch_line and not_advice: %#v", summary)
	}
	if summary["indicator_evidence"] == "" {
		t.Errorf("summary should carry indicator_evidence alongside cluster evidence: %#v", summary)
	}
	for _, key := range []string{"vol_of_vol", "credit_spreads", "funding_stress"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("%s missing from regime MCP/JSON shape", key)
		}
	}
	warnings, ok := wire["warning_details"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("warning_details missing or wrong shape: %#v", wire["warning_details"])
	}
	// Streak + Quality nested under each indicator row.
	vixRow, ok := wire["vix_term_structure"].(map[string]any)
	if !ok {
		t.Fatalf("vix_term_structure missing")
	}
	if _, ok := vixRow["streak"]; !ok {
		t.Errorf("vix_term_structure.streak missing (CLI --explain shows it)")
	}
	if _, ok := vixRow["vix_quality"]; !ok {
		t.Errorf("vix_term_structure.vix_quality missing (CLI --explain shows it)")
	}
	for _, key := range []string{"band", "band_reason", "thresholds", "as_of"} {
		if _, ok := vixRow[key]; !ok {
			t.Errorf("vix_term_structure.%s missing (agent compact metadata)", key)
		}
	}
}
