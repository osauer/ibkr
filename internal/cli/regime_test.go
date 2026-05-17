package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// regimeFixture returns the realistic mid-session envelope used across
// every renderer test: VIX OK + live, HYG stale + yellow band, USD/JPY
// OK + weekly change, gamma computing + ETA, breadth structurally
// unavailable. Mirrors the cmd/_preview fixture so visual and test
// expectations stay synchronised.
func regimeFixture() *rpc.RegimeSnapshotResult {
	vix := 18.43
	vix3m := 21.36
	ratio := vix / vix3m
	hyg := 79.55
	spy := 737.34
	hyg50 := 80.10
	spy52 := 749.30
	usdjpy := 158.7285
	close7 := 158.05
	weekly := 0.43
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
			Status:     rpc.RegimeStatusStale,
			HYGPrice:   &hyg,
			HYG50DMA:   &hyg50,
			SPYPrice:   &spy,
			SPY52WHigh: &spy52,
			Notes:      "HYG/SPY notes",
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status:       rpc.RegimeStatusOK,
			Symbol:       "USD.JPY",
			Last:         &usdjpy,
			Close7DAgo:   &close7,
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

// TestRenderRegime_CompositeVerdictAndCount pins the headline: bold
// verdict line per the spec's interpretation table, dim count summary
// naming ranked and unranked separately. The fixture has 2 green + 1
// yellow + 0 red ranked rows, so the verdict is "Normal regime".
func TestRenderRegime_CompositeVerdictAndCount(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, regimeFixture()); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Risk Regime",
		"Normal regime",
		"2 green",
		"1 yellow",
		"0 red",
		"3 of 5 ranked",
		"2 unranked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composite header missing %q\n%s", want, out)
		}
	}
}

// TestRenderRegime_RowsFitOneLineEach pins the compressed layout: each
// of the five indicators occupies exactly one row in the rendered body
// (excluding header, composite, blank, and footer lines). The old
// renderer used 4 lines per indicator; the redesign collapses to one.
func TestRenderRegime_RowsFitOneLineEach(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	indicators := []string{"VIX/VIX3M", "HYG vs SPY", "USD/JPY", "SPX γ-zero", "SPX breadth"}
	for _, name := range indicators {
		hits := strings.Count(stdout.String(), name)
		if hits != 1 {
			t.Errorf("%s should appear on exactly one row (got %d):\n%s", name, hits, stdout.String())
		}
	}
}

// TestRenderRegime_BandWordsAppearOnRankedRows pins the band column:
// ranked rows carry "green" / "yellow" / "red" verbatim; unranked rows
// use the em-dash placeholder. Color escapes are filtered out via the
// no-color env so this asserts on raw text shape.
func TestRenderRegime_BandWordsAppearOnRankedRows(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	for _, want := range []string{"green", "yellow"} {
		if !strings.Contains(out, want) {
			t.Errorf("band word %q missing:\n%s", want, out)
		}
	}
}

// TestRenderRegime_GammaComputingInlinesETA pins the computing-state
// row: ETA + progress sit in the value cell, not on a separate "re-run"
// hint line. The old renderer wrote three lines for this state; the
// redesign collapses to one.
func TestRenderRegime_GammaComputingInlinesETA(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	for _, want := range []string{"computing", "42s ETA", "40%"} {
		if !strings.Contains(out, want) {
			t.Errorf("gamma row missing %q on the same line:\n%s", want, out)
		}
	}
}

// TestRenderRegime_BreadthUnavailableReasonInline pins the structurally-
// missing row: the reason ("S5FI feed not entitled") sits in the dim
// parenthetical, the value cell shows "unavailable". No fake zero.
func TestRenderRegime_BreadthUnavailableReasonInline(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	if !strings.Contains(out, "unavailable") {
		t.Errorf("breadth row should surface unavailable in value cell:\n%s", out)
	}
	if !strings.Contains(out, "S5FI") {
		t.Errorf("breadth row should surface the reason inline:\n%s", out)
	}
	if strings.Contains(out, "0.0%") {
		t.Errorf("unavailable breadth must not render a fake zero value:\n%s", out)
	}
}

// TestRenderRegime_NoSpecDocLine pins the removed-feature: the old
// renderer echoed the spec-doc path on stdout, which served no human
// purpose (the JSON envelope still carries it for programmatic
// consumers).
func TestRenderRegime_NoSpecDocLine(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	if strings.Contains(stdout.String(), "Spec doc:") {
		t.Errorf("spec-doc path should not echo to stdout in the default render:\n%s", stdout.String())
	}
}

