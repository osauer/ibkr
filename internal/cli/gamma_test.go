package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// gammaReadyFixture returns a realistic Ready-state envelope: combined
// SPY+SPX scope, agreement on long-γ, non-zero magnitude, populated
// citations + convention so default-mode and --explain assertions have
// the same payload to query.
func gammaReadyFixture() *rpc.GammaZeroSPXResult {
	now := time.Date(2026, 5, 23, 4, 25, 0, 0, time.UTC) // 06:25 CEST
	spy := &rpc.GammaZeroComputed{
		Scope:                   rpc.GammaZeroScopeSPY,
		SpotUnderlying:          743.73,
		SpotAt:                  now,
		GammaSign:               "positive",
		GammaTotalAbs:           1.8e9,
		GammaTotalAbsConvention: "sign-agnostic",
		LegCount:                1052,
		PricedLegCount:          1200,
		Method:                  "bs-gamma-profile-v3-stickymoneyness-0dte-split",
		Source:                  "SPY",
		AsOf:                    now,
	}
	spx := &rpc.GammaZeroComputed{
		Scope:                   rpc.GammaZeroScopeSPX,
		SpotUnderlying:          5430.0,
		GammaSign:               "positive",
		GammaTotalAbs:           4.2e9,
		GammaTotalAbsConvention: "sign-agnostic",
		LegCount:                2150,
		PricedLegCount:          2400,
		Method:                  "bs-gamma-profile-v3-stickymoneyness-0dte-split",
		Source:                  "SPX",
		AsOf:                    now,
	}
	combined := &rpc.GammaZeroComputed{
		Scope:                   rpc.GammaZeroScopeCombined,
		GammaTotalAbs:           6.0e9,
		GammaTotalAbsConvention: "sign-agnostic",
		LegCount:                3202,
		PricedLegCount:          3600,
		Params:                  rpc.GammaZeroParams{StrikeWidthPct: 0.10},
		Expirations:             []string{"2026-05-26", "2026-05-29"},
		Method:                  "bs-gamma-profile-v3-stickymoneyness-0dte-split",
		Source:                  "SPY+SPX",
		AsOf:                    now,
		PerIndex:                map[string]*rpc.GammaZeroComputed{"SPY": spy, "SPX": spx},
		RegimeAgreement:         "agree:long-gamma",
		MethodologyCitations: []string{
			"Perfiliev (2022) — BS-sweep baseline",
			"Derman / Daglish-Hull-Suo — sticky-moneyness skew dynamics",
			"SqueezeMetrics (2017) — naive-sign GEX, deprecated 2022+",
			"Cboe 2025 — 0DTE = ~59% of SPX volume",
		},
	}
	return &rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: combined,
	}
}

