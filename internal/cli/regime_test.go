package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// These tests pin the current "plain dump" renderer one call at a time.
// They exist as a safety net before the v0.22.0 redesign (composite badge
// + one-line indicators + compressed thresholds). When the new renderer
// lands, replace this file with tests that pin the new shape — do not
// keep both, because the visible strings these assert on (`(ok)`,
// `(stale)`, the raw status enum in parens) are exactly what the redesign
// removes.

// regimeFixture returns a regime envelope shaped after the spec's live-
// test snippet (docs/specs/risk-regime-dashboard.md, "Live-test result on
// 2026-05-17"). One row in each state the renderer branches on:
//   - VIX:    ok, ratio populated
//   - HYG:    stale, primary spot landed, 52w high missing
//   - USDJPY: ok, weekly change populated
//   - gamma:  computing, ETA + progress hint
//   - breadth: unavailable (S5FI not entitled on retail IBKR)
//
// Adjust per-test for the specific branch under test.
func regimeFixture() *rpc.RegimeSnapshotResult {
	vix := 18.43
	vix3m := 21.36
	ratio := vix / vix3m
	hyg := 79.55
	spy := 737.34
	hyg50 := 80.10
	usdjpy := 158.7285
	weekly := 0.42
	return &rpc.RegimeSnapshotResult{
		AsOf:    time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC),
		SpecDoc: "docs/specs/risk-regime-dashboard.md",
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status:   rpc.RegimeStatusOK,
			VIX:      &vix,
			VIX3M:    &vix3m,
			Ratio:    &ratio,
			DataType: rpc.MarketDataLive,
			Notes:    "VIX/VIX3M notes",
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:        rpc.RegimeStatusStale,
			HYGPrice:      &hyg,
			HYG50DMA:      &hyg50,
			SPYPrice:      &spy,
			Notes:         "HYG/SPY notes",
			FieldsMissing: []string{"spy_52w_high"},
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status:       rpc.RegimeStatusOK,
			Symbol:       "USD.JPY",
			Last:         &usdjpy,
			WeeklyChange: &weekly,
			Notes:        "USD/JPY notes",
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusComputing,
			Envelope: rpc.GammaZeroSPXResult{
				Status:     rpc.GammaZeroStatusComputing,
				EtaSeconds: 42,
				Progress:   40,
			},
			Notes: "gamma notes",
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusUnavailable,
			Notes:  "breadth notes",
		},
	}
}

// TestRenderRegimeText_AllFiveIndicatorsRender pins the headline shape:
// each of the five numbered rows is present, in spec order. The redesign
// preserves the indicator set but flattens to one line per row — this
// test will be rewritten to assert the new badge column.
func TestRenderRegimeText_AllFiveIndicatorsRender(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, regimeFixture()); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Risk Regime Snapshot",
		"1. VIX/VIX3M ratio",
		"2. HYG vs SPY divergence",
		"3. USD/JPY",
		"4. SPX dealer zero-gamma",
		"5. SPX breadth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestRenderRegimeText_VIXRatioPopulated pins the OK-path value line for
// indicator 1: VIX and VIX3M values + computed ratio + status enum in
// parens. The status-in-parens is one of the readability smells the
// redesign removes.
func TestRenderRegimeText_VIXRatioPopulated(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	for _, want := range []string{"VIX 18.43", "VIX3M 21.36", "0.863", "(ok)"} {
		if !strings.Contains(out, want) {
			t.Errorf("VIX row missing %q\n%s", want, out)
		}
	}
}

// TestRenderRegimeText_FieldsMissingAdvisoryRenders pins the "(missing:
// spy_52w_high)" advisory line under the HYG row. Today this is the only
// hint a reader gets that a sub-field didn't land.
func TestRenderRegimeText_FieldsMissingAdvisoryRenders(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	if !strings.Contains(out, "missing: spy_52w_high") {
		t.Errorf("expected fields_missing advisory on HYG row:\n%s", out)
	}
}

// TestRenderRegimeText_GammaComputingShowsETA pins the loading-state
// branch for indicator 4: ETA + progress + a "re-run later" hint instead
// of a value. The redesign collapses this to a single inline cell.
func TestRenderRegimeText_GammaComputingShowsETA(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	for _, want := range []string{"computing", "eta 42s", "progress 40%", "re-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("gamma row missing %q\n%s", want, out)
		}
	}
}

// TestRenderRegimeText_BreadthUnavailable pins the structurally-missing
// row: breadth shows status + a pointer to the notes for the reason,
// no fake zero value.
func TestRenderRegimeText_BreadthUnavailable(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	if !strings.Contains(out, "unavailable") {
		t.Errorf("breadth row should surface status verbatim:\n%s", out)
	}
	if strings.Contains(out, "0.0%") {
		t.Errorf("unavailable breadth must not render a fake zero value:\n%s", out)
	}
}
