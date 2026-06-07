package rpc

import "time"

const (
	MethodMarketEventsSnapshot = "market_events.snapshot"

	MarketEventsKind               = "ibkr.market_events"
	MarketEventsSchemaVersion      = "market-events-v1"
	MarketEventsFingerprintVersion = "market-events-fp-v1"

	MarketEventBorrowInventoryTight = "borrow_inventory_tight"
	MarketEventBorrowFeeExtreme     = "borrow_fee_extreme"
	MarketEventRegSHOThreshold      = "reg_sho_threshold"
	MarketEventLULDPause            = "luld_pause"
	MarketEventLULDRecent           = MarketEventLULDPause
	MarketEventHaltRegulatoryOrNews = "halt_regulatory_or_news"

	MarketEventStatusActive   = "active"
	MarketEventStatusRecent   = "recent"
	MarketEventStatusInactive = "inactive"
	MarketEventStatusUnknown  = "unknown"
	MarketEventStatusStale    = "stale"
	MarketEventStatusDegraded = "degraded"

	MarketEventSeverityContext = "context"
	MarketEventSeverityWatch   = "watch"
	MarketEventSeverityAct     = "act"
	MarketEventSeverityBlock   = "block"

	MarketEventRoleContext          = "context"
	MarketEventRoleProposalModifier = "proposal_modifier"
	MarketEventRoleHardBlocker      = "hard_blocker"
)

type MarketEventsParams struct {
	Symbol  string   `json:"symbol,omitempty"`
	Symbols []string `json:"symbols,omitempty"`
}

type MarketEventsResult struct {
	Kind           string                       `json:"kind"`
	SchemaVersion  string                       `json:"schema_version"`
	AsOf           time.Time                    `json:"as_of"`
	Symbols        []string                     `json:"symbols,omitempty"`
	Flags          []MarketEventFlag            `json:"flags,omitempty"`
	BySymbol       map[string][]MarketEventFlag `json:"by_symbol,omitempty"`
	SourceHealth   []SourceHealth               `json:"source_health,omitempty"`
	Fingerprint    Fingerprint                  `json:"fingerprint,omitzero"`
	WarningDetails []DataWarning                `json:"warning_details,omitempty"`
	NotExecution   string                       `json:"not_execution,omitempty"`
}

type MarketEventFlag struct {
	ID             string        `json:"id"`
	Symbol         string        `json:"symbol"`
	Label          string        `json:"label"`
	Status         string        `json:"status"`
	Severity       string        `json:"severity"`
	Role           string        `json:"role"`
	Source         string        `json:"source"`
	SourceURL      string        `json:"source_url,omitempty"`
	AsOf           time.Time     `json:"as_of,omitzero"`
	ObservedAt     time.Time     `json:"observed_at,omitzero"`
	ExpiresAt      time.Time     `json:"expires_at,omitzero"`
	Value          *float64      `json:"value,omitempty"`
	Unit           string        `json:"unit,omitempty"`
	Details        []string      `json:"details,omitempty"`
	WarningDetails []DataWarning `json:"warning_details,omitempty"`
}
