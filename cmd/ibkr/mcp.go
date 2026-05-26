// mcp.go hosts the `ibkr mcp` subcommand: a stdio MCP (Model Context
// Protocol) server that exposes the daemon's read-only surface to local
// MCP clients (Claude Desktop, Cursor, Continue, etc.). The MCP server
// dials — and autospawns if needed — the same Unix socket the CLI uses,
// so a single daemon serves both surfaces.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/mcp"
)

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

	socketPath := dial.DefaultSocketPath()
	conn, err := dial.Connect(socketPath)
	if errors.Is(err, dial.ErrSocketMissing) {
		conn, err = dial.AutospawnAndConnect(socketPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr mcp: %v\n", err)
		return 1
	}
	defer conn.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := mcp.NewServer(conn, effectiveVersion())
	// Tool calls and streaming subscriptions use short-lived daemon
	// connections so a timed-out MCP call cannot leave a late daemon reply
	// queued on the server's shared control socket. The dialer reuses the
	// same socket path the unary conn was opened against, so the daemon's
	// subscription manager reference-counts every (CLI watch, MCP
	// subscriber, snapshot poll) into one IBKR line.
	srv.SetDialer(func() (*dial.Conn, error) {
		return dial.Connect(socketPath)
	})
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "ibkr mcp: %v\n", err)
		return 1
	}
	return 0
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
