package rpc

import "time"

const (
	// MethodBriefSnapshot composes the operator's daily brief. It is a pure
	// read: callers do not supply an origin and the daemon must not stamp,
	// journal, or advance any runtime clock while serving it.
	MethodBriefSnapshot = "brief.snapshot"
	// MethodBriefAck records the human attestation associated with a rendered
	// brief. The daemon accepts human origins only.
	MethodBriefAck = "brief.ack"

	BriefStatusOK          = "ok"
	BriefStatusDegraded    = "degraded"
	BriefStatusUnavailable = "unavailable"

	BriefKindMorning = "morning"
	BriefKindEOD     = "eod"
)

// BriefSnapshotParams is deliberately empty. In particular it carries no
// origin: reads never gain write authority from their caller.
type BriefSnapshotParams struct{}

// BriefAckParams identifies the exact rendered brief being attested.
type BriefAckParams struct {
	Kind             string `json:"kind"`
	BriefFingerprint string `json:"brief_fingerprint"`
	Origin           string `json:"origin,omitempty"`
}

// BriefAckResult reports a new stamp or an idempotent already-complete no-op.
type BriefAckResult struct {
	OK               bool      `json:"ok"`
	Kind             string    `json:"kind"`
	Day              string    `json:"day"`
	At               time.Time `json:"at"`
	AlreadyStamped   bool      `json:"already_stamped,omitempty"`
	BriefFingerprint string    `json:"brief_fingerprint,omitempty"`
	Message          string    `json:"message,omitempty"`
}

// BriefRowState is embedded by every brief row and section. Detail is
// human-facing disclosure; Status is one of ok, degraded, or unavailable.
type BriefRowState struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type BriefMarketSection struct {
	BriefRowState
	Regime  BriefRegimeRow  `json:"regime"`
	Breadth BriefBreadthRow `json:"breadth"`
	Gamma   BriefGammaRow   `json:"gamma"`
	Canary  BriefCanaryRow  `json:"canary"`
}

type BriefRegimeRow struct {
	BriefRowState
	Stage   string `json:"stage,omitempty"`
	Verdict string `json:"verdict,omitempty"`
}

