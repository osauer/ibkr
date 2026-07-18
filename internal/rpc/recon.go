package rpc

import "time"

// Post-trade reconciliation contract (docs/design/post-trade-truth.md).
// recon.snapshot is read-only and works from retained statement files;
// recon.dismiss is a human-only governance write. Nothing here touches
// broker writes, submit eligibility, or the order path.

const (
	// MethodReconSnapshot regenerates and returns the reconciliation
	// report from retained Flex statements and the declared capital-event
	// ledger. Params may request a background statement fetch.
	MethodReconSnapshot = "recon.snapshot"
	// MethodReconBacktest builds the full-window backtest report: every
	// statement flow labeled for the operator's flow-list review, plus the
	// capital-ladder replay over the statement equity series
	// (docs/design/operator-ergonomics.md, accelerated R3 gate).
	// Measurement only — it changes no matching, sign-off, or enforcement.
	MethodReconBacktest = "recon.backtest"
	// MethodReconDismiss records a human resolution for one exception
	// line: reviewed, explained, deliberately not a ledger event.
	MethodReconDismiss = "recon.dismiss"
)

// Recon report statuses.
const (
	ReconStatusActive      = "active"      // report produced under approved recon keys
	ReconStatusUnapproved  = "unapproved"  // [recon] policy keys missing; no matching possible
	ReconStatusUnavailable = "unavailable" // no retained statements yet
	ReconStatusDegraded    = "degraded"    // report produced but some retained files failed to parse
)

// Recon exception categories.
const (
	ReconMissingFromLedger = "missing_from_ledger"
	ReconLedgerOnly        = "ledger_only"
	ReconAmountMismatch    = "amount_mismatch"
	ReconDateMismatch      = "date_mismatch"
	ReconAmbiguous         = "ambiguous"
	ReconUncategorized     = "uncategorized"
)

// ReconSnapshotParams tunes one snapshot call.
type ReconSnapshotParams struct {
	// Refresh kicks one background statement fetch (single-flight); the
	// returned report is still built from already-retained files.
	Refresh bool `json:"refresh,omitempty"`
}

// ReconException is one line the operator must look at (or has resolved
// by dismissal). Amounts are base-currency and stay on this local surface.
type ReconException struct {
	LineID      string    `json:"line_id"`
	Category    string    `json:"category"`
	Type        string    `json:"type,omitempty"`
	Description string    `json:"description,omitempty"`
	ValueDate   time.Time `json:"value_date,omitzero"`
	AmountBase  *float64  `json:"amount_base,omitempty"`
	// EventAt/EventAmountBase reference the declared event side of a
	// mismatch or ledger_only exception.
	EventAt         time.Time `json:"event_at,omitzero"`
	EventAmountBase *float64  `json:"event_amount_base,omitempty"`
	// PreGenesis marks a flow value-dated before the runtime capital
	// state's genesis: it is already embedded in the seeded baseline and,
	// once reviewed, resolves by a journaled dismissal — it is disclosed
	// here, never auto-excluded (a restatement inside an attested window
	// must still surface).
	PreGenesis    bool   `json:"pre_genesis,omitempty"`
	Note          string `json:"note,omitempty"`
	Dismissed     bool   `json:"dismissed,omitempty"`
	DismissReason string `json:"dismiss_reason,omitempty"`
}

// ReconEquityCheck compares the statement equity series with the runtime
// capital state — a data-quality disclosure, not an exception. Divergence
// is computed only from a same-day pair; when SameDay is false,
// RuntimeEquityBase and RuntimeAsOf are the latest observation for context
// only and DivergencePct is absent.
type ReconEquityCheck struct {
	StatementDate      time.Time `json:"statement_date,omitzero"`
	StatementTotalBase float64   `json:"statement_total_base"`
	RuntimeEquityBase  *float64  `json:"runtime_equity_base,omitempty"`
	RuntimeAsOf        time.Time `json:"runtime_as_of,omitzero"`
	DivergencePct      *float64  `json:"divergence_pct,omitempty"`
	SameDay            bool      `json:"same_day"`
}

// ReconBacktestFlow labels one statement flow for the operator's full-window
// flow-list review.
type ReconBacktestFlow struct {
	LineID      string    `json:"line_id"`
	Type        string    `json:"type,omitempty"`
	Description string    `json:"description,omitempty"`
	ValueDate   time.Time `json:"value_date,omitzero"`
	AmountBase  *float64  `json:"amount_base,omitempty"`
	PreGenesis  bool      `json:"pre_genesis,omitempty"`
	// Status is "matched" or the recon exception category the flow
	// carries on the current report.
	Status    string `json:"status"`
	Dismissed bool   `json:"dismissed,omitempty"`
}

