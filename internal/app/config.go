package app

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/dial"
)

const (
	DefaultAddr        = "127.0.0.1:8765"
	DefaultPairingTTL  = 5 * time.Minute
	DefaultPollEvery   = 5 * time.Second
	DefaultCanaryEvery = time.Minute
)

type Options struct {
	Addr        string
	PublicURL   string
	StateDir    string
	SocketPath  string
	Version     string
	PairingTTL  time.Duration
	PollEvery   time.Duration
	CanaryEvery time.Duration
}

func DefaultOptions(version string) Options {
	// docgen:env IBKR_APP_ADDR | HTTP listen address for `ibkr app`. Defaults to `127.0.0.1:8765`.
	addr := strings.TrimSpace(os.Getenv("IBKR_APP_ADDR"))
	if addr == "" {
		addr = DefaultAddr
	}
	// docgen:env IBKR_APP_STATE_DIR | Directory for `ibkr app` paired devices, alert settings, VAPID keys, and alert history. Defaults to `$XDG_STATE_HOME/ibkr/app` or `$HOME/.local/state/ibkr/app`.
	stateDir := strings.TrimSpace(os.Getenv("IBKR_APP_STATE_DIR"))
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	// docgen:env IBKR_APP_PUBLIC_URL | Public trusted HTTPS base URL for the `ibkr app` PWA/relay origin. Defaults to the local HTTP listen address for LAN/dev use.
	publicURL := strings.TrimRight(strings.TrimSpace(os.Getenv("IBKR_APP_PUBLIC_URL")), "/")
	if publicURL == "" {
		publicURL = "http://" + addr
		if host, port, err := net.SplitHostPort(addr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
			publicURL = "http://127.0.0.1:" + port
		}
	}
	return Options{
		Addr:        addr,
		PublicURL:   publicURL,
		StateDir:    stateDir,
		SocketPath:  dial.DefaultSocketPath(),
		Version:     version,
		PairingTTL:  DefaultPairingTTL,
		PollEvery:   DefaultPollEvery,
		CanaryEvery: DefaultCanaryEvery,
	}
}

func defaultStateDir() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "app")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ibkr", "app")
}
