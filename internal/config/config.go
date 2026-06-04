// Package config loads and validates the ibkr daemon's TOML configuration.
//
// Schema is auto-by-default: a missing config file (or absent fields) tells
// the daemon to discover the gateway endpoint and TLS mode at startup. Any
// field the user does write is binding — discovery skips that dimension.
// This avoids the "loose by default but strict only on tls=true" asymmetry
// the v0.3.x layout had.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Gateway holds the four pinnable connection knobs. Pointer fields
// distinguish "user wrote this value" (binding) from "field absent" (auto).
//
// Host is plain string because "" and "127.0.0.1" carry the same meaning:
// auto-discovery only probes loopback. A non-loopback host implies "I know
// where my gateway is" and is treated as pinned.
//
// Account is plain string because empty already means "auto-detect via
// managedAccounts" in the SDK.
type Gateway struct {
	// Host pins the IB Gateway / TWS host; empty (the default) defers to auto-discovery on loopback (127.0.0.1), any non-empty value skips probing.
	Host string `toml:"host"`
	// Port pins the IB Gateway / TWS API port (typically 4001/4002 for IB Gateway live/paper, 7496/7497 for TWS live/paper); absent (nil) defers to port-probing during discovery.
	Port *int `toml:"port"`
	// ClientID pins the IBKR API clientID for the primary connection (default 15); collisions with another running ibkr process auto-walk to the next free ID via the SDK's retry path.
	ClientID *int `toml:"client_id"`
	// BreadthClientID is the IBKR clientID used by the dedicated
	// historical-bar connector that backs the SPX breadth refresh.
	// Default 16 (one above the primary's default 15). Pinned to its
	// own connection so breadth's 503-name fan-out runs against a
	// separate 40-msg/sec and 60-historical/10-min rate-limit budget
	// rather than competing with interactive RPCs and the gamma
	// option-leg fan-out on the primary client. Collision with the
	// primary cid is handled by the existing MaxClientIDRetries
	// fall-through in pkg/ibkr — bulk silently increments to the next
	// free ID, same as primary does.
	BreadthClientID *int `toml:"breadth_client_id"`
	// Account pins the IBKR account ID like "U1234567"; empty (default) defers to the gateway's managedAccounts list — fine for single-account logins, required disambiguator when the login carries multiple accounts.
	Account string `toml:"account"`
	// TLS pins TLS mode for the API socket: absent (nil) auto-tries plain first then TLS, `true` forces TLS-only with no plain fallback, `false` forces plain — setting the field disables fallback in either direction.
	TLS *bool `toml:"tls"`
}

// PortPinned reports whether the user pinned a port. Discovery skips the
// port-probe step when true and uses Port directly.
func (g Gateway) PortPinned() bool { return g.Port != nil }

// TLSPinned reports whether the user pinned a TLS mode. The SDK's
// EnableTLSFallback is set to false (no fallback) when true, regardless of
// which mode was pinned — matches the strict-on-explicit-set contract.
func (g Gateway) TLSPinned() bool { return g.TLS != nil }

// HostOrDefault returns Host if set, else 127.0.0.1.
func (g Gateway) HostOrDefault() string {
	if g.Host == "" {
		return "127.0.0.1"
	}
	return g.Host
}

// ClientIDOrDefault returns ClientID if pinned, else 15.
func (g Gateway) ClientIDOrDefault() int {
	if g.ClientID == nil {
		return 15
	}
	return *g.ClientID
}

// BreadthClientIDOrDefault returns the clientID for the bulk-historical
// breadth connector. Default 16 — one above the primary default — so a
// fresh install gets two non-colliding IDs without any config tweak.
// Collision with the primary cid is non-fatal: the pkg/ibkr handshake's
// MaxClientIDRetries fall-through walks the bulk attempt to the next
// free ID.
func (g Gateway) BreadthClientIDOrDefault() int {
	if g.BreadthClientID == nil {
		return 16
	}
	return *g.BreadthClientID
}

// PortOrZero returns Port (dereferenced) or 0 if unset. Callers should
// check PortPinned first; the zero is a sentinel for "discover."
func (g Gateway) PortOrZero() int {
	if g.Port == nil {
		return 0
	}
	return *g.Port
}

// TLSOrFalse returns TLS (dereferenced) or false if unset. Callers should
// check TLSPinned first; the false is a sentinel meaning "auto, try plain
// first" — distinct from a binding tls=false.
func (g Gateway) TLSOrFalse() bool {
	if g.TLS == nil {
		return false
	}
	return *g.TLS
}

