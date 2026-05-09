// Package cli implements the ibkr CLI's user-facing subcommands. The CLI
// process is stateless; each subcommand opens a Unix-socket connection to
// the daemon, sends one or more JSON-RPC calls, formats the response, and
// exits. A missing daemon is autospawned on demand by the dispatcher in
// cmd/ibkr.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/dial"
)

// Env is the per-invocation context shared by every subcommand.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Conn   *dial.Conn
}

// CommandFunc is the signature implemented by every subcommand handler.
type CommandFunc func(ctx context.Context, env *Env, args []string) int

// Run dispatches the subcommand named by cmd. Returns the process exit code.
//
// Args are reordered so all flags come before positional arguments — Go's
// flag package stops at the first non-flag token, but users naturally write
// `ibkr quote AAPL --json` rather than `ibkr quote --json AAPL`.
func Run(ctx context.Context, env *Env, cmd string, args []string) int {
	c, ok := lookupCommand(cmd)
	if !ok || c.Fn == nil {
		fmt.Fprintf(env.Stderr, "ibkr: unknown subcommand %q (run `ibkr --help`)\n", cmd)
		return 2
	}
	return c.Fn(ctx, env, hoistFlags(args))
}

// parseExit converts a *flag.FlagSet.Parse error into a process exit code.
// flag.ErrHelp means --help was passed and Usage already ran cleanly → 0;
// any other parse error → 2 (matches Go's default ExitOnError behavior).
func parseExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// hoistFlags moves -flag and --flag tokens (and their values, if separate)
// to the front of the slice while preserving relative order on each side.
// Long-form `--flag=value` is treated as a single token.
func hoistFlags(in []string) []string {
	flags, positional := []string{}, []string{}
	skipNext := false
	for i, a := range in {
		if skipNext {
			skipNext = false
			flags = append(flags, a)
			continue
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			// Detect "--flag value" (value on next token) vs "--flag=value".
			if !strings.Contains(a, "=") && i+1 < len(in) && !strings.HasPrefix(in[i+1], "-") {
				// Heuristic: only treat next as value if the flag is one of the
				// known value-taking flags. False positives are tolerable since
				// runQuote's positional parser re-checks shape.
				if isValueFlag(strings.TrimLeft(a, "-")) {
					skipNext = true
				}
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

func isValueFlag(name string) bool {
	switch name {
	case "expiry", "width", "side", "rate", "timeout", "limit", "symbol",
		"type", "sort", "days", "by":
		return true
	}
	return false
}

// Command bundles a subcommand's name, one-line summary, optional usage
// example, and handler. One slice — single source of truth for both the
// dispatcher and the help table. `status` is listed first because users
// hitting any other command without a healthy gateway will be redirected
// here by the gateway_unavailable hint.
type Command struct {
	Name    string
	Summary string
	Usage   string // optional one-line usage example shown in `ibkr X --help`
	Fn      CommandFunc
}

// commands is populated in init() to break the package-init cycle that
// would otherwise form: var → handler → flagSet → lookupCommand → var.
// Order is load-bearing for the help table (status first).
var commands []Command

func init() {
	commands = []Command{
		{"status", "Daemon + gateway health (run this first if anything fails)", "ibkr status [--json]", runStatus},
		{"account", "Account summary snapshot (NLV, BP, cash, margin)", "ibkr account [--json]", runAccount},
		{"positions", "List open positions (stocks + options)", "ibkr positions [--symbol SYM] [--type stk|opt] [--sort alpha|pnl|value] [--by underlying] [--json]", runPositions},
		{"quote", "Snapshot or stream quotes for symbols / option contracts", "ibkr quote SYM[,SYM…] | ibkr quote SYM YYMMDD C|P STRIKE [--watch --rate 250ms] [--json]", runQuote},
		{"chain", "Option chain table (ATM ± width strikes)", "ibkr chain SYM --expiry YYYY-MM-DD [--width 5] [--side calls|puts|both] [--json]", runChain},
		{"history", "Daily OHLCV bars for a symbol", "ibkr history SYM [--days 90] [--json]", runHistory},
		{"scan", "Run a configured scanner preset", "ibkr scan <preset> | ibkr scan list [--json]", runScan},
		{"version", "Print version, commit, build date", "ibkr version", nil}, // version is handled in cmd/ibkr/main.go before dispatch
	}
}

// lookupCommand returns the Command with the given name. n=7, scan is fine
// and avoids the package-init cycle a map var would create (commands →
// handler → flagSet → map → commands).
func lookupCommand(name string) (Command, bool) {
	for _, c := range commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// PrintUsage writes the top-level help text.
func PrintUsage(w io.Writer) {
	fmt.Fprintln(w, "ibkr — Interactive Brokers CLI (read-only)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: ibkr <subcommand> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-10s  %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Add --json to any subcommand to emit machine-readable output.")
	fmt.Fprintln(w, "First run? Try `ibkr status` to verify the gateway is reachable.")
}

// flagSet builds a *flag.FlagSet wired to the env's writers and equipped
// with a custom Usage that matches the top-level help style. Parse errors
// go to stderr; the Usage output (triggered by --help, after flags are
// registered) goes to stdout.
func flagSet(env *Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("ibkr "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	cmd, known := lookupCommand(name)
	fs.Usage = func() {
		w := env.Stdout
		if known {
			fmt.Fprintf(w, "ibkr %s — %s\n\n", cmd.Name, cmd.Summary)
			if cmd.Usage != "" {
				fmt.Fprintf(w, "Usage: %s\n\n", cmd.Usage)
			}
		} else {
			fmt.Fprintf(w, "Usage of ibkr %s\n\n", name)
		}
		var any bool
		fs.VisitAll(func(f *flag.Flag) {
			if !any {
				fmt.Fprintln(w, "Flags:")
				any = true
			}
			fmt.Fprintf(w, "  --%-10s  %s\n", f.Name, f.Usage)
		})
	}
	return fs
}

// printJSON writes obj as indented JSON, returning a non-zero exit code if
// marshal fails (which would indicate a programming error).
func printJSON(env *Env, obj any) int {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		fmt.Fprintf(env.Stderr, "ibkr: encode json: %v\n", err)
		return 1
	}
	return 0
}

// fail writes a friendly error line and returns code 1. If the underlying
// message looks like a gateway-unavailable error from the daemon, an extra
// hint line is appended pointing the user at `ibkr status`.
func fail(env *Env, format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(env.Stderr, "ibkr: %s\n", msg)
	if isGatewayUnavailable(msg) {
		fmt.Fprintln(env.Stderr, "  hint: run `ibkr status` to inspect the gateway connection,")
		fmt.Fprintln(env.Stderr, "        then start IB Gateway (or check ~/.local/state/ibkr/ibkrd.log) and retry.")
	}
	return 1
}

// isGatewayUnavailable matches the error.Code prefix the daemon emits for
// CodeGatewayUnavailable. Kept loose because the message arrives flattened.
func isGatewayUnavailable(msg string) bool {
	return strings.Contains(msg, "gateway_unavailable")
}

// formatMoney renders a USD-style amount with grouping; "$ 248,310.42".
func formatMoney(v float64) string {
	if v == 0 {
		return "$         —"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	grouped := groupThousands(intPart)
	out := "$ " + grouped + frac
	if neg {
		return "-" + out
	}
	return out
}

func groupThousands(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	out := ""
	for i, r := range s {
		if i > 0 && (n-i)%3 == 0 {
			out += ","
		}
		out += string(r)
	}
	return out
}

// formatTimeShort returns "HH:MM:SS Z" suitable for status lines.
func formatTimeShort(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("15:04:05 MST")
}

// dataTypeBadge surfaces non-live data clearly. On the happy path (live or
// empty) it returns the empty string — the badge is signal only when data
// is delayed/frozen and the user needs to know.
func dataTypeBadge(dt string) string {
	if dt == "" || dt == "live" {
		return ""
	}
	return "data=" + dt + " ⚠"
}

// suffixBadge prefixes the badge with `  ·  ` when present, returning the
// empty string otherwise. Callers can append this to a header line without
// trailing whitespace on the live path.
func suffixBadge(dt string) string {
	b := dataTypeBadge(dt)
	if b == "" {
		return ""
	}
	return "  ·  " + b
}

// orDash renders a *float64 with the given format, or "—" if nil.
func orDash(p *float64, format string) string {
	if p == nil {
		return "       —"
	}
	return fmt.Sprintf(format, *p)
}

// formatSize renders a quote size compactly: 850, 1.2K, 12K, 1.4M.
// Returns "—" for nil or zero so quote tables stay legible.
func formatSize[T int | int64](p *T) string {
	if p == nil {
		return "—"
	}
	v := int64(*p)
	if v <= 0 {
		return "—"
	}
	switch {
	case v < 1000:
		return fmt.Sprintf("%d", v)
	case v < 10_000:
		return fmt.Sprintf("%.1fK", float64(v)/1000)
	case v < 1_000_000:
		return fmt.Sprintf("%dK", v/1000)
	case v < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1_000_000)
	default:
		return fmt.Sprintf("%dM", v/1_000_000)
	}
}
