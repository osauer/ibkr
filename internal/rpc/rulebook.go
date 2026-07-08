package rpc

import (
	"time"

	"github.com/osauer/ibkr/internal/risk"
)

// MethodRulesSnapshot returns the daily trading-rulebook checklist evaluated
// against the current book. Advisory-only: nothing in this result may alter
// submit eligibility or any gated broker-write path.
const MethodRulesSnapshot = "rules.snapshot"

// RulesSnapshotParams selects optional evaluation scope. Zero value means the
// full 14-rule checklist over all held names.
type RulesSnapshotParams struct {
	// Symbol narrows per-name offender lists to one underlying; portfolio
	// rules still evaluate portfolio-wide.
	Symbol string `json:"symbol,omitempty"`
}

// EarningsInfo is the per-name earnings context the rules consumed, so
// surfaces can show where each date came from.
type EarningsInfo struct {
	Symbol string `json:"symbol"`
	// Date is the next earnings date in ET (YYYY-MM-DD), empty when unknown.
	Date string `json:"date,omitempty"`
	// TimeOfDay is "amc", "bmo", or "" when unspecified.
	TimeOfDay string `json:"time_of_day,omitempty"`
	// Estimated marks provider-flagged estimated (unconfirmed) dates.
	Estimated bool `json:"estimated,omitempty"`
	// Source is fetched | override | unknown.
	Source string `json:"source"`
	// ObservedAt is when the fetched value was last confirmed from the
	// provider; zero for overrides and unknowns.
	ObservedAt time.Time `json:"observed_at,omitzero"`
	Stale      bool      `json:"stale,omitempty"`
}

// RulesResult is the rules.snapshot payload. Rows come from the pure
// internal/risk evaluator; this envelope adds provenance and input health.
type RulesResult struct {
	AsOf time.Time `json:"as_of"`
	// Enabled mirrors features.rulebook.enabled; when false Rules is empty
	// and Status says disabled.
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"` // ok | degraded | disabled
	// Rules holds all rows in rulebook order; Ranked holds indexes into
	// Rules sorted hardest-first so renderers agree on ordering without
	// re-deriving it.
	Rules  []risk.RuleRow `json:"rules"`
	Ranked []int          `json:"ranked,omitempty"`
	// BreachCounts summarizes row counts by status for compact surfaces.
	BreachCounts map[string]int `json:"breach_counts,omitempty"`
	// InputHealth is the result-level gate: when positions or account are
	// pending/stale/absent every portfolio-dependent row is unknown, never
	// pass. One entry per source: positions, account, tape, earnings.
	InputHealth []SourceHealth `json:"input_health,omitempty"`
	Earnings    []EarningsInfo `json:"earnings,omitempty"`
	// Policy provenance, mirroring proposals/canary.
	PolicyID          string       `json:"policy_id"`
	PolicyVersion     int          `json:"policy_version"`
	PolicyFingerprint *Fingerprint `json:"policy_fingerprint,omitempty"`
	// BaseCurrency scopes every *_base impact figure.
	BaseCurrency string `json:"base_currency,omitempty"`
}
