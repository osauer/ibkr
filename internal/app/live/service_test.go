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
		calendar:  &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:   &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions: &rpc.PositionsResult{Stocks: []rpc.PositionView{}},
		canary:    &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}, Severity: risk.SeverityWatch, Action: "watch"},
		trading:   &rpc.TradingStatus{CanPreview: true, PreviewRequired: true},
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
	if snap.Calendar == nil || snap.Calendar.Session.State != "regular" {
		t.Fatalf("calendar missing from snapshot: %#v", snap.Calendar)
	}
	if snap.Account == nil || snap.Account.BaseCurrency != "USD" {
		t.Fatalf("account missing from snapshot: %#v", snap.Account)
	}
	if snap.Canary == nil || snap.Canary.Fingerprint.Key != "fp-1" {
		t.Fatalf("canary missing from snapshot: %#v", snap.Canary)
	}
	if snap.Trading == nil || !snap.Trading.CanPreview {
		t.Fatalf("trading missing from snapshot: %#v", snap.Trading)
	}

	seen := map[string]bool{}
	for range 7 {
		select {
		case ev := <-ch:
			seen[ev.Type] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for live events; seen=%v", seen)
		}
	}
	for _, want := range []string{"status", "market_calendar", "account", "positions", "trading", "canary", "snapshot"} {
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
	calendar  *rpc.MarketCalendarResult
	account   *rpc.AccountResult
	positions *rpc.PositionsResult
	canary    *rpc.CanaryResult
	trading   *rpc.TradingStatus
}

func (c *fakeClient) Status(context.Context) (*rpc.HealthResult, error) {
	return c.status, nil
}

func (c *fakeClient) MarketCalendar(context.Context) (*rpc.MarketCalendarResult, error) {
	return c.calendar, nil
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

func (c *fakeClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	return c.trading, nil
}

func (c *fakeClient) RiskPlan(context.Context, string, *rpc.CanaryResult) (*rpc.RiskPlanResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderPreview(context.Context, rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	return nil, nil
}

func (c *fakeClient) OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	return nil, nil
}
