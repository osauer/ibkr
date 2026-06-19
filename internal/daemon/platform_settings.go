package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

type platformSettingsStore struct {
	path string
	mu   sync.Mutex
	data platformSettingsData
}

type platformSettingsData struct {
	Version  int                         `json:"version"`
	Features platformFeatureSettingsData `json:"features"`
	Trading  platformTradingSettingsData `json:"trading"`
	Regime   platformRegimeSettingsData  `json:"regime"`
}

type platformRegimeSettingsData struct {
	Journal platformRegimeJournalSettingsData `json:"journal"`
}

type platformRegimeJournalSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type platformFeatureSettingsData struct {
	PurgeRestore    platformPurgeRestoreSettingsData    `json:"purge_restore"`
	StockProtection platformStockProtectionSettingsData `json:"stock_protection"`
}

type platformPurgeRestoreSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type platformStockProtectionSettingsData struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type platformTradingSettingsData struct {
	MaxNotional           *float64 `json:"max_notional,omitempty"`
	MaxOptionContracts    *int     `json:"max_option_contracts,omitempty"`
	AllowStockShort       *bool    `json:"allow_stock_short,omitempty"`
	AllowOptionSellToOpen *bool    `json:"allow_option_sell_to_open,omitempty"`
	// Freeze is the runtime trading brake: true blocks every new broker
	// write (place/modify/purge/restore/proposals) via
	// brokerWriteAuthorization while cancels stay allowed. Unlike the
	// limits above it is not gated on tradingLimitWritability — a brake
	// must engage even when order entry is otherwise misconfigured.
	Freeze *bool `json:"freeze,omitempty"`
}

func defaultPlatformSettingsPath() (string, error) {
	return defaultTradingStatePath("platform-settings.json")
}

func newPlatformSettingsStore(path string) (*platformSettingsStore, error) {
	s := &platformSettingsStore{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *platformSettingsStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.data.Version = 1
			return nil
		}
		return fmt.Errorf("read platform settings: %w", err)
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return fmt.Errorf("decode platform settings: %w", err)
	}
	if s.data.Version == 0 {
		s.data.Version = 1
	}
	return nil
}

