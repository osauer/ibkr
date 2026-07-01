package ibkr

import (
	"testing"
	"time"
)

// These tests pin the daily-P&L self-heal (MaybeResubscribeStaleDailyPnL).
// reqPnL is subscribed once at connect and pushes on change; the gateway can
// silently stop feeding it (observed 2026-07-01: a pre-market subscribe never
// resumed after the open, so daily P&L froze at the pre-market value while
// account-updates — hence NLV — kept flowing). SubscribeAccountPnL is
// idempotent and cannot revive a dead-but-"subscribed" stream, so a stale frame
// during market hours must force a cancel+re-request.

func newPnLResubRig(t *testing.T) *Connector {
	t.Helper()
	c, _, _ := newAcctResubscribeRig(t)
	return c
}

func seedStaleAccountPnL(c *Connector, acct string, asOf time.Time) {
	v := 621.28
	c.SeedAccountDailyPnLForTest(acct, AccountDailyPnL{DailyPnL: &v, AsOf: asOf})
}

func TestMaybeResubscribeStaleDailyPnL_HealsStaleStreamDuringMarketHours(t *testing.T) {
	c := newPnLResubRig(t)
	now := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	c.pnlResubNow = func() time.Time { return now }

	seedStaleAccountPnL(c, "DU1234567", now.Add(-10*time.Minute))
	c.SeedPositionDailyPnLForTest(4391, PositionDailyPnL{})

	if !c.MaybeResubscribeStaleDailyPnL(true) {
		t.Fatal("stale frame during market hours: want rebuild (true), got false")
	}

	c.pnl.mu.RLock()
	gotReq := c.pnl.accountReqID
	gotAcct := c.pnl.accountAcct
	posReq, posOK := c.pnl.positionReqIDs[4391]
	c.pnl.mu.RUnlock()
	if gotReq <= 0 {
		t.Fatalf("account reqID after heal = %d, want a fresh positive id (was seeded -1)", gotReq)
	}
	if gotAcct != "DU1234567" {
		t.Fatalf("account after heal = %q, want DU1234567 preserved", gotAcct)
	}
	if !posOK || posReq <= 0 {
		t.Fatalf("position 4391 reqID after heal = %d (ok=%v), want a fresh positive id", posReq, posOK)
	}
}

func TestMaybeResubscribeStaleDailyPnL_NoopWhenMarketClosed(t *testing.T) {
	c := newPnLResubRig(t)
	now := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	c.pnlResubNow = func() time.Time { return now }
	seedStaleAccountPnL(c, "DU1234567", now.Add(-10*time.Minute))

	if c.MaybeResubscribeStaleDailyPnL(false) {
		t.Fatal("market closed: want no rebuild (false)")
	}
	c.pnl.mu.RLock()
	gotReq := c.pnl.accountReqID
	c.pnl.mu.RUnlock()
	if gotReq != -1 {
		t.Fatalf("account reqID = %d, want unchanged seed (-1) when market closed", gotReq)
	}
}

func TestMaybeResubscribeStaleDailyPnL_NoopWhenFresh(t *testing.T) {
	c := newPnLResubRig(t)
	now := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	c.pnlResubNow = func() time.Time { return now }
	seedStaleAccountPnL(c, "DU1234567", now.Add(-5*time.Second)) // well inside the window

	if c.MaybeResubscribeStaleDailyPnL(true) {
		t.Fatal("fresh frame: want no rebuild (false)")
	}
}

func TestMaybeResubscribeStaleDailyPnL_NoopWhenNeverSubscribed(t *testing.T) {
	c := newPnLResubRig(t)
	now := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	c.pnlResubNow = func() time.Time { return now }
	// No seed: accountReqID == 0, AsOf zero — the startup path owns this.
	if c.MaybeResubscribeStaleDailyPnL(true) {
		t.Fatal("never subscribed: want no rebuild (false)")
	}
}

func TestMaybeResubscribeStaleDailyPnL_Throttle(t *testing.T) {
	c := newPnLResubRig(t)
	now := time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC)
	c.pnlResubNow = func() time.Time { return now }
	seedStaleAccountPnL(c, "DU1234567", now.Add(-10*time.Minute))

	if !c.MaybeResubscribeStaleDailyPnL(true) {
		t.Fatal("first stale call: want rebuild")
	}

	// Gateway still silent: re-seed a stale frame. A poll inside the throttle
	// window must stay quiet (an inverted comparison would self-perpetuate).
	seedStaleAccountPnL(c, "DU1234567", now.Add(-10*time.Minute))
	now = now.Add(dailyPnLStaleResubscribe - time.Second)
	if c.MaybeResubscribeStaleDailyPnL(true) {
		t.Fatal("inside throttle window: want no rebuild")
	}

	// At the throttle boundary (>=), re-arm.
	seedStaleAccountPnL(c, "DU1234567", now.Add(-10*time.Minute))
	now = now.Add(time.Second)
	if !c.MaybeResubscribeStaleDailyPnL(true) {
		t.Fatal("at throttle boundary: want rebuild")
	}
}
