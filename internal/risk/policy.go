package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const PolicyFingerprintVersion = "risk-policy-fp-v1"

// Policy holds the shared risk thresholds used by live monitors and planners.
// The first version is intentionally small: it captures the canary's current
// policy in one place so risk-plan can consume the same vocabulary later.
type Policy struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Version string `json:"version"`

	MarginUrgentPct float64 `json:"margin_urgent_pct"`
	MarginActPct    float64 `json:"margin_act_pct"`
	MarginWatchPct  float64 `json:"margin_watch_pct"`
	MarginTargetPct float64 `json:"margin_target_pct"`

	GrossExposureWatchPct float64 `json:"gross_exposure_watch_pct"`
	NetDeltaWatchPct      float64 `json:"net_delta_watch_pct"`
	GrossDeltaWatchPct    float64 `json:"gross_delta_watch_pct"`

	GrossExposureStressActPct float64 `json:"gross_exposure_stress_act_pct"`
	NetDeltaStressActPct      float64 `json:"net_delta_stress_act_pct"`
	GrossDeltaStressActPct    float64 `json:"gross_delta_stress_act_pct"`

	GrossExposureStressUrgentPct float64 `json:"gross_exposure_stress_urgent_pct"`
	NetDeltaStressUrgentPct      float64 `json:"net_delta_stress_urgent_pct"`
	GrossDeltaStressUrgentPct    float64 `json:"gross_delta_stress_urgent_pct"`

	SingleNameExposureWatchPct float64 `json:"single_name_exposure_watch_pct"`
	SingleNameDeltaWatchPct    float64 `json:"single_name_delta_watch_pct"`
	SingleNameTargetPct        float64 `json:"single_name_target_pct"`

	OptionGreeksMinCoveragePct float64 `json:"option_greeks_min_coverage_pct"`

	SPYDropPct      float64 `json:"spy_drop_pct"`
	SPYHardDropPct  float64 `json:"spy_hard_drop_pct"`
	SPYCrashPct     float64 `json:"spy_crash_pct"`
	SPYRallyPct     float64 `json:"spy_rally_pct"`
	SPYHardRallyPct float64 `json:"spy_hard_rally_pct"`

	VIXSpikePct     float64 `json:"vix_spike_pct"`
	VIXHardSpikePct float64 `json:"vix_hard_spike_pct"`
	VIXCrushPct     float64 `json:"vix_crush_pct"`
	VIXHardCrushPct float64 `json:"vix_hard_crush_pct"`

	DailyPnLWatchPct float64 `json:"daily_pnl_watch_pct"`
	DailyPnLActPct   float64 `json:"daily_pnl_act_pct"`

	Reduce ReducePolicy `json:"reduce"`
}

type ReducePolicy struct {
	FrontDTE                int     `json:"front_dte"`
	MidDTE                  int     `json:"mid_dte"`
	HedgeOffsetMinPct       float64 `json:"hedge_offset_min_pct"`
	HedgeOffsetMaxPct       float64 `json:"hedge_offset_max_pct"`
	MaxOptionSpreadAbs      float64 `json:"max_option_spread_abs"`
	MaxOptionSpreadPctOfMid float64 `json:"max_option_spread_pct_of_mid"`
	OrderType               string  `json:"order_type"`
	TIF                     string  `json:"tif"`
	AllowMarketOrders       bool    `json:"allow_market_orders"`
}

func DefaultPolicy() Policy {
	return Policy{
		Name:    "active-v1",
		Profile: "active-v1",
		Version: "risk-policy-v1",

		MarginUrgentPct: 10,
		MarginActPct:    20,
		MarginWatchPct:  35,
		MarginTargetPct: 25,

		GrossExposureWatchPct: 150,
		NetDeltaWatchPct:      125,
		GrossDeltaWatchPct:    150,

		GrossExposureStressActPct: 100,
		NetDeltaStressActPct:      80,
		GrossDeltaStressActPct:    100,

		GrossExposureStressUrgentPct: 150,
		NetDeltaStressUrgentPct:      125,
		GrossDeltaStressUrgentPct:    150,

		SingleNameExposureWatchPct: 35,
		SingleNameDeltaWatchPct:    35,
		SingleNameTargetPct:        25,

		OptionGreeksMinCoveragePct: 80,

		SPYDropPct:      -1.5,
		SPYHardDropPct:  -2.5,
		SPYCrashPct:     -4,
		SPYRallyPct:     1.5,
		SPYHardRallyPct: 2.5,

		VIXSpikePct:     10,
		VIXHardSpikePct: 20,
		VIXCrushPct:     -10,
		VIXHardCrushPct: -20,

		DailyPnLWatchPct: 5,
		DailyPnLActPct:   10,

		Reduce: ReducePolicy{
			FrontDTE:                21,
			MidDTE:                  60,
			HedgeOffsetMinPct:       20,
			HedgeOffsetMaxPct:       120,
			MaxOptionSpreadAbs:      0.30,
			MaxOptionSpreadPctOfMid: 25,
			OrderType:               "LMT",
			TIF:                     "DAY",
			AllowMarketOrders:       false,
		},
	}
}

