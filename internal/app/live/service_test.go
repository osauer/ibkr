package live

import (
	"context"
	"sync"
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
		quotes: map[string]rpc.Quote{
			"SPY": {Symbol: "SPY", Price: new(500.0), ChangePct: new(0.4), DataType: rpc.MarketDataLive},
			"QQQ": {Symbol: "QQQ", Price: new(420.0), ChangePct: new(0.5), DataType: rpc.MarketDataLive},
			"IWM": {Symbol: "IWM", Price: new(210.0), ChangePct: new(0.2), DataType: rpc.MarketDataLive},
			"VIX": {Symbol: "VIX", Price: new(18.0), ChangePct: new(-2.0), DataType: rpc.MarketDataLive},
			"HYG": {Symbol: "HYG", Price: new(78.0), ChangePct: new(0.1), DataType: rpc.MarketDataLive},
			"TLT": {Symbol: "TLT", Price: new(92.0), ChangePct: new(-0.1), DataType: rpc.MarketDataLive},
		},
		regime:  &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}, Composite: rpc.RegimeComposite{Verdict: "Stress signal present", ClusterRedCount: 1, ClusterRankedCount: 6}},
		canary:  &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}, Severity: risk.SeverityWatch, Action: "watch"},
		trading: &rpc.TradingStatus{CanPreview: true, PreviewRequired: true},
	}
	svc := New(client, time.Minute, time.Minute)
	ch, release := svc.Subscribe()
	defer release()
	canarySeen := make(chan rpc.CanaryResult, 1)
	svc.OnCanary = func(_ context.Context, canary rpc.CanaryResult) {
		canarySeen <- canary
	}

	snap := svc.PollOnce(context.Background())
	if snap.Version != 2 {
		t.Fatalf("snapshot version=%d, want 2", snap.Version)
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
	if snap.Quotes == nil || len(snap.Quotes.Quotes) != 6 || snap.Quotes.Quotes["QQQ"].Symbol != "QQQ" || snap.Quotes.Quotes["TLT"].Symbol != "TLT" {
		t.Fatalf("market quotes missing from snapshot: %#v", snap.Quotes)
	}
	if snap.Regime == nil || snap.Regime.Fingerprint.Key != "regime-1" {
		t.Fatalf("regime missing from snapshot: %#v", snap.Regime)
	}
	if snap.Trading == nil || !snap.Trading.CanPreview {
		t.Fatalf("trading missing from snapshot: %#v", snap.Trading)
	}

	seen := map[string]bool{}
	for range 9 {
		select {
		case ev := <-ch:
			seen[ev.Type] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for live events; seen=%v", seen)
		}
	}
	for _, want := range []string{"status", "market_calendar", "account", "positions", "market_quotes", "trading", "regime", "canary", "snapshot"} {
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

func TestStartPublishesStatusBeforeFullPollCompletes(t *testing.T) {
	t.Parallel()
	canaryBlock := make(chan struct{})
	client := &fakeClient{
		status:      &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		calendar:    &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:     &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions:   &rpc.PositionsResult{Stocks: []rpc.PositionView{}},
		quotes:      map[string]rpc.Quote{"SPY": {Symbol: "SPY", Price: new(500.0)}},
		regime:      &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}},
		canary:      &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		trading:     &rpc.TradingStatus{CanPreview: true, PreviewRequired: true},
		canaryBlock: canaryBlock,
	}
	svc := New(client, time.Hour, time.Hour)
	ch, release := svc.Subscribe()
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Start(ctx)
		close(done)
	}()
	defer func() {
		close(canaryBlock)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("live service did not stop")
		}
	}()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("subscription closed before status event")
			}
			if ev.Type == "status" {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for startup status event")
		}
	}
}

