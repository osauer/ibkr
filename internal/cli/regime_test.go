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
	indicators := []string{"VIX/VIX3M", "HYG vs SPY", "USD/JPY", "SPY γ-zero", "SPX breadth"}
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
		// Honesty floor: a positive "Normal regime" verdict requires
		// at least verdictFloor (3) ranked rows. Below that the
		// renderer surfaces "Insufficient signal" instead of bold-
		// green-on-thin-coverage. Confirmed in review: the original
		// v0.22.0 dashboard on weekend frozen data printed "Normal
		// regime" with 1 of 5 ranked — exactly the misleading state
		// this floor blocks.
		{"one green ranked = insufficient", 1, 0, 0, 5, "Insufficient signal — too few indicators ranked"},
		{"two green ranked = insufficient", 2, 0, 0, 5, "Insufficient signal — too few indicators ranked"},
		{"one red + one yellow = insufficient (below floor even with reds)", 0, 1, 1, 5, "Insufficient signal — too few indicators ranked"},
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
		got := rowVIXTerm(time.Now(), mk(tc.ratio)).band
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
		got := rowUSDJPY(time.Now(), mk(tc.chg)).band
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
	row := rowHYGSPY(time.Now(), rpc.RegimeHYGSPYDivergence{
		Status:   rpc.RegimeStatusOK,
		HYGPrice: &hyg, HYG50DMA: &hyg50,
		SPYPrice: &spy, // SPY52WHigh nil
	})
	if row.band != bandUnranked {
		t.Errorf("HYG<50dma + 52w missing should leave row unranked, got %v", row.band)
	}
}

// ----- per-scalar provenance (Quality envelope, v0.24.x+) -----

// TestRenderRegime_FirmLiveOmitsQualityTag pins the "no clutter for
// fresh data" rule: when every value on a row came from a firm-live
// gateway tick, the renderer must NOT add a quality annotation. The
// firm-live state IS the default — surfacing it would just add ink.
func TestRenderRegime_FirmLiveOmitsQualityTag(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	vix := 18.43
	vix3m := 21.36
	ratio := vix / vix3m
	row := rowVIXTerm(now, rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusOK, VIX: &vix, VIX3M: &vix3m, Ratio: &ratio,
		VIXQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm,
		},
		VIX3MQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm,
		},
	})
	if row.quality != "" {
		t.Errorf("firm-live row should have empty quality tag, got %q", row.quality)
	}
}

// TestRenderRegime_DerivedSPY52WHighShowsEstimate pins the
// methodology-honesty path: when SPY's 52-week high comes from the
// history-fallback (max(High) over 252 daily bars) rather than the
// live tick-165 (Misc Stats), the row carries a "· est" annotation.
// This is the case the user asked for: a renderer that says
// "this value is derived" rather than presenting it as a firm tick.
func TestRenderRegime_DerivedSPY52WHighShowsEstimate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	derivedAt := now.Add(-18 * time.Second) // history fetch finished 18s before snapshot
	hyg := 79.55
	hyg50 := 80.10
	spy := 737.34
	spy52 := 749.30
	row := rowHYGSPY(now, rpc.RegimeHYGSPYDivergence{
		Status:   rpc.RegimeStatusOK,
		HYGPrice: &hyg, HYG50DMA: &hyg50, SPYPrice: &spy, SPY52WHigh: &spy52,
		HYGQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm,
		},
		HYG50DMAQuality: &rpc.Quality{
			AsOf: derivedAt, FreshnessClass: rpc.FreshnessDerived, Confidence: rpc.ConfidenceEstimate,
		},
		SPYQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm,
		},
		SPY52WHighQuality: &rpc.Quality{
			AsOf: derivedAt, FreshnessClass: rpc.FreshnessDerived, Confidence: rpc.ConfidenceEstimate,
			Source: "SPY 252d max(High) fallback",
		},
	})
	if !strings.Contains(row.quality, "est") {
		t.Errorf("derived-fallback row should carry an 'est' tag, got %q", row.quality)
	}
	if !strings.Contains(row.quality, "18s") {
		t.Errorf("derived row 18s old should include age in tag, got %q", row.quality)
	}
}