// Daemon holds runtime knobs for the daemon process.
type Daemon struct {
	// IdleTimeout is how long the auto-spawned daemon stays alive between CLI calls (default 15m, accepts any Go duration string like "1h" or "0s"); set "0s" to disable idle-shutdown when running long cold-start jobs such as the first breadth fan-out under `ibkr daemon --foreground`.
	IdleTimeout duration `toml:"idle_timeout"`
	// LogLevel is the daemon's log verbosity — one of "debug", "info" (default), "warn", or "error".
	LogLevel string `toml:"log_level"`
}

// Trading holds local order-entry gates. A missing [trading] section is
// intentionally non-trading; TWS / Gateway broker permissions remain the final
// authority even after these local gates pass.
type Trading struct {
	// Enabled controls whether any order preview/place/modify/cancel command can progress beyond the local gate (default false).
	Enabled bool `toml:"enabled"`
	// Mode selects the target account class for local safety checks: "paper" (default) or "live".
	Mode string `toml:"mode"`
	// RequirePreview forces every write through a submit-eligible preview token. Defaults to true; false is invalid for the shipped trading release.
	RequirePreview *bool `toml:"require_preview"`
	// MaxNotional caps first-release equity/ETF order notional before broker WhatIf; default 10000 in account currency.
	MaxNotional float64 `toml:"max_notional"`
	// MaxOptionContracts caps first-release single-leg option quantity; default 5.
	MaxOptionContracts int `toml:"max_option_contracts"`
	// AllowStockShort permits stock short/opening flip previews when true. Default false.
	AllowStockShort bool `toml:"allow_stock_short"`
	// AllowOptionSellToOpen permits option sell-to-open previews when true. Default false.
	AllowOptionSellToOpen bool `toml:"allow_option_sell_to_open"`
	// AllowOptionMarketOrders permits option market orders when true. Default false; first trading release still blocks this.
	AllowOptionMarketOrders bool `toml:"allow_option_market_orders"`
	// AllowLive is the explicit local live-trading override. Default false.
	AllowLive bool `toml:"allow_live"`
	// LiveAckAccount must match the pinned live account before live writes are allowed.
	LiveAckAccount string `toml:"live_ack_account"`
	// LiveAckEndpoint must match host:port for the pinned live endpoint before live writes are allowed.
	LiveAckEndpoint string `toml:"live_ack_endpoint"`
	// PaperSmokeMaxAge is how long a paper trading smoke remains acceptable for live enablement. Defaults to 168h.
	PaperSmokeMaxAge duration `toml:"paper_smoke_max_age"`
	// MCPEnabled controls whether MCP write tools may progress beyond preview/status. Default false.
	MCPEnabled bool `toml:"mcp_enabled"`
	// MCPMode selects MCP scope: "preview" (default), "paper-write", or "live-write".
	MCPMode string `toml:"mcp_mode"`
	// MCPNonceTTL controls how long CLI-minted human nonces remain valid. Defaults to 5m.
	MCPNonceTTL duration `toml:"mcp_nonce_ttl"`
}

const (
	TradingModePaper = "paper"
	TradingModeLive  = "live"

	MCPModePreview    = "preview"
	MCPModePaperWrite = "paper-write"
	MCPModeLiveWrite  = "live-write"
)

// WithDefaults returns t with default values applied without granting trading.
func (t Trading) WithDefaults() Trading {
	if t.Mode == "" {
		t.Mode = TradingModePaper
	}
	if t.RequirePreview == nil {
		v := true
		t.RequirePreview = &v
	}
	if t.MaxNotional == 0 {
		t.MaxNotional = 10000
	}
	if t.MaxOptionContracts == 0 {
		t.MaxOptionContracts = 5
	}
	if t.PaperSmokeMaxAge == 0 {
		t.PaperSmokeMaxAge = duration(168 * time.Hour)
	}
	if t.MCPMode == "" {
		t.MCPMode = MCPModePreview
	}
	if t.MCPNonceTTL == 0 {
		t.MCPNonceTTL = duration(5 * time.Minute)
	}
	return t
}

// PreviewRequired reports the resolved preview-token requirement.
func (t Trading) PreviewRequired() bool {
	if t.RequirePreview == nil {
		return true
	}
	return *t.RequirePreview
}

// PaperSmokeMaxAgeDuration returns the resolved paper-smoke freshness window.
func (t Trading) PaperSmokeMaxAgeDuration() time.Duration {
	if t.PaperSmokeMaxAge == 0 {
		return 168 * time.Hour
	}
	return t.PaperSmokeMaxAge.Std()
}

// MCPNonceTTLDuration returns the resolved MCP human-nonce lifetime.
func (t Trading) MCPNonceTTLDuration() time.Duration {
	if t.MCPNonceTTL == 0 {
		return 5 * time.Minute
	}
	return t.MCPNonceTTL.Std()
}

