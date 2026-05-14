package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestRenderAccount_DailyPnLBlock pins the new Daily P&L block: visible
// when DailyPnL is non-nil, with the optional unrealized/realized lines
// when those decompositions are populated.
func TestRenderAccount_DailyPnLBlock(t *testing.T) {
	t.Parallel()
	daily := 1247.30
	unreal := 962.10
	real_ := 285.20
	a := &rpc.AccountResult{
		AccountID:          "U1234567",
		BaseCurrency:       "USD",
		NetLiquidation:     100000,
		DailyPnL:           &daily,
		DailyPnLUnrealized: &unreal,
		DailyPnLRealized:   &real_,
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderAccountText(env, a); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{"Daily P&L", "1,247.30", "of which unrealized", "962.10", "of which realized", "285.20"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestRenderAccount_DailyPnLSilentWhenNil — when the daemon couldn't
// reach reqPnL (pre-handshake, unentitled, gateway down), no block
// renders at all. Silent failure, not "—" placeholder lines.
func TestRenderAccount_DailyPnLSilentWhenNil(t *testing.T) {
	t.Parallel()
	a := &rpc.AccountResult{
		AccountID:      "U1234567",
		BaseCurrency:   "USD",
		NetLiquidation: 100000,
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderAccountText(env, a); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if strings.Contains(out, "Daily P&L") {
		t.Errorf("Daily P&L line should be omitted when DailyPnL is nil:\n%s", out)
	}
}

// TestRenderAccount_DailyPnLOnlyBareDaily — older gateway versions
// emit just the bare dailyPnL field. The block renders, but the
// unrealized/realized lines stay hidden.
func TestRenderAccount_DailyPnLOnlyBareDaily(t *testing.T) {
	t.Parallel()
	daily := 50.00
	a := &rpc.AccountResult{
		AccountID:      "U1234567",
		BaseCurrency:   "USD",
		NetLiquidation: 100000,
		DailyPnL:       &daily,
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderAccountText(env, a)
	out := stdout.String()
	if !strings.Contains(out, "Daily P&L") {
		t.Errorf("Daily P&L line should render:\n%s", out)
	}
	if strings.Contains(out, "of which unrealized") {
		t.Errorf("unrealized sub-line should be omitted on short-form payloads:\n%s", out)
	}
	if strings.Contains(out, "of which realized") {
		t.Errorf("realized sub-line should be omitted on short-form payloads:\n%s", out)
	}
}

// TestRenderPositions_DailyColumnVisibleWhenPopulated — at least one
// row carries a non-nil DailyPnL → DAILY P&L column header appears.
func TestRenderPositions_DailyColumnVisibleWhenPopulated(t *testing.T) {
	t.Parallel()
	d := 47.30
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", Quantity: 100, AvgCost: 134.20, Mark: 145.22,
				MarketValue: 14522, UnrealizedPnL: 1102, DailyPnL: &d},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	if !strings.Contains(out, "DAILY P&L") {
		t.Errorf("missing DAILY P&L column header:\n%s", out)
	}
	if !strings.Contains(out, "47.30") {
		t.Errorf("missing daily value:\n%s", out)
	}
}

// TestRenderPositions_DailyColumnSuppressedWhenAllNil — every row has
// DailyPnL == nil. The column is suppressed entirely (no header, no
// column of em-dashes).
func TestRenderPositions_DailyColumnSuppressedWhenAllNil(t *testing.T) {
	t.Parallel()
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", Quantity: 100, AvgCost: 134.20, Mark: 145.22,
				MarketValue: 14522, UnrealizedPnL: 1102},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	if strings.Contains(out, "DAILY P&L") {
		t.Errorf("DAILY P&L column should be hidden when no row has a value:\n%s", out)
	}
}

// TestRenderPositions_DailyColumnEmDashOnMixed — at least one row has
// a value (so the column renders) and another row has nil (so it
// shows em-dash). Pins the "honest mixed state" contract.
func TestRenderPositions_DailyColumnEmDashOnMixed(t *testing.T) {
	t.Parallel()
	d := 100.00
	res := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", Quantity: 100, AvgCost: 100, Mark: 105,
				MarketValue: 10500, UnrealizedPnL: 500, DailyPnL: &d},
			{Symbol: "NVDA", Quantity: 50, AvgCost: 400, Mark: 410,
				MarketValue: 20500, UnrealizedPnL: 500}, // DailyPnL nil
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderPositionsText(env, res)
	out := stdout.String()
	if !strings.Contains(out, "DAILY P&L") {
		t.Errorf("DAILY P&L column should appear:\n%s", out)
	}
	if !strings.Contains(out, "100.00") {
		t.Errorf("missing populated daily value:\n%s", out)
	}
	// NVDA's line carries an em-dash for daily — the same glyph the
	// rest of the renderer uses for nil values.
	if !strings.Contains(out, "—") {
		t.Errorf("expected em-dash for NVDA's nil daily P&L:\n%s", out)
	}
}

// TestFormatPnLCcyRight_RendersSignedAndPrefixed pins the helper used by
// renderDailyPnL: positive renders with currency prefix, negative is
// preserved, exactly-zero is a real number (not an em-dash).
func TestFormatPnLCcyRight_RendersSignedAndPrefixed(t *testing.T) {
	t.Parallel()
	env := &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	// Color off so we test the bare string layer.
	if got := env.formatPnLCcyRight(1247.30, "USD", 0); !strings.Contains(got, "$ 1,247.30") {
		t.Errorf("positive USD = %q, want contains \"$ 1,247.30\"", got)
	}
	if got := env.formatPnLCcyRight(-50.00, "EUR", 0); !strings.Contains(got, "-€ 50.00") {
		t.Errorf("negative EUR = %q, want contains \"-€ 50.00\"", got)
	}
	if got := env.formatPnLCcyRight(0, "USD", 0); strings.Contains(got, "—") {
		t.Errorf("zero P&L should render as \"$ 0.00\", not em-dash: %q", got)
	}
}

// TestFormatPnLCcyPtrRight_NilEmDash pins the nil-as-em-dash path.
func TestFormatPnLCcyPtrRight_NilEmDash(t *testing.T) {
	t.Parallel()
	env := &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	got := env.formatPnLCcyPtrRight(nil, "USD", 14)
	if !strings.Contains(got, "—") {
		t.Errorf("nil should produce em-dash placeholder: %q", got)
	}
}
