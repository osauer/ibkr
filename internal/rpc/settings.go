package rpc

import "time"

const (
	SettingsAccessRead  = "read"
	SettingsAccessWrite = "write"

	SettingsSourceRuntime  = "runtime"
	SettingsSourceConfig   = "config"
	SettingsSourceBuild    = "build"
	SettingsSourceObserved = "observed"
)

type SettingsBool struct {
	Value  bool   `json:"value"`
	Access string `json:"access"`
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
}

type SettingsFloat struct {
	Value  float64 `json:"value"`
	Access string  `json:"access"`
	Source string  `json:"source"`
	Reason string  `json:"reason,omitempty"`
}

type SettingsInt struct {
	Value  int    `json:"value"`
	Access string `json:"access"`
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
}

type SettingsString struct {
	Value  string `json:"value"`
	Access string `json:"access"`
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
}

type PlatformSettings struct {
	Kind       string                    `json:"kind"`
	Features   PlatformFeatureSettings   `json:"features"`
	Trading    PlatformTradingSettings   `json:"trading"`
	AutoTrade  PlatformAutoTradeSettings `json:"auto_trade"`
	Regime     PlatformRegimeSettings    `json:"regime"`
	MarketData PlatformMarketDataSetting `json:"market_data"`
	Build      PlatformBuildSettings     `json:"build"`
	AsOf       time.Time                 `json:"as_of"`
}

// PlatformRegimeSettings holds the regime engine's runtime preferences.
// Deliberately one knob: the confirmation-gate values (depth, streaks,
// co-sign, max ages) are code-owned pending_backtest policy — user-tunable
// gates would fork the decisions journal's comparability
// (docs/design/regime-calibration.md Part 6).
type PlatformRegimeSettings struct {
	Journal RegimeJournalSettings `json:"journal"`
}

// RegimeJournalSettings controls the regime-decisions forward-collection
// journal ($XDG_STATE_HOME/ibkr/regime-decisions.jsonl).
type RegimeJournalSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

type PlatformFeatureSettings struct {
	PurgeRestore    PurgeRestoreSettings    `json:"purge_restore"`
	StockProtection StockProtectionSettings `json:"stock_protection"`
	Rulebook        RulebookSettings        `json:"rulebook"`
}

type PurgeRestoreSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

type StockProtectionSettings struct {
	Enabled SettingsBool `json:"enabled"`
}

// RulebookSettings controls the advisory trading rulebook
// (docs/design/trading-rulebook.md): the 12-rule daily checklist plus its
// manual earnings-date overrides. Disabling hides the SPA card, empties
// rules.snapshot, and stops advisory rule_* preview warnings; it cannot
// affect broker-write gating in either direction.
type RulebookSettings struct {
	Enabled SettingsBool `json:"enabled"`
	// EarningsOverrides maps SYMBOL → "YYYY-MM-DD" (optional "Tamc"/"Tbmo"
	// suffix). Overrides are authoritative over fetched dates for rules
	// 6-8; set a symbol to null to clear it.
	EarningsOverrides SettingsStringMap `json:"earnings_overrides"`
}

// SettingsStringMap is a map-valued setting with the standard
// access/source/reason contract.
type SettingsStringMap struct {
	Value  map[string]string `json:"value,omitempty"`
	Access string            `json:"access"`
	Source string            `json:"source"`
	Reason string            `json:"reason,omitempty"`
}

type PlatformTradingSettings struct {
	// Freeze is the runtime trading brake: true blocks every new broker
	// write while cancels stay allowed. Toggled via
	// `ibkr settings set trading.freeze=true|false`.
	Freeze               SettingsBool         `json:"freeze"`
	Mode                 SettingsString       `json:"mode"`
	Account              SettingsString       `json:"account"`
	Endpoint             SettingsString       `json:"endpoint"`
	ClientID             SettingsInt          `json:"client_id"`
	MCPTrading           SettingsString       `json:"mcp_trading"`
	LiveOverride         SettingsString       `json:"live_override"`
	BuildWritesAvailable SettingsBool         `json:"build_writes_available"`
	Limits               TradingLimitSettings `json:"limits"`
}

type PlatformAutoTradeSettings struct {
	ProposalsEnabled SettingsBool   `json:"proposals_enabled"`
	FastPathEnabled  SettingsBool   `json:"fast_path_enabled"`
	PolicyFile       SettingsString `json:"policy_file"`
	HotReload        SettingsBool   `json:"hot_reload"`
	ReloadInterval   SettingsString `json:"reload_interval"`
	ProposalCadence  SettingsString `json:"proposal_cadence"`
}

type TradingLimitSettings struct {
	MaxNotional           SettingsFloat `json:"max_notional"`
	MaxOptionContracts    SettingsInt   `json:"max_option_contracts"`
	AllowStockShort       SettingsBool  `json:"allow_stock_short"`
	AllowOptionSellToOpen SettingsBool  `json:"allow_option_sell_to_open"`
}

type PlatformMarketDataSetting struct {
	Quality PlatformMarketDataQuality `json:"quality"`
}

type PlatformMarketDataQuality struct {
	Status      string              `json:"status"`
	Summary     string              `json:"summary,omitempty"`
	QuoteCounts map[string]int      `json:"quote_counts,omitempty"`
	DataQuality []DataQualityHealth `json:"data_quality,omitempty"`
	Access      string              `json:"access"`
	Source      string              `json:"source"`
	Reason      string              `json:"reason,omitempty"`
	ObservedAt  time.Time           `json:"observed_at,omitzero"`
}

type PlatformBuildSettings struct {
	Channel                 SettingsString `json:"channel"`
	TradingWritesAvailable  SettingsBool   `json:"trading_writes_available"`
	ExperimentalTradingNote string         `json:"experimental_trading_note,omitempty"`
}
