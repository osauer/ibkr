package ibkr

import (
	"testing"
	"time"
)

func TestProcessSystemNoticeSkipsDerivativeInactive(t *testing.T) {
	c := &Connector{}
	alias := reqAliasEntry{
		symbol:      "AMD",
		secType:     "OPT",
		localSymbol: "AMD 20251121C250",
	}
	note := &systemNotification{
		code:    200,
		message: "No security definition has been found for the request",
	}

	c.processSystemNotice(alias, note)

	if c.IsSymbolInactive("AMD") {
		t.Fatalf("expected AMD to remain active despite option system notice")
	}
}

func TestProcessSystemNoticeMarksStockInactive(t *testing.T) {
	c := &Connector{}
	alias := reqAliasEntry{
		symbol:  "HGENQ",
		secType: "STK",
	}
	note := &systemNotification{
		code:    200,
		message: "No security definition has been found for the request",
	}

	c.processSystemNotice(alias, note)

	if !c.IsSymbolInactive("HGENQ") {
		t.Fatalf("expected HGENQ to be marked inactive")
	}
}

func TestProcessSystemNoticeMarksRoutedStockInactiveByRoute(t *testing.T) {
	c := &Connector{}
	alias := reqAliasEntry{
		symbol:   "MBG",
		secType:  "STK",
		exchange: "SMART",
		currency: "USD",
	}
	note := &systemNotification{
		code:    200,
		message: "No security definition has been found for the request",
	}

	c.processSystemNotice(alias, note)

	if c.IsSymbolInactive("MBG") {
		t.Fatalf("bare MBG should remain usable for an explicit non-US route")
	}
	key := MarketDataKeyForContract(Contract{Symbol: "MBG", SecType: "STK", Exchange: "SMART", Currency: "USD"})
	if !c.IsSymbolInactive(key) {
		t.Fatalf("expected failed route %q to be marked inactive", key)
	}
}

func TestProcessSystemNoticeTracksDataFarmProblemAndRecovery(t *testing.T) {
	c := &Connector{}
	brokenAt := time.Date(2026, time.June, 1, 8, 20, 0, 0, time.UTC)
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		code:      2103,
		message:   "Market data farm connection is broken:usopt",
		timestamp: brokenAt,
	})

	farms := c.DataFarmStatuses()
	if len(farms) != 1 {
		t.Fatalf("farms len=%d, want 1: %+v", len(farms), farms)
	}
	if got := farms[0]; got.Name != "usopt" || got.Type != "market" || got.Status != "disconnected" || got.Code != 2103 {
		t.Fatalf("farm = %+v, want usopt market disconnected 2103", got)
	}

	okAt := brokenAt.Add(time.Minute)
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		code:      2104,
		message:   "Market data farm connection is OK:usopt",
		timestamp: okAt,
	})
	farms = c.DataFarmStatuses()
	if len(farms) != 1 {
		t.Fatalf("farms len after OK=%d, want 1: %+v", len(farms), farms)
	}
	if got := farms[0]; got.Name != "usopt" || got.Status != "ok" || got.AsOf != okAt {
		t.Fatalf("farm after OK = %+v, want usopt ok at %s", got, okAt)
	}
}

func TestProcessSystemNoticeTracksConnectivityBreak(t *testing.T) {
	c := &Connector{}
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		code:    2110,
		message: "Connectivity between TWS and server is broken",
	})

	farms := c.DataFarmStatuses()
	if len(farms) != 1 {
		t.Fatalf("farms len=%d, want 1: %+v", len(farms), farms)
	}
	if got := farms[0]; got.Name != "tws-server" || got.Type != "connectivity" || got.Status != "broken" {
		t.Fatalf("farm = %+v, want tws-server connectivity broken", got)
	}
}
