package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// handshakeWaitBudget bounds how long `ibkr status` waits for the daemon's
// IB Gateway handshake to land. Sub-second on a healthy gateway. The
// daemon's watchdog (issue #2) populates LastError after ~12s when the
// gateway TCPs but never completes the API handshake, so this budget
// only matters on the slow happy path (TLS fallback or a sluggish
// gateway). 25s comfortably covers both.
const handshakeWaitBudget = 25 * time.Second

// handshakePollInterval is the cadence at which `status` re-asks the
// daemon for its health snapshot during the wait.
const handshakePollInterval = 500 * time.Millisecond

func runStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.HealthResult
	if err := env.Conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
		return fail(env, "status: %v", err)
	}

	// On a freshly autospawned daemon the handshake hasn't landed by the
	// time this first call returns. Wait for a definite verdict (connected
	// or error) so the user gets a real answer instead of "retry in a few
	// seconds." Skipped under --json: scripts want a snapshot, not a wait.
	if !*jsonOut && isHandshakeInFlight(res) {
		fetch := func(ctx context.Context) (rpc.HealthResult, error) {
			var r rpc.HealthResult
			err := env.Conn.Call(ctx, rpc.MethodStatusHealth, nil, &r)
			return r, err
		}
		res = waitForHandshake(ctx, env.Stderr, fetch, res, handshakeWaitBudget, handshakePollInterval)
	}

	if *jsonOut {
		return printJSON(env, res)
	}
	renderStatusText(env, &res)
	if !res.Connected {
		return 1
	}
	return 0
}

