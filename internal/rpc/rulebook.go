package rpc

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
)

// MethodRulesSnapshot returns the daily trading-rulebook checklist evaluated
// against the current book. Advisory-only: nothing in this result may alter
// submit eligibility or any gated broker-write path.
const MethodRulesSnapshot = "rules.snapshot"

// RulebookPolicyFingerprintVersion labels the advisory rulebook policy
// fingerprint's JSON projection. Sibling-policy pins compare policy ID and
// version rather than fingerprint keys, and journals remain point-in-time
// records.
const RulebookPolicyFingerprintVersion = "rulebook-fp-v3"

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
	// Source is fetched | override | verified_terminal | unknown.
	// Provider-level provenance lives in Providers; Terminal carries the
	// exact-contract evidence when no future issuer earnings event applies.
	Source string `json:"source"`
	// Status is date or a typed unresolved outcome. Conflicting provider
	// dates never populate Date.
	Status string `json:"status,omitempty"`
	// Reason is a stable aggregate explanation such as single_source or
	// conflicting_sources; it never contains provider free text.
	Reason string `json:"reason,omitempty"`
	// ObservedAt is when the fetched value was last confirmed from the
	// provider; zero for overrides and unknowns.
	ObservedAt time.Time              `json:"observed_at,omitzero"`
	Stale      bool                   `json:"stale,omitempty"`
	Providers  []EarningsProviderInfo `json:"providers,omitempty"`
	Terminal   *EarningsTerminalInfo  `json:"terminal,omitempty"`
}

// Earnings statuses are the closed aggregate/provider outcome vocabulary.
const (
	EarningsStatusDate                    = "date"
	EarningsStatusNoDatePublished         = "no_date_published"
	EarningsStatusUnsupportedSecurity     = "unsupported_security"
	EarningsStatusFormatChange            = "format_change"
	EarningsStatusTransportFailure        = "transport_failure"
	EarningsStatusConflictingSources      = "conflicting_sources"
	EarningsStatusTerminalNonReporting    = "terminal_non_reporting"
	EarningsStatusTerminalEvidenceExpired = "terminal_evidence_expired"
)

// EarningsTerminalInfo is compiled, reviewed evidence that one exact broker
// contract no longer has a future issuer earnings cycle. It is deliberately
// contract-bound rather than symbol-wide: ticker reuse or a different listing
// must fall back to ordinary provider resolution. RevalidateAfter is a hard
// fail-closed boundary; expired evidence becomes unknown until the catalog is
// reviewed and updated. AuthorityReviewedAt is the monotonic catalog watermark
// that also survives explicit record revocation.
type EarningsTerminalInfo struct {
	ContractConID        int                         `json:"contract_con_id"`
	Issuer               string                      `json:"issuer"`
	CIK                  string                      `json:"cik,omitempty"`
	Classification       string                      `json:"classification"`
	EffectiveDate        string                      `json:"effective_date"`
	VerifiedAt           time.Time                   `json:"verified_at"`
	RevalidateAfter      time.Time                   `json:"revalidate_after"`
	AuthorityRevision    int64                       `json:"authority_revision"`
	AuthorityReviewedAt  time.Time                   `json:"authority_reviewed_at"`
	AuthorityFingerprint string                      `json:"authority_fingerprint"`
	Evidence             []EarningsEvidenceReference `json:"evidence"`
}

// EarningsEvidenceReference is one allowlisted primary-source document used
// by the terminal classification. These strings are compiled authority, not
// instructions parsed from live provider content.
type EarningsEvidenceReference struct {
	Authority string `json:"authority"`
	Document  string `json:"document"`
	URL       string `json:"url"`
}

// EarningsProviderInfo is one provider's latest typed outcome. A transport
// failure may coexist with a retained LastGoodDate, but Date is populated only
// when the latest attempt itself returned a usable date.
type EarningsProviderInfo struct {
	Provider     string         `json:"provider"`
	Status       string         `json:"status"`
	Date         string         `json:"date,omitempty"`
	TimeOfDay    string         `json:"time_of_day,omitempty"`
	Estimated    bool           `json:"estimated,omitempty"`
	ObservedAt   time.Time      `json:"observed_at,omitzero"`
	AttemptedAt  time.Time      `json:"attempted_at,omitzero"`
	NextAttempt  *time.Time     `json:"next_attempt,omitempty"`
	LastGoodDate string         `json:"last_good_date,omitempty"`
	LastFailure  *SourceFailure `json:"last_failure,omitempty"`
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
	// pass. Canonical snapshots carry exactly one entry for account,
	// positions, earnings, regime_stage, and tape.
	InputHealth []SourceHealth `json:"input_health,omitempty"`
	Earnings    []EarningsInfo `json:"earnings,omitempty"`
	// Policy provenance, mirroring proposals/canary.
	PolicyID          string       `json:"policy_id"`
	PolicyVersion     int          `json:"policy_version"`
	PolicyFingerprint *Fingerprint `json:"policy_fingerprint,omitempty"`
	// BaseCurrency scopes every *_base impact figure.
	BaseCurrency string `json:"base_currency,omitempty"`
}
