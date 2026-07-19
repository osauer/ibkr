package live

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestPollOnceCachesSnapshotAndPublishesEvents(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		status:    &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		calendar:  &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:   &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions: &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: "STK"}}},
		quotes: map[string]rpc.Quote{
			"SPY": {Symbol: "SPY", Price: new(500.0), ChangePct: new(0.4), DataType: rpc.MarketDataLive},
			"QQQ": {Symbol: "QQQ", Price: new(420.0), ChangePct: new(0.5), DataType: rpc.MarketDataLive},
			"IWM": {Symbol: "IWM", Price: new(210.0), ChangePct: new(0.2), DataType: rpc.MarketDataLive},
			"VIX": {Symbol: "VIX", Price: new(18.0), ChangePct: new(-2.0), DataType: rpc.MarketDataLive},
			"HYG": {Symbol: "HYG", Price: new(78.0), ChangePct: new(0.1), DataType: rpc.MarketDataLive},
			"TLT": {Symbol: "TLT", Price: new(92.0), ChangePct: new(-0.1), DataType: rpc.MarketDataLive},
		},
		regime:       &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}, Composite: rpc.RegimeComposite{Verdict: "Stress signal present", ClusterRedCount: 1, ClusterRankedCount: 6}},
		canary:       &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}, Severity: risk.SeverityWatch, Action: "watch"},
		brief:        &rpc.BriefResult{BriefFingerprint: "brief-1"},
		marketEvents: &rpc.MarketEventsResult{Kind: rpc.MarketEventsKind, SchemaVersion: rpc.MarketEventsSchemaVersion, Fingerprint: rpc.Fingerprint{Key: "market-events-1"}},
		trading:      &rpc.TradingStatus{CanPreview: true},
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
	if snap.MarketEvents == nil || snap.MarketEvents.Fingerprint.Key != "market-events-1" {
		t.Fatalf("market events missing from snapshot: %#v", snap.MarketEvents)
	}
	if snap.Regime == nil || snap.Regime.Fingerprint.Key != "regime-1" {
		t.Fatalf("regime missing from snapshot: %#v", snap.Regime)
	}
	if snap.Trading == nil || !snap.Trading.CanPreview {
		t.Fatalf("trading missing from snapshot: %#v", snap.Trading)
	}
	if snap.Settings == nil || !snap.Settings.Features.PurgeRestore.Enabled.Value {
		t.Fatalf("settings missing from snapshot: %#v", snap.Settings)
	}
	if got := snap.Settings.MarketData.Quality.Status; got != "unknown" {
		t.Fatalf("settings market-data quality status = %q, want daemon-owned unknown", got)
	}

	seen := map[string]bool{}
	for range 16 {
		select {
		case ev := <-ch:
			seen[ev.Type] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for live events; seen=%v", seen)
		}
	}
	for _, want := range []string{"status", "market_calendar", "account", "positions", "market_events", "market_quotes", "trading", "auto_trade", "proposals", "opportunities", "settings", "regime", "canary", "rules", "brief", "snapshot"} {
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

func TestBriefPollsOnCanaryCadenceWithoutAcknowledging(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	client := &fakeClient{brief: &rpc.BriefResult{BriefFingerprint: "brief-1"}}
	svc := New(client, 5*time.Second, time.Minute)
	svc.now = func() time.Time { return now }

	svc.PollOnce(context.Background())
	briefCalls, ackCalls := client.BriefCounts()
	if briefCalls != 1 || ackCalls != 0 {
		t.Fatalf("after first poll: brief=%d ack=%d, want 1/0", briefCalls, ackCalls)
	}

	now = now.Add(30 * time.Second)
	svc.PollOnce(context.Background())
	briefCalls, ackCalls = client.BriefCounts()
	if briefCalls != 1 || ackCalls != 0 {
		t.Fatalf("before cadence: brief=%d ack=%d, want 1/0", briefCalls, ackCalls)
	}

	now = now.Add(31 * time.Second)
	svc.PollOnce(context.Background())
	briefCalls, ackCalls = client.BriefCounts()
	if briefCalls != 2 || ackCalls != 0 {
		t.Fatalf("after cadence: brief=%d ack=%d, want 2/0", briefCalls, ackCalls)
	}
}

func TestBriefPollErrorKeepsLastGoodSnapshotAndSetsSourceMeta(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	client := &fakeClient{
		status: &rpc.HealthResult{Connected: true},
		brief:  &rpc.BriefResult{BriefFingerprint: "brief-last-good"},
	}
	svc := New(client, 5*time.Second, time.Minute)
	svc.now = func() time.Time { return now }
	first := svc.PollOnce(context.Background())
	if first.Brief == nil || first.Brief.BriefFingerprint != "brief-last-good" {
		t.Fatalf("first brief=%#v", first.Brief)
	}

	client.briefErr = errors.New("brief composition unavailable")
	now = now.Add(time.Minute)
	got := svc.PollOnce(context.Background())
	if got.Brief == nil || got.Brief.BriefFingerprint != "brief-last-good" {
		t.Fatalf("last good brief was not retained: %#v", got.Brief)
	}
	if got.Status == nil || !got.Status.Connected {
		t.Fatalf("rest of snapshot was not retained: %#v", got.Status)
	}
	if got.Sources["brief"].Error != "brief composition unavailable" {
		t.Fatalf("brief source meta=%#v", got.Sources["brief"])
	}
	found := false
	for _, sourceErr := range got.Errors {
		if sourceErr.Source == "brief" && sourceErr.Message == "brief composition unavailable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("brief source error missing: %#v", got.Errors)
	}
}

func TestSnapshotBriefCloneIsIndependent(t *testing.T) {
	t.Parallel()
	brief := &rpc.BriefResult{
		Market: rpc.BriefMarketSection{
			Breadth: rpc.BriefBreadthRow{PctAbove50DMA: new(51.0)},
			Gamma:   rpc.BriefGammaRow{Spot: new(6000.0)},
		},
		Calendar: rpc.BriefCalendarSection{MarketEvents: []rpc.BriefMarketEventRow{{Symbols: []string{"AAA"}}}},
		Portfolio: rpc.BriefPortfolioSection{
			Account:       rpc.BriefAccountRow{EquityBase: new(100.0)},
			Movers:        rpc.BriefMoversRow{Rows: []rpc.BriefMover{{Symbol: "AAA"}}},
			PremiumAtRisk: rpc.BriefMoneyCoverageRow{AmountBase: new(10.0)},
			WorkingOrders: rpc.BriefCountRow{Count: new(1)},
		},
		RiskLimits: rpc.BriefRiskSection{
			Capital:     rpc.BriefCapitalRow{ConsumedPct: new(20.0)},
			Latch:       rpc.BriefLatchRow{AgeDays: new(2)},
			Overrides:   rpc.BriefOverridesRow{Rows: []rpc.BriefOverride{{Control: "limit"}}},
			PolicyDrift: rpc.BriefPolicyDriftRow{Rows: []rpc.PolicyPinStatus{{Policy: "rules"}}},
		},
		Process: rpc.BriefProcessSection{
			Reconcile:  rpc.BriefReconcileRow{DaysRemaining: new(3)},
			OneTap:     rpc.BriefOneTapRow{Blockers: []string{"blocked"}},
			RulesDelta: rpc.BriefRulesDeltaRow{Transitions: []rpc.BriefRuleTransition{{RuleID: "r1"}}, Added: []string{"r2"}, Removed: []string{"r3"}},
			Artefacts:  rpc.BriefArtefactsRow{Rows: []rpc.BriefArtefact{{Kind: "morning"}}},
		},
	}
	svc := New(&fakeClient{}, time.Minute, time.Minute)
	svc.snapshot = Snapshot{Brief: brief}
	got := svc.Snapshot()

	*got.Brief.Market.Breadth.PctAbove50DMA = 0
	*got.Brief.Market.Gamma.Spot = 0
	got.Brief.Calendar.MarketEvents[0].Symbols[0] = "MUTATED"
	*got.Brief.Portfolio.Account.EquityBase = 0
	got.Brief.Portfolio.Movers.Rows[0].Symbol = "MUTATED"
	*got.Brief.Portfolio.PremiumAtRisk.AmountBase = 0
	*got.Brief.Portfolio.WorkingOrders.Count = 0
	*got.Brief.RiskLimits.Capital.ConsumedPct = 0
	*got.Brief.RiskLimits.Latch.AgeDays = 0
	got.Brief.RiskLimits.Overrides.Rows[0].Control = "MUTATED"
	got.Brief.RiskLimits.PolicyDrift.Rows[0].Policy = "MUTATED"
	*got.Brief.Process.Reconcile.DaysRemaining = 0
	got.Brief.Process.OneTap.Blockers[0] = "MUTATED"
	got.Brief.Process.RulesDelta.Transitions[0].RuleID = "MUTATED"
	got.Brief.Process.RulesDelta.Added[0] = "MUTATED"
	got.Brief.Process.RulesDelta.Removed[0] = "MUTATED"
	got.Brief.Process.Artefacts.Rows[0].Kind = "MUTATED"

	current := svc.Snapshot().Brief
	if *current.Market.Breadth.PctAbove50DMA != 51 || *current.Market.Gamma.Spot != 6000 ||
		current.Calendar.MarketEvents[0].Symbols[0] != "AAA" || *current.Portfolio.Account.EquityBase != 100 ||
		current.Portfolio.Movers.Rows[0].Symbol != "AAA" || *current.Portfolio.PremiumAtRisk.AmountBase != 10 ||
		*current.Portfolio.WorkingOrders.Count != 1 || *current.RiskLimits.Capital.ConsumedPct != 20 ||
		*current.RiskLimits.Latch.AgeDays != 2 || current.RiskLimits.Overrides.Rows[0].Control != "limit" ||
		current.RiskLimits.PolicyDrift.Rows[0].Policy != "rules" || *current.Process.Reconcile.DaysRemaining != 3 ||
		current.Process.OneTap.Blockers[0] != "blocked" || current.Process.RulesDelta.Transitions[0].RuleID != "r1" ||
		current.Process.RulesDelta.Added[0] != "r2" || current.Process.RulesDelta.Removed[0] != "r3" ||
		current.Process.Artefacts.Rows[0].Kind != "morning" {
		t.Fatalf("mutating returned brief changed service snapshot: %#v", current)
	}
}

func TestSnapshotDeepCopiesNudgeContextAndConsumedPercentage(t *testing.T) {
	t.Parallel()
	consumed := 31.25
	svc := New(&fakeClient{}, time.Minute, time.Minute)
	svc.snapshot = Snapshot{Nudges: &rpc.NudgesSnapshotResult{Context: &rpc.NudgeSnapshotContext{
		Shadow: &rpc.NudgeShadowSummary{Count: 7},
		Drawdown: &rpc.NudgeDrawdownSummary{
			Tier: rpc.NudgeDrawdownTierBlock, ConsumedPct: &consumed,
		},
	}}}

	got := svc.Snapshot()
	got.Nudges.Context.Shadow.Count = 99
	got.Nudges.Context.Drawdown.Tier = "HOSTILE"
	*got.Nudges.Context.Drawdown.ConsumedPct = 0
	current := svc.Snapshot()
	if current.Nudges.Context.Shadow.Count != 7 || current.Nudges.Context.Drawdown.Tier != rpc.NudgeDrawdownTierBlock || current.Nudges.Context.Drawdown.ConsumedPct == nil || *current.Nudges.Context.Drawdown.ConsumedPct != 31.25 {
		t.Fatalf("mutating returned nudge context changed cached authority state: %+v", current.Nudges.Context)
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
		trading:     &rpc.TradingStatus{CanPreview: true},
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
		trading:    &rpc.TradingStatus{CanPreview: true},
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
		trading: &rpc.TradingStatus{CanPreview: true},
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

func TestMarketQuoteContractsSkipFreshStreamedBaselines(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	existing := &MarketQuotes{
		AsOf:   now.Add(-time.Second),
		Quotes: map[string]rpc.Quote{},
	}
	for _, item := range marketQuoteContracts {
		label := normalizeQuoteLabel(item.label)
		existing.Quotes[label] = rpc.Quote{
			Symbol:       label,
			QuotePriceAt: now.Add(-time.Second),
			AsOf:         now.Add(-time.Second),
		}
	}
	stock := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1}
	positions := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{{Underlying: "AAPL", Stock: &stock}},
	}

	got := marketQuoteContractsFor(positions, existing, now, 15*time.Second)
	if len(got) != 1 || got[0].label != "AAPL" {
		t.Fatalf("contracts=%#v, want only held AAPL after fresh baselines", got)
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

func TestNudgesPollSeparatesTransportFromDaemonSourceHealth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	healthAt := now.Add(-time.Minute)
	client := &fakeClient{nudges: &rpc.NudgesSnapshotResult{
		AsOf:                  now,
		ConfirmedFlowCoverage: &rpc.NudgeConfirmedFlowCoverage{CoverageFrom: healthAt},
		SourceHealth: rpc.NudgeSourceHealth{
			Policy:         rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
			Reconciliation: rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusUnavailable, Reason: rpc.NudgeHealthReasonSourceUnavailable, AsOf: healthAt},
			Capital:        rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
			Pins:           rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
			Cadence:        rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
			ConfirmedFlow:  rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
		},
	}}
	service := New(client, time.Hour, time.Hour)
	service.now = func() time.Time { return now }
	service.PollNudgesOnce(t.Context())
	snap := service.Snapshot()
	if snap.Nudges == nil || snap.Nudges.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed {
		t.Fatalf("nudges=%+v, want daemon-suppressed snapshot", snap.Nudges)
	}
	source := snap.Sources["nudges"]
	if source.State != SourceStateCurrent || source.Reason != SourceReasonNone || !source.LastSuccessAt.Equal(now) {
		t.Fatalf("poll source=%+v, want current separate from daemon suppression", source)
	}

	client.nudgesErr = errors.New("socket path /private/sentinel token")
	now = now.Add(time.Minute)
	service.PollNudgesOnce(t.Context())
	snap = service.Snapshot()
	if snap.Nudges == nil {
		t.Fatal("last successful nudge snapshot was discarded")
	}
	source = snap.Sources["nudges"]
	if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || source.Error != "" || !source.LastSuccessAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("poll source=%+v, want allowlisted unavailable state without raw error", source)
	}
}

