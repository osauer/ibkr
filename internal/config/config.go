// Package config loads and validates the ibkr daemon's TOML configuration.
//
// The active profile selects which gateway the daemon connects to; named
// scan presets are also resolved from the same file. Defaults exist so that
// a freshly installed binary connects to a local IB Gateway running on the
// standard live API port without any user intervention.
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

// Profile holds connection details for a single gateway endpoint.
type Profile struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	ClientID int    `toml:"client_id"`
	Account  string `toml:"account"`
	TLS      bool   `toml:"tls"`
}

// Daemon holds runtime knobs for the ibkrd process.
type Daemon struct {
	IdleTimeout duration `toml:"idle_timeout"`
	LogLevel    string   `toml:"log_level"`
}

// Scan holds a single scanner preset definition. Timeout is per-preset and
// optional; <=0 falls back to the daemon's default (20s) which already
// covers IBKR's typical scanner response window with margin.
type Scan struct {
	Type     string   `toml:"type"`
	Exchange string   `toml:"exchange"`
	Limit    int      `toml:"limit"`
	Timeout  duration `toml:"timeout"`
}

// Config is the on-disk shape of ~/.config/ibkr/config.toml.
type Config struct {
	DefaultProfile string             `toml:"default_profile"`
	Profiles       map[string]Profile `toml:"profiles"`
	Daemon         Daemon             `toml:"daemon"`
	Scans          map[string]Scan    `toml:"scans"`
}

// Resolved is the validated, defaults-applied view a daemon actually uses.
type Resolved struct {
	ProfileName string
	Profile     Profile
	Daemon      Daemon
	Scans       map[string]Scan
}

// duration is a time.Duration that decodes from a TOML string ("30m").
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
// minimally-populated Config so the daemon can fall back to defaults.
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
	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// Resolve picks the profile by name (or default) and applies safety defaults.
// An unknown profile name is an error; an empty config is acceptable and
// returns the live profile defaults documented in the README.
func (c *Config) Resolve(profileName string) (*Resolved, error) {
	if profileName == "" {
		profileName = c.DefaultProfile
	}
	if profileName == "" {
		profileName = "live"
	}

	prof, ok := c.Profiles[profileName]
	if !ok {
		if len(c.Profiles) == 0 {
			prof = defaultProfile(profileName)
		} else {
			return nil, fmt.Errorf("unknown profile %q (have: %s)", profileName, strings.Join(profileNames(c.Profiles), ", "))
		}
	}
	prof = applyProfileDefaults(profileName, prof)

	dae := c.Daemon
	if dae.IdleTimeout == 0 {
		dae.IdleTimeout = duration(30 * time.Minute)
	}
	if dae.LogLevel == "" {
		dae.LogLevel = "info"
	}

	scans := c.Scans
	if scans == nil {
		scans = defaultScans()
	}

	return &Resolved{
		ProfileName: profileName,
		Profile:     prof,
		Daemon:      dae,
		Scans:       scans,
	}, nil
}

func profileNames(m map[string]Profile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func defaultProfile(name string) Profile {
	port := 4001
	id := 15
	if name == "paper" {
		port = 4002
		id = 16
	}
	return Profile{
		Host:     "127.0.0.1",
		Port:     port,
		ClientID: id,
		Account:  "",
		TLS:      false,
	}
}

func applyProfileDefaults(name string, p Profile) Profile {
	if p.Host == "" {
		p.Host = "127.0.0.1"
	}
	if p.Port == 0 {
		switch name {
		case "paper":
			p.Port = 4002
		default:
			p.Port = 4001
		}
	}
	if p.ClientID == 0 {
		p.ClientID = 15
		if name == "paper" {
			p.ClientID = 16
		}
	}
	return p
}

func defaultScans() map[string]Scan {
	return map[string]Scan{
		"top-movers":  {Type: "TOP_PERC_GAIN", Exchange: "STK.US.MAJOR", Limit: 20},
		"high-iv":     {Type: "HIGH_OPT_IMP_VOLAT", Exchange: "STK.US", Limit: 20},
		"unusual-vol": {Type: "HOT_BY_VOLUME", Exchange: "STK.US.MAJOR", Limit: 20},
		"most-active": {Type: "MOST_ACTIVE", Exchange: "STK.US.MAJOR", Limit: 20},
	}
}
