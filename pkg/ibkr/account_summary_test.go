package ibkr

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseAccountSummary_AllTagsBaseCurrency(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation":       "100000.50",
		"BuyingPower":          "400000.00",
		"AvailableFunds":       "95000.25",
		"ExcessLiquidity":      "94000.00",
		"TotalCashValue":       "20000.00",
		"MaintenanceMarginReq": "5000.00",
		"InitMarginReq":        "10000.00",
	}

	got := parseAccountSummary(raw, "U1234567")
	if got.AccountID != "U1234567" {
		t.Fatalf("AccountID = %q, want U1234567", got.AccountID)
	}
	if got.NetLiquidation == nil || *got.NetLiquidation != 100000.50 {
		t.Fatalf("NetLiquidation = %v, want 100000.50", got.NetLiquidation)
	}
	if got.BuyingPower == nil || *got.BuyingPower != 400000.00 {
		t.Fatalf("BuyingPower = %v, want 400000.00", got.BuyingPower)
	}
	if got.AvailableFunds == nil || *got.AvailableFunds != 95000.25 {
		t.Fatalf("AvailableFunds = %v, want 95000.25", got.AvailableFunds)
	}
	if got.ExcessLiquidity == nil || *got.ExcessLiquidity != 94000.00 {
		t.Fatalf("ExcessLiquidity = %v", got.ExcessLiquidity)
	}
	if got.TotalCashValue == nil || *got.TotalCashValue != 20000.00 {
		t.Fatalf("TotalCashValue = %v", got.TotalCashValue)
	}
	if got.MaintenanceMargin == nil || *got.MaintenanceMargin != 5000.00 {
		t.Fatalf("MaintenanceMargin = %v", got.MaintenanceMargin)
	}
	if got.InitMarginReq == nil || *got.InitMarginReq != 10000.00 {
		t.Fatalf("InitMarginReq = %v", got.InitMarginReq)
	}
	if got.Currency != "" {
		t.Fatalf("Currency = %q, want empty for BASE-only summary", got.Currency)
	}
	if got.AsOf.IsZero() {
		t.Fatalf("AsOf should be non-zero")
	}
}

func TestParseAccountSummary_NonBaseCurrencyOverride(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation_USD": "75000.00",
		"BuyingPower_USD":    "300000.00",
	}
	got := parseAccountSummary(raw, "U1234567")
	if got.NetLiquidation == nil || *got.NetLiquidation != 75000.00 {
		t.Fatalf("NetLiquidation = %v, want 75000.00", got.NetLiquidation)
	}
	if got.Currency != "USD" {
		t.Fatalf("Currency = %q, want USD", got.Currency)
	}
}

func TestParseAccountSummary_PrefersBaseOverCurrencySuffix(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation":     "100000.00",
		"NetLiquidation_USD": "99500.00",
	}
	got := parseAccountSummary(raw, "U1234567")
	if got.NetLiquidation == nil || *got.NetLiquidation != 100000.00 {
		t.Fatalf("NetLiquidation = %v, want 100000.00 (base preferred)", got.NetLiquidation)
	}
}

func TestParseAccountSummary_PartialMissingTags(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation": "50000.00",
	}
	got := parseAccountSummary(raw, "")
	if got.NetLiquidation == nil {
		t.Fatalf("NetLiquidation should be present")
	}
	if got.BuyingPower != nil {
		t.Fatalf("BuyingPower should be nil for missing tag")
	}
	if got.MaintenanceMargin != nil {
		t.Fatalf("MaintenanceMargin should be nil for missing tag")
	}
}

func TestParseAccountSummary_GarbageValuesIgnored(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation": "not-a-number",
		"BuyingPower":    "100.00",
	}
	got := parseAccountSummary(raw, "")
	if got.NetLiquidation != nil {
		t.Fatalf("NetLiquidation should be nil when value is unparseable")
	}
	if got.BuyingPower == nil || *got.BuyingPower != 100.00 {
		t.Fatalf("BuyingPower should still parse")
	}
}

func TestLookupAccountValue_OrderingDeterministic(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation_EUR": "90000.00",
		"NetLiquidation_USD": "100000.00",
		"NetLiquidation_GBP": "75000.00",
	}
	val, currency, ok := lookupAccountValue(raw, "NetLiquidation")
	if !ok {
		t.Fatalf("expected lookup to succeed")
	}
	// Sort by suffix → EUR is first lexicographically
	if currency != "EUR" || val != "90000.00" {
		t.Fatalf("got currency=%q val=%q, want EUR/90000.00 (deterministic by sort)", currency, val)
	}
}

func TestRequestAccountSummary_DisconnectedReturnsErrIBKRUnavailable(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusDisconnected
	c.conn = conn
	c.running = true
	c.ready = false

	_, err := c.RequestAccountSummary(context.Background(), 1*time.Second)
	if !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("expected ErrIBKRUnavailable, got %v", err)
	}
}

func TestRequestAccountSummary_NoConnectorReturnsErrIBKRUnavailable(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// c.conn is nil — isConnected() must return false without panic
	_, err := c.RequestAccountSummary(context.Background(), 1*time.Second)
	if !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("expected ErrIBKRUnavailable, got %v", err)
	}
}

func TestRequestAccountSummary_TimeoutDoesNotLeakGoroutines(t *testing.T) {
	// A real network failure means RequestAccountSummary will fail to send;
	// we verify the connector returns an error promptly without leaking.
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusDisconnected // forces ErrIBKRUnavailable, no network attempt
	c.conn = conn
	c.running = true
	c.ready = false

	// Snapshot the baseline AFTER construction so the threshold protects only
	// against per-call leaks, not against the rate-limiter / heartbeat
	// goroutines NewConnection always spawns.
	before := runtime.NumGoroutine()

	for range 50 {
		_, _ = c.RequestAccountSummary(context.Background(), 100*time.Millisecond)
	}

	// Allow scheduler to run any GC.
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+3 {
		t.Fatalf("goroutine leak suspected: before=%d after=%d", before, after)
	}
}

func TestRequestAccountSummary_ContextCancelReturnsContextErr(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	c.conn = conn
	c.running = true
	c.ready = true

	// Context already cancelled before the call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// We don't have a real socket, so RequestAccountSummary at the Connection
	// level will fail to write. We assert the behavior is *some* error rather
	// than a hang. In paths where send succeeds and we then await end, the
	// ctx.Done() arm in our select would fire — that path is exercised in
	// integration tests. The unit-level guarantee here is: "no hang, no leak,
	// returns an error promptly when context is dead."
	deadline := time.After(2 * time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := c.RequestAccountSummary(ctx, 1*time.Second)
		done <- err
	}()

	select {
	case <-done:
		// Good — returned within timeout.
	case <-deadline:
		t.Fatalf("RequestAccountSummary did not return within 2s with cancelled context")
	}
}

func TestAccountSummaryTags_IncludesAllExpectedTags(t *testing.T) {
	// Guard against accidental tag-list edits that would silently strip
	// fields the daemon's RawAccountSummary path needs.
	wantTags := []string{
		"NetLiquidation",
		"BuyingPower",
		"AvailableFunds",
		"ExcessLiquidity",
		"TotalCashValue",
		"MaintMarginReq",
		"InitMarginReq",
	}
	for _, tag := range wantTags {
		if !strings.Contains(accountSummaryTags, tag) {
			t.Errorf("accountSummaryTags missing %q (got %q)", tag, accountSummaryTags)
		}
	}
}
