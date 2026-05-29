package risk

// Policy holds the shared risk thresholds used by live monitors and planners.
// The first version is intentionally small: it captures the canary's current
// policy in one place so risk-plan can consume the same vocabulary later.
type Policy struct {
	Name string `json:"name"`

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
}

func DefaultPolicy() Policy {
	return Policy{
		Name: "canary-default",

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
	}
}
