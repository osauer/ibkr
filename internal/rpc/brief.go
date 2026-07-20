package rpc

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
)

const (
	// MethodBriefSnapshot composes the operator's daily brief. It is a pure
	// read: callers do not supply an origin and the daemon must not stamp,
	// journal, or advance any runtime clock while serving it.
	MethodBriefSnapshot = "brief.snapshot"
	// MethodBriefAck records the human attestation associated with a rendered
	// brief. The daemon accepts human origins only.
	MethodBriefAck = "brief.ack"

	// Brief row statuses separate risk conditions from data conditions:
	// attention means the underlying VALUES describe a state a trader must
	// look at (latched drawdown, breached tier, active override); degraded
	// and unavailable describe input quality only and must never be used to
	// signal a risk condition, nor vice versa.
	BriefStatusOK          = "ok"
	BriefStatusAttention   = "attention"
	BriefStatusDegraded    = "degraded"
	BriefStatusUnavailable = "unavailable"

	BriefKindMorning = "morning"
	BriefKindEOD     = "eod"
	BriefKindMonthly = "monthly"

	BriefMonthlyPulseNotDue    = "not_due"
	BriefMonthlyPulseDue       = "due"
	BriefMonthlyPulseCompleted = "completed"
	BriefMonthlyPulseBlocked   = "blocked"

	// Render evidence proves only that a paired surface rendered the brief;
	// origin alone must never be treated as stronger proof of human attention.
	BriefAckEvidenceRender = risk.MonthlyPulseEvidenceRender
)

// BriefSnapshotParams is deliberately empty. In particular it carries no
// origin: reads never gain write authority from their caller.
type BriefSnapshotParams struct{}

// BriefAckParams identifies the exact rendered brief being attested.
type BriefAckParams struct {
	Kind             string `json:"kind"`
	BriefFingerprint string `json:"brief_fingerprint"`
	Month            string `json:"month,omitempty"`
	Evidence         string `json:"evidence,omitempty"`
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
	Month            string    `json:"month,omitempty"`
	Evidence         string    `json:"evidence,omitempty"`
	Message          string    `json:"message,omitempty"`
}

// BriefRowState is embedded by every brief row and section. Detail is
// human-facing disclosure; Status is one of ok, attention, degraded, or
// unavailable. Sections roll up their worst child (attention outranks
// degraded) and state completeness in Detail.
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

// BriefMoversRow aggregates daily P&L by underlying (stock plus option legs
// per name — the same basis as the Underlyings panel) so the two surfaces
// reconcile. OtherPnLBase/OtherCount carry the residual beyond the top rows
// so the row's implied total matches the account daily P&L attribution.
type BriefMoversRow struct {
	BriefRowState
	Rows         []BriefMover `json:"rows"`
	OtherPnLBase *float64     `json:"other_daily_pnl_base,omitempty"`
	OtherCount   int          `json:"other_count,omitempty"`
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
	// PeakAsOf is when the current adjusted peak was observed. Provenance,
	// not decoration: a peak stamped during a closed session or a reconnect
	// window is the tell that exposes a poisoned observation.
	PeakAsOf     time.Time `json:"peak_as_of,omitzero"`
	BaseCurrency string    `json:"base_currency,omitempty"`
}

type BriefLatchRow struct {
	BriefRowState
	Latched bool      `json:"latched"`
	At      time.Time `json:"latched_at,omitzero"`
	AgeDays *int      `json:"age_days,omitempty"`
	// ConsumedPctAtLatch is the consumed share recorded when the latch
	// engaged, so later data glitches cannot rewrite why it fired.
	ConsumedPctAtLatch *float64 `json:"consumed_pct_at_latch,omitempty"`
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
	Reconcile    BriefReconcileRow     `json:"reconcile"`
	AutoExtend   BriefAutoExtendRow    `json:"auto_extend"`
	OneTap       BriefOneTapRow        `json:"one_tap"`
	RulesDelta   BriefRulesDeltaRow    `json:"rules_delta"`
	Artefacts    BriefArtefactsRow     `json:"artefacts"`
	MonthlyPulse *BriefMonthlyPulseRow `json:"monthly_pulse,omitempty"`
}