func TestNudgesPollUsesDedicatedOneMinuteCadence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	client := &fakeClient{nudges: readyNudges(now)}
	service := New(client, time.Second, time.Hour)
	service.now = func() time.Time { return now }
	service.PollOnce(t.Context())
	now = now.Add(59 * time.Second)
	service.PollOnce(t.Context())
	now = now.Add(time.Second)
	service.PollOnce(t.Context())
	client.quoteMu.Lock()
	calls := client.nudgeCalls
	client.quoteMu.Unlock()
	if calls != 2 {
		t.Fatalf("nudges calls=%d, want initial and one-minute poll", calls)
	}
}

func TestNudgesSourceStartsNotObservedAndAgesToStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	client := &fakeClient{nudges: readyNudges(now)}
	service := New(client, time.Hour, time.Hour)
	service.now = func() time.Time { return now }
	startup := service.Snapshot().Sources["nudges"]
	if startup.State != SourceStateNotObserved || startup.Reason != SourceReasonNotObserved || !startup.LastSuccessAt.IsZero() {
		t.Fatalf("startup source=%+v", startup)
	}
	service.PollNudgesOnce(t.Context())
	now = now.Add(nudgesPollEvery + time.Nanosecond)
	aged := service.Snapshot().Sources["nudges"]
	if aged.State != SourceStateStale || aged.Reason != SourceReasonPollStale || aged.LastSuccessAt.IsZero() {
		t.Fatalf("aged source=%+v", aged)
	}
}

