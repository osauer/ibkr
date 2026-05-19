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

// TestRenderStatus_BackgroundLine pins the v0.27.4 behaviour: the
// `Background:` line appears iff `result.BackgroundTasks` is non-empty,
// and the names render comma-separated. Empty list omits the line
// entirely so an idle daemon's status display stays compact.
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
			name:  "single task",
			tasks: []rpc.BackgroundTaskStatus{{Name: "breadth-spx"}},
			want:  "Background:     breadth-spx",
		},
		{
			name: "multiple tasks render comma-separated",
			tasks: []rpc.BackgroundTaskStatus{
				{Name: "breadth-spx"},
				{Name: "gamma-zero"},
			},
			want: "Background:     breadth-spx, gamma-zero",
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