func (p Policy) PolicyProfile() string {
	if p.Profile != "" {
		return p.Profile
	}
	return p.Name
}

func (p Policy) PolicyVersion() string {
	return p.Version
}

func (p Policy) FingerprintKey() string {
	projection := struct {
		Profile string       `json:"profile"`
		Version string       `json:"version"`
		Policy  policyFields `json:"policy"`
	}{
		Profile: p.PolicyProfile(),
		Version: p.PolicyVersion(),
		Policy: policyFields{
			MarginUrgentPct:              p.MarginUrgentPct,
			MarginActPct:                 p.MarginActPct,
			MarginWatchPct:               p.MarginWatchPct,
			MarginTargetPct:              p.MarginTargetPct,
			GrossExposureWatchPct:        p.GrossExposureWatchPct,
			NetDeltaWatchPct:             p.NetDeltaWatchPct,
			GrossDeltaWatchPct:           p.GrossDeltaWatchPct,
			GrossExposureStressActPct:    p.GrossExposureStressActPct,
			NetDeltaStressActPct:         p.NetDeltaStressActPct,
			GrossDeltaStressActPct:       p.GrossDeltaStressActPct,
			GrossExposureStressUrgentPct: p.GrossExposureStressUrgentPct,
			NetDeltaStressUrgentPct:      p.NetDeltaStressUrgentPct,
			GrossDeltaStressUrgentPct:    p.GrossDeltaStressUrgentPct,
			SingleNameExposureWatchPct:   p.SingleNameExposureWatchPct,
			SingleNameDeltaWatchPct:      p.SingleNameDeltaWatchPct,
			SingleNameTargetPct:          p.SingleNameTargetPct,
			OptionGreeksMinCoveragePct:   p.OptionGreeksMinCoveragePct,
			SPYDropPct:                   p.SPYDropPct,
			SPYHardDropPct:               p.SPYHardDropPct,
			SPYCrashPct:                  p.SPYCrashPct,
			SPYRallyPct:                  p.SPYRallyPct,
			SPYHardRallyPct:              p.SPYHardRallyPct,
			VIXSpikePct:                  p.VIXSpikePct,
			VIXHardSpikePct:              p.VIXHardSpikePct,
			VIXCrushPct:                  p.VIXCrushPct,
			VIXHardCrushPct:              p.VIXHardCrushPct,
			DailyPnLWatchPct:             p.DailyPnLWatchPct,
			DailyPnLActPct:               p.DailyPnLActPct,
			Reduce:                       p.Reduce,
		},
	}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type policyFields struct {
	MarginUrgentPct              float64      `json:"margin_urgent_pct"`
	MarginActPct                 float64      `json:"margin_act_pct"`
	MarginWatchPct               float64      `json:"margin_watch_pct"`
	MarginTargetPct              float64      `json:"margin_target_pct"`
	GrossExposureWatchPct        float64      `json:"gross_exposure_watch_pct"`
	NetDeltaWatchPct             float64      `json:"net_delta_watch_pct"`
	GrossDeltaWatchPct           float64      `json:"gross_delta_watch_pct"`
	GrossExposureStressActPct    float64      `json:"gross_exposure_stress_act_pct"`
	NetDeltaStressActPct         float64      `json:"net_delta_stress_act_pct"`
	GrossDeltaStressActPct       float64      `json:"gross_delta_stress_act_pct"`
	GrossExposureStressUrgentPct float64      `json:"gross_exposure_stress_urgent_pct"`
	NetDeltaStressUrgentPct      float64      `json:"net_delta_stress_urgent_pct"`
	GrossDeltaStressUrgentPct    float64      `json:"gross_delta_stress_urgent_pct"`
	SingleNameExposureWatchPct   float64      `json:"single_name_exposure_watch_pct"`
	SingleNameDeltaWatchPct      float64      `json:"single_name_delta_watch_pct"`
	SingleNameTargetPct          float64      `json:"single_name_target_pct"`
	OptionGreeksMinCoveragePct   float64      `json:"option_greeks_min_coverage_pct"`
	SPYDropPct                   float64      `json:"spy_drop_pct"`
	SPYHardDropPct               float64      `json:"spy_hard_drop_pct"`
	SPYCrashPct                  float64      `json:"spy_crash_pct"`
	SPYRallyPct                  float64      `json:"spy_rally_pct"`
	SPYHardRallyPct              float64      `json:"spy_hard_rally_pct"`
	VIXSpikePct                  float64      `json:"vix_spike_pct"`
	VIXHardSpikePct              float64      `json:"vix_hard_spike_pct"`
	VIXCrushPct                  float64      `json:"vix_crush_pct"`
	VIXHardCrushPct              float64      `json:"vix_hard_crush_pct"`
	DailyPnLWatchPct             float64      `json:"daily_pnl_watch_pct"`
	DailyPnLActPct               float64      `json:"daily_pnl_act_pct"`
	Reduce                       ReducePolicy `json:"reduce"`
}