// SPX holds the SPX-related daemon knobs. Currently just the members
// auto-refresh toggle; grouping under [spx] gives future SPX-scoped
// configs (e.g. fetcher concurrency, sweep tunables) a natural home
// without proliferating top-level TOML sections.
//
// When MembersAutoRefresh is true (default) the daemon fetches
// Wikipedia's constituent list daily at 02:30 ET plus on startup if
// the cached file is stale; when false it loads whatever is on disk
// (or the binary's embedded fallback) and never reaches out.
//
// Use case for pinning off: regulated traders running reproducibility
// audits, air-gapped boxes, anyone debugging breadth drift. The
// IBKR_SPX_MEMBERS_AUTO_REFRESH env var overrides this field at
// runtime: "1" force-enables, "0" force-disables, anything else
// (including unset) defers to the TOML value. Symmetric semantics —
// the env is a bidirectional override, not a one-way kill switch.
type SPX struct {
	// MembersAutoRefresh controls whether the daemon refreshes the S&P 500 constituent list from Wikipedia daily at 02:30 ET (default true; set false to pin the embedded baseline) — overridden symmetrically by the `IBKR_SPX_MEMBERS_AUTO_REFRESH` env var (`1` force-on, `0` force-off).
	//
	// Pointer type lets the daemon distinguish an explicit `members_auto_refresh = true` from "field absent" — both enable the refresher today, but a future "user opted in" vs "default behaviour" distinction stays additive.
	MembersAutoRefresh *bool `toml:"members_auto_refresh"`
}

// MembersAutoRefreshEnabled returns the resolved value of
// [spx] members_auto_refresh. Defaults to true when the field is
// absent — the refresher is opt-out, not opt-in.
func (s SPX) MembersAutoRefreshEnabled() bool {
	if s.MembersAutoRefresh == nil {
		return true
	}
	return *s.MembersAutoRefresh
}

// Scan holds a single scanner preset. Timeout is per-preset and optional;
// <=0 falls back to the daemon's default (20s).
type Scan struct {
	Type     string `toml:"type"`
	Exchange string `toml:"exchange"`
	// Instrument is the IBKR scanner instrument token, such as STK for US stocks or STOCK.EU for European stocks; empty defaults to STK.
	Instrument string   `toml:"instrument"`
	Limit      int      `toml:"limit"`
	Timeout    duration `toml:"timeout"`
}

// Config is the on-disk shape of ~/.config/ibkr/config.toml.
type Config struct {
	Gateway Gateway         `toml:"gateway"`
	Daemon  Daemon          `toml:"daemon"`
	Trading Trading         `toml:"trading"`
	SPX     SPX             `toml:"spx"`
	Scans   map[string]Scan `toml:"scans"`
}

// Resolved is the validated, defaults-applied view a daemon actually uses.
// Gateway carries the raw (pointer-fielded) user input; discovery happens
// later in internal/discover and produces concrete values.
type Resolved struct {
	Gateway Gateway
	Daemon  Daemon
	Trading Trading
	SPX     SPX
	Scans   map[string]Scan
}

// duration is a time.Duration that decodes from a TOML string ("5m").
type duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = duration(v)
	return nil
}

// Std returns the underlying time.Duration.
func (d duration) Std() time.Duration { return time.Duration(d) }

// SetIdleTimeout overrides the daemon's idle timeout. Used by --foreground
// (set 0 to disable idle-shutdown) and by tests that want a fast-firing
// idle watcher; the underlying field type is unexported so callers outside
// this package cannot construct it directly.
func (d *Daemon) SetIdleTimeout(t time.Duration) {
	d.IdleTimeout = duration(t)
}

// DefaultPath returns the canonical config path for the current user.
func DefaultPath() string {
	// docgen:env IBKR_CONFIG | Override the config.toml path. Defaults to `$XDG_CONFIG_HOME/ibkr/config.toml` or `$HOME/.config/ibkr/config.toml`.
	if v := os.Getenv("IBKR_CONFIG"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ibkr", "config.toml")
}

// Load reads and parses the config file at path. A missing file yields a
// zero-value Config — every field nil/empty, meaning "fully auto."
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Reject unknown keys instead of silently dropping them. The previous
	// behavior masked stale-schema configs (e.g. `[profiles.live]` from an
	// older proposal) as "fully auto" — the daemon then probed all ports
	// and the user's pinned settings were silently ignored.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("config %s: unknown key(s): %s (see README §Configuration for the supported schema: [gateway], [daemon], [trading], [spx], [scans.<name>])", path, strings.Join(keys, ", "))
	}
	return cfg, nil
}

