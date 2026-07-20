package rpc

import "time"

// MethodRegimeHistory serves the regime-decision journal timeline from the
// daemon's derived history index (history.db). Read-only: the JSONL
// journal stays the evidence of record; the index is rebuildable at any
// time by deleting history.db and restarting the daemon.
const MethodRegimeHistory = "regime.history"

// MethodRulesHistory serves the rulebook transition journal timeline from
// the same derived index. Advisory/read-only end to end — nothing in
// these results touches submit eligibility or any broker-write path.
const MethodRulesHistory = "rules.history"

// RegimeHistoryParams selects a window of indexed regime decisions.
// Boundary grammar mirrors orders.history: RFC3339 timestamps or
// YYYY-MM-DD UTC days.
type RegimeHistoryParams struct {
	// Since is the inclusive lower boundary: RFC3339, or YYYY-MM-DD
	// meaning the start of that UTC day. Empty = 7 days before Until.
	Since string `json:"since,omitempty"`
	// Until is the upper boundary: RFC3339 (exclusive), or YYYY-MM-DD
	// meaning that whole UTC day stays included. Empty = now.
	Until string `json:"until,omitempty"`
	// Stage filters on the exact lifecycle stage word (for example
	// early_warning). Empty matches all stages.
	Stage string `json:"stage,omitempty"`
	// Limit caps returned rows, newest first; default 50, max 500.
	Limit int `json:"limit,omitempty"`
}

// RegimeHistoryEntry is one journaled regime decision. Free-text fields
// (Verdict) are journal data for display, never parsed into authority.
type RegimeHistoryEntry struct {
	At                 time.Time `json:"at"`
	SessionKey         string    `json:"session_key,omitempty"`
	TapeSession        string    `json:"tape_session,omitempty"`
	Stage              string    `json:"stage"`
	Severity           string    `json:"severity,omitempty"`
	Readiness          string    `json:"readiness,omitempty"`
	Confidence         string    `json:"confidence,omitempty"`
	Verdict            string    `json:"verdict,omitempty"`
	ClusterRed         int       `json:"cluster_red_count"`
	ClusterYellow      int       `json:"cluster_yellow_count"`
	ClusterEligibleRed int       `json:"cluster_eligible_red_count"`
	Fingerprint        string    `json:"fingerprint,omitempty"`
}

// HistoryIndexHealth discloses index freshness on every history result so
// a catching-up or stalled index is never silently presented as complete:
// when JournalBytes exceeds IngestedBytes, rows may be missing.
type HistoryIndexHealth struct {
	// LastIngestAt is when the index last committed ingested bytes for
	// this source; zero when nothing has been ingested yet.
	LastIngestAt time.Time `json:"last_ingest_at,omitzero"`
	// IngestedBytes is the journal byte watermark fully mirrored into the
	// index (complete lines only).
	IngestedBytes int64 `json:"ingested_bytes"`
	// JournalBytes is the journal file's current on-disk size.
	JournalBytes int64 `json:"journal_bytes"`
}

// RegimeHistoryResult is the regime.history envelope: the filtered window,
// newest first, with total-vs-returned counts and index health.
type RegimeHistoryResult struct {
	AsOf       time.Time            `json:"as_of"`
	Since      time.Time            `json:"since"`
	Until      time.Time            `json:"until"`
	Entries    []RegimeHistoryEntry `json:"entries"`
	Count      int                  `json:"count"`
	TotalCount int                  `json:"total_count"`
	Limit      int                  `json:"limit"`
	Truncated  bool                 `json:"truncated"`
	Index      HistoryIndexHealth   `json:"index"`
}