func TestRenderGammaSkippedBannerUsesWarningDetailsAfterJSONRoundTrip(t *testing.T) {
	t.Parallel()
	wire := &rpc.GammaZeroComputed{
		WarningDetails: []rpc.GammaWarningDetail{
			{Code: "spx_unavailable:zero_magnitude", Scope: "SPX"},
		},
		Warnings: []string{"spx_unavailable:zero_magnitude"},
	}
	var payload []byte
	var err error
	if payload, err = json.Marshal(wire); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTrip rpc.GammaZeroComputed
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(roundTrip.Warnings) != 0 {
		t.Fatalf("warnings should be internal-only after JSON round trip: %v", roundTrip.Warnings)
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if !renderGammaSkippedBanner(env, &roundTrip) {
		t.Fatal("expected SPX skipped banner from warning_details")
	}
	if got := stdout.String(); !strings.Contains(got, "SPX skipped") ||
		!strings.Contains(got, "zero usable gamma magnitude") {
		t.Fatalf("banner did not explain SPX skip: %q", got)
	}
}

// TestRenderGamma_HeroHasTitleTimestampAnchor pins the shared hero
// shape applied to gamma: a title on the same line as a timestamp
// (joined with "  ·  "), followed by an indented SPY spot anchor.
func TestRenderGamma_HeroHasTitleTimestampAnchor(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	// Title + timestamp share one line.
	if !strings.Contains(out, "Dealer gamma · SPY+SPX") {
		t.Errorf("hero title missing:\n%s", out)
	}
	if !strings.Contains(out, "  ·  ") {
		t.Errorf("hero title/timestamp separator missing:\n%s", out)
	}
	// Anchor — SPY spot price, indented two spaces.
	if !strings.Contains(out, "  SPY 743.73") {
		t.Errorf("hero anchor with SPY spot missing:\n%s", out)
	}
}

func TestRenderGamma_HeroSummaryColorFollowsRegime(t *testing.T) {
	t.Parallel()

	t.Run("long gamma is green", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
		if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
			t.Fatalf("code=%d", code)
		}
		out := stdout.String()
		if !strings.Contains(out, ansiBold+ansiGreen+"SPY and SPX both long-γ") {
			t.Fatalf("long-gamma hero summary should be bold green:\n%q", out)
		}
	})

	t.Run("short gamma is red", func(t *testing.T) {
		t.Parallel()
		fix := gammaReadyFixture()
		fix.Result.RegimeAgreement = "agree:short-gamma"
		for _, sub := range fix.Result.PerIndex {
			sub.GammaSign = "negative"
		}

		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
		if code := renderGammaText(env, fix, false); code != 0 {
			t.Fatalf("code=%d", code)
		}
		out := stdout.String()
		if !strings.Contains(out, ansiBold+ansiRed+"SPY and SPX both short-γ") {
			t.Fatalf("short-gamma hero summary should be bold red:\n%q", out)
		}
	})
}

// TestRenderGamma_DefaultOmitsMetadataBlock pins U1: the metadata block
// (Skew model, Method, Source, Compute, Derived IV, Leg count, Scope)
// no longer renders in default mode. Citations and the sign-convention
// disclosure are also gated behind --explain.
func TestRenderGamma_DefaultOmitsMetadataBlock(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, banned := range []string{
		"Method      bs-gamma",
		"Source      ",
		"Leg count   ",
		"Skew model",
		"Citations",
		"Perfiliev (2022)",
		"Disclosure:",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("default render must not surface %q (--explain only):\n%s", banned, out)
		}
	}
}

// TestRenderGamma_DefaultShowsMagnitudeCoPrimary pins U1.5: magnitude
// is promoted to a peer line with the convention label drawn from the
// wire's GammaTotalAbsConvention field.
func TestRenderGamma_DefaultShowsMagnitudeCoPrimary(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Magnitude") {
		t.Errorf("default render should surface a Magnitude line:\n%s", out)
	}
	if !strings.Contains(out, "$6.00B per 1% move") {
		t.Errorf("Magnitude line should carry formatted total + per-move suffix:\n%s", out)
	}
	if !strings.Contains(out, "(sign-agnostic)") {
		t.Errorf("Magnitude line should label convention from wire:\n%s", out)
	}
}

// TestRenderGamma_MagnitudeOmittedWhenZero pins the conditional render:
// a Result with GammaTotalAbs=0 (no-aggregator-data case) skips the
// Magnitude line entirely rather than printing "$0.00 per 1% move".
func TestRenderGamma_MagnitudeOmittedWhenZero(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.GammaTotalAbs = 0
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if strings.Contains(out, "Magnitude") {
		t.Errorf("zero magnitude should omit the Magnitude line:\n%s", out)
	}
}

