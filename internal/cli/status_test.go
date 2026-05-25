package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestRenderStatus_BackgroundLine pins the rendering contract: the
// `Background` line appears iff `result.BackgroundTasks` is non-empty;
// wire tokens are mapped to short verb phrases (so the row reads as
// English); phrases are comma-separated when multiple tasks run; an
// unknown token falls through verbatim. Empty list omits the line.
func TestRenderStatus_BackgroundLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tasks   []rpc.BackgroundTaskStatus
		want    string // substring that MUST appear
		notWant string // substring that MUST NOT appear
	}{
		{
			name:    "idle daemon omits line",
			tasks:   nil,
			notWant: "Background:",
		},
		{
			name:  "single task renders as verb phrase",
			tasks: []rpc.BackgroundTaskStatus{{Name: "breadth-spx"}},
			want:  "Background     refreshing rolling SPX breadth",
		},
		{
			name: "multiple tasks render comma-separated",
			tasks: []rpc.BackgroundTaskStatus{
				{Name: "breadth-spx"},
				{Name: "gamma-zero"},
			},
			want: "Background     refreshing rolling SPX breadth, computing dealer zero-gamma",
		},
		{
			name:  "unknown token falls through verbatim",
			tasks: []rpc.BackgroundTaskStatus{{Name: "future-task"}},
			want:  "Background     future-task",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
			res := &rpc.HealthResult{
				DaemonVersion:   "test",
				Connected:       true,
				ServerVersion:   200,
				BackgroundTasks: tc.tasks,
			}
			renderStatusText(env, res)
			got := stdout.String()
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("status missing expected substring %q:\n%s", tc.want, got)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("status contained unexpected substring %q:\n%s", tc.notWant, got)
			}
		})
	}
}

func TestIsHandshakeInFlight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		want bool
	}{
		{"connected", rpc.HealthResult{Connected: true}, false},
		{"degraded with error", rpc.HealthResult{LastError: "boom"}, false},
		{"connecting (no error yet)", rpc.HealthResult{}, true},
		{"connected wins over stale error", rpc.HealthResult{Connected: true, LastError: "stale"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHandshakeInFlight(tc.in); got != tc.want {
				t.Fatalf("isHandshakeInFlight(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// waitForHandshake must return immediately when the fetcher reports a
// connected gateway on the first poll — no extra polls, no busy-wait.
func TestWaitForHandshakeReturnsOnConnected(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		calls.Add(1)
		return rpc.HealthResult{Connected: true}, nil
	}
	var w bytes.Buffer
	res := waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{}, 5*time.Second, 1*time.Millisecond)
	if !res.Connected {
		t.Fatalf("expected Connected, got %+v", res)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 fetch, got %d", got)
	}
}

// Once the daemon reports a LastError (handshake failed), the wait must
// stop and surface that result — don't keep polling against a known-bad
// gateway.
func TestWaitForHandshakeReturnsOnError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		calls.Add(1)
		return rpc.HealthResult{LastError: "dial timeout"}, nil
	}
	var w bytes.Buffer
	res := waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{}, 5*time.Second, 1*time.Millisecond)
	if res.LastError != "dial timeout" {
		t.Fatalf("expected LastError preserved, got %+v", res)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 fetch, got %d", got)
	}
}

// When the gateway is wedged (every poll returns "still connecting"),
// the wait must hit its budget and return the last snapshot. Verifies
// the bound holds even with a fast poll interval — no infinite loop.
func TestWaitForHandshakeRespectsBudget(t *testing.T) {
	t.Parallel()
	// Fetcher mirrors real daemon behavior: static fields (DaemonVersion,
	// Profile, …) come back populated even while Connected is still false.
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		return rpc.HealthResult{DaemonVersion: "v1"}, nil
	}
	var w bytes.Buffer
	start := time.Now()
	res := waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{DaemonVersion: "v1"}, 80*time.Millisecond, 10*time.Millisecond)
	elapsed := time.Since(start)

	if res.Connected || res.LastError != "" {
		t.Fatalf("expected still-connecting result, got %+v", res)
	}
	if res.DaemonVersion != "v1" {
		t.Fatalf("expected DaemonVersion preserved, got %+v", res)
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("wait returned too early: %s (budget 80ms)", elapsed)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("wait overshot budget: %s (budget 80ms)", elapsed)
	}
}

