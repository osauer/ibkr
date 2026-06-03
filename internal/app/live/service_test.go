package live

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

func TestPollOnceCachesSnapshotAndPublishesEvents(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		status:    &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		account:   &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions: &rpc.PositionsResult{Stocks: []rpc.PositionView{}},
		canary:    &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}, Severity: risk.SeverityWatch, Action: "watch"},
	}
	svc := New(client, time.Minute, time.Minute)
	ch, release := svc.Subscribe()
	defer release()
	canarySeen := make(chan rpc.CanaryResult, 1)
	svc.OnCanary = func(_ context.Context, canary rpc.CanaryResult) {
		canarySeen <- canary
	}

	snap := svc.PollOnce(context.Background())
	if snap.Version != 1 {
		t.Fatalf("snapshot version=%d, want 1", snap.Version)
	}
	if snap.Status == nil || !snap.Status.Connected {
		t.Fatalf("status missing from snapshot: %#v", snap.Status)
	}
	if snap.Account == nil || snap.Account.BaseCurrency != "USD" {
		t.Fatalf("account missing from snapshot: %#v", snap.Account)
	}
	if snap.Canary == nil || snap.Canary.Fingerprint.Key != "fp-1" {
		t.Fatalf("canary missing from snapshot: %#v", snap.Canary)
	}

	seen := map[string]bool{}
	for range 5 {
		select {
		case ev := <-ch:
			seen[ev.Type] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for live events; seen=%v", seen)
		}
	}
	for _, want := range []string{"status", "account", "positions", "canary", "snapshot"} {
		if !seen[want] {
			t.Fatalf("missing event %q; seen=%v", want, seen)
		}
	}
	select {
	case got := <-canarySeen:
		if got.Action != "watch" {
			t.Fatalf("OnCanary action=%q, want watch", got.Action)
		}
	case <-time.After(time.Second):
		t.Fatalf("OnCanary was not called")
	}
	diag := svc.Diagnostics()
	if diag.Subscribers != 1 {
		t.Fatalf("subscribers=%d, want 1", diag.Subscribers)
	}
	if diag.LastEventAt["snapshot"].IsZero() {
		t.Fatalf("snapshot event timestamp missing: %#v", diag.LastEventAt)
	}
}

type fakeClient struct {
	status    *rpc.HealthResult
	account   *rpc.AccountResult
	positions *rpc.PositionsResult
	canary    *rpc.CanaryResult
}

func (c *fakeClient) Status(context.Context) (*rpc.HealthResult, error) {
	return c.status, nil
}

func (c *fakeClient) Account(context.Context) (*rpc.AccountResult, error) {
	return c.account, nil
}

func (c *fakeClient) Positions(context.Context) (*rpc.PositionsResult, error) {
	return c.positions, nil
}

func (c *fakeClient) Canary(context.Context) (*rpc.CanaryResult, error) {
	return c.canary, nil
}