func (s *platformSettingsStore) snapshot() platformSettingsData {
	if s == nil {
		return platformSettingsData{Version: 1}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

func (s *platformSettingsStore) update(fn func(*platformSettingsData) error) error {
	if s == nil {
		return errBadRequest("runtime settings store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	if next.Version == 0 {
		next.Version = 1
	}
	if err := fn(&next); err != nil {
		return err
	}
	s.data = next
	return s.saveLocked()
}

func (s *platformSettingsStore) saveLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writePrivateStateAtomic(s.path, raw)
}

func (s *Server) handleSettingsGet() (*rpc.PlatformSettings, error) {
	health := s.handleStatusHealth()
	out := s.platformSettingsSnapshot(&platformSettingsObserved{
		DataQuality: health.DataQuality,
		ObservedAt:  s.orderNow(),
	})
	return &out, nil
}

func (s *Server) handleSettingsUpdate(_ context.Context, req *rpc.Request) (*rpc.PlatformSettings, error) {
	if len(bytes.TrimSpace(req.Params)) == 0 {
		return nil, errBadRequest("settings patch body is required")
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(req.Params, &patch); err != nil {
		return nil, errBadRequest("decode settings patch: " + err.Error())
	}
	origin := ""
	if raw, ok := patch["origin"]; ok {
		delete(patch, "origin")
		if err := json.Unmarshal(raw, &origin); err != nil {
			return nil, errBadRequest("decode settings origin: " + err.Error())
		}
	}
	if len(patch) == 0 {
		health := s.handleStatusHealth()
		out := s.platformSettingsSnapshot(&platformSettingsObserved{
			DataQuality: health.DataQuality,
			ObservedAt:  s.orderNow(),
		})
		return &out, nil
	}
	if err := s.applyPlatformSettingsPatch(patch, origin); err != nil {
		return nil, err
	}
	health := s.handleStatusHealth()
	out := s.platformSettingsSnapshot(&platformSettingsObserved{
		DataQuality: health.DataQuality,
		ObservedAt:  s.orderNow(),
	})
	return &out, nil
}

func (s *Server) applyPlatformSettingsPatch(patch map[string]json.RawMessage, origin string) error {
	for key := range patch {
		switch key {
		case "features", "trading", "regime":
		default:
			return errBadRequest("unknown settings field " + key)
		}
	}
	if _, ok := patch["trading"]; ok {
		// Trading safety limits are part of the broker-write surface: when
		// the configured route is live, agent-origin sessions may not loosen
		// them. Paper keeps the full path open for testing.
		if s.cfg.Trading.Mode == config.TradingModeLive && !originIsHuman(origin) {
			return errBadRequest("live trading settings are blocked for agent-origin requests; a human must change trading limits from an interactive terminal")
		}
	}
	limitsWritable, reason := s.tradingLimitWritability()
	return s.platformSettings.update(func(next *platformSettingsData) error {
		for key, raw := range patch {
			switch key {
			case "features":
				if err := applyFeatureSettingsPatch(next, raw); err != nil {
					return err
				}
			case "trading":
				if err := applyTradingSettingsPatch(next, raw, limitsWritable, reason); err != nil {
					return err
				}
			case "regime":
				if err := applyRegimeSettingsPatch(next, raw); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func applyRegimeSettingsPatch(next *platformSettingsData, raw json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return errBadRequest("regime must be an object")
	}
	for key, raw := range obj {
		switch key {
		case "journal":
			var journal map[string]json.RawMessage
			if err := json.Unmarshal(raw, &journal); err != nil {
				return errBadRequest("regime.journal must be an object")
			}
			for jkey, jraw := range journal {
				switch jkey {
				case "enabled":
					v, err := nullableBool(jraw)
					if err != nil {
						return errBadRequest("regime.journal.enabled must be true, false, or null")
					}
					next.Regime.Journal.Enabled = v
				default:
					return errBadRequest("unknown settings field regime.journal." + jkey)
				}
			}
		default:
			return errBadRequest("unknown settings field regime." + key)
		}
	}
	return nil
}

func applyFeatureSettingsPatch(next *platformSettingsData, raw json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return errBadRequest("features must be an object")
	}
	for key, raw := range obj {
		switch key {
		case "purge_restore":
			var purge map[string]json.RawMessage
			if err := json.Unmarshal(raw, &purge); err != nil {
				return errBadRequest("features.purge_restore must be an object")
			}
			for pkey, praw := range purge {
				switch pkey {
				case "enabled":
					v, err := nullableBool(praw)
					if err != nil {
						return errBadRequest("features.purge_restore.enabled must be true, false, or null")
					}
					next.Features.PurgeRestore.Enabled = v
				default:
					return errBadRequest("unknown settings field features.purge_restore." + pkey)
				}
			}
		case "stock_protection":
			var stock map[string]json.RawMessage
			if err := json.Unmarshal(raw, &stock); err != nil {
				return errBadRequest("features.stock_protection must be an object")
			}
			for skey, sraw := range stock {
				switch skey {
				case "enabled":
					v, err := nullableBool(sraw)
					if err != nil {
						return errBadRequest("features.stock_protection.enabled must be true, false, or null")
					}
					next.Features.StockProtection.Enabled = v
				default:
					return errBadRequest("unknown settings field features.stock_protection." + skey)
				}
			}
		default:
			return errBadRequest("unknown settings field features." + key)
		}
	}
	return nil
}

func applyTradingSettingsPatch(next *platformSettingsData, raw json.RawMessage, writable bool, reason string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return errBadRequest("trading must be an object")
	}
	for key, raw := range obj {
		switch key {
		case "freeze":
			v, err := nullableBool(raw)
			if err != nil {
				return errBadRequest("trading.freeze must be true, false, or null")
			}
			next.Trading.Freeze = v
		case "limits":
			if !writable {
				return errBadRequest("trading.limits is read-only: " + reason)
			}
			var limits map[string]json.RawMessage
			if err := json.Unmarshal(raw, &limits); err != nil {
				return errBadRequest("trading.limits must be an object")
			}
			for lkey, lraw := range limits {
				switch lkey {
				case "max_notional":
					v, err := nullableFloat(lraw)
					if err != nil || (v != nil && *v <= 0) {
						return errBadRequest("trading.limits.max_notional must be a positive number or null")
					}
					next.Trading.MaxNotional = v
				case "max_option_contracts":
					v, err := nullableInt(lraw)
					if err != nil || (v != nil && *v <= 0) {
						return errBadRequest("trading.limits.max_option_contracts must be a positive integer or null")
					}
					next.Trading.MaxOptionContracts = v
				case "allow_stock_short":
					v, err := nullableBool(lraw)
					if err != nil {
						return errBadRequest("trading.limits.allow_stock_short must be true, false, or null")
					}
					next.Trading.AllowStockShort = v
				case "allow_option_sell_to_open":
					v, err := nullableBool(lraw)
					if err != nil {
						return errBadRequest("trading.limits.allow_option_sell_to_open must be true, false, or null")
					}
					next.Trading.AllowOptionSellToOpen = v
				default:
					return errBadRequest("unknown settings field trading.limits." + lkey)
				}
			}
		default:
			return errBadRequest("settings field trading." + key + " is read-only")
		}
	}
	return nil
}

func nullableBool(raw json.RawMessage) (*bool, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func nullableFloat(raw json.RawMessage) (*float64, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func nullableInt(raw json.RawMessage) (*int, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Server) platformSettingsSnapshot(observed *platformSettingsObserved) rpc.PlatformSettings {
	data := s.platformSettings.snapshot()
	trading := s.effectiveTradingConfig()
	autoTrade := config.AutoTrade{}.WithDefaults()
	if s.cfg != nil {
		autoTrade = s.cfg.AutoTrade.WithDefaults()
	}
	status := s.currentTradingStatus()
	limitsWritable, limitReason := s.tradingLimitWritability()
	purgeEnabled := true
	purgeSource := rpc.SettingsSourceRuntime
	if data.Features.PurgeRestore.Enabled != nil {
		purgeEnabled = *data.Features.PurgeRestore.Enabled
	}
	stockProtectionEnabled := true
	if data.Features.StockProtection.Enabled != nil {
		stockProtectionEnabled = *data.Features.StockProtection.Enabled
	}
	limitAccess := rpc.SettingsAccessRead
	if limitsWritable {
		limitAccess = rpc.SettingsAccessWrite
		limitReason = ""
	}
	limitSource := func(runtime bool) string {
		if runtime && limitsWritable {
			return rpc.SettingsSourceRuntime
		}
		return rpc.SettingsSourceConfig
	}
	out := rpc.PlatformSettings{
		Kind: "ibkr.platform_settings",
		Features: rpc.PlatformFeatureSettings{
			PurgeRestore: rpc.PurgeRestoreSettings{
				Enabled: rpc.SettingsBool{Value: purgeEnabled, Access: rpc.SettingsAccessWrite, Source: purgeSource},
			},
			StockProtection: rpc.StockProtectionSettings{
				Enabled: rpc.SettingsBool{Value: stockProtectionEnabled, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime},
			},
		},
		Trading: rpc.PlatformTradingSettings{
			Freeze:               settingsBool(s.tradingFrozen(), rpc.SettingsAccessWrite, rpc.SettingsSourceRuntime, "runtime brake: true blocks new broker writes; cancels stay allowed"),
			Mode:                 settingsString(status.Mode, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, `set [trading].mode in config.toml to "disabled", "paper", or "live"`),
			Account:              settingsString(status.Account, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [gateway].account in config.toml"),
			Endpoint:             settingsString(status.Endpoint, rpc.SettingsAccessRead, rpc.SettingsSourceObserved, "observed from daemon gateway discovery/config"),
			ClientID:             settingsInt(status.ClientID, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [gateway].client_id in config.toml"),
			MCPTrading:           settingsString(status.MCPTrading, rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "MCP broker-write controls are not exposed"),
			LiveOverride:         settingsString(status.LiveOverride, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, `computed from [trading].mode and active blockers; "ready" only on an unblocked live route`),
			BuildWritesAvailable: settingsBool(orderWritesAvailable, rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "controlled by the ibkr build"),
			Limits: rpc.TradingLimitSettings{
				MaxNotional:           settingsFloat(trading.MaxNotional, limitAccess, limitSource(data.Trading.MaxNotional != nil), limitReason),
				MaxOptionContracts:    settingsInt(trading.MaxOptionContracts, limitAccess, limitSource(data.Trading.MaxOptionContracts != nil), limitReason),
				AllowStockShort:       settingsBool(trading.AllowStockShort, limitAccess, limitSource(data.Trading.AllowStockShort != nil), limitReason),
				AllowOptionSellToOpen: settingsBool(trading.AllowOptionSellToOpen, limitAccess, limitSource(data.Trading.AllowOptionSellToOpen != nil), limitReason),
			},
		},
		AutoTrade: rpc.PlatformAutoTradeSettings{
			ProposalsEnabled: settingsBool(autoTrade.ProposalsEnabledResolved(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].proposals_enabled in config.toml"),
			FastPathEnabled:  settingsBool(autoTrade.FastPathEnabledResolved(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].fast_path_enabled in config.toml"),
			PolicyFile:       settingsString(autoTrade.PolicyFile, rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].policy_file in config.toml"),
			HotReload:        settingsBool(autoTrade.HotReloadEnabled(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].hot_reload in config.toml"),
			ReloadInterval:   settingsString(autoTrade.ReloadIntervalDuration().String(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].reload_interval in config.toml"),
			ProposalCadence:  settingsString(autoTrade.ProposalCadenceDuration().String(), rpc.SettingsAccessRead, rpc.SettingsSourceConfig, "set [auto_trade].proposal_cadence in config.toml"),
		},
		Regime: rpc.PlatformRegimeSettings{
			Journal: rpc.RegimeJournalSettings{
				Enabled: settingsBool(regimeJournalEnabledFrom(data), rpc.SettingsAccessWrite, rpc.SettingsSourceRuntime, "regime-decisions forward-collection journal (calibration corpus); safe to disable"),
			},
		},
		MarketData: rpc.PlatformMarketDataSetting{Quality: observedMarketDataQuality(observed)},
		Build: rpc.PlatformBuildSettings{
			Channel:                settingsString(buildChannel(), rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "controlled by the ibkr build"),
			TradingWritesAvailable: settingsBool(orderWritesAvailable, rpc.SettingsAccessRead, rpc.SettingsSourceBuild, "controlled by the ibkr build"),
		},
		AsOf: s.orderNow(),
	}
	if orderWritesAvailable {
		out.Build.ExperimentalTradingNote = "experimental trading build; runtime limit overrides are writable only when [trading].mode is paper or live"
	} else {
		out.Build.ExperimentalTradingNote = "stable read-only build; trading limit edits are disabled"
	}
	return out
}

func regimeJournalEnabledFrom(data platformSettingsData) bool {
	if data.Regime.Journal.Enabled == nil {
		return true
	}
	return *data.Regime.Journal.Enabled
}

func buildChannel() string {
	if orderWritesAvailable {
		return "experimental-trading"
	}
	return "stable"
}

func settingsBool(value bool, access, source, reason string) rpc.SettingsBool {
	return rpc.SettingsBool{Value: value, Access: access, Source: source, Reason: reason}
}

func settingsFloat(value float64, access, source, reason string) rpc.SettingsFloat {
	return rpc.SettingsFloat{Value: value, Access: access, Source: source, Reason: reason}
}

func settingsInt(value int, access, source, reason string) rpc.SettingsInt {
	return rpc.SettingsInt{Value: value, Access: access, Source: source, Reason: reason}
}

func settingsString(value, access, source, reason string) rpc.SettingsString {
	return rpc.SettingsString{Value: value, Access: access, Source: source, Reason: reason}
}

func (s *Server) tradingLimitWritability() (bool, string) {
	if !orderWritesAvailable {
		return false, "stable build exposes trading limits as read-only"
	}
	tr := config.Trading{}.WithDefaults()
	if s != nil && s.cfg != nil {
		tr = s.cfg.Trading.WithDefaults()
	}
	if !tr.OrderEntryEnabled() {
		return false, `set [trading].mode to "paper" or "live" before editing runtime safety limits`
	}
	return true, ""
}

func (s *Server) effectiveTradingConfig() config.Trading {
	tr := config.Trading{}.WithDefaults()
	if s != nil && s.cfg != nil {
		tr = s.cfg.Trading.WithDefaults()
	}
	if ok, _ := s.tradingLimitWritability(); !ok {
		return tr
	}
	data := s.platformSettings.snapshot()
	if data.Trading.MaxNotional != nil {
		tr.MaxNotional = *data.Trading.MaxNotional
	}
	if data.Trading.MaxOptionContracts != nil {
		tr.MaxOptionContracts = *data.Trading.MaxOptionContracts
	}
	if data.Trading.AllowStockShort != nil {
		tr.AllowStockShort = *data.Trading.AllowStockShort
	}
	if data.Trading.AllowOptionSellToOpen != nil {
		tr.AllowOptionSellToOpen = *data.Trading.AllowOptionSellToOpen
	}
	return tr
}

func (s *Server) purgeRestoreEnabled() bool {
	data := s.platformSettings.snapshot()
	if data.Features.PurgeRestore.Enabled == nil {
		return true
	}
	return *data.Features.PurgeRestore.Enabled
}

func (s *Server) stockProtectionEnabled() bool {
	data := s.platformSettings.snapshot()
	if data.Features.StockProtection.Enabled == nil {
		return true
	}
	return *data.Features.StockProtection.Enabled
}

// tradingFrozen reports the runtime trading brake. Default (unset/null) is
// not frozen; only an explicit trading.freeze=true engages it.
func (s *Server) tradingFrozen() bool {
	if s == nil {
		return false
	}
	data := s.platformSettings.snapshot()
	return data.Trading.Freeze != nil && *data.Trading.Freeze
}

type platformSettingsObserved struct {
	Quotes      map[string]rpc.Quote
	DataQuality []rpc.DataQualityHealth
	ObservedAt  time.Time
}

func observedMarketDataQuality(observed *platformSettingsObserved) rpc.PlatformMarketDataQuality {
	out := rpc.PlatformMarketDataQuality{
		Status:  "unknown",
		Summary: "no observed market-data snapshot yet",
		Access:  rpc.SettingsAccessRead,
		Source:  rpc.SettingsSourceObserved,
		Reason:  "observed from quote, position, chain, and status surfaces; entitlements are never stored",
	}
	if observed == nil {
		return out
	}
	out.DataQuality = append([]rpc.DataQualityHealth(nil), observed.DataQuality...)
	out.ObservedAt = observed.ObservedAt
	counts := map[string]int{}
	for _, q := range observed.Quotes {
		key := strings.TrimSpace(q.DataType)
		if key == "" {
			key = rpc.MarketDataLive
		}
		counts[key]++
	}
	if len(counts) > 0 {
		out.QuoteCounts = counts
	}
	switch {
	case len(out.DataQuality) > 0:
		out.Status = "degraded"
		out.Summary = "observed decision surfaces report degraded data quality"
	case len(counts) == 0:
		out.Status = "unknown"
		out.Summary = "no quote feed state observed yet"
	case counts[rpc.MarketDataDelayed] > 0 || counts[rpc.MarketDataDelayedFrozen] > 0:
		out.Status = "delayed"
		out.Summary = "one or more observed quotes are delayed"
	default:
		out.Status = "ok"
		out.Summary = "observed quotes look live or usable"
	}
	return out
}
