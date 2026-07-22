package ibkr

import "testing"

func TestOrderBrokerErrorStatusUsesTypedCodeAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
		want string
	}{
		{name: "duplicate order id", code: 103, want: "Rejected"},
		{name: "minimum tick", code: 110, want: "Rejected"},
		{name: "broker reject", code: 201, want: "Rejected"},
		{name: "broker cancel", code: 202, want: "Cancelled"},
		{name: "generic request validation is not order-terminal", code: 321, want: ""},
		{name: "unknown order message", code: 399, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := orderBrokerErrorStatus(tt.code); got != tt.want {
				t.Fatalf("orderBrokerErrorStatus(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestOrderErrorLifecycleKeepsUnknownRejectTextNonterminal(t *testing.T) {
	t.Parallel()
	connector := readyBrokerEvidenceTestConnector(t)
	connector.openOrders["42"] = new(trackedOrder)
	var got OrderLifecycleEvent
	connector.RegisterOrderLifecycleHandler(func(ev OrderLifecycleEvent) { got = ev })

	connector.notifyOrderErrorLifecycle(42, 399, "untrusted text says reject", "")

	if got.Type != OrderLifecycleEventError || got.ErrorCode != 399 || got.Status != "" {
		t.Fatalf("lifecycle event = %+v, want typed code 399 with no terminal status", got)
	}
	if got.Message == "" {
		t.Fatal("raw broker message must remain available as audit text")
	}
}

func TestSystemNotice321RequestIDCollisionDoesNotRejectOrder(t *testing.T) {
	t.Parallel()
	connector := readyBrokerEvidenceTestConnector(t)
	connector.orderMu.Lock()
	connector.brokerOrderIndex["77"] = "ord-77"
	connector.orderMu.Unlock()
	request := connector.createHistoricalRequest(77, "AAPL")
	var got OrderLifecycleEvent
	connector.RegisterOrderLifecycleHandler(func(ev OrderLifecycleEvent) { got = ev })

	connector.processSystemNotice(reqAliasEntry{symbol: "AAPL", secType: "STK"}, &systemNotification{
		tickerID: 77,
		code:     321,
		message:  "request validation rejected",
	})

	if got.ErrorCode != 321 || got.Status != "" {
		t.Fatalf("colliding lifecycle event = %+v, want nonterminal typed code 321", got)
	}
	select {
	case result := <-request.result:
		t.Fatalf("order-ID collision must not consume the request result: %+v", result)
	default:
	}
}
