// ibkr is the Interactive Brokers command-line client. The binary is
// stateless: each invocation opens a Unix socket to the long-lived ibkrd
// daemon, sends one or more JSON-RPC calls, formats the response, and exits.
//
// If the socket is missing on first use, the daemon is auto-spawned in the
// background; the next invocation reuses the now-running daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/osauer/ibkr/internal/cli"
	"github.com/osauer/ibkr/internal/dial"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		cli.PrintUsage(os.Stdout)
		return
	}

	cmd := args[0]
	rest := args[1:]

	if cmd == "version" || cmd == "--version" {
		fmt.Printf("ibkr %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	// `ibkr <cmd> --help` should not spawn the daemon — render help and exit.
	for _, a := range rest {
		if a == "--help" || a == "-h" || a == "-help" {
			env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr}
			os.Exit(cli.Run(context.Background(), env, cmd, rest))
		}
	}

	socketPath := dial.DefaultSocketPath()
	conn, err := dial.Connect(socketPath)
	if errors.Is(err, dial.ErrSocketMissing) {
		if err := spawnDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "ibkr: failed to start ibkrd: %v\n", err)
			os.Exit(1)
		}
		conn, err = dial.WaitForSocket(socketPath, 6*time.Second)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Conn: conn}
	os.Exit(cli.Run(ctx, env, cmd, rest))
}

// spawnDaemon starts ibkrd detached from the CLI process. It locates the
// daemon binary by looking next to the running CLI binary; if that fails
// it falls back to PATH lookup.
func spawnDaemon() error {
	bin, err := locateDaemonBinary()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Detach: we don't Wait for it.
	go func() { _ = cmd.Process.Release() }()
	return nil
}

func locateDaemonBinary() (string, error) {
	if v := os.Getenv("IBKRD_BIN"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v, nil
		}
	}
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), "ibkrd")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if v, err := exec.LookPath("ibkrd"); err == nil {
		return v, nil
	}
	return "", fmt.Errorf("ibkrd binary not found (set IBKRD_BIN or install alongside ibkr)")
}
