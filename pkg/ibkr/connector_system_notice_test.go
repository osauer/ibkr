package ibkr

import "testing"

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
