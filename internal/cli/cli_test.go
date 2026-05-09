package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

// fail() should append a troubleshooting hint when the underlying daemon
// error carries the gateway_unavailable code prefix. Other error shapes
// pass through unchanged.
func TestFailAppendsHintForGatewayUnavailable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		format     string
		args       []any
		wantHint   bool
		wantSubstr string
	}{
		{
			name:       "gateway unavailable triggers hint",
			format:     "account: %v",
			args:       []any{"gateway_unavailable: ibkr connection unavailable"},
			wantHint:   true,
			wantSubstr: "gateway_unavailable",
		},
		{
			name:     "bad request does not trigger hint",
			format:   "scan: %v",
			args:     []any{"bad_request: unknown preset"},
			wantHint: false,
		},
		{
			name:     "internal error does not trigger hint",
			format:   "%s",
			args:     []any{"internal: marshal failed"},
			wantHint: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			env := &Env{Stdout: &bytes.Buffer{}, Stderr: &stderr}
			code := fail(env, tc.format, tc.args...)
			if code != 1 {
				t.Fatalf("expected exit code 1, got %d", code)
			}
			out := stderr.String()
			if tc.wantSubstr != "" && !strings.Contains(out, tc.wantSubstr) {
				t.Fatalf("stderr missing %q: %s", tc.wantSubstr, out)
			}
			hasHint := strings.Contains(out, "ibkr status")
			if hasHint != tc.wantHint {
				t.Fatalf("wantHint=%v, gotHint=%v, stderr=%q", tc.wantHint, hasHint, out)
			}
		})
	}
}

// hoistFlags moves -flag tokens (and their values) ahead of positional args
// so users can write `ibkr quote AAPL --json`.
func TestHoistFlagsReordersValueFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "trailing --json hoisted",
			in:   []string{"AAPL", "--json"},
			want: []string{"--json", "AAPL"},
		},
		{
			name: "value flag with following value hoisted as pair",
			in:   []string{"AAPL", "--expiry", "2026-06-19"},
			want: []string{"--expiry", "2026-06-19", "AAPL"},
		},
		{
			name: "value flag with = form hoisted as one token",
			in:   []string{"AAPL", "--expiry=2026-06-19"},
			want: []string{"--expiry=2026-06-19", "AAPL"},
		},
		{
			name: "flag before positional preserved",
			in:   []string{"--json", "AAPL"},
			want: []string{"--json", "AAPL"},
		},
		{
			name: "purely positional unchanged",
			in:   []string{"AAPL", "MSFT"},
			want: []string{"AAPL", "MSFT"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hoistFlags(tc.in)
			if !equalSlice(got, tc.want) {
				t.Fatalf("hoistFlags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// dataTypeBadge stays silent on the happy path (live/empty) and surfaces
// only when the data is delayed/frozen.
func TestDataTypeBadgeQuietOnLive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"live", ""},
		{"delayed", "data=delayed ⚠"},
		{"frozen", "data=frozen ⚠"},
		{"delayed_frozen", "data=delayed_frozen ⚠"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := dataTypeBadge(tc.in); got != tc.want {
				t.Fatalf("dataTypeBadge(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// lookupCommand must find every entry in commands, with no extras.
func TestCommandRegistryConsistency(t *testing.T) {
	t.Parallel()
	for _, c := range commands {
		got, ok := lookupCommand(c.Name)
		if !ok {
			t.Errorf("lookupCommand missing %q", c.Name)
			continue
		}
		if got.Summary != c.Summary {
			t.Errorf("%s: summary mismatch", c.Name)
		}
	}
	if _, ok := lookupCommand("definitely-not-a-command"); ok {
		t.Errorf("lookupCommand returned true for unknown name")
	}
	// status must be the first entry — the help table ordering is load-bearing
	// for the "run this first" UX.
	if commands[0].Name != "status" {
		t.Errorf("commands[0] = %q, want status", commands[0].Name)
	}
}

// Run must intercept --help and -h, exit 0, and not invoke the handler.
func TestRunInterceptsHelp(t *testing.T) {
	t.Parallel()
	for _, helpFlag := range []string{"--help", "-h", "-help"} {
		t.Run(helpFlag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr}
			code := Run(context.Background(), env, "account", []string{helpFlag})
			if code != 0 {
				t.Fatalf("Run(account, %s) = %d, want 0", helpFlag, code)
			}
			out := stdout.String()
			if !strings.Contains(out, "ibkr account") {
				t.Errorf("help output missing `ibkr account` header: %q", out)
			}
			if !strings.Contains(out, "Account summary") {
				t.Errorf("help output missing summary: %q", out)
			}
			if stderr.Len() != 0 {
				t.Errorf("expected no stderr on --help, got %q", stderr.String())
			}
		})
	}
}

// formatTLSField must surface fallback when configured ≠ negotiated, so the
// user can spot a silent downgrade attempt or a successful TLS upgrade.
func TestFormatTLSField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		want string
	}{
		{"disconnected shows configured only", rpc.HealthResult{Connected: false, GatewayTLS: true}, "(tls=true)"},
		{"connected, matching false", rpc.HealthResult{Connected: true, GatewayTLS: false, NegotiatedTLS: false}, "(tls=false)"},
		{"connected, matching true", rpc.HealthResult{Connected: true, GatewayTLS: true, NegotiatedTLS: true}, "(tls=true)"},
		{"fallback up to TLS", rpc.HealthResult{Connected: true, GatewayTLS: false, NegotiatedTLS: true}, "(tls=true, configured=false ⚠ fallback)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTLSField(tc.in); got != tc.want {
				t.Fatalf("formatTLSField = %q, want %q", got, tc.want)
			}
		})
	}
}