// TestRenderRegime_ExplainModeIncludesNotes pins the --explain footer:
// when explain=true, the renderer appends the daemon's per-indicator
// notes prose verbatim. Default mode (explain=false) omits them, with
// a one-line hint about --explain instead.
func TestRenderRegime_ExplainModeIncludesNotes(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, regimeFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{"VIX/VIX3M notes", "HYG/SPY notes", "USD/JPY notes", "gamma notes", "breadth notes"} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderRegime_DefaultOmitsNotesHasHint pins the default-mode
// shape: the giant spec-prose notes do not appear, replaced by a
// one-line "pass --explain" hint at the bottom.
func TestRenderRegime_DefaultOmitsNotesHasHint(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	if strings.Contains(out, "VIX/VIX3M notes") {
		t.Errorf("default render should omit the full notes prose:\n%s", out)
	}
	if !strings.Contains(out, "--explain") {
		t.Errorf("default render should hint at --explain:\n%s", out)
	}
}

// ----- composite-table verdict mapping (spec table, lines 109-115) -----

func TestRegimeComposite_VerdictTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		green  int
		yellow int
		red    int
		total  int
		want   string
	}{
		{"all green = normal", 5, 0, 0, 5, "Normal regime"},
		{"two yellow = normal", 3, 2, 0, 5, "Normal regime"},
		{"three yellow = elevated", 2, 3, 0, 5, "Elevated alert — review positioning"},
		{"five yellow = elevated", 0, 5, 0, 5, "Elevated alert — review positioning"},
		{"one red = watch", 2, 2, 1, 5, "Watch closely, prep defensive moves"},
		{"two red = watch", 1, 2, 2, 5, "Watch closely, prep defensive moves"},
		{"three red = regime shift", 0, 2, 3, 5, "Regime shift likely — execute pre-committed plan"},
		{"four red = regime shift", 0, 1, 4, 5, "Regime shift likely — execute pre-committed plan"},
		{"five red (full ranked) = full risk-off", 0, 0, 5, 5, "Full risk-off conditions"},
		// Coverage edge: 3 red but two unranked → still "regime shift", not "full"
		{"three red with two unranked", 0, 0, 3, 5, "Regime shift likely — execute pre-committed plan"},
		// Coverage edge: nothing ranked → no verdict claim
		{"all unranked", 0, 0, 0, 5, "No ranked indicators — see rows below for state"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := regimeComposite{green: tc.green, yellow: tc.yellow, red: tc.red, total: tc.total}
			c.ranked = c.green + c.yellow + c.red
			c.unranked = c.total - c.ranked
			if got := c.verdict(); got != tc.want {
				t.Errorf("verdict for %v = %q, want %q", tc, got, tc.want)
			}
		})
	}
}

// TestRegimeRow_VIXBands pins the VIX/VIX3M row's threshold mapping
// against the spec's three-band split (<0.92 green; 0.92-1.00 yellow;
// >1.00 red). The renderer's defaults track the spec verbatim.
func TestRegimeRow_VIXBands(t *testing.T) {
	t.Parallel()
	mk := func(r float64) rpc.RegimeVIXTerm {
		v, v3 := 18.0, r*18.0
		return rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK, VIX: &v, VIX3M: &v3, Ratio: &r}
	}
	cases := []struct {
		ratio float64
		want  regimeBand
	}{
		{0.85, bandGreen},
		{0.91, bandGreen},
		{0.92, bandYellow},
		{0.99, bandYellow},
		{1.00, bandRed},
		{1.10, bandRed},
	}
	for _, tc := range cases {
		got := rowVIXTerm(mk(tc.ratio)).band
		if got != tc.want {
			t.Errorf("ratio=%.2f band=%v want %v", tc.ratio, got, tc.want)
		}
	}
}

// TestRegimeRow_USDJPYWatchesYenStrengthening pins the asymmetric
// banding: yen *strengthening* (USD/JPY *falling*) is the risk signal,
// not USD strengthening. WeeklyChange convention: positive when USD
// rose, negative when USD fell — so -1% is the start of the yellow
// band, -2% the start of red.
func TestRegimeRow_USDJPYWatchesYenStrengthening(t *testing.T) {
	t.Parallel()
	mk := func(chg float64) rpc.RegimeUSDJPY {
		last := 158.0
		return rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, Last: &last, WeeklyChange: &chg}
	}
	cases := []struct {
		chg  float64
		want regimeBand
		why  string
	}{
		{+0.5, bandGreen, "USD up 0.5%/wk is calm"},
		{+5.0, bandGreen, "USD strengthening alone is not the risk signal"},
		{-0.5, bandGreen, "yen up 0.5%/wk is calm"},
		{-1.0, bandYellow, "yen +1% triggers yellow"},
		{-1.9, bandYellow, "still yellow"},
		{-2.0, bandRed, "yen +2% triggers red"},
		{-5.0, bandRed, "stays red"},
	}
	for _, tc := range cases {
		got := rowUSDJPY(mk(tc.chg)).band
		if got != tc.want {
			t.Errorf("chg=%+.1f%% band=%v want %v (%s)", tc.chg, got, tc.want, tc.why)
		}
	}
}

// TestRegimeRow_HYGSPYUnrankedWhen52WMissing pins the renderer's
// honesty-floor: when HYG is below its 50dma but SPY's 52w high
// didn't land, the yellow-band trigger ("SPY within 3% of high") is
// unknown — so the row is unranked rather than guessed at.
func TestRegimeRow_HYGSPYUnrankedWhen52WMissing(t *testing.T) {
	t.Parallel()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	row := rowHYGSPY(rpc.RegimeHYGSPYDivergence{
		Status:   rpc.RegimeStatusOK,
		HYGPrice: &hyg, HYG50DMA: &hyg50,
		SPYPrice: &spy, // SPY52WHigh nil
	})
	if row.band != bandUnranked {
		t.Errorf("HYG<50dma + 52w missing should leave row unranked, got %v", row.band)
	}
	if !strings.Contains(row.reason, "spy_52w_high") {
		t.Errorf("reason should name the missing field for the reader: %q", row.reason)
	}
}
