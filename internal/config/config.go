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
	Host     string `toml:"host"`
	Port     *int   `toml:"port"`
	ClientID *int   `toml:"client_id"`
	Account  string `toml:"account"`
	TLS      *bool  `toml:"tls"`
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
	IdleTimeout duration `toml:"idle_timeout"`
	LogLevel    string   `toml:"log_level"`
}

// Scan holds a single scanner preset. Timeout is per-preset and optional;
// <=0 falls back to the daemon's default (20s).
type Scan struct {
	Type     string   `toml:"type"`
	Exchange string   `toml:"exchange"`
	Limit    int      `toml:"limit"`
	Timeout  duration `toml:"timeout"`
}

// Config is the on-disk shape of ~/.config/ibkr/config.toml.
type Config struct {
	Gateway Gateway         `toml:"gateway"`
	Daemon  Daemon          `toml:"daemon"`
	Scans   map[string]Scan `toml:"scans"`
}

// Resolved is the validated, defaults-applied view a daemon actually uses.
// Gateway carries the raw (pointer-fielded) user input; discovery happens
// later in internal/discover and produces concrete values.
type Resolved struct {
	Gateway Gateway
	Daemon  Daemon
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
		return nil, fmt.Errorf("config %s: unknown key(s): %s (see README §Configuration for the supported schema: [gateway], [daemon], [scans.<name>])", path, strings.Join(keys, ", "))
	}
	return cfg, nil
}

// Resolve applies daemon-level defaults and returns the Resolved view.
// Gateway is passed through verbatim (pointer fields preserved) so
// discovery can see what was pinned vs. left auto.
func (c *Config) Resolve() (*Resolved, error) {
	dae := c.Daemon
	if dae.IdleTimeout == 0 {
		dae.IdleTimeout = duration(5 * time.Minute)
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
		Scans:   scans,
	}, nil
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

// IntPtr / BoolPtr are convenience helpers for tests and code constructing
// a Gateway programmatically. Hand-written pointer literals are a noisy
// idiom in Go; these read better at the call site.
func IntPtr(v int) *int    { return &v }
func BoolPtr(v bool) *bool { return &v }