type BriefBreadthRow struct {
	BriefRowState
	PctAbove50DMA  *float64  `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA *float64  `json:"pct_above_200dma,omitempty"`
	NetNewHighsPct *float64  `json:"net_new_highs_pct,omitempty"`
	AsOf           time.Time `json:"as_of,omitzero"`
	DataType       string    `json:"data_type,omitempty"`
}

type BriefGammaRow struct {
	BriefRowState
	Spot      *float64  `json:"spot,omitempty"`
	ZeroGamma *float64  `json:"zero_gamma,omitempty"`
	GapPct    *float64  `json:"gap_pct,omitempty"`
	GammaSign string    `json:"gamma_sign,omitempty"`
	AsOf      time.Time `json:"as_of,omitzero"`
}

type BriefCanaryRow struct {
	BriefRowState
	Action   string `json:"action,omitempty"`
	Severity string `json:"severity,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

type BriefCalendarSection struct {
	BriefRowState
	Session      BriefSessionRow       `json:"session"`
	MarketEvents []BriefMarketEventRow `json:"market_events"`
}

type BriefSessionRow struct {
	BriefRowState
	Market   string    `json:"market,omitempty"`
	State    string    `json:"state,omitempty"`
	IsOpen   bool      `json:"is_open"`
	Open     time.Time `json:"open,omitzero"`
	Close    time.Time `json:"close,omitzero"`
	NextOpen time.Time `json:"next_open,omitzero"`
}

// BriefMarketEventRow summarizes one approved held-name event family.
type BriefMarketEventRow struct {
	BriefRowState
	Kind    string   `json:"kind"` // earnings | halt | ssr | borrow
	Count   int      `json:"count"`
	Symbols []string `json:"symbols,omitempty"`
}

type BriefPortfolioSection struct {
	BriefRowState
	Account       BriefAccountRow       `json:"account"`
	Movers        BriefMoversRow        `json:"movers"`
	PremiumAtRisk BriefMoneyCoverageRow `json:"premium_at_risk"`
	HedgeCost     BriefMoneyCoverageRow `json:"hedge_cost"`
	WorkingOrders BriefCountRow         `json:"working_orders"`
}

type BriefAccountRow struct {
	BriefRowState
	EquityBase   *float64  `json:"equity_base,omitempty"`
	DailyPnLBase *float64  `json:"daily_pnl_base,omitempty"`
	BaseCurrency string    `json:"base_currency,omitempty"`
	AsOf         time.Time `json:"as_of,omitzero"`
}

type BriefMover struct {
	Symbol       string  `json:"symbol"`
	DailyPnLBase float64 `json:"daily_pnl_base"`
}

type BriefMoversRow struct {
	BriefRowState
	Rows []BriefMover `json:"rows"`
}

type BriefMoneyCoverageRow struct {
	BriefRowState
	AmountBase   *float64 `json:"amount_base,omitempty"`
	BaseCurrency string   `json:"base_currency,omitempty"`
	IncludedLegs int      `json:"included_legs"`
	ExcludedLegs int      `json:"excluded_legs"`
}

type BriefCountRow struct {
	BriefRowState
	Count *int `json:"count,omitempty"`
}

type BriefRiskSection struct {
	BriefRowState
	Capital     BriefCapitalRow     `json:"capital"`
	Latch       BriefLatchRow       `json:"latch"`
	Overrides   BriefOverridesRow   `json:"overrides"`
	PolicyDrift BriefPolicyDriftRow `json:"policy_drift"`
}

type BriefCapitalRow struct {
	BriefRowState
	Tier             string   `json:"tier,omitempty"`
	Enforcement      string   `json:"enforcement,omitempty"`
	ConsumedPct      *float64 `json:"consumed_pct,omitempty"`
	DrawdownBase     *float64 `json:"drawdown_base,omitempty"`
	AdjustedPeakBase *float64 `json:"adjusted_peak_base,omitempty"`
	BaseCurrency     string   `json:"base_currency,omitempty"`
}

type BriefLatchRow struct {
	BriefRowState
	Latched bool      `json:"latched"`
	At      time.Time `json:"latched_at,omitzero"`
	AgeDays *int      `json:"age_days,omitempty"`
}

type BriefOverride struct {
	Control   string    `json:"control"`
	ExpiresAt time.Time `json:"expires_at"`
}

type BriefOverridesRow struct {
	BriefRowState
	Rows []BriefOverride `json:"rows"`
}

type BriefPolicyDriftRow struct {
	BriefRowState
	Rows []PolicyPinStatus `json:"rows"`
}

type BriefProcessSection struct {
	BriefRowState
	Reconcile  BriefReconcileRow  `json:"reconcile"`
	AutoExtend BriefAutoExtendRow `json:"auto_extend"`
	OneTap     BriefOneTapRow     `json:"one_tap"`
	RulesDelta BriefRulesDeltaRow `json:"rules_delta"`
	Artefacts  BriefArtefactsRow  `json:"artefacts"`
}

type BriefReconcileRow struct {
	BriefRowState
	LastReconciledAt time.Time `json:"last_reconciled_at,omitzero"`
	Source           string    `json:"source,omitempty"`
	Deadline         time.Time `json:"deadline,omitzero"`
	DaysRemaining    *int      `json:"days_remaining,omitempty"`
}

type BriefAutoExtendRow struct {
	BriefRowState
	ReportID string    `json:"report_id,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

type BriefOneTapRow struct {
	BriefRowState
	ReportID string   `json:"report_id,omitempty"`
	Signable bool     `json:"signable"`
	Blockers []string `json:"blockers,omitempty"`
}

type BriefRuleTransition struct {
	RuleID string `json:"rule_id"`
	From   string `json:"from"`
	To     string `json:"to"`
}

type BriefRulesDeltaRow struct {
	BriefRowState
	BaselineAt                 time.Time             `json:"baseline_at,omitzero"`
	Transitions                []BriefRuleTransition `json:"transitions,omitempty"`
	Added                      []string              `json:"added,omitempty"`
	Removed                    []string              `json:"removed,omitempty"`
	RulebookFingerprintChanged bool                  `json:"rulebook_fingerprint_changed"`
	BaselineFingerprint        string                `json:"baseline_fingerprint,omitempty"`
	CurrentFingerprint         string                `json:"current_fingerprint,omitempty"`
}

type BriefArtefact struct {
	BriefRowState
	Kind        string    `json:"kind"`
	Cadence     string    `json:"cadence"` // daily | weekly
	Declared    bool      `json:"declared"`
	Completed   bool      `json:"completed"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

type BriefArtefactsRow struct {
	BriefRowState
	Rows []BriefArtefact `json:"rows"`
}

// BriefResult is the complete typed daily brief. BriefFingerprint hashes the
// five composed sections only; AsOf and stamp-target state are deliberately
// outside the content identity.
type BriefResult struct {
	AsOf              time.Time             `json:"as_of"`
	BriefFingerprint  string                `json:"brief_fingerprint"`
	StampTarget       string                `json:"stamp_target,omitempty"`
	StampTargetReason string                `json:"stamp_target_reason,omitempty"`
	Market            BriefMarketSection    `json:"market"`
	Calendar          BriefCalendarSection  `json:"calendar"`
	Portfolio         BriefPortfolioSection `json:"portfolio"`
	RiskLimits        BriefRiskSection      `json:"risk_limits"`
	Process           BriefProcessSection   `json:"process"`
}