// ctx cancellation (Ctrl+C) must short-circuit the wait — return the
// last good snapshot immediately rather than spinning out the budget.
func TestWaitForHandshakeRespectsContextCancel(t *testing.T) {
	t.Parallel()
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		return rpc.HealthResult{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the loop runs

	var w bytes.Buffer
	start := time.Now()
	res := waitForHandshake(ctx, &w, fetch, rpc.HealthResult{DaemonVersion: "init"}, 5*time.Second, 100*time.Millisecond)
	elapsed := time.Since(start)

	if res.DaemonVersion != "init" {
		t.Fatalf("expected initial snapshot returned, got %+v", res)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel ignored; took %s", elapsed)
	}
}

// A transient RPC error during polling must not panic the CLI — return
// the last good snapshot and stop polling. (Daemon could be SIGTERMing
// mid-status, etc.) The "last good" snapshot is whichever fetch most
// recently succeeded — we don't fall back to the original initial.
func TestWaitForHandshakeReturnsOnFetchError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		n := calls.Add(1)
		if n > 1 {
			return rpc.HealthResult{}, errors.New("conn closed")
		}
		return rpc.HealthResult{DaemonVersion: "fresh"}, nil
	}
	var w bytes.Buffer
	res := waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{DaemonVersion: "init"}, 5*time.Second, 1*time.Millisecond)

	if res.DaemonVersion != "fresh" {
		t.Fatalf("expected most-recent successful snapshot returned, got %+v", res)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("expected at least 2 fetch attempts (one ok, one erroring); got %d", got)
	}
}

// The progress UI ("waiting for IB Gateway handshake (up to N)" + dots)
// must land on stderr so it doesn't pollute structured stdout from
// neighboring shell commands.
func TestWaitForHandshakeWritesProgressToWriter(t *testing.T) {
	t.Parallel()
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		return rpc.HealthResult{Connected: true}, nil
	}
	var w bytes.Buffer
	_ = waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{}, 5*time.Second, 1*time.Millisecond)
	out := w.String()
	if !strings.Contains(out, "waiting for IB Gateway handshake") {
		t.Fatalf("progress message missing from output: %q", out)
	}
	if !strings.Contains(out, ".") {
		t.Fatalf("expected at least one progress dot in output: %q", out)
	}
}

func TestRenderStatus_FlightDeckShape(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.HealthResult{
		DaemonVersion: "v1.0.0",
		UptimeSeconds: 1842,
		Account:       "DU0000000",
		GatewayHost:   "127.0.0.1",
		GatewayPort:   4001,
		PortOrigin:    "discovered",
		ClientID:      17,
		Connected:     true,
		ServerVersion: 178,
		Members: rpc.MembersHealth{
			Source:       "cache",
			AsOf:         time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC),
			Count:        503,
			RefreshState: "healthy",
		},
	}
	renderStatusText(env, res)
	got := stdout.String()
	for _, want := range []string{
		"IBKR Gateway  READY",
		"Session        DU0000000 via 127.0.0.1:4001 (tls=false, discovered), client 17",
		"Market data    Live",
		"Daemon         v1.0.0, up 30m42s",
		"TWS            API server 178",
		"SPX members    cache:2026-05-22, 503 names",
		"Next concern   None",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
}

