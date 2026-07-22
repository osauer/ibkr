package ibkr

import "testing"

func TestParseOrderLifecycleEventOpenOrder(t *testing.T) {
	t.Parallel()
	fields := []string{
		"5", "38", "1001", "265598", "AAPL", "STK", "", "0", "", "1", "SMART", "USD", "", "AAPL",
		"BUY", "10", "LMT", "190.5", "0", "DAY", "Submitted", "987654",
	}
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok {
		t.Fatal("ParseOrderLifecycleEvent(openOrder) ok=false")
	}
	if ev.Type != OrderLifecycleEventOpenOrder || ev.OrderID != 1001 || ev.PermID != 987654 {
		t.Fatalf("open order IDs parsed wrong: %+v", ev)
	}
	if ev.ConID != 265598 || ev.ClientIDPresent {
		t.Fatalf("legacy open-order identity provenance parsed wrong: %+v", ev)
	}
	if ev.Symbol != "AAPL" || ev.SecType != "STK" || ev.Action != "BUY" || ev.TotalQuantity != 10 || ev.LimitPrice != 190.5 || ev.TIF != "DAY" {
		t.Fatalf("open order fields parsed wrong: %+v", ev)
	}
	if ev.Raw[2] != "1001" {
		t.Fatalf("raw fields not retained: %+v", ev.Raw)
	}
}

func TestParseOrderLifecycleEventOrderStatus(t *testing.T) {
	t.Parallel()
	fields := []string{"3", "1", "1001", "Submitted", "2", "8", "190.25", "987654", "190.5", "0", "0", "", "0"}
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok {
		t.Fatal("ParseOrderLifecycleEvent(orderStatus) ok=false")
	}
	if ev.Type != OrderLifecycleEventStatus || ev.OrderID != 1001 || ev.Status != "Submitted" {
		t.Fatalf("status identity parsed wrong: %+v", ev)
	}
	if ev.Filled != 2 || ev.Remaining != 8 || ev.AvgFillPrice != 190.25 || ev.LastFillPrice != 190.5 || ev.PermID != 987654 || ev.ClientID != 0 || !ev.ClientIDPresent {
		t.Fatalf("status quantities parsed wrong: %+v", ev)
	}
}

func TestParseOrderLifecycleEventExecDetails(t *testing.T) {
	t.Parallel()
	fields := []string{
		"11", "11", "-1", "1001", "265598", "AAPL", "STK", "", "0", "", "1", "NASDAQ", "USD", "AAPL",
		"0000e1.65f2.01.01", "20260528 09:31:02", "DU1234567", "NASDAQ", "BOT", "2", "190.3",
		"987654", "31", "0", "2", "190.3", "ibkr-20260528-093100",
	}
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok {
		t.Fatal("ParseOrderLifecycleEvent(execDetails) ok=false")
	}
	if ev.Type != OrderLifecycleEventExecDetails || ev.OrderID != 1001 || ev.ExecID == "" {
		t.Fatalf("exec identity parsed wrong: %+v", ev)
	}
	if ev.Symbol != "AAPL" || ev.ExecutionSide != "BOT" || ev.Shares != 2 || ev.Price != 190.3 || ev.PermID != 987654 || ev.ClientID != 31 || ev.OrderRef != "ibkr-20260528-093100" {
		t.Fatalf("exec details parsed wrong: %+v", ev)
	}
}

func TestParseOrderLifecycleEventIgnoresMalformed(t *testing.T) {
	t.Parallel()
	if _, ok := ParseOrderLifecycleEvent([]string{"5", "38"}); ok {
		t.Fatal("malformed openOrder should be ignored")
	}
	if _, ok := ParseOrderLifecycleEvent([]string{"999", "x"}); ok {
		t.Fatal("unknown message should be ignored")
	}
}

func TestConnectorOrderLifecycleHandlerReceivesBrokerStatus(t *testing.T) {
	t.Parallel()
	c := readyBrokerEvidenceTestConnector(t)
	received := make(chan OrderLifecycleEvent, 1)
	c.RegisterOrderLifecycleHandler(func(ev OrderLifecycleEvent) {
		received <- ev
	})

	c.notifyOrderLifecycle([]string{"3", "1", "1001", "Submitted", "0", "1", "0", "987654", "0", "31", "0", "", "0"})

	select {
	case ev := <-received:
		if ev.Type != OrderLifecycleEventStatus || ev.OrderID != 1001 || ev.Status != "Submitted" || ev.PermID != 987654 {
			t.Fatalf("event = %+v, want Submitted status for order 1001", ev)
		}
	default:
		t.Fatal("lifecycle handler was not called")
	}
}

func TestConnectorOrderLifecycleHandlerIgnoresWhatIfOpenOrder(t *testing.T) {
	t.Parallel()
	c := readyBrokerEvidenceTestConnector(t)
	received := make(chan OrderLifecycleEvent, 1)
	c.RegisterOrderLifecycleHandler(func(ev OrderLifecycleEvent) {
		received <- ev
	})

	c.notifyOrderLifecycle([]string{
		"5", "38", "1001", "265598", "AAPL", "STK", "", "0", "", "1", "SMART", "USD", "", "AAPL",
		"BUY", "10", "LMT", "190.5", "0", "DAY", "1", "Submitted", "987654",
	})

	select {
	case ev := <-received:
		t.Fatalf("what-if event should be ignored, got %+v", ev)
	default:
	}
}