// ReconBacktestCrossing compares the first replayed crossing of one capital
// tier with the runtime journal observation, when recorded.
type ReconBacktestCrossing struct {
	Tier                string    `json:"tier"` // warn | block
	ReplayedAt          time.Time `json:"replayed_at,omitzero"`
	ReplayedConsumedPct float64   `json:"replayed_consumed_pct"`
	RuntimeAt           time.Time `json:"runtime_at,omitzero"`
}

// ReconBacktestReplay is the capital-ladder replay over broker statement EOD
// equity, with the comparable runtime observations disclosed alongside it.
type ReconBacktestReplay struct {
	Days                    int                     `json:"days"`
	FirstDay                time.Time               `json:"first_day,omitzero"`
	LastDay                 time.Time               `json:"last_day,omitzero"`
	ReplayedPeakBase        float64                 `json:"replayed_peak_base"`
	ReplayedPeakAt          time.Time               `json:"replayed_peak_at,omitzero"`
	RuntimePeakBase         *float64                `json:"runtime_peak_base,omitempty"`
	RuntimePeakAt           time.Time               `json:"runtime_peak_at,omitzero"`
	PeakDivergencePct       *float64                `json:"peak_divergence_pct,omitempty"`
	Crossings               []ReconBacktestCrossing `json:"crossings,omitempty"`
	SameDayComparisons      int                     `json:"same_day_comparisons"`
	MaxSameDayDivergencePct *float64                `json:"max_same_day_divergence_pct,omitempty"`
	Notes                   []string                `json:"notes,omitempty"`
}

// ReconBacktestResult is the full-window recon backtest payload. It is
// read-only measurement and changes no matching, sign-off, or enforcement.
type ReconBacktestResult struct {
	AsOf               time.Time            `json:"as_of"`
	Status             string               `json:"status"`
	ReportID           string               `json:"report_id,omitempty"`
	StatementAsOf      time.Time            `json:"statement_as_of,omitzero"`
	CoverageFrom       time.Time            `json:"coverage_from,omitzero"`
	CoverageTo         time.Time            `json:"coverage_to,omitzero"`
	GenesisAt          time.Time            `json:"genesis_at,omitzero"`
	PolicyFingerprint  *Fingerprint         `json:"policy_fingerprint,omitempty"`
	Flows              []ReconBacktestFlow  `json:"flows,omitempty"`
	FlowCounts         map[string]int       `json:"flow_counts,omitempty"`
	ClassifiedCounts   map[string]int       `json:"classified_counts,omitempty"`
	UncategorizedCount int                  `json:"uncategorized_count"`
	EquityDays         int                  `json:"equity_days"`
	Replay             *ReconBacktestReplay `json:"replay,omitempty"`
	Message            string               `json:"message,omitempty"`
	InputHealth        []SourceHealth       `json:"input_health,omitempty"`
}

// ReconFetchStatus reports statement-source health. It never carries the
// token or any request detail.
type ReconFetchStatus struct {
	Configured  bool      `json:"configured"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
}

// ReconResult is the recon.snapshot payload.
type ReconResult struct {
	AsOf   time.Time `json:"as_of"`
	Status string    `json:"status"`
	// ReportID pins the exact exception set; the reconcile verb must
	// reference it and refuses when unresolved exceptions remain.
	ReportID string `json:"report_id,omitempty"`
	// StatementAsOf is when the newest ingested statement was generated
	// by IBKR — the freshness the max_report_age_days policy key bounds.
	StatementAsOf time.Time         `json:"statement_as_of,omitzero"`
	CoverageFrom  time.Time         `json:"coverage_from,omitzero"`
	CoverageTo    time.Time         `json:"coverage_to,omitzero"`
	Counts        map[string]int    `json:"counts,omitempty"`
	Exceptions    []ReconException  `json:"exceptions,omitempty"`
	Unresolved    int               `json:"unresolved"`
	Equity        *ReconEquityCheck `json:"equity,omitempty"`
	Fetch         ReconFetchStatus  `json:"fetch"`
	Message       string            `json:"message,omitempty"`
	InputHealth   []SourceHealth    `json:"input_health,omitempty"`
}

// ReconDismissParams records one human resolution.
type ReconDismissParams struct {
	LineID string `json:"line_id"`
	Reason string `json:"reason"`
	Origin string `json:"origin,omitempty"`
}