func TestNextConcernPriority(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		want string
	}{
		{
			name: "gateway error wins",
			in:   rpc.HealthResult{LastError: "dial timeout", DataType: rpc.MarketDataDelayed},
			want: "Gateway offline: dial timeout",
		},
		{
			name: "handshake pending",
			in:   rpc.HealthResult{},
			want: "Gateway handshake still in progress",
		},
		{
			name: "market data before members",
			in: rpc.HealthResult{
				Connected: true,
				DataType:  rpc.MarketDataFrozen,
				Members:   rpc.MembersHealth{Source: "cache", RefreshState: "parse_failed"},
			},
			want: "Market data is Frozen",
		},
		{
			name: "members refresh",
			in: rpc.HealthResult{
				Connected: true,
				Members:   rpc.MembersHealth{Source: "cache", RefreshState: "parse_failed"},
			},
			want: "SPX members refresh parse_failed",
		},
		{
			name: "background work",
			in: rpc.HealthResult{
				Connected:       true,
				BackgroundTasks: []rpc.BackgroundTaskStatus{{Name: "gamma-zero"}},
			},
			want: "Background work: computing dealer zero-gamma",
		},
		{
			name: "none",
			in:   rpc.HealthResult{Connected: true},
			want: "None",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextConcern(tc.in)
			if got.Text != tc.want {
				t.Fatalf("nextConcern(%+v) = %q, want %q", tc.in, got.Text, tc.want)
			}
		})
	}
}

func TestStatusVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		want string
	}{
		{
			name: "ready",
			in:   rpc.HealthResult{Connected: true},
			want: "READY",
		},
		{
			name: "background is still ready",
			in: rpc.HealthResult{
				Connected:       true,
				BackgroundTasks: []rpc.BackgroundTaskStatus{{Name: "gamma-zero"}},
			},
			want: "READY",
		},
		{
			name: "market data warning",
			in:   rpc.HealthResult{Connected: true, DataType: rpc.MarketDataDelayed},
			want: "ATTENTION",
		},
		{
			name: "starting",
			in:   rpc.HealthResult{},
			want: "STARTING",
		},
		{
			name: "offline",
			in:   rpc.HealthResult{LastError: "dial timeout"},
			want: "OFFLINE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusVerdict(tc.in)
			if got.Text != tc.want {
				t.Fatalf("statusVerdict(%+v) = %q, want %q", tc.in, got.Text, tc.want)
			}
		})
	}
}

// TestFormatMembersValue pins the four rendering variants of the
// S&P500 members row: healthy (no refresh: tail), pinned (env/config),
// silent rot (parse_failed / network_failed). Zero-value source omits
// the line entirely so a daemon that hasn't populated MembersHealth
// yet doesn't show a misleading "S&P500 members: :" row.
func TestFormatMembersValue(t *testing.T) {
	t.Parallel()
	d := time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		health rpc.MembersHealth
		want   string
		empty  bool
	}{
		{
			name:   "healthy cache",
			health: rpc.MembersHealth{Source: "cache", AsOf: d, Count: 503, RefreshState: "healthy"},
			want:   "cache:2026-05-22, 503 names",
		},
		{
			name:   "healthy embedded",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: "healthy"},
			want:   "embedded:2026-05-22, 503 names",
		},
		{
			name:   "empty refresh state (no refresher attached) treated as healthy",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: ""},
			want:   "embedded:2026-05-22, 503 names",
		},
		{
			name:   "parse failure surfaces",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: "parse_failed"},
			want:   "embedded:2026-05-22, 503 names, refresh parse_failed",
		},
		{
			name:   "network failure surfaces",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: "network_failed"},
			want:   "embedded:2026-05-22, 503 names, refresh network_failed",
		},
		{
			name:   "disabled config",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: "disabled (config)"},
			want:   "embedded:2026-05-22, 503 names, refresh disabled (config)",
		},
		{
			name:   "disabled env on cache file",
			health: rpc.MembersHealth{Source: "cache", AsOf: d, Count: 503, RefreshState: "disabled (env)"},
			want:   "cache:2026-05-22, 503 names, refresh disabled (env)",
		},
		{
			name:   "empty source omits row",
			health: rpc.MembersHealth{},
			empty:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatMembersValue(tc.health)
			if tc.empty {
				if got != "" {
					t.Errorf("want empty, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("missing substring %q:\n%s", tc.want, got)
			}
		})
	}
}
