// mcp.go hosts the `ibkr mcp` subcommand: a stdio MCP (Model Context
// Protocol) server that exposes the daemon's read-only surface to local
// MCP clients (Claude Desktop, Cursor, Continue, etc.). Tool/resource requests
// lazily dial — and autospawn if needed — the same Unix socket the CLI uses, so
// a single daemon serves both surfaces without an idle MCP process holding it
// open.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/mcp"
)

const mcpParentPollInterval = 2 * time.Second

func runMCP(args []string) int {
	// MCP servers take no flags today. Reject extras explicitly so a
	// typo doesn't get silently swallowed and leave the client wondering.
	if len(args) > 0 {
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "-help") {
			printMCPUsage(os.Stdout)
			return 0
		}
		fmt.Fprintln(os.Stderr, "ibkr mcp: takes no arguments")
		return 2
	}

	ctx, cancel := mcpLifecycleContext()
	defer cancel()

	socketPath := dial.DefaultSocketPath()
	srv := mcp.NewServer(nil, effectiveVersion())
	// Tool calls and streaming subscriptions use short-lived daemon
	// connections so a timed-out MCP call cannot leave a late daemon reply
	// queued on a shared control socket. The daemon is opened lazily on the
	// first request, so an MCP process leaked by its host cannot keep the
	// daemon's active-connection count above zero while idle.
	srv.SetContextDialer(func(ctx context.Context) (*dial.Conn, error) {
		return dialMCPDaemon(ctx, socketPath)
	})
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "ibkr mcp: %v\n", err)
		return 1
	}
	return 0
}

func mcpLifecycleContext() (context.Context, context.CancelFunc) {
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	ctx, cancel := context.WithCancel(sigCtx)
	initialParent := os.Getppid()
	go watchMCPParent(ctx, cancel, initialParent, os.Getppid, mcpParentPollInterval)
	return ctx, func() {
		cancel()
		stopSignals()
	}
}

func watchMCPParent(ctx context.Context, cancel context.CancelFunc, initialParent int, getppid func() int, interval time.Duration) {
	if initialParent <= 1 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if current := getppid(); current <= 1 || current != initialParent {
				cancel()
				return
			}
		}
	}
}

func dialMCPDaemon(ctx context.Context, socketPath string) (*dial.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := dial.Connect(socketPath)
	if errors.Is(err, dial.ErrSocketMissing) {
		conn, err = dial.AutospawnAndConnectContext(ctx, socketPath)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func printMCPUsage(w *os.File) {
	fmt.Fprintln(w, "ibkr mcp - run the stdio MCP server for local AI clients")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: ibkr mcp")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Configure your MCP host with an absolute command path and the arg \"mcp\":")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "{")
	fmt.Fprintln(w, `  "mcpServers": {`)
	fmt.Fprintln(w, `    "ibkr": { "command": "/ABSOLUTE/PATH/TO/ibkr", "args": ["mcp"] }`)
	fmt.Fprintln(w, "  }")
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The server exposes read-only ibkr_* tools plus the ibkr://quote/{symbol}")
	fmt.Fprintln(w, "resource template. resources/read returns one snapshot; resources/subscribe")
	fmt.Fprintln(w, "streams quote updates until unsubscribe or client shutdown.")
}
