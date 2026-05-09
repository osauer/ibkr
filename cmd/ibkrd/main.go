// ibkrd is the long-lived daemon process that owns the IB Gateway connection
// and exposes a Unix-socket JSON-RPC surface to short-lived ibkr CLI clients.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/osauer/ibkr/internal/cache"
	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/daemon"
	"github.com/osauer/ibkr/internal/dial"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		cfgPath    = flag.String("config", "", "config file path (default $XDG_CONFIG_HOME/ibkr/config.toml)")
		profile    = flag.String("profile", "", "profile to use (default from config; falls back to live)")
		socket     = flag.String("socket", "", "unix socket path (default $XDG_RUNTIME_DIR/ibkr/ibkrd.sock)")
		logPath    = flag.String("log", "", "log file path (default ~/.local/state/ibkr/ibkrd.log; 'stderr' for stderr)")
		foreground = flag.Bool("foreground", false, "run in foreground; do not idle-shutdown")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("ibkrd %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	resolved, err := cfg.Resolve(*profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	if *foreground {
		// foreground = no idle shutdown
		resolved.Daemon.IdleTimeout = 0
	}

	socketPath := *socket
	if socketPath == "" {
		socketPath = dial.DefaultSocketPath()
	}

	logWriter, err := openLog(*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		os.Exit(2)
	}
	defer func() {
		if c, ok := logWriter.(io.Closer); ok {
			_ = c.Close()
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cacheDir := filepath.Join(filepath.Dir(socketPath))
	contracts, err := cache.OpenJSONCache(filepath.Join(cacheDir, "contracts.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open contracts cache: %v\n", err)
		os.Exit(2)
	}
	inactive, err := cache.OpenInactiveStore(filepath.Join(cacheDir, "inactive.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open inactive store: %v\n", err)
		os.Exit(2)
	}

	logger := daemon.NewLogger(logWriter, resolved.Daemon.LogLevel)

	srv := daemon.New(daemon.Options{
		Config:        resolved,
		SocketPath:    socketPath,
		Version:       version,
		ContractCache: contracts,
		Inactive:      inactive,
		Logger:        logger,
	})
	defer srv.Stop()

	if err := srv.Start(ctx); err != nil {
		logger.Errorf("start: %v", err)
		os.Exit(1)
	}
	<-ctx.Done()
	_ = slog.Default()
}

func openLog(path string) (io.Writer, error) {
	if path == "stderr" {
		return os.Stderr, nil
	}
	if path == "" {
		path = dial.DefaultLogPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return f, nil
}
