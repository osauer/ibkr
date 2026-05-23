// daemon.go hosts the in-binary daemon subcommand. Invoked manually for
// foreground debugging (`ibkr daemon --foreground`) or automatically by
// the CLI's autospawn path (`ibkr daemon`, detached, output to log).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/daemon"
	"github.com/osauer/ibkr/internal/dial"
)

func runDaemon(args []string) {
	fs := flag.NewFlagSet("ibkr daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default $XDG_CONFIG_HOME/ibkr/config.toml)")
	socket := fs.String("socket", "", "unix socket path (default $XDG_RUNTIME_DIR/ibkr/ibkr.sock)")
	logPath := fs.String("log", "", "log file path (default ~/.local/state/ibkr/ibkr-daemon.log; 'stderr' for stderr)")
	foreground := fs.Bool("foreground", false, "run in foreground; do not idle-shutdown")
	showVer := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *showVer {
		printVersion(os.Stdout, "ibkr daemon", false)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	if *foreground {
		resolved.Daemon.SetIdleTimeout(0)
	}

	socketPath := *socket
	if socketPath == "" {
		socketPath = dial.DefaultSocketPath()
	}

	logWriter, err := openDaemonLog(*logPath)
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

	logger := daemon.NewLogger(logWriter, resolved.Daemon.LogLevel)

	srv := daemon.New(daemon.Options{
		Config:     resolved,
		SocketPath: socketPath,
		Version:    effectiveVersion(),
		Logger:     logger,
	})
	defer srv.Stop()

	if err := srv.Start(ctx); err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			logger.Infof("Another daemon is already running for socket %s; exiting cleanly", socketPath)
			return
		}
		logger.Errorf("start: %v", err)
		os.Exit(1)
	}
}

func openDaemonLog(path string) (io.Writer, error) {
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