// TestRenderGamma_ColdRendersFriendlyExplainer pins the cold-state UX:
// when the daemon returns Status=cold (e.g. off-hours with no v3 cache
// yet), the renderer prints a clear explainer instead of the harsh
// "without a result payload" defensive fallback. Includes the hint to
// pass --force.
func TestRenderGamma_ColdRendersFriendlyExplainer(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusCold}
	if code := renderGammaText(env, res, false); code != 0 {
		t.Fatalf("cold should exit 0, got %d", code)
	}
	out := stdout.String()
	if strings.Contains(out, "without a result payload") {
		t.Errorf("cold path should not emit the defensive fallback:\n%s", out)
	}
	for _, want := range []string{
		"no data yet",
		"cold cache",
		"first call of each NY",
		"--force",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cold explainer missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderGamma_ColdRendersDaemonReason(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.GammaZeroSPXResult{
		Status:     rpc.GammaZeroStatusCold,
		ColdReason: "persisted gamma cache for spy+spx was rejected: per_index[SPX]: zero-gamma invalid result",
		ColdAction: "Run `ibkr gamma --force` for a diagnostic off-hours recompute.",
	}
	if code := renderGammaText(env, res, false); code != 0 {
		t.Fatalf("cold should exit 0, got %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Reason      persisted gamma cache",
		"per_index[SPX]",
		"diagnostic off-hours recompute",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cold reason render missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderGamma_ExplainSurfacesMetadata pins B6: with --explain set,
// the methodology block + citations + sign-convention disclosure all
// render. The citations come verbatim from MethodologyCitations.
func TestRenderGamma_ExplainSurfacesMetadata(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Method      bs-gamma-profile-v3-stickymoneyness-0dte-split",
		"Source      SPY+SPX",
		"Leg count   3202",
		"Citations",
		"Perfiliev (2022) — BS-sweep baseline",
		"Cboe 2025 — 0DTE",
		"Disclosure:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderGamma_DefaultShowsPerIndexCompact pins U1.4: combined-mode
// default render surfaces one compact line per index ("SPY  no crossing
// · long-γ · 1052 legs") instead of the old multi-line Per-index block.
func TestRenderGamma_DefaultShowsPerIndexCompact(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"SPY   no crossing · long-γ · 1052 GEX legs",
		"SPX   no crossing · long-γ · 2150 GEX legs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("per-index compact line missing %q:\n%s", want, out)
		}
	}
}

// TestRenderGamma_ConvenienceConventionFallback pins the legacy-daemon
// case: when GammaTotalAbsConvention is empty (older daemon), the
// renderer falls back to "sign-agnostic" rather than emitting a stray
// "()" parenthetical.
func TestRenderGamma_ConvenienceConventionFallback(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.GammaTotalAbsConvention = ""
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "(sign-agnostic)") {
		t.Errorf("missing-convention fallback should be 'sign-agnostic':\n%s", out)
	}
}

// TestGammaHeroSummary_SingleAndCombined pins the hero summary line
// across the three Scope shapes: combined uses formatRegimeAgreement;
// SPY/SPX-only produce a compact single-index regime statement.
func TestGammaHeroSummary_SingleAndCombined(t *testing.T) {
	t.Parallel()
	mk := func(scope, sign string, zg *float64) *rpc.GammaZeroSPXResult {
		return &rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				Scope:           scope,
				SpotUnderlying:  743.73,
				GammaSign:       sign,
				ZeroGamma:       zg,
				RegimeAgreement: "agree:long-gamma",
				PerIndex: map[string]*rpc.GammaZeroComputed{
					"SPY": {Scope: rpc.GammaZeroScopeSPY, GammaSign: "positive"},
					"SPX": {Scope: rpc.GammaZeroScopeSPX, GammaSign: "positive"},
				},
			},
		}
	}
	cases := []struct {
		name  string
		input *rpc.GammaZeroSPXResult
		want  string
	}{
		{"combined", mk(rpc.GammaZeroScopeCombined, "positive", nil), "SPY and SPX both long-γ"},
		{"spy_only_long", mk(rpc.GammaZeroScopeSPY, "positive", nil), "SPY long-γ"},
		{"spx_only_short", mk(rpc.GammaZeroScopeSPX, "negative", nil), "SPX short-γ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gammaHeroSummary(tc.input)
			if !strings.Contains(got, tc.want) {
				t.Errorf("summary=%q want substring %q", got, tc.want)
			}
		})
	}
}

// TestRenderGamma_ExplainCarriesScalingCaveat pins C4: the
// --explain block always carries a short scaling caveat naming the
// S² scaling and states that zero-gamma levels stay per-index rather
// than pretending the combined envelope has one price scale.
func TestRenderGamma_ExplainCarriesScalingCaveat(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Scaling",
		"S² scaling",
		"combined |Γ|·OI sums the books",
		"zero-gamma levels stay per-index",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain missing scaling-caveat marker %q:\n%s", want, out)
		}
	}
}