func TestPollOncePublishesPositionsBeforeMarketQuotesComplete(t *testing.T) {
	t.Parallel()
	quoteBlock := make(chan struct{})
	client := &fakeClient{
		status:     &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		calendar:   &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:    &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions:  &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "SAP"}}},
		quotes:     map[string]rpc.Quote{"SPY": {Symbol: "SPY", Price: new(500.0)}},
		regime:     &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}},
		canary:     &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		trading:    &rpc.TradingStatus{CanPreview: true, PreviewRequired: true},
		quoteBlock: quoteBlock,
	}
	svc := New(client, time.Hour, time.Hour)
	ch, release := svc.Subscribe()
	defer release()

	done := make(chan struct{})
	go func() {
		svc.PollOnce(context.Background())
		close(done)
	}()
	defer func() {
		close(quoteBlock)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("PollOnce did not stop")
		}
	}()

	for {
		select {
		case ev := <-ch:
			if ev.Type != "snapshot" {
				continue
			}
			snap, ok := ev.Data.(Snapshot)
			if !ok {
				t.Fatalf("snapshot event data type %T, want Snapshot", ev.Data)
			}
			if snap.Positions != nil && len(snap.Positions.Stocks) == 1 {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for early positions snapshot")
		}
	}
}

func TestMarketQuoteStreamFrameKeepsChangeAnchor(t *testing.T) {
	t.Parallel()
	svc := New(&fakeClient{}, time.Minute, time.Minute)
	prev := 500.0
	svc.snapshot.Quotes = &MarketQuotes{
		Quotes: map[string]rpc.Quote{
			"SPY": {Symbol: "SPY", PrevClose: new(prev)},
		},
	}

	last := 505.0
	svc.applyMarketQuoteFrame("SPY", rpc.Frame{T: time.Date(2026, 6, 4, 15, 30, 0, 0, time.UTC), Last: new(last), DataType: rpc.MarketDataLive})
	got := svc.Snapshot().Quotes.Quotes["SPY"]
	if got.Price == nil || *got.Price != 505.0 {
		t.Fatalf("stream frame price=%v, want 505", got.Price)
	}
	if got.ChangePct == nil || *got.ChangePct != 1.0 {
		t.Fatalf("stream frame change_pct=%v, want 1.0", got.ChangePct)
	}
	if got.PriceSource != "last" || got.DataType != rpc.MarketDataLive {
		t.Fatalf("stream frame metadata source=%q data_type=%q", got.PriceSource, got.DataType)
	}
}

func TestMergeMarketQuotesPreservesLastGoodStreamQuote(t *testing.T) {
	t.Parallel()
	oldSPY := 500.0
	newQQQ := 420.0
	existing := &MarketQuotes{
		AsOf: time.Date(2026, 6, 4, 15, 30, 0, 0, time.UTC),
		Quotes: map[string]rpc.Quote{
			"SPY": {Symbol: "SPY", Price: &oldSPY},
		},
	}
	update := &MarketQuotes{
		AsOf: time.Date(2026, 6, 4, 15, 31, 0, 0, time.UTC),
		Quotes: map[string]rpc.Quote{
			"QQQ": {Symbol: "QQQ", Price: &newQQQ},
		},
		Errors: map[string]string{"SPY": "snapshot timeout"},
	}

	got := mergeMarketQuotes(existing, update)
	if got.Quotes["SPY"].Price == nil || *got.Quotes["SPY"].Price != oldSPY {
		t.Fatalf("SPY last-good quote lost: %#v", got.Quotes["SPY"])
	}
	if got.Quotes["QQQ"].Price == nil || *got.Quotes["QQQ"].Price != newQQQ {
		t.Fatalf("QQQ update missing: %#v", got.Quotes["QQQ"])
	}
	if got.Errors["SPY"] != "snapshot timeout" {
		t.Fatalf("SPY error=%q, want snapshot timeout", got.Errors["SPY"])
	}
}

func TestPollOnceIncludesHeldUnderlyingQuotes(t *testing.T) {
	t.Parallel()
	aaplPrice := 207.42
	stock := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1}
	client := &fakeClient{
		status:    &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		calendar:  &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:   &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions: &rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{{Underlying: "AAPL", Stock: &stock}}, Portfolio: &rpc.PositionsPortfolio{}},
		quotes: map[string]rpc.Quote{
			"AAPL": {Symbol: "AAPL", Price: &aaplPrice, DataType: rpc.MarketDataLive},
		},
		regime:  &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}},
		canary:  &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		trading: &rpc.TradingStatus{CanPreview: true, PreviewRequired: true},
	}
	svc := New(client, time.Minute, time.Minute)

	snap := svc.PollOnce(context.Background())
	got := snap.Quotes.Quotes["AAPL"]
	if got.Price == nil || *got.Price != aaplPrice {
		t.Fatalf("AAPL quote missing from market_quotes: %#v", snap.Quotes)
	}
	var routed rpc.ContractParams
	for _, call := range client.QuoteCalls() {
		if call.Symbol == "AAPL" {
			routed = call
			break
		}
	}
	if routed.Symbol != "AAPL" || routed.SecType != "STK" || routed.Currency != "USD" {
		t.Fatalf("AAPL quote routed as %#v, want STK/USD", routed)
	}
}