// TestRenderRegime_GammaModelledAnnotation pins the gamma row's
// methodology disclosure: the BS-sweep zero-flip estimate carries a
// "modelled" tag because it's not a measurement — it's a model with
// documented caveats (sign convention, sticky IV during sweep).
func TestRenderRegime_GammaModelledAnnotation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	flip := 5125.4
	gap := 1.5
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{ZeroGamma: &flip, GapPct: &gap, Method: "perfiliev-bs-sweep-v1"},
		},
		ZeroGammaQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessModelled, Confidence: rpc.ConfidenceProxy,
			Source: "perfiliev-bs-sweep-v1",
		},
	})
	if !strings.Contains(row.quality, "modelled") {
		t.Errorf("gamma row should carry 'modelled' tag, got %q", row.quality)
	}
}

// TestRenderRegime_ExplainIncludesQualityBlocks pins the --explain
// path: every populated Quality envelope surfaces as a per-scalar
// provenance line, so a reader can audit any single number without
// reading the methodology spec. The block names the field, the
// confidence + freshness, and the source string.
func TestRenderRegime_ExplainIncludesQualityBlocks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	fix := regimeFixture()
	fix.AsOf = now
	// Attach a sampling of Quality envelopes covering each kind.
	fix.VIXTermStructure.VIXQuality = &rpc.Quality{
		AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm, Source: "VIX tick",
	}
	fix.HYGSPYDivergence.SPY52WHighQuality = &rpc.Quality{
		AsOf: now.Add(-18 * time.Second), FreshnessClass: rpc.FreshnessDerived, Confidence: rpc.ConfidenceEstimate,
		Source: "SPY 252d max(High) fallback",
	}
	// Gamma fixture is in "computing" state — replace with Ready+modelled.
	flip := 5125.4
	gap := 1.5
	fix.GammaZero.Status = rpc.RegimeStatusOK
	fix.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{ZeroGamma: &flip, GapPct: &gap, Method: "perfiliev-bs-sweep-v1"},
	}
	fix.GammaZero.ZeroGammaQuality = &rpc.Quality{
		AsOf: now, FreshnessClass: rpc.FreshnessModelled, Confidence: rpc.ConfidenceProxy,
		Source: "perfiliev-bs-sweep-v1",
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, fix, true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"VIX tick", "firm", "live",
		"SPY 252d max(High) fallback", "estimate", "derived",
		"perfiliev-bs-sweep-v1", "modelled", "proxy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing Quality marker %q:\n%s", want, out)
		}
	}
}

// TestRenderRegime_NilQualityFallsBackToStaleTick pins back-compat:
// rows with nil Quality (e.g. legacy daemons, or paths that never
// populated provenance) fall back to the pre-Quality renderer behaviour
// — Status==Stale surfaces "· stale tick" exactly as before. Nothing
// should panic, and the JSON-shape consumers see no regression.
func TestRenderRegime_NilQualityFallsBackToStaleTick(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	// regimeFixture has all Quality pointers nil and HYGSPYDivergence at
	// Status=Stale — the legacy path should produce "· stale tick".
	if code := renderRegimeText(env, regimeFixture()); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "stale tick") {
		t.Errorf("nil-Quality stale row should show '· stale tick' suffix:\n%s", out)
	}
	for _, fresh := range []string{"· est", "· modelled", "· frozen"} {
		if strings.Contains(out, fresh) {
			t.Errorf("nil-Quality render should not invent quality tag %q:\n%s", fresh, out)
		}
	}
}

// ----- v0.26.0: gamma row no-crossing branches + jargon cleanup -----

// TestRegimeRow_GammaPositiveSignBandsGreen pins the new no-crossing
// branch: when the swept profile never crosses zero AND the whole
// profile is positive, the row ranks green (dealer long-γ across the
// window, stabilizing regime). Previously this hid behind an
// "unavailable" glyph; now it surfaces as a real regime statement.
func TestRegimeRow_GammaPositiveSignBandsGreen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				GammaSign:      "positive",
				GammaTotalAbs:  2.7e9,
				// ZeroGamma / GapPct both nil — no crossing in window
			},
		},
	})
	if row.band != bandGreen {
		t.Errorf("positive GammaSign should band green, got %v", row.band)
	}
	if !strings.Contains(row.reason, "long-γ") {
		t.Errorf("positive GammaSign reason should mention long-γ, got %q", row.reason)
	}
	if !strings.Contains(row.value, "737") {
		t.Errorf("value cell should still surface spot, got %q", row.value)
	}
}

// TestRegimeRow_GammaNegativeSignBandsRed pins the amplifying-regime
// branch: short-γ across the whole sweep is a red band, not unranked.
func TestRegimeRow_GammaNegativeSignBandsRed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				GammaSign:      "negative",
				GammaTotalAbs:  3.1e9,
			},
		},
	})
	if row.band != bandRed {
		t.Errorf("negative GammaSign should band red, got %v", row.band)
	}
	if !strings.Contains(row.reason, "short-γ") {
		t.Errorf("negative GammaSign reason should mention short-γ, got %q", row.reason)
	}
}

