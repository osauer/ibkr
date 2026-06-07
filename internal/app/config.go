package app

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/dial"
)

const (
	DefaultAddr        = "0.0.0.0:8765"
	DefaultPairingTTL  = 5 * time.Minute
	DefaultPollEvery   = 5 * time.Second
	DefaultCanaryEvery = time.Minute
)

type Options struct {
	Addr             string
	PublicURL        string
	PublicURLFromEnv bool
	Remote           bool
	RemoteURL        string
	StateDir         string
	SocketPath       string
	Version          string
	PairingTTL       time.Duration
	PollEvery        time.Duration
	CanaryEvery      time.Duration
}

func DefaultOptions(version string) Options {
	// docgen:env IBKR_APP_ADDR | HTTP listen address for `ibkr app`. Defaults to `0.0.0.0:8765` so a paired phone on the LAN can reach the app.
	addr := strings.TrimSpace(os.Getenv("IBKR_APP_ADDR"))
	if addr == "" {
		addr = DefaultAddr
	}
	// docgen:env IBKR_APP_STATE_DIR | Directory for `ibkr app` paired devices, alert settings, VAPID keys, and alert history. Defaults to `$XDG_STATE_HOME/ibkr/app` or `$HOME/.local/state/ibkr/app`.
	stateDir := strings.TrimSpace(os.Getenv("IBKR_APP_STATE_DIR"))
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	// docgen:env IBKR_APP_PUBLIC_URL | Public trusted HTTPS base URL for the `ibkr app` PWA/relay origin. Defaults to a LAN URL for wildcard listen addresses, falling back to loopback when no LAN address is available.
	publicURL := strings.TrimRight(strings.TrimSpace(os.Getenv("IBKR_APP_PUBLIC_URL")), "/")
	publicURLFromEnv := publicURL != ""
	if publicURL == "" {
		publicURL = PublicURLForAddr(addr)
	}
	// docgen:env IBKR_APP_REMOTE | Enable the outbound Cloudflare Worker relay for `ibkr app`, making pairing URLs reachable through the public relay origin.
	remote := parseBoolEnv(os.Getenv("IBKR_APP_REMOTE"))
	// docgen:env IBKR_APP_REMOTE_URL | Cloudflare Worker relay base URL for `ibkr app --remote`. Defaults to `https://remote.osauer.dev`.
	remoteURL := strings.TrimRight(strings.TrimSpace(os.Getenv("IBKR_APP_REMOTE_URL")), "/")
	if remoteURL == "" {
		remoteURL = relayDefaultURL()
	}
	return Options{
		Addr:             addr,
		PublicURL:        publicURL,
		PublicURLFromEnv: publicURLFromEnv,
		Remote:           remote,
		RemoteURL:        remoteURL,
		StateDir:         stateDir,
		SocketPath:       dial.DefaultSocketPath(),
		Version:          version,
		PairingTTL:       DefaultPairingTTL,
		PollEvery:        DefaultPollEvery,
		CanaryEvery:      DefaultCanaryEvery,
	}
}

func parseBoolEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func relayDefaultURL() string {
	return "https://remote.osauer.dev"
}

func defaultStateDir() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "app")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ibkr", "app")
}