// formatSize renders bid/ask sizes and volume compactly. Zero/nil → "—";
// numbers are abbreviated K/M past the threshold so columns stay legible.
func TestFormatSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   *int
		want string
	}{
		{nil, "—"},
		{intPtr(0), "—"},
		{intPtr(-5), "—"},
		{intPtr(42), "42"},
		{intPtr(999), "999"},
		{intPtr(1200), "1.2K"},
		{intPtr(12345), "12K"},
		{intPtr(1_400_000), "1.4M"},
		{intPtr(12_500_000), "12M"},
	}
	for _, tc := range cases {
		got := formatSize(tc.in)
		if got != tc.want {
			t.Errorf("formatSize(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderQuoteSnapshotIncludesSizeColumns(t *testing.T) {
	t.Parallel()
	bid, ask, last := 207.85, 207.88, 207.87
	bidSz, askSz := 100, 200
	vol := int64(12_400_000)
	q := rpc.Quote{
		Symbol: "AAPL", Bid: &bid, Ask: &ask, Last: &last,
		BidSize: &bidSz, AskSize: &askSz, Volume: &vol,
		IVStatus: "unavailable", DataType: "live",
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderQuoteSnapshotText(env, []rpc.Quote{q}); code != 0 {
		t.Fatalf("renderQuoteSnapshotText: code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{"BID_SZ", "ASK_SZ", "VOLUME", "100", "200", "12M"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func intPtr(v int) *int { return &v }

// renderPositionsByUnderlying produces one block per underlying showing
// stock + option legs and a Group line. Pure-options underlyings omit the
// Stock line.
func TestRenderPositionsByUnderlying(t *testing.T) {
	t.Parallel()
	stock := rpc.PositionView{Symbol: "AAPL", Quantity: 100, AvgCost: 192, Mark: 207, MarketValue: 20700, UnrealizedPnL: 1500}
	opt := rpc.PositionView{Symbol: "AAPL", Right: "C", Strike: 215, Expiry: "20260619", Quantity: 5, MarketValue: 4700, UnrealizedPnL: 1290}
	tslaPut := rpc.PositionView{Symbol: "TSLA", Right: "P", Strike: 200, Expiry: "20260516", Quantity: 2, MarketValue: 800, UnrealizedPnL: -90}
	res := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{
			{Underlying: "AAPL", Stock: &stock, Options: []rpc.PositionView{opt}, GroupMarketValue: 25400, GroupUnrealizedPnL: 2790},
			{Underlying: "TSLA", Options: []rpc.PositionView{tslaPut}, GroupMarketValue: 800, GroupUnrealizedPnL: -90},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderPositionsByUnderlying(env, res); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "AAPL") || !strings.Contains(out, "TSLA") {
		t.Errorf("missing underlying headers:\n%s", out)
	}
	if !strings.Contains(out, "Stock") {
		t.Errorf("AAPL block missing Stock leg:\n%s", out)
	}
	// TSLA has no stock leg — no "Stock" line should appear under it. Find
	// the TSLA section and verify.
	tslaIdx := strings.Index(out, "TSLA")
	if tslaIdx < 0 {
		t.Fatalf("TSLA block not found")
	}
	tslaBlock := out[tslaIdx:]
	if strings.Contains(tslaBlock, "    Stock ") {
		t.Errorf("TSLA pure-options group should not have Stock line:\n%s", tslaBlock)
	}
	if !strings.Contains(out, "Group") {
		t.Errorf("missing Group totals:\n%s", out)
	}
}

// Realized P&L is rendered only when at least one row carries a non-zero
// value, otherwise the column is omitted to avoid dead width.
func TestPositionsRealizedColumnOnlyShownWhenNonZero(t *testing.T) {
	t.Parallel()
	t.Run("all zero realized → no column", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		res := &rpc.PositionsResult{
			Stocks: []rpc.PositionView{
				{Symbol: "AAPL", Quantity: 100, AvgCost: 192, Mark: 207, MarketValue: 20700, UnrealizedPnL: 1500},
			},
		}
		_ = renderPositionsText(env, res)
		// "UNREAL P&L" contains "REAL P&L" as a substring, so look for the
		// realized header preceded by whitespace (table column separator).
		if strings.Contains(stdout.String(), "  REAL P&L") {
			t.Errorf("expected REAL P&L column to be hidden when all zero:\n%s", stdout.String())
		}
	})
	t.Run("one non-zero realized → column present", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		res := &rpc.PositionsResult{
			Stocks: []rpc.PositionView{
				{Symbol: "AAPL", Quantity: 100, AvgCost: 192, Mark: 207, MarketValue: 20700, UnrealizedPnL: 1500, RealizedPnL: 220.50},
			},
		}
		_ = renderPositionsText(env, res)
		out := stdout.String()
		if !strings.Contains(out, "  REAL P&L") {
			t.Errorf("expected REAL P&L column when row carries non-zero:\n%s", out)
		}
		if !strings.Contains(out, "220.50") {
			t.Errorf("expected realized value rendered:\n%s", out)
		}
	})
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
