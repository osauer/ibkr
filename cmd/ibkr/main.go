// ibkr is the Interactive Brokers command-line client. The binary is
// stateless: each invocation opens a Unix socket to the long-lived daemon
// process, sends one or more JSON-RPC calls, formats the response, and
// exits.
//
// The same binary runs the daemon under the `daemon` subcommand —
// invoked manually for foreground debugging, or auto-spawned by the CLI
// when no socket exists. There is no separate `ibkrd` binary in v0.4+;
// the previous two-binary layout was collapsed to simplify packaging
// and remove the binary-discovery dance.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
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
		printVersion(os.Stdout, "ibkr", hasJSONFlag(rest))
		return
	}

	// `ibkr daemon` is the long-lived background mode. Special-cased
	// before the autospawn path so that running it manually does the
	// right thing (start the daemon) instead of trying to dial its own
	// socket. The autospawn path also calls back into this entrypoint
	// with the same arg, via os.Executable() — single-binary discovery.
	if cmd == "daemon" {
		runDaemon(rest)
		return
	}

	// `ibkr mcp` runs the stdio MCP server, spoken by Claude Desktop and
	// other local MCP clients. Like `daemon`, special-cased before
	// autospawn — the MCP server itself dials (and autospawns if needed)
	// the daemon socket internally so it stays responsive across tool
	// calls.
	if cmd == "mcp" {
		os.Exit(runMCP(rest))
	}

	// `ibkr setup [client]` writes the MCP server entry into local AI
	// client config files (e.g. claude_desktop_config.json). Purely local
	// — no daemon involvement, special-cased here so we skip the dial.
	if cmd == "setup" {
		os.Exit(runSetup(rest))
	}

	color := cli.ShouldColor(os.Stdout)

	// `ibkr <cmd> --help` should not spawn the daemon — render help and exit.
	for _, a := range rest {
		if a == "--help" || a == "-h" || a == "-help" {
			env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Color: color}
			os.Exit(cli.Run(context.Background(), env, cmd, rest))
		}
	}

	// Reject unknown subcommands before autospawn — sparing a dormant
	// install a 100ms+ daemon startup just to fail with the same
	// "unknown subcommand" message cli.Run would produce.
	if !cli.IsKnown(cmd) {
		env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Color: color}
		os.Exit(cli.Run(context.Background(), env, cmd, rest))
	}

	socketPath := dial.DefaultSocketPath()
	conn, err := dial.Connect(socketPath)
	if errors.Is(err, dial.ErrSocketMissing) {
		conn, err = dial.AutospawnAndConnect(socketPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibkr: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// `status` already prints daemon_version in its body, so the extra
	// pre-flight check would just be noise there. Every other command
	// gets a version-skew check — a fast status.health round-trip whose
	// only output is a stderr warning if the daemon was built from a
	// different revision than this CLI binary.
	if cmd != "status" {
		warnIfDaemonVersionMismatch(conn, version)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Apply a default per-invocation deadline so a non-responsive daemon
	// (deadlocked, SIGSTOP'd, kernel-stuck) cannot hang the CLI forever.
	// Streaming commands (`quote --watch` is the only one today) bypass
	// — a long-lived watch must outlive any unary budget.
	if !isStreamingInvocation(cmd, rest) {
		var dlCancel context.CancelFunc
		ctx, dlCancel = context.WithTimeout(ctx, 60*time.Second)
		defer dlCancel()
	}

	env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Conn: conn, Color: color}
	os.Exit(cli.Run(ctx, env, cmd, rest))
}

// warnIfDaemonVersionMismatch fires a tight-timeout status.health call
// after Connect and prints a stderr warning if the daemon was built from
// a different revision than this CLI binary. Best-effort: any RPC error
// (timeout, daemon mid-restart, transport hiccup) is swallowed because a
// failure here must not interfere with the user's actual command.
//
// Quiet cases (no warning):
//   - exact version match
//   - either side stamps the "dev" placeholder — a dev build can't sensibly
//     compare against a tagged release, and "warn against yourself every
//     run" is the wrong default for a working tree
func warnIfDaemonVersionMismatch(conn *dial.Conn, cliVersion string) {
	if cliVersion == "" || cliVersion == "dev" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	daemonVersion, err := conn.DaemonVersion(ctx)
	if err != nil || daemonVersion == "" || daemonVersion == "dev" {
		return
	}
	if daemonVersion == cliVersion {
		return
	}
	fmt.Fprintf(os.Stderr,
		"ibkr: warning: CLI version %s does not match daemon version %s — restart the daemon to pick up the new binary (kill the running ibkr daemon; the next CLI call will respawn it).\n",
		cliVersion, daemonVersion)
}

// isStreamingInvocation reports whether the CLI invocation will hold the
// daemon socket open for an open-ended stream. Today only `quote --watch`
// qualifies.
func isStreamingInvocation(cmd string, args []string) bool {
	if cmd != "quote" {
		return false
	}
	for _, a := range args {
		if a == "--watch" || a == "-watch" || a == "--watch=true" {
			return true
		}
	}
	return false
}