func TestMarketQuoteErrorIncludesDynamicSymbols(t *testing.T) {
	t.Parallel()
	got := marketQuoteError(map[string]string{
		"aapl": "snapshot timeout",
		"SPY":  "farm disconnected",
	})
	want := "SPY: farm disconnected | AAPL: snapshot timeout"
	if got != want {
		t.Fatalf("marketQuoteError()=%q, want %q", got, want)
	}
}

type fakeClient struct {
	status    *rpc.HealthResult
	calendar  *rpc.MarketCalendarResult
	account   *rpc.AccountResult
	positions *rpc.PositionsResult
	quotes    map[string]rpc.Quote
	quoteErrs map[string]error
	regime    *rpc.RegimeMonitorResult
	canary    *rpc.CanaryResult
	trading   *rpc.TradingStatus

	canaryBlock <-chan struct{}
	quoteBlock  <-chan struct{}
	quoteMu     sync.Mutex
	quoteCalls  []rpc.ContractParams
}

func (c *fakeClient) Status(context.Context) (*rpc.HealthResult, error) {
	return c.status, nil
}

func (c *fakeClient) MarketCalendar(context.Context) (*rpc.MarketCalendarResult, error) {
	return c.calendar, nil
}

func (c *fakeClient) MarketCalendarFor(context.Context, string) (*rpc.MarketCalendarResult, error) {
	return c.calendar, nil
}

func (c *fakeClient) Account(context.Context) (*rpc.AccountResult, error) {
	return c.account, nil
}

func (c *fakeClient) Positions(context.Context) (*rpc.PositionsResult, error) {
	return c.positions, nil
}

func (c *fakeClient) Quote(_ context.Context, contract rpc.ContractParams) (*rpc.Quote, error) {
	if c.quoteBlock != nil {
		<-c.quoteBlock
	}
	c.quoteMu.Lock()
	c.quoteCalls = append(c.quoteCalls, contract)
	c.quoteMu.Unlock()
	if err := c.quoteErrs[contract.Symbol]; err != nil {
		return nil, err
	}
	q, ok := c.quotes[contract.Symbol]
	if !ok {
		q = rpc.Quote{Symbol: contract.Symbol, Contract: contract, DataType: rpc.MarketDataLive}
	}
	return &q, nil
}

func (c *fakeClient) QuoteCalls() []rpc.ContractParams {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	return append([]rpc.ContractParams(nil), c.quoteCalls...)
}

func (c *fakeClient) StreamQuote(context.Context, rpc.ContractParams, func(rpc.Frame) error) error {
	return nil
}

func (c *fakeClient) Canary(context.Context) (*rpc.CanaryResult, error) {
	return c.canary, nil
}

func (c *fakeClient) CanaryWithRegime(context.Context) (*rpc.CanaryResult, *rpc.RegimeMonitorResult, error) {
	if c.canaryBlock != nil {
		<-c.canaryBlock
	}
	return c.canary, c.regime, nil
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

func (c *fakeClient) OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	return nil, nil
}

func (c *fakeClient) OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	return nil, nil
}

func (c *fakeClient) PurgeStatus(context.Context, rpc.PurgeStatusParams) (*rpc.PurgeStatusResult, error) {
	return nil, nil
}

func (c *fakeClient) PurgeExecute(context.Context, rpc.PurgeExecuteParams) (*rpc.PurgeExecuteResult, error) {
	return nil, nil
}

func (c *fakeClient) PurgeRestorePreview(context.Context, rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error) {
	return nil, nil
}

func (c *fakeClient) PurgeRestoreExecute(context.Context, rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error) {
	return nil, nil
}