// TestRegimeRow_GammaNoDataStaysUnranked pins the genuine "empty
// profile" path: with no crossing AND no signed signal, the row stays
// unranked (current behaviour for empty / pathological compute).
func TestRegimeRow_GammaNoDataStaysUnranked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				GammaSign:      "no_data",
			},
		},
	})
	if row.band != bandUnranked {
		t.Errorf("no_data sign should stay unranked, got %v", row.band)
	}
}

// TestRegimeRow_GammaUsesGammaZeroLabel pins the jargon cleanup: the
// value cell uses "γ-zero", not "flip". Same for the reason strings.
// "flip" stays in code identifiers but does not appear in user-facing
// row text.
func TestRegimeRow_GammaUsesGammaZeroLabel(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	zg := 735.0
	gap := 0.5
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				ZeroGamma:      &zg,
				GapPct:         &gap,
			},
		},
	})
	if strings.Contains(row.value, "flip") {
		t.Errorf("value cell must not contain 'flip' jargon: %q", row.value)
	}
	if strings.Contains(row.reason, "flip") {
		t.Errorf("reason must not contain 'flip' jargon: %q", row.reason)
	}
	if !strings.Contains(row.value, "γ-zero") {
		t.Errorf("value cell should label the crossing as 'γ-zero': %q", row.value)
	}
}

// TestQualityTag_ModelledSuppressesShortAge pins the new modelled-age
// behaviour: a model output 37 s old reads as fresh, not stale. The
// "· modelled" tag stays unannotated until the model age crosses 5 min,
// at which point the tag adopts a stale-model warning form.
func TestQualityTag_ModelledSuppressesShortAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	cases := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"1s", 1 * time.Second, "· modelled"},
		{"37s", 37 * time.Second, "· modelled"},
		{"4m_59s", 4*time.Minute + 59*time.Second, "· modelled"},
		{"6m", 6 * time.Minute, "· modelled 6m old"},
		{"45m", 45 * time.Minute, "· modelled 45m old"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &rpc.Quality{
				AsOf:           now.Add(-tc.age),
				FreshnessClass: rpc.FreshnessModelled,
				Confidence:     rpc.ConfidenceProxy,
			}
			got := qualityTag(now, q)
			if got != tc.want {
				t.Errorf("age=%s tag=%q, want %q", tc.age, got, tc.want)
			}
		})
	}
}

// TestRenderRegime_ExplainCarriesGammaZeroExplanation pins the
// renderer's --explain extension: the gamma row carries a plain-English
// paragraph explaining what the level means and how to read the bands,
// so a non-quant reader doesn't need to leave the terminal for the
// methodology spec.
func TestRenderRegime_ExplainCarriesGammaZeroExplanation(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, regimeFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	// Three load-bearing phrases — the read must explain what γ-zero is,
	// what happens above it, and what happens below it.
	for _, want := range []string{"γ-zero", "stabilizing", "amplifying"} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing γ-zero plain-English term %q:\n%s", want, out)
		}
	}
}

// TestRenderRegime_ExplainSurfacesDerivedIVDisclosure pins the
// "compute used N derived IVs" line that fires when any leg used the
// BS-IV Newton-Raphson fallback. The fallback runs pre-market when the
// gateway's model-computation engine is idle; the disclosure makes the
// prior-session-price anchor visible to a reader auditing the result.
func TestRenderRegime_ExplainSurfacesDerivedIVDisclosure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	fix := regimeFixture()
	fix.AsOf = now
	zg := 735.0
	gap := 0.27
	fix.GammaZero.Status = rpc.RegimeStatusOK
	fix.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			SpotUnderlying: 737.0,
			ZeroGamma:      &zg,
			GapPct:         &gap,
			LegCount:       240,
			DerivedIVLegs:  240,
			Method:         "perfiliev-bs-sweep-v1",
		},
	}
	fix.GammaZero.ZeroGammaQuality = &rpc.Quality{
		AsOf: now, FreshnessClass: rpc.FreshnessModelled, Confidence: rpc.ConfidenceProxy,
		Source: "perfiliev-bs-sweep-v1 · BS-IV from prior-session last price",
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, fix, true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{"240/240 legs", "BS-IV", "prior-session"} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing derived-IV disclosure %q:\n%s", want, out)
		}
	}
}