// Resolve applies daemon-level defaults and returns the Resolved view.
// Gateway is passed through verbatim (pointer fields preserved) so
// discovery can see what was pinned vs. left auto.
func (c *Config) Resolve() (*Resolved, error) {
	dae := c.Daemon
	if dae.IdleTimeout == 0 {
		// 15 min default (was 5 min). Combined with the persistent option
		// contract cache (PrewarmOptionChain) and the soft-TTL gamma
		// refresh, the cost of a daemon restart is now multi-minute
		// recompute — short idle windows cost more than they save.
		dae.IdleTimeout = duration(15 * time.Minute)
	}
	if dae.LogLevel == "" {
		dae.LogLevel = "info"
	}

	scans := c.Scans
	if scans == nil {
		scans = defaultScans()
	}

	return &Resolved{
		Gateway: c.Gateway,
		Daemon:  dae,
		Trading: c.Trading.WithDefaults(),
		SPX:     c.SPX,
		Scans:   scans,
	}, nil
}

// SPXMembersAutoRefreshFromEnv resolves IBKR_SPX_MEMBERS_AUTO_REFRESH
// as a bidirectional override of the [spx] members_auto_refresh TOML
// field:
//
//   - "1"               → returns (true, true): explicit force-on.
//   - "0"               → returns (false, true): explicit force-off.
//   - unset / other     → returns (false, false): defer to TOML.
//     Garbage values are silently ignored rather than rejected; env-var
//     typos are a CI friction we'd rather not fail-loud on, and there's
//     no realistic compliance posture that wants "fail when the env is
//     present but malformed."
//
// The second return ("forced") distinguishes "env actively overrode
// the TOML" from "env unset, TOML governs." The status renderer uses
// this to pick the "disabled (env)" vs "disabled (config)" suffix.
//
// Lives next to the SPX type so the precedence rules don't have to be
// re-derived at every call site.
func SPXMembersAutoRefreshFromEnv() (enabled bool, forced bool) {
	// docgen:env IBKR_SPX_MEMBERS_AUTO_REFRESH | Symmetric override of `[spx] members_auto_refresh`. `1` force-enables, `0` force-disables, unset / other defers to TOML.
	switch os.Getenv("IBKR_SPX_MEMBERS_AUTO_REFRESH") {
	case "1":
		return true, true
	case "0":
		return false, true
	default:
		return false, false
	}
}

// defaultScans is the built-in preset set, used when the user has no
// [scans.*] block in config.toml. Every Type / Exchange string here was
// validated against a live IB Gateway server-version 203 catalog via
// `ibkr scan params` before being committed — see the validation script
// in the v0.11 PR. If a future gateway drops one of these scanCodes the
// preset will return an error to the user; `ibkr scan params` is the
// canonical recovery path.
//
// Selection rationale (US stock + options trader):
//   - Direction symmetric: top-movers + top-losers, not just gainers.
//   - Volume both ways: most-active is raw share volume (mega-caps);
//     unusual-vol is volume relative to a stock's own history (catches
//     names that are unusual *for themselves*).
//   - Two options scans because they cover different use cases:
//     high-iv-rank surfaces names where IV is elevated vs. their own
//     history (the option-seller's "something brewing" signal);
//     unusual-opt-vol surfaces names where option volume is hot vs.
//     average (the flow signal).
//   - gappers catches the open-print earnings/news reactions that drive
//     the first hour of every session.
//
// Absolute-IV (HIGH_OPT_IMP_VOLAT) was deliberately not included — it
// surfaces the same structural high-IV biotech/SPAC names every day and
// is rarely the actionable signal an option seller wants. Users who
// still want it can copy this map into their config.toml and add the
// extra preset.
func defaultScans() map[string]Scan {
	return map[string]Scan{
		"top-movers":      {Type: "TOP_PERC_GAIN", Exchange: "STK.US.MAJOR", Limit: 20},
		"top-losers":      {Type: "TOP_PERC_LOSE", Exchange: "STK.US.MAJOR", Limit: 20},
		"most-active":     {Type: "MOST_ACTIVE", Exchange: "STK.US.MAJOR", Limit: 20},
		"unusual-vol":     {Type: "HOT_BY_VOLUME", Exchange: "STK.US.MAJOR", Limit: 20},
		"gappers":         {Type: "HIGH_OPEN_GAP", Exchange: "STK.US.MAJOR", Limit: 20},
		"high-iv-rank":    {Type: "HIGH_OPT_IMP_VOLAT_OVER_HIST", Exchange: "STK.US", Limit: 20},
		"unusual-opt-vol": {Type: "HOT_BY_OPT_VOLUME", Exchange: "STK.US.MAJOR", Limit: 20},
	}
}