// renderStatusText prints the health snapshot as the user-facing status
// screen. Split out from runStatus so the preview tool and future tests
// can exercise the rendering without a live socket.
func renderStatusText(env *Env, res *rpc.HealthResult) {
	out := env.Stdout

	// State is the headline signal: ok / degraded / starting. Color it so
	// the answer to "is the gateway up?" lands on the first line without
	// having to scan the Connected row. degraded → yellow (warning,
	// matches the data-type badges); starting → dim (transient, no
	// action needed); ok → plain.
	state := "ok"
	connecting := false
	if !res.Connected {
		if res.LastError != "" {
			state = env.yellow("degraded ⚠ gateway not connected")
		} else {
			state = env.dim("starting · gateway handshake in progress")
			connecting = true
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "ibkr daemon %s  ·  uptime %s  ·  %s\n",
		res.DaemonVersion, time.Duration(res.UptimeSeconds)*time.Second, state)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Account:        %s\n", nonEmpty(res.Account, "auto-detect"))
	fmt.Fprintf(out, "  Gateway:        %s:%d %s\n", res.GatewayHost, res.GatewayPort, formatGatewayBadge(*res))
	if len(res.Alternates) > 0 {
		fmt.Fprintf(out, "                  also up: %s\n", joinPorts(res.Alternates))
	}
	fmt.Fprintf(out, "  Client ID:      %d\n", res.ClientID)
	// Bold the Connected value: it's the single hero answer per screen —
	// everything else on the page is context for this one yes/no.
	fmt.Fprintf(out, "  Connected:      %s\n", env.bold(fmt.Sprintf("%v", res.Connected)))
	switch {
	case res.Connected:
		fmt.Fprintf(out, "  Server version: %d\n", res.ServerVersion)
		dt := nonEmpty(res.DataType, rpc.MarketDataLive)
		if !rpc.IsLiveDataType(res.DataType) {
			dt = env.yellow(dt + " ⚠")
		}
		fmt.Fprintf(out, "  Data type:      %s\n", dt)
	case connecting:
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  Handshake did not complete within %s. Check the daemon log for\n", handshakeWaitBudget)
		fmt.Fprintln(out, "  the underlying error, then verify in IB Gateway:")
		fmt.Fprintln(out, "    Configure → Settings → API → Settings → 'Enable ActiveX and Socket Clients'")
		fmt.Fprintln(out, "    Trusted IPs include 127.0.0.1 (or empty)")
		fmt.Fprintln(out, "    Login fully completed (not paused at 2FA)")
		fmt.Fprintln(out, env.dim("  Daemon log: ~/.local/state/ibkr/ibkr-daemon.log"))
	default:
		if res.LastError != "" {
			fmt.Fprintf(out, "  Reason:         %s\n", env.red(res.LastError))
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  Daemon log: ~/.local/state/ibkr/ibkr-daemon.log"))
	}
	// Surface daemon-internal long-running work the user can't see
	// from the CLI otherwise. Empty list → omit the line entirely
	// (idle is the common case). Stable wire tokens are mapped to
	// short noun phrases so the row reads as English when one or
	// more long-running computes (breadth-spx refresh, gamma-zero
	// compute) are in flight at the same time.
	if len(res.BackgroundTasks) > 0 {
		phrases := make([]string, 0, len(res.BackgroundTasks))
		for _, t := range res.BackgroundTasks {
			phrases = append(phrases, backgroundTaskPhrase(t.Name))
		}
		fmt.Fprintf(out, "  Background:     %s\n", strings.Join(phrases, ", "))
	}
	if line := formatMembersLine(res.Members); line != "" {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out)
}

// formatMembersLine renders the SPX-members row that lives under the
// breadth surface. Returns the empty string when the daemon hasn't
// populated the field (engine construction failed) — caller omits the
// line entirely in that case.
//
// Healthy line (refresh state implicit):
//
//	Members:        cache:2026-05-22  count:503
//
// Unhealthy / pinned variants:
//
//	Members:        embedded:2026-05-22  count:503  refresh:parse_failed
//	Members:        cache:2026-05-22     count:503  refresh:disabled (env)
//
// The bracketed `refresh:` segment is omitted in the healthy case —
// the source token + as_of already carry the answer to "is my data
// fresh?" Surfacing the refresh state only when it needs attention
// keeps the steady-state row uncluttered.
func formatMembersLine(m rpc.MembersHealth) string {
	if m.Source == "" {
		return ""
	}
	asOf := "—"
	if !m.AsOf.IsZero() {
		asOf = m.AsOf.Format("2006-01-02")
	}
	base := fmt.Sprintf("  Members:        %s:%s  count:%d", m.Source, asOf, m.Count)
	// Empty / "healthy" → omit the refresh: tail. Disabled and
	// failure states render explicitly so a user looking at
	// unexpected breadth values can spot the cause in one glance.
	if m.RefreshState == "" || m.RefreshState == "healthy" {
		return base
	}
	return base + "  refresh:" + m.RefreshState
}

// isHandshakeInFlight reports whether the daemon has reported neither a
// successful connection nor a connect error yet — i.e. the gateway
// handshake goroutine started but hasn't produced a verdict.
func isHandshakeInFlight(res rpc.HealthResult) bool {
	return !res.Connected && res.LastError == ""
}

// backgroundTaskPhrase renders a stable wire token (the one the
// daemon's backgroundTasks() emits) as a short verb phrase suitable
// for the status row. Unknown tokens fall through verbatim so a
// daemon shipping a new task name still appears in `ibkr status`
// even before the CLI has been updated.
func backgroundTaskPhrase(token string) string {
	switch token {
	case "breadth-spx":
		return "refreshing rolling SPX breadth"
	case "gamma-zero":
		return "computing dealer zero-gamma"
	case "regime-prewarm":
		return "warming regime cache"
	default:
		return token
	}
}

// healthFetcher is the closure waitForHandshake uses to re-poll the
// daemon. Indirected so tests can drive the wait deterministically
// without a live socket.
type healthFetcher func(ctx context.Context) (rpc.HealthResult, error)

// waitForHandshake polls fetch until it returns a verdict (Connected, or
// LastError set) or budget elapses.
func waitForHandshake(ctx context.Context, w io.Writer, fetch healthFetcher, initial rpc.HealthResult, budget, pollInterval time.Duration) rpc.HealthResult {
	fmt.Fprintf(w, "ibkr: waiting for IB Gateway handshake (up to %s)", budget)
	defer fmt.Fprintln(w)

	res := initial
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return res
		case <-time.After(pollInterval):
		}
		fmt.Fprint(w, ".")
		next, err := fetch(ctx)
		if err != nil {
			return res
		}
		res = next
		if !isHandshakeInFlight(res) {
			return res
		}
	}
	return res
}

// formatGatewayBadge renders the TLS state and the discovery origin
// compactly. Examples:
//
//	(tls=false, discovered)
//	(tls=true, pinned)
//	(tls=true, configured=false ⚠ fallback)
//
// The fallback marker only fires when configured ≠ negotiated, which can
// only happen when EnableTLSFallback is on (i.e. TLS auto). Pinned TLS
// (true or false) suppresses fallback entirely, so configured always
// equals negotiated.
func formatGatewayBadge(res rpc.HealthResult) string {
	tlsTok := fmt.Sprintf("tls=%v", res.GatewayTLS)
	if res.Connected && res.GatewayTLS != res.NegotiatedTLS {
		tlsTok = fmt.Sprintf("tls=%v, configured=%v ⚠ fallback", res.NegotiatedTLS, res.GatewayTLS)
	}
	origin := res.PortOrigin
	if origin == "" {
		// Pre-AUTO daemon or empty discovery info — render compactly.
		return fmt.Sprintf("(%s)", tlsTok)
	}
	return fmt.Sprintf("(%s, %s)", tlsTok, origin)
}

func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = fmt.Sprintf("%d", p)
	}
	return strings.Join(parts, ", ")
}