// BriefMonthlyPulseRow has its own status vocabulary rather than embedding
// BriefRowState (whose status is ok|degraded|unavailable). It remains optional
// until the later daemon composition lands, preserving current brief identity.
type BriefMonthlyPulseRow struct {
	Status      string    `json:"status"` // not_due | due | completed | blocked
	Month       string    `json:"month,omitempty"`
	DueAt       time.Time `json:"due_at,omitzero"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
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

// BriefProposalsRow reports how many protection proposals were offered versus
// acted on over the most recent recorded session, derived read-only from the
// trade-proposal-outcomes journal. It carries counts and the covered day only:
// no proposal keys, symbols, order references, or tokens reach the wire.
type BriefProposalsRow struct {
	BriefRowState
	Day     string `json:"day,omitempty"`
	Offered int    `json:"offered"`
	Acted   int    `json:"acted"`
}

// BriefCapitalEventsRow frames the drawdown latch and adjusted-peak provenance
// as post-trade capital events for the Review movement. The fields mirror the
// existing latch/peak facts; nothing new is invented.
type BriefCapitalEventsRow struct {
	BriefRowState
	Latched            bool      `json:"latched"`
	LatchedAt          time.Time `json:"latched_at,omitzero"`
	LatchAgeDays       *int      `json:"latch_age_days,omitempty"`
	ConsumedPctAtLatch *float64  `json:"consumed_pct_at_latch,omitempty"`
	AdjustedPeakBase   *float64  `json:"adjusted_peak_base,omitempty"`
	PeakAsOf           time.Time `json:"peak_as_of,omitzero"`
	BaseCurrency       string    `json:"base_currency,omitempty"`
}

// BriefReviewSection is the post-trade movement over the last completed
// session. Its rows are a regrouping of existing brief facts (plus the
// read-only proposals-offered-vs-acted derivation); the section rolls up its
// worst child exactly like every other brief section.
type BriefReviewSection struct {
	BriefRowState
	SessionPnL    BriefAccountRow       `json:"session_pnl"`
	Attribution   BriefMoversRow        `json:"attribution"`
	RulesDelta    BriefRulesDeltaRow    `json:"rules_delta"`
	Proposals     BriefProposalsRow     `json:"proposals"`
	Overrides     BriefOverridesRow     `json:"overrides"`
	CapitalEvents BriefCapitalEventsRow `json:"capital_events"`
	Reconcile     BriefReconcileRow     `json:"reconcile"`
	AutoExtend    BriefAutoExtendRow    `json:"auto_extend"`
	OneTap        BriefOneTapRow        `json:"one_tap"`
	WorkingOrders BriefCountRow         `json:"working_orders"`
}

// BriefReadySection is the pre-trade movement for today. Its rows regroup the
// existing market, calendar, risk-capacity, and desk-readiness facts.
type BriefReadySection struct {
	BriefRowState
	Regime        BriefRegimeRow        `json:"regime"`
	Breadth       BriefBreadthRow       `json:"breadth"`
	Gamma         BriefGammaRow         `json:"gamma"`
	Canary        BriefCanaryRow        `json:"canary"`
	Session       BriefSessionRow       `json:"session"`
	MarketEvents  []BriefMarketEventRow `json:"market_events"`
	Capital       BriefCapitalRow       `json:"capital"`
	Latch         BriefLatchRow         `json:"latch"`
	PremiumAtRisk BriefMoneyCoverageRow `json:"premium_at_risk"`
	HedgeCost     BriefMoneyCoverageRow `json:"hedge_cost"`
	PolicyDrift   BriefPolicyDriftRow   `json:"policy_drift"`
	Artefacts     BriefArtefactsRow     `json:"artefacts"`
	MonthlyPulse  *BriefMonthlyPulseRow `json:"monthly_pulse,omitempty"`
}

// BriefResult is the complete typed daily brief, composed as two process
// movements: Review (post-trade of the last completed session) and Ready
// (pre-trade for today). BriefFingerprint hashes the two composed movements
// only; AsOf and stamp-target state are deliberately outside the content
// identity. The daemon composes both movements; surfaces render them verbatim.
type BriefResult struct {
	AsOf              time.Time          `json:"as_of"`
	BriefFingerprint  string             `json:"brief_fingerprint"`
	StampTarget       string             `json:"stamp_target,omitempty"`
	StampTargetReason string             `json:"stamp_target_reason,omitempty"`
	Review            BriefReviewSection `json:"review"`
	Ready             BriefReadySection  `json:"ready"`
}