func TestNudgesExplicitOutageRemainsUnavailableAfterFreshnessBudget(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	client := &fakeClient{nudges: readyNudges(now)}
	service := New(client, time.Hour, time.Hour)
	service.now = func() time.Time { return now }
	service.PollNudgesOnce(t.Context())
	client.nudgesErr = errors.New("transport sentinel")
	now = now.Add(2 * nudgesPollEvery)
	service.PollNudgesOnce(t.Context())
	source := service.Snapshot().Sources["nudges"]
	if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable {
		t.Fatalf("explicit outage was aged to stale: %+v", source)
	}
}

func readyNudges(at time.Time) *rpc.NudgesSnapshotResult {
	ok := rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: at}
	return &rpc.NudgesSnapshotResult{AsOf: at, SourceHealth: rpc.NudgeSourceHealth{
		Policy: ok, Reconciliation: ok, Capital: ok, Pins: ok, Cadence: ok, ConfirmedFlow: ok,
	}, ConfirmedFlowCoverage: &rpc.NudgeConfirmedFlowCoverage{CoverageFrom: at}}
}

type fakeClient struct {
	status       *rpc.HealthResult
	calendar     *rpc.MarketCalendarResult
	account      *rpc.AccountResult
	positions    *rpc.PositionsResult
	quotes       map[string]rpc.Quote
	quoteErrs    map[string]error
	regime       *rpc.RegimeMonitorResult
	canary       *rpc.CanaryResult
	brief        *rpc.BriefResult
	briefErr     error
	nudges       *rpc.NudgesSnapshotResult
	nudgesErr    error
	marketEvents *rpc.MarketEventsResult
	trading      *rpc.TradingStatus

	canaryBlock <-chan struct{}
	quoteBlock  <-chan struct{}
	quoteMu     sync.Mutex
	quoteCalls  []rpc.ContractParams
	briefCalls  int
	briefAcks   int
	nudgeCalls  int
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

func (c *fakeClient) MarketEvents(context.Context, rpc.MarketEventsParams) (*rpc.MarketEventsResult, error) {
	return c.marketEvents, nil
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

func (c *fakeClient) Rules(context.Context) (*rpc.RulesResult, error) {
	return &rpc.RulesResult{Enabled: true, Status: "ok"}, nil
}

func (c *fakeClient) Brief(context.Context) (*rpc.BriefResult, error) {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	c.briefCalls++
	return c.brief, c.briefErr
}

func (c *fakeClient) NudgesSnapshot(context.Context) (*rpc.NudgesSnapshotResult, error) {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	c.nudgeCalls++
	return c.nudges, c.nudgesErr
}

func (c *fakeClient) NudgesCutoverReview(context.Context, rpc.NudgesCutoverReviewParams) (*rpc.NudgesCutoverReviewResult, error) {
	return nil, nil
}

func (c *fakeClient) BriefAck(context.Context, rpc.BriefAckParams) (*rpc.BriefAckResult, error) {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	c.briefAcks++
	return &rpc.BriefAckResult{OK: true}, nil
}

func (c *fakeClient) BriefCounts() (brief, ack int) {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	return c.briefCalls, c.briefAcks
}

func (c *fakeClient) ReconcileSignoff(context.Context, rpc.CapitalEventParams) (*rpc.RiskPolicyWriteResult, error) {
	return &rpc.RiskPolicyWriteResult{OK: true}, nil
}

func (c *fakeClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	return c.trading, nil
}

func (c *fakeClient) AutoTradeStatus(context.Context) (*rpc.AutoTradeStatus, error) {
	return &rpc.AutoTradeStatus{ProposalsEnabled: true, FastPathEnabled: true}, nil
}

func (c *fakeClient) OpportunitiesStatus(context.Context) (*rpc.OpportunityStatus, error) {
	return &rpc.OpportunityStatus{Enabled: true}, nil
}

func (c *fakeClient) OpportunitiesSnapshot(context.Context, rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error) {
	return &rpc.OpportunitySnapshot{Kind: rpc.OpportunitySnapshotKind, SchemaVersion: rpc.OpportunitySnapshotSchemaVersion, Revision: "empty", Opportunities: []rpc.Opportunity{}}, nil
}

func (c *fakeClient) OpportunitiesRefresh(context.Context, rpc.OpportunityRefreshParams) (*rpc.OpportunitySnapshot, error) {
	return c.OpportunitiesSnapshot(context.Background(), rpc.OpportunitySnapshotParams{})
}

func (c *fakeClient) OpportunitiesPreviewExercise(context.Context, rpc.OpportunityExercisePreviewParams) (*rpc.OpportunityExercisePreviewResult, error) {
	return nil, nil
}

func (c *fakeClient) OpportunitiesSubmitExercise(context.Context, rpc.OpportunityExerciseSubmitParams) (*rpc.OpportunityExerciseSubmitResult, error) {
	return nil, nil
}

func (c *fakeClient) OpportunitiesIgnore(context.Context, rpc.OpportunityIgnoreParams) (*rpc.OpportunityIgnoreResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsSnapshot(context.Context, rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error) {
	return &rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, Revision: "empty", Proposals: []rpc.TradeProposal{}}, nil
}

func (c *fakeClient) TradeProposalsRefresh(context.Context, rpc.TradeProposalRefreshParams) (*rpc.TradeProposalSnapshot, error) {
	return c.TradeProposalsSnapshot(context.Background(), rpc.TradeProposalSnapshotParams{})
}

func (c *fakeClient) TradeProposalsPreview(context.Context, rpc.TradeProposalPreviewParams) (*rpc.TradeProposalPreviewResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsSubmit(context.Context, rpc.TradeProposalSubmitParams) (*rpc.TradeProposalSubmitResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsReducePreview(context.Context, rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsReduceSubmit(context.Context, rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsReducePortfolioPreview(context.Context, rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsReducePortfolioSubmit(context.Context, rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsIgnore(context.Context, rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error) {
	return nil, nil
}

func (c *fakeClient) Settings(context.Context) (*rpc.PlatformSettings, error) {
	return &rpc.PlatformSettings{
		Kind: "ibkr.platform_settings",
		Features: rpc.PlatformFeatureSettings{
			PurgeRestore: rpc.PurgeRestoreSettings{
				Enabled: rpc.SettingsBool{Value: true, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime},
			},
		},
		MarketData: rpc.PlatformMarketDataSetting{
			Quality: rpc.PlatformMarketDataQuality{Status: "unknown", Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceObserved},
		},
	}, nil
}

func (c *fakeClient) UpdateSettings(context.Context, json.RawMessage) (*rpc.PlatformSettings, error) {
	return c.Settings(context.Background())
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