// RulesHistoryParams selects a window of indexed rulebook transitions;
// boundary and limit semantics match RegimeHistoryParams.
type RulesHistoryParams struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Rule filters on the exact journal rule id (for example
	// single_name_exposure). Empty matches all rules.
	Rule  string `json:"rule,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// RuleTransitionEntry is one journaled rule status transition. Evidence is
// journal free text for display, never parsed into authority.
type RuleTransitionEntry struct {
	At                time.Time `json:"at"`
	Rule              string    `json:"rule"`
	Status            string    `json:"status"`
	Was               string    `json:"was,omitempty"`
	Evidence          string    `json:"evidence,omitempty"`
	PolicyID          string    `json:"policy_id,omitempty"`
	PolicyVersion     int       `json:"policy_version,omitempty"`
	PolicyFingerprint string    `json:"policy_fingerprint,omitempty"`
}

// RulesHistoryResult is the rules.history envelope — the same shape as
// RegimeHistoryResult with rule-transition entries.
type RulesHistoryResult struct {
	AsOf       time.Time             `json:"as_of"`
	Since      time.Time             `json:"since"`
	Until      time.Time             `json:"until"`
	Entries    []RuleTransitionEntry `json:"entries"`
	Count      int                   `json:"count"`
	TotalCount int                   `json:"total_count"`
	Limit      int                   `json:"limit"`
	Truncated  bool                  `json:"truncated"`
	Index      HistoryIndexHealth    `json:"index"`
}

// MethodCanaryHistory serves the canary-decision journal timeline
// (canary-decisions.jsonl) from the same derived index. Read-only journal
// evidence — nothing here touches submit eligibility or any broker-write
// path.
const MethodCanaryHistory = "canary.history"

// CanaryHistoryParams selects a window of indexed canary decisions;
// boundary and limit semantics match RegimeHistoryParams.
type CanaryHistoryParams struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Severity filters on the exact journal severity word (for example
	// watch, act). Empty matches all severities.
	Severity string `json:"severity,omitempty"`
	// Action filters on the exact journal action word (for example defend,
	// watch). Empty matches all actions.
	Action string `json:"action,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// CanaryHistoryEntry is one journaled canary decision. Summary is journal
// free text for display, never parsed into authority.
type CanaryHistoryEntry struct {
	At          time.Time `json:"at"`
	SessionKey  string    `json:"session_key,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Account     string    `json:"account,omitempty"`
	AccountMode string    `json:"account_mode,omitempty"`
	Action      string    `json:"action,omitempty"`
	Severity    string    `json:"severity"`
	Direction   string    `json:"direction,omitempty"`
	MarketStage string    `json:"market_stage,omitempty"`
	// PortfolioAlertRelevant mirrors the producer-stamped verdict; nil
	// means the journal line predates the stamp.
	PortfolioAlertRelevant *bool  `json:"portfolio_alert_relevant,omitempty"`
	InputHealth            string `json:"input_health,omitempty"`
	Summary                string `json:"summary,omitempty"`
}

// CanaryHistoryResult is the canary.history envelope — the phase-1 history
// envelope with canary entries.
type CanaryHistoryResult struct {
	AsOf       time.Time            `json:"as_of"`
	Since      time.Time            `json:"since"`
	Until      time.Time            `json:"until"`
	Entries    []CanaryHistoryEntry `json:"entries"`
	Count      int                  `json:"count"`
	TotalCount int                  `json:"total_count"`
	Limit      int                  `json:"limit"`
	Truncated  bool                 `json:"truncated"`
	Index      HistoryIndexHealth   `json:"index"`
}

// MethodReconEquity serves the statement-derived daily equity series
// (statement_equity_days, from retained Flex statements) joined with the
// declared capital-event ledger, from the derived history index.
// Read-only: statements and journals stay the evidence of record.
const MethodReconEquity = "recon.equity"

// ReconEquityParams selects a window of equity days. Boundary grammar
// matches RegimeHistoryParams; the default lookback is 90 days because
// the series is daily-granular.
type ReconEquityParams struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Limit caps returned days, newest first; default 200, max 1000.
	Limit int `json:"limit,omitempty"`
}

// EquityDayEntry is one derived statement-equity day. SourceStmt names the
// retained statement file the value came from; WhenGenerated is the
// restatement authority (newest statement wins per day).
type EquityDayEntry struct {
	Day           string    `json:"day"`
	AccountID     string    `json:"account_id"`
	EquityBase    float64   `json:"equity_base"`
	SourceStmt    string    `json:"source_stmt"`
	WhenGenerated time.Time `json:"when_generated,omitzero"`
}

// CapitalEventEntry is one declared capital event from
// capital-events.jsonl, rendered alongside the equity series.
type CapitalEventEntry struct {
	At          time.Time `json:"at"`
	Type        string    `json:"type"`
	AmountBase  float64   `json:"amount_base,omitempty"`
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	Note        string    `json:"note,omitempty"`
	Origin      string    `json:"origin,omitempty"`
	ReportID    string    `json:"report_id,omitempty"`
}

// ReconEquityResult is the recon.equity envelope: the equity-day window
// newest first, capital events over the same window (hard-capped, newest
// first), and two health blocks — the capital journal source and the
// statement file set (bytes = summed retained XML sizes).
type ReconEquityResult struct {
	AsOf            time.Time           `json:"as_of"`
	Since           time.Time           `json:"since"`
	Until           time.Time           `json:"until"`
	Days            []EquityDayEntry    `json:"days"`
	Count           int                 `json:"count"`
	TotalCount      int                 `json:"total_count"`
	Limit           int                 `json:"limit"`
	Truncated       bool                `json:"truncated"`
	Events          []CapitalEventEntry `json:"events"`
	EventsTruncated bool                `json:"events_truncated"`
	Index           HistoryIndexHealth  `json:"index"`
	Statements      HistoryIndexHealth  `json:"statements"`
}
