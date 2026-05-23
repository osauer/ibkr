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
	"github.com/osauer/ibkr/internal/rpc"
)

// Env is the per-invocation context shared by every subcommand.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Conn   *dial.Conn
	// Color is true when ANSI color escapes should be emitted on Stdout.
	// Computed once in main.go via ShouldColor(Stdout) so renderers don't
	// re-syscall stat() per value. Defaults to false in tests (Stdout is
	// usually a *bytes.Buffer), keeping golden-substring assertions stable.
	Color bool
}

// CommandFunc is the signature implemented by every subcommand handler.
type CommandFunc func(ctx context.Context, env *Env, args []string) int

// Run dispatches the subcommand named by cmd. Returns the process exit code.
//
// Args are reordered so all flags come before positional arguments — Go's
// flag package stops at the first non-flag token, but users naturally write
// `ibkr quote AAPL --json` rather than `ibkr quote --json AAPL`.
//
// On an unknown subcommand we print the full top-level usage to stderr,
// not just the bare hint, so a user who typo'd or guessed wrong sees the
// real list of verbs immediately. Pattern matches git/kubectl/gh.
func Run(ctx context.Context, env *Env, cmd string, args []string) int {
	c, ok := lookupCommand(cmd)
	if !ok || c.Fn == nil {
		fmt.Fprintf(env.Stderr, "ibkr: unknown subcommand %q\n\n", cmd)
		PrintUsage(env.Stderr)
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
		"type", "sort", "days", "by",
		"entry", "stop", "risk-pct", "lot", "fx",
		"exchange", "instrument":
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
		{"account", "Account summary snapshot (NLV, BP, cash, margin, daily P&L)", "ibkr account [--watch --rate 1s] [--json]", runAccount},
		{"positions", "List open positions (stocks + options)", "ibkr positions [--symbol SYM] [--type stk|opt] [--sort alpha|pnl|value] [--by underlying] [--watch --rate 1s] [--json]", runPositions},
		{"quote", "Snapshot or stream quotes for symbols / option contracts", "ibkr quote SYM[,SYM…] [--watch --rate 250ms] | ibkr quote SYM YYMMDD C|P STRIKE [--json]", runQuote},
		{"chain", "Option chain table or expiry list", "ibkr chain SYM [--expiry YYYY-MM-DD [--width 5] [--side calls|puts|both]] [--no-iv] [--all-expiries] [--json]", runChain},
		{"history", "Daily OHLCV bars for a symbol", "ibkr history SYM [--days 90] [--json]", runHistory},
		{"breadth", "S&P 500 breadth — % above 50/200-DMA + new-highs/new-lows, computed locally from constituent fan-out (~60 min cold)", "ibkr breadth [--days 30] [--json]", runBreadth},
		{"gamma", "Combined SPY+SPX dealer zero-gamma estimate (default; --only spy|spx to narrow; heavy compute, once per NY trading day)", "ibkr gamma [--no-wait] [--force] [--only spy|spx] [--explain] [--json]", runGamma},
		{"regime", "Risk-regime snapshot: all 5 indicators in one call (VIX term, HYG/SPY, USD/JPY, gamma, breadth)", "ibkr regime [--json]", runRegime},
		{"scan", "Run a scanner preset or an ad-hoc scan; dump the gateway catalog with `scan params`", "ibkr scan <preset> | ibkr scan list | ibkr scan params [--instrument STK] [--raw] | ibkr scan --type SCANCODE --exchange LOCATIONCODE [--limit N] [--json]", runScan},
		{"size", "Fixed-fractional position sizing pegged to live NLV", "ibkr size --symbol SYM --entry F --stop F [--risk-pct 1.0] [--side long|short] [--lot 1] [--fx 1.0] [--json]", runSize},
		{"setup", "Wire ibkr into a local AI client (default: claude-desktop)", "ibkr setup [claude-desktop]", nil},                               // dispatched in cmd/ibkr/main.go — no daemon contact
		{"update", "Self-update the ibkr binary from the latest GitHub release", "ibkr update [--check] [--force] [--restart|--no-restart]", nil}, // dispatched in cmd/ibkr/main.go — no daemon contact
		{"version", "Print version, commit, build date", "ibkr version", nil},                                                                     // version is handled in cmd/ibkr/main.go before dispatch
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

// IsKnown reports whether name is a registered subcommand. Used by
// cmd/ibkr to skip the daemon autospawn for typos and unknown commands —
// otherwise `ibkr nonsense` would spawn ibkrd just to fail with
// "unknown subcommand", which is wasteful and confusing if it tips a
// dormant install into a long startup.
func IsKnown(name string) bool {
	_, ok := lookupCommand(name)
	return ok
}

// Commands returns the registered subcommand entries in declaration order.
// Exported so the MCP server's parity test can assert that every CLI command
// has an MCP tool counterpart (or is on the documented exclude list).
func Commands() []Command {
	out := make([]Command, len(commands))
	copy(out, commands)
	return out
}

// PrintUsage writes the top-level help text. The subcommand list is
// followed by global hints — most importantly the per-command --help
// pointer, since users discovering the tool need to know that every
// subcommand has its own flag list.
func PrintUsage(w io.Writer) {
	fmt.Fprintln(w, "ibkr — Interactive Brokers CLI (read-only)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: ibkr <subcommand> [flags] [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-10s  %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `ibkr <subcommand> --help` to see the flags it supports.")
	fmt.Fprintln(w, "Add --json to any subcommand to emit machine-readable output.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Color: respects NO_COLOR=1 to disable; IBKR_COLOR=always|never overrides.")
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
	return printJSONTo(env, env.Stdout, obj)
}

// printJSONTo is printJSON with an explicit destination writer. Used by
// renderers that emit to a buffer (watch loop) before flushing to stdout.
func printJSONTo(env *Env, out io.Writer, obj any) int {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		fmt.Fprintf(env.Stderr, "ibkr: encode json: %v\n", err)
		return 1
	}
	return 0
}

// fail writes a friendly error line and returns code 1. If the underlying
// message looks like a gateway-unavailable error from the daemon, an extra
// hint line is appended pointing the user at `ibkr status`. The hint
// covers both common cases — a missing/down gateway AND a freshly-spawned
// daemon whose handshake hasn't completed — without being prescriptive.
func fail(env *Env, format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(env.Stderr, "ibkr: %s\n", msg)
	if isGatewayUnavailable(msg) {
		fmt.Fprintln(env.Stderr, "  hint: run `ibkr status` to see whether the daemon is still")
		fmt.Fprintln(env.Stderr, "        connecting (retry in a few seconds) or the gateway is")
		fmt.Fprintln(env.Stderr, "        down (start IB Gateway; check ~/.local/state/ibkr/ibkr-daemon.log).")
	}
	return 1
}

// isGatewayUnavailable matches the error.Code prefix the daemon emits for
// CodeGatewayUnavailable. Kept loose because the message arrives flattened.
func isGatewayUnavailable(msg string) bool {
	return strings.Contains(msg, "gateway_unavailable")
}

// formatMoney renders a USD-style amount with grouping; "$ 248,310.42".
// Used by renderers that work with intrinsically USD-only data (chain
// strikes, history, scan rows); position renderers thread the actual
// currency through formatMoneyCcy.
func formatMoney(v float64) string {
	return formatMoneyCcy(v, "USD")
}

// formatMoneyCcy renders a money amount with the right currency prefix.
// Symbols ($, €, £) for the common cases; the 3-letter ISO code as a
// prefix for everything else. Empty currency falls back to "$" to match
// formatMoney's USD default. The width of the prefix is padded so a
// column of mixed currencies stays roughly aligned (1 char for symbols,
// 3 chars for ISO).
func formatMoneyCcy(v float64, ccy string) string {
	prefix := moneyPrefix(ccy)
	if v == 0 {
		// Em-dash placeholder; width matches the legacy "$         —"
		// when prefix is "$ " (length 2) so existing tables don't shift.
		return prefix + "        —"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	grouped := groupThousands(intPart)
	out := prefix + grouped + frac
	if neg {
		return "-" + out
	}
	return out
}

// formatMoneyBare renders the amount with no currency prefix at all.
// Use this when the currency is already named on the same line (e.g.
// the Portfolio block in `ibkr positions` shows "Dollar delta X USD"
// and the X should be currency-neutral).
func formatMoneyBare(v float64) string {
	if v == 0 {
		return "         —"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	out := groupThousands(intPart) + frac
	if neg {
		return "-" + out
	}
	return out
}

// moneyPrefix maps an ISO currency code to a short prefix suitable for
// inline money rendering. Symbols for the handful of currencies that
// have one; the ISO code itself for everything else. Always ends in a
// space so callers can concatenate cleanly without extra glue.
func moneyPrefix(ccy string) string {
	switch strings.ToUpper(strings.TrimSpace(ccy)) {
	case "", "USD":
		return "$ "
	case "EUR":
		return "€ "
	case "GBP":
		return "£ "
	case "JPY":
		return "¥ "
	default:
		return strings.ToUpper(strings.TrimSpace(ccy)) + " "
	}
}

func groupThousands(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var out strings.Builder
	for i, r := range s {
		if i > 0 && (n-i)%3 == 0 {
			out.WriteString(",")
		}
		out.WriteString(string(r))
	}
	return out.String()
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
// is delayed/frozen and the user needs to know. Tinted yellow when color
// is enabled so it pops above the table without needing the ⚠ glyph to do
// all the work.
func (e *Env) dataTypeBadge(dt string) string {
	if rpc.IsLiveDataType(dt) {
		return ""
	}
	return e.yellow("data=" + dt + " ⚠")
}

// suffixBadge prefixes the badge with `  ·  ` when present, returning the
// empty string otherwise. Callers can append this to a header line without
// trailing whitespace on the live path.
func (e *Env) suffixBadge(dt string) string {
	b := e.dataTypeBadge(dt)
	if b == "" {
		return ""
	}
	return "  ·  " + b
}

// formatMoneyNeg renders a balance amount, painting only the negative case
// red and the zero case dim — positive balances stay uncolored. Use for
// non-P&L money fields (cash, margin, NLV) where a positive number is
// neutral signal but a negative one (cash debit, blown account) should
// catch the eye. Different from formatPnL by design: balance views would
// look celebratory if every positive number were green.
func (e *Env) formatMoneyNeg(v float64) string {
	return e.formatMoneyNegCcy(v, "USD")
}

// colorBySign wraps s with the env's red/green/dim color based on v.
// Centralises the sign→color rule shared by the money / P&L / day-change
// / Greeks formatters. signMode picks how zero is treated:
//
//	signNeg  — only negative is colored (red); zero/positive plain;
//	           equal-zero is dim. Used by money displays where positive is
//	           the resting state.
//	signPnL  — negative red, positive green, zero dim. Used by P&L,
//	           DayChange, Greeks delta — anywhere both directions matter.
func (e *Env) colorBySign(v float64, s string, mode signMode) string {
	switch mode {
	case signNeg:
		if v < 0 {
			return e.red(s)
		}
		if v == 0 {
			return e.dim(s)
		}
		return s
	case signPnL:
		if v > 0 {
			return e.green(s)
		}
		if v < 0 {
			return e.red(s)
		}
		return e.dim(s)
	}
	return s
}

type signMode int

const (
	signNeg signMode = iota // negative red, positive plain, zero dim
	signPnL                 // positive green, negative red, zero dim
)

// formatMoneyNegCcy is the currency-aware variant of formatMoneyNeg.
// Empty currency falls through to "$" so existing test golden output
// stays valid until callers thread their currency through.
func (e *Env) formatMoneyNegCcy(v float64, ccy string) string {
	return e.colorBySign(v, formatMoneyCcy(v, ccy), signNeg)
}

// formatMoneyNegCcyRight is formatMoneyNegCcy with right-alignment
// padding to w visible cells, preserving the color wrap. Padding is
// applied before the ANSI wrap so column alignment holds regardless of
// color state. Used by the account renderer so a column of mixed
// magnitudes ("€ 5,968.85" vs "€ 992,841.68") reads as a single column.
func (e *Env) formatMoneyNegCcyRight(v float64, ccy string, w int) string {
	s := formatMoneyCcy(v, ccy)
	if pad := w - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signNeg)
}

// formatPnL renders a P&L amount with optional column padding, colored
// green/red by sign when env.Color is on. Width=0 disables padding (use
// at the last column). Padding is applied BEFORE the ANSI wrap so escape
// codes don't perturb column alignment — the visible width matches the
// requested width regardless of color state.
func (e *Env) formatPnL(v float64, width int) string {
	s := formatMoney(v)
	if width > 0 {
		if pad := width - len(s); pad > 0 {
			s += strings.Repeat(" ", pad)
		}
	}
	return e.colorBySign(v, s, signPnL)
}

// formatPnLRight is formatPnL but right-aligns the value within the
// given visible width by prepending spaces. Used by the Portfolio
// aggregate where numeric columns line up on the right edge — a
// thousands-grouped money string varies in width and would otherwise
// leave the trailing unit text at random positions.
func (e *Env) formatPnLRight(v float64, width int) string {
	s := formatMoney(v)
	if pad := width - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signPnL)
}

// formatPnLPtrRight wraps formatPnLRight for *float64 P&L pointers, where
// nil means "no data" and renders as an em-dash to match every other
// nil-able numeric column. The wire contract is "nil = unavailable,
// never zero-substituted" — em-dash on nil keeps the column honest.
func (e *Env) formatPnLPtrRight(v *float64, width int) string {
	if v == nil {
		return padDash(width)
	}
	return e.formatPnLRight(*v, width)
}

// formatPnLCcyRight is formatPnLRight with a currency prefix attached
// (using the same prefix table as formatMoneyCcy). For account-level
// P&L lines where the base currency belongs next to every figure for
// non-USD accounts. Width counts visible cells of the full prefix+value.
func (e *Env) formatPnLCcyRight(v float64, ccy string, width int) string {
	s := formatMoneyCcyForPnL(v, ccy)
	if pad := width - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signPnL)
}

// formatPnLCcyPtrRight is formatPnLCcyRight with nil handling.
func (e *Env) formatPnLCcyPtrRight(v *float64, ccy string, width int) string {
	if v == nil {
		return padDash(width)
	}
	return e.formatPnLCcyRight(*v, ccy, width)
}

// formatMoneyCcyForPnL is formatMoneyCcy without the zero-as-em-dash
// branch — for sign-coloured P&L lines a value of exactly zero is a
// real result ("flat day") and must render as a number, not a dash.
// The em-dash for "no data" is reserved for nil pointers handled by
// the *Ptr* wrappers.
func formatMoneyCcyForPnL(v float64, ccy string) string {
	prefix := moneyPrefix(ccy)
	neg := v < 0
	abs := v
	if neg {
		abs = -v
	}
	s := fmt.Sprintf("%.2f", abs)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	out := prefix + groupThousands(intPart) + frac
	if neg {
		return "-" + out
	}
	return out
}

// formatSignedGrouped renders v with a leading sign and the integer
// part separated by commas, at the given decimal precision. Use for
// non-money numeric values (effective delta, gamma, vega) where the
// magnitude can run into 4-5 digits — bare `%+.Nf` is harder to read
// at a glance than `+30,572.9`.
func formatSignedGrouped(v float64, decimals int) string {
	neg := v < 0
	abs := v
	if neg {
		abs = -v
	}
	s := fmt.Sprintf("%.*f", decimals, abs)
	dot := strings.IndexByte(s, '.')
	var intPart, frac string
	if dot >= 0 {
		intPart, frac = s[:dot], s[dot:]
	} else {
		intPart, frac = s, ""
	}
	sign := "+"
	if neg {
		sign = "-"
	}
	return sign + groupThousands(intPart) + frac
}

// visibleLen returns the visible (terminal-cell) length of s, ignoring
// ANSI CSI escape sequences (\x1b[...m). Box-drawing and currency-
// symbol runes are counted as one cell each, matching how terminals
// render them. Use this in lieu of len() when computing widths for
// strings that may have been color-wrapped.
func visibleLen(s string) int {
	n := 0
	in := false
	for _, r := range s {
		if in {
			if r == 'm' {
				in = false
			}
			continue
		}
		if r == '\x1b' {
			in = true
			continue
		}
		n++
	}
	return n
}

// padRightVisible pads s on the right with spaces until its visible
// length is w, preserving any embedded ANSI escapes. No-op when s is
// already at or beyond width.
func padRightVisible(s string, w int) string {
	if d := w - visibleLen(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// padLeftVisible is padRightVisible's right-aligning counterpart.
func padLeftVisible(s string, w int) string {
	if d := w - visibleLen(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// padDash returns a right-aligned em-dash placeholder of visible width w.
// Em-dash is one terminal column despite being three UTF-8 bytes, so we
// can't rely on Printf's %Ns width verb (it counts bytes). Used for empty
// numeric cells so columns stay aligned whether or not data is present.
func padDash(w int) string {
	if w <= 1 {
		return "—"
	}
	return strings.Repeat(" ", w-1) + "—"
}

// orDash renders v as a right-aligned float of visible width w, or a
// right-aligned em-dash placeholder of the same visible width when v is
// nil. Width is the visible column width — em-dash counts as 1 column,
// not its 3 UTF-8 bytes.
func orDash(p *float64, w int) string {
	if p == nil {
		return padDash(w)
	}
	return fmt.Sprintf("%*.2f", w, *p)
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
