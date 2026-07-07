package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// RulebookPolicy carries the operator-tunable thresholds for the 12-rule
// daily trading rulebook (docs/design/trading-rulebook.md). Sibling
// thresholds measuring the same aggregation live in the canary policy
// (compiled, single-name watch 35) and the protection policy (TOML,
// risk-reduction target 25); bars differ by surface question, observations
// must not — see the aggregation-consistency test.
type RulebookPolicy struct {
	ID      string `toml:"id" json:"id"`
	Version int    `toml:"version" json:"version"`

	// Rule 1 — single_name_exposure (% of NLV, delta-dollar exposure).
	SingleNameWatchPct float64 `toml:"single_name_watch_pct" json:"single_name_watch_pct"`
	SingleNameActPct   float64 `toml:"single_name_act_pct" json:"single_name_act_pct"`

	// Rule 2 — option_line_premium (% of NLV, long option line market value).
	OptionLineWatchPct float64 `toml:"option_line_watch_pct" json:"option_line_watch_pct"`
	OptionLineActPct   float64 `toml:"option_line_act_pct" json:"option_line_act_pct"`

	// Rule 3 — cash_sell_only (% of NLV; act when cash ratio is below).
	CashSellOnlyPct float64 `toml:"cash_sell_only_pct" json:"cash_sell_only_pct"`

	// Rule 4 — extrinsic_budget (% of NLV, Σ long-option extrinsic).
	ExtrinsicWatchPct float64 `toml:"extrinsic_watch_pct" json:"extrinsic_watch_pct"`
	ExtrinsicActPct   float64 `toml:"extrinsic_act_pct" json:"extrinsic_act_pct"`

	// Rule 5 — expiry_runway (calendar DTE bounds for long options).
	RunwayWatchDTE      int     `toml:"runway_watch_dte" json:"runway_watch_dte"`
	RunwayActDTE        int     `toml:"runway_act_dte" json:"runway_act_dte"`
	RunwayITMDeltaFloor float64 `toml:"runway_itm_delta_floor" json:"runway_itm_delta_floor"`

	// Rule 8 — earnings_size_freeze (US sessions to earnings).
	EarningsFreezeSessions int `toml:"earnings_freeze_sessions" json:"earnings_freeze_sessions"`

	// Rules 9/10 — tape thresholds (day-change %).
	RedOnGreenNameDropPct float64 `toml:"red_on_green_name_drop_pct" json:"red_on_green_name_drop_pct"`
	RedOnGreenSPYUpPct    float64 `toml:"red_on_green_spy_up_pct" json:"red_on_green_spy_up_pct"`
	WinnerTrimDayUpPct    float64 `toml:"winner_trim_day_up_pct" json:"winner_trim_day_up_pct"`
	WinnerTrimMinExpoPct  float64 `toml:"winner_trim_min_exposure_pct" json:"winner_trim_min_exposure_pct"`

	// Rule 12 — hedge_integrity band (% of gross long delta-dollars).
	HedgeBandMinPct float64 `toml:"hedge_band_min_pct" json:"hedge_band_min_pct"`
	HedgeBandMaxPct float64 `toml:"hedge_band_max_pct" json:"hedge_band_max_pct"`
	// HedgeSymbols is the policy-owned index list whose long puts classify
	// as hedges (rules 5 and 12). Uppercased on load.
	HedgeSymbols []string `toml:"hedge_symbols" json:"hedge_symbols"`

	// GreeksGapFloorPctNLV is the materiality floor: a name whose legs
	// missing delta exceed this notional share of NLV renders its
	// exposure-dependent rows unknown instead of silently understating.
	GreeksGapFloorPctNLV float64 `toml:"greeks_gap_floor_pct_nlv" json:"greeks_gap_floor_pct_nlv"`

	// EarningsStaleDays bounds trust in a fetched earnings date; older
	// observations flip rules 6-8 to unknown until refreshed or overridden.
	EarningsStaleDays int `toml:"earnings_stale_days" json:"earnings_stale_days"`
}

// DefaultRulebookPolicy returns the embedded rulebook-v1 defaults — the
// numbers agreed with the operator on 2026-07-07 from the trader review.
func DefaultRulebookPolicy() RulebookPolicy {
	return RulebookPolicy{
		ID:                     "rulebook-v1",
		Version:                1,
		SingleNameWatchPct:     30,
		SingleNameActPct:       40,
		OptionLineWatchPct:     5,
		OptionLineActPct:       10,
		CashSellOnlyPct:        -25,
		ExtrinsicWatchPct:      10,
		ExtrinsicActPct:        15,
		RunwayWatchDTE:         14,
		RunwayActDTE:           7,
		RunwayITMDeltaFloor:    0.70,
		EarningsFreezeSessions: 3,
		RedOnGreenNameDropPct:  -1.5,
		RedOnGreenSPYUpPct:     0.5,
		WinnerTrimDayUpPct:     4,
		WinnerTrimMinExpoPct:   15,
		HedgeBandMinPct:        25,
		HedgeBandMaxPct:        35,
		HedgeSymbols:           []string{"SPY", "SPX", "SPXW", "QQQ", "IWM"},
		GreeksGapFloorPctNLV:   1,
		EarningsStaleDays:      10,
	}
}

// Normalize uppercases and sorts the hedge list so fingerprints are stable
// regardless of TOML ordering.
func (p *RulebookPolicy) Normalize() {
	for i, s := range p.HedgeSymbols {
		p.HedgeSymbols[i] = strings.ToUpper(strings.TrimSpace(s))
	}
	sort.Strings(p.HedgeSymbols)
}

// FingerprintKey hashes the full policy so every result discloses exactly
// which thresholds produced it (mirrors risk.Policy.FingerprintKey).
func (p RulebookPolicy) FingerprintKey() string {
	q := p
	q.Normalize()
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%.4f|%.4f|%.4f|%.4f|%.4f|%.4f|%.4f|%d|%d|%.4f|%d|%.4f|%.4f|%.4f|%.4f|%.4f|%.4f|%s|%.4f|%d",
		q.ID, q.Version,
		q.SingleNameWatchPct, q.SingleNameActPct,
		q.OptionLineWatchPct, q.OptionLineActPct,
		q.CashSellOnlyPct,
		q.ExtrinsicWatchPct, q.ExtrinsicActPct,
		q.RunwayWatchDTE, q.RunwayActDTE, q.RunwayITMDeltaFloor,
		q.EarningsFreezeSessions,
		q.RedOnGreenNameDropPct, q.RedOnGreenSPYUpPct,
		q.WinnerTrimDayUpPct, q.WinnerTrimMinExpoPct,
		q.HedgeBandMinPct, q.HedgeBandMaxPct,
		strings.Join(q.HedgeSymbols, ","),
		q.GreeksGapFloorPctNLV, q.EarningsStaleDays,
	)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// IsHedgeSymbol reports whether sym is on the policy hedge list.
func (p RulebookPolicy) IsHedgeSymbol(sym string) bool {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	return slices.Contains(p.HedgeSymbols, sym)
}
