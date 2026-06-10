package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestParseNasdaqRegSHOThresholdList(t *testing.T) {
	t.Parallel()
	raw := strings.NewReader(strings.Join([]string{
		"Symbol|Security Name|Market Category|Reg SHO Threshold Flag|Rule3210",
		"CRWV|CoreWeave Inc.|Q|Y|N",
		"MSFT|Microsoft Corp.|Q|N|N",
		"20260605180000",
		"",
	}, "\n"))
	got, err := parseNasdaqRegSHO(raw)
	if err != nil {
		t.Fatalf("parse Reg SHO: %v", err)
	}
	if _, ok := got.Symbols["CRWV"]; !ok {
		t.Fatalf("CRWV threshold row missing: %+v", got.Symbols)
	}
	if _, ok := got.Symbols["MSFT"]; ok {
		t.Fatalf("non-threshold row should not be flagged: %+v", got.Symbols)
	}
	if got.AsOf.IsZero() {
		t.Fatalf("as_of not parsed")
	}
}

func TestParseNasdaqRegSHOAllowsValidEmptyThresholdList(t *testing.T) {
	t.Parallel()
	raw := strings.NewReader(strings.Join([]string{
		"Symbol|Security Name|Market Category|Reg SHO Threshold Flag|Rule3210",
		"MSFT|Microsoft Corp.|Q|N|N",
		"20260605180000",
		"",
	}, "\n"))
	got, err := parseNasdaqRegSHO(raw)
	if err != nil {
		t.Fatalf("parse empty threshold list: %v", err)
	}
	if len(got.Symbols) != 0 {
		t.Fatalf("threshold symbols=%+v, want empty", got.Symbols)
	}
}

func TestParseNasdaqTradeHaltsClassifiesActiveAndRecent(t *testing.T) {
	t.Parallel()
	raw := strings.NewReader(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
<pubDate>Fri, 05 Jun 2026 14:30:00 GMT</pubDate>
<item>
	<IssueSymbol>CRWV</IssueSymbol>
	<IssueName>CoreWeave Inc.</IssueName>
	<Market>NASDAQ</Market>
	<ReasonCode>T7</ReasonCode>
	<HaltDate>06/05/2026</HaltDate>
	<HaltTime>10:15:00.000</HaltTime>
	<PauseThresholdPrice>123.45</PauseThresholdPrice>
</item>
<item>
	<IssueSymbol>MSFT</IssueSymbol>
	<IssueName>Microsoft Corp.</IssueName>
	<Market>NASDAQ</Market>
	<ReasonCode>T1</ReasonCode>
	<HaltDate>06/05/2026</HaltDate>
	<HaltTime>09:15:00.000</HaltTime>
	<ResumptionDate>06/05/2026</ResumptionDate>
	<ResumptionTradeTime>09:45:00.000</ResumptionTradeTime>
</item>
</channel></rss>`)
	entry, err := parseNasdaqTradeHalts(raw)
	if err != nil {
		t.Fatalf("parse halts: %v", err)
	}
	if len(entry.Records) != 2 {
		t.Fatalf("records=%d, want 2", len(entry.Records))
	}
	now := time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC)
	luld, ok := marketEventHaltFlag("CRWV", entry.Records[0], entry, now)
	if !ok || luld.ID != rpc.MarketEventLULDRecent || luld.Status != rpc.MarketEventStatusActive || luld.Severity != rpc.MarketEventSeverityBlock {
		t.Fatalf("active LULD flag = %+v ok=%v", luld, ok)
	}
	halt, ok := marketEventHaltFlag("MSFT", entry.Records[1], entry, now)
	if !ok || halt.ID != rpc.MarketEventHaltRegulatoryOrNews || halt.Status != rpc.MarketEventStatusRecent || halt.Severity != rpc.MarketEventSeverityWatch {
		t.Fatalf("recent halt flag = %+v ok=%v", halt, ok)
	}
}

func TestMarketEventBorrowInventoryFlagThresholds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	if _, ok := marketEventBorrowInventoryFlag("CRWV", ibkrlib.MarketData{ShortableShares: 500}, now); ok {
		t.Fatal("unobserved shortable shares should be unknown, not false-active")
	}
	flag, ok := marketEventBorrowInventoryFlag("CRWV", ibkrlib.MarketData{ShortableObserved: true, ShortableShares: 500}, now)
	if !ok || flag.Severity != rpc.MarketEventSeverityAct || flag.Label != "Borrow scarce" {
		t.Fatalf("scarce flag = %+v ok=%v", flag, ok)
	}
	flag, ok = marketEventBorrowInventoryFlag("CRWV", ibkrlib.MarketData{ShortableObserved: true, ShortableShares: 5_000}, now)
	if !ok || flag.Severity != rpc.MarketEventSeverityWatch || flag.Label != "Borrow tight" {
		t.Fatalf("tight flag = %+v ok=%v", flag, ok)
	}
	if _, ok := marketEventBorrowInventoryFlag("CRWV", ibkrlib.MarketData{ShortableObserved: true, ShortableShares: 50_000}, now); ok {
		t.Fatal("ample borrow inventory should not emit an inactive false flag")
	}
}

func TestParseIBKRBorrowFeesAndEmitExtremeFlag(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	raw := strings.NewReader(strings.Join([]string{
		"#BOF|2026.06.06|11:45:03",
		"#SYM|CUR|NAME|CON|ISIN|REBATERATE|FEERATE|AVAILABLE|",
		"CRWV|USD|COREWEAVE INC|123456789|US0000000001|-70.2500|75.2500|1500|",
		"MSFT|USD|MICROSOFT CORP|272093|US5949181045|4.7500|0.2500|8000000|",
		"",
	}, "\n"))
	entry, err := parseIBKRBorrowFees(raw)
	if err != nil {
		t.Fatalf("parse IBKR borrow fees: %v", err)
	}
	if entry.AsOf.IsZero() {
		t.Fatal("as_of not parsed")
	}
	flag, ok := marketEventBorrowFeeFlag("CRWV", entry.Symbols["CRWV"], entry, now)
	if !ok {
		t.Fatal("expected borrow_fee_extreme flag")
	}
	if flag.ID != rpc.MarketEventBorrowFeeExtreme || flag.Severity != rpc.MarketEventSeverityAct || flag.Role != rpc.MarketEventRoleProposalModifier {
		t.Fatalf("borrow fee flag classification = %+v", flag)
	}
	if flag.Unit != "pct_annualized" || flag.Value == nil || *flag.Value != 75.25 {
		t.Fatalf("borrow fee value/unit = value %v unit %q", flag.Value, flag.Unit)
	}
	if flag.AsOf.IsZero() || flag.Source == "" {
		t.Fatalf("borrow fee source/as_of missing: %+v", flag)
	}
	if _, ok := marketEventBorrowFeeFlag("MSFT", entry.Symbols["MSFT"], entry, now); ok {
		t.Fatal("low borrow fee should not emit an inactive false flag")
	}
}

func TestMarketEventBorrowFeesSnapshotIndexesBySymbol(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	cache.regSHO = marketEventRegSHOEntry{FetchedAt: now, AsOf: now, Symbols: map[string]marketEventRegSHORecord{}}
	cache.halts = marketEventHaltsEntry{FetchedAt: now, AsOf: now}
	cache.borrowFees = marketEventBorrowFeeEntry{
		FetchedAt: now,
		AsOf:      now.Add(-15 * time.Minute),
		SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
		Symbols: map[string]marketEventBorrowFeeRecord{
			"CRWV": {Symbol: "CRWV", Currency: "USD", Name: "COREWEAVE INC", FeeRate: 55.5, Available: 10_000},
		},
	}

	res := cache.snapshot(context.Background(), []string{"CRWV"}, nil, nil)
	flags := res.BySymbol["CRWV"]
	if len(flags) == 0 {
		t.Fatalf("by_symbol missing CRWV flag: %+v", res)
	}
	found := false
	for _, flag := range flags {
		if flag.ID == rpc.MarketEventBorrowFeeExtreme {
			found = true
			if flag.Value == nil || *flag.Value != 55.5 {
				t.Fatalf("borrow fee flag value = %v, want 55.5", flag.Value)
			}
		}
	}
	if !found {
		t.Fatalf("borrow_fee_extreme flag missing from by_symbol: %+v", flags)
	}
}

func TestMarketEventBorrowFeeStaleCacheFallback(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	cache.borrowFees = marketEventBorrowFeeEntry{
		FetchedAt: now.Add(-marketEventsBorrowFeeFreshFor - time.Minute),
		AsOf:      now.Add(-marketEventsBorrowFeeFreshFor - time.Minute),
		Symbols:   map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65}},
	}
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		return marketEventBorrowFeeEntry{}, errors.New("network down")
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	entry, health, err := cache.loadBorrowFees(context.Background(), now)
	if err != nil {
		t.Fatalf("stale borrow-fee cache fallback should not fail: %v", err)
	}
	if _, ok := entry.Symbols["CRWV"]; !ok {
		t.Fatalf("cached symbol missing: %+v", entry.Symbols)
	}
	if health.Status != rpc.SourceStatusStale {
		t.Fatalf("health status=%q, want stale", health.Status)
	}
}

func TestMarketEventFingerprintIgnoresBorrowFeeTimestampChurn(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	build := func(asOf time.Time) rpc.MarketEventsResult {
		value := 75.25
		res := rpc.MarketEventsResult{
			Kind:          rpc.MarketEventsKind,
			SchemaVersion: rpc.MarketEventsSchemaVersion,
			AsOf:          now,
			Symbols:       []string{"CRWV"},
			Flags: []rpc.MarketEventFlag{{
				ID:         rpc.MarketEventBorrowFeeExtreme,
				Symbol:     "CRWV",
				Label:      "Fee extreme",
				Status:     rpc.MarketEventStatusActive,
				Severity:   rpc.MarketEventSeverityAct,
				Role:       rpc.MarketEventRoleProposalModifier,
				Source:     "IBKR short stock availability",
				AsOf:       asOf,
				ObservedAt: now,
				Value:      &value,
				Unit:       "pct_annualized",
			}},
		}
		res.Fingerprint = rpc.BuildMarketEventsFingerprint(&res)
		return res
	}
	a := build(now.Add(-time.Minute))
	b := build(now.Add(-2 * time.Minute))
	if !reflect.DeepEqual(a.Flags[0].Value, b.Flags[0].Value) {
		t.Fatal("test setup value mismatch")
	}
	if a.Fingerprint.Key != b.Fingerprint.Key {
		t.Fatalf("fingerprint churned on timestamp only: %q != %q", a.Fingerprint.Key, b.Fingerprint.Key)
	}
}

func TestMarketEventRegSHOStaleCacheFallback(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	cache.regSHO = marketEventRegSHOEntry{
		FetchedAt: now.Add(-13 * time.Hour),
		AsOf:      now.Add(-13 * time.Hour),
		Symbols:   map[string]marketEventRegSHORecord{"CRWV": {Symbol: "CRWV"}},
	}
	orig := marketEventsHTTPClient
	marketEventsHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}
	t.Cleanup(func() { marketEventsHTTPClient = orig })

	entry, health, err := cache.loadRegSHO(context.Background(), now)
	if err != nil {
		t.Fatalf("stale cache fallback should not fail: %v", err)
	}
	if _, ok := entry.Symbols["CRWV"]; !ok {
		t.Fatalf("cached symbol missing: %+v", entry.Symbols)
	}
	if health.Status != rpc.SourceStatusStale {
		t.Fatalf("health status=%q, want stale", health.Status)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestMarketEventCacheShortableAbsence pins the session-scoped negative
// cache behind the borrow-inventory polls: a symbol observed absent in
// one NY session is skipped for that session only, and a gateway
// reconnect (clearShortableAbsence) re-arms the probe immediately.
// Without this memory, every market-events snapshot re-burned the full
// poll budget per non-US name whose shortable tick never arrives.
func TestMarketEventCacheShortableAbsence(t *testing.T) {
	t.Parallel()
	cache := newMarketEventCache(nil)
	sessionA := "2026-06-09"
	sessionB := "2026-06-10"

	if cache.shortableAbsentThisSession("DTE", sessionA) {
		t.Fatal("fresh cache should not report absence")
	}
	cache.rememberShortableAbsent("DTE", sessionA)
	if !cache.shortableAbsentThisSession("DTE", sessionA) {
		t.Error("absence recorded for session A should skip within session A")
	}
	if cache.shortableAbsentThisSession("DTE", sessionB) {
		t.Error("absence from session A must not carry into session B")
	}
	if cache.shortableAbsentThisSession("SAP", sessionA) {
		t.Error("absence is per-symbol")
	}

	cache.clearShortableAbsence()
	if cache.shortableAbsentThisSession("DTE", sessionA) {
		t.Error("clearShortableAbsence (reconnect) should re-arm the probe")
	}
}

// TestMarketEventBorrowFeeFailureMemory pins the retry-suppression
// window: after a failed fetch (observed live: ftp3.interactivebrokers
// port 21 filtered → full dial-timeout hang), snapshots within
// marketEventsBorrowFeeRetryAfter must NOT re-attempt the fetch — the
// hang was being re-paid on every canary run. Past the window, exactly
// one retry fires; success clears the memory.
func TestMarketEventBorrowFeeFailureMemory(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })

	var fetchCalls int
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{}, errors.New("dial tcp: i/o timeout")
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	if _, health, err := cache.loadBorrowFees(context.Background(), now); err == nil || health.Status != rpc.SourceStatusUnknown {
		t.Fatalf("first failure: err=%v status=%s", err, health.Status)
	}
	if fetchCalls != 1 {
		t.Fatalf("first call: fetch ran %d times, want 1", fetchCalls)
	}

	// Within the retry window: no fetch attempt, same degraded health.
	within := now.Add(marketEventsBorrowFeeRetryAfter - time.Minute)
	if _, health, err := cache.loadBorrowFees(context.Background(), within); err == nil || health.Status != rpc.SourceStatusUnknown {
		t.Fatalf("suppressed call: err=%v status=%s", err, health.Status)
	}
	if fetchCalls != 1 {
		t.Fatalf("suppressed call must not re-fetch: fetch ran %d times", fetchCalls)
	}

	// Past the window: one retry, now succeeding, clears the memory.
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{
			AsOf:    now,
			Symbols: map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65}},
		}, nil
	}
	past := now.Add(marketEventsBorrowFeeRetryAfter + time.Minute)
	if _, health, err := cache.loadBorrowFees(context.Background(), past); err != nil || health.Status != rpc.SourceStatusOK {
		t.Fatalf("recovery call: err=%v status=%s", err, health.Status)
	}
	if fetchCalls != 2 {
		t.Fatalf("recovery: fetch ran %d times, want 2", fetchCalls)
	}
	if !cache.borrowFeesFailedAt.IsZero() {
		t.Fatal("success should clear the failure memory")
	}

	// Stale-cache variant: a later failure serves the cached list
	// without retrying inside the window.
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{}, errors.New("network down again")
	}
	expired := past.Add(marketEventsBorrowFeeFreshFor + time.Minute)
	if entry, health, err := cache.loadBorrowFees(context.Background(), expired); err != nil || health.Status != rpc.SourceStatusStale || len(entry.Symbols) == 0 {
		t.Fatalf("stale fallback after failure: err=%v status=%s symbols=%d", err, health.Status, len(entry.Symbols))
	}
	if fetchCalls != 3 {
		t.Fatalf("stale-fallback failure: fetch ran %d times, want 3", fetchCalls)
	}
	if entry, health, err := cache.loadBorrowFees(context.Background(), expired.Add(time.Minute)); err != nil || health.Status != rpc.SourceStatusStale || len(entry.Symbols) == 0 {
		t.Fatalf("suppressed stale fallback: err=%v status=%s symbols=%d", err, health.Status, len(entry.Symbols))
	}
	if fetchCalls != 3 {
		t.Fatalf("suppressed stale fallback must not re-fetch: fetch ran %d times", fetchCalls)
	}
}

// TestRegSHODatedWalkSkipsRedirect pins the 302 handling: Nasdaq
// redirects missing dated threshold files to an HTML error page, and
// following it parsed as an EMPTY success — caching "no threshold
// symbols" for 12h and never reaching the newest real file. The
// no-redirect policy turns the 302 into a status error so the walk
// proceeds to the prior day.
func TestRegSHODatedWalkSkipsRedirect(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	body := strings.Join([]string{
		"Symbol|Security Name|Market Category|Reg SHO Threshold Flag|Rule3210",
		"CRWV|CoreWeave Inc.|Q|Y|N",
		"20260609180000",
		"",
	}, "\n")
	orig := marketEventsHTTPClient
	marketEventsHTTPClient = &http.Client{
		CheckRedirect: marketEventsNoRedirect,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "20260610") {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://www.nasdaqtrader.com/Error.aspx"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}
			if strings.Contains(req.URL.Path, "20260609") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}
			return nil, errors.New("unexpected URL " + req.URL.String())
		}),
	}
	t.Cleanup(func() { marketEventsHTTPClient = orig })

	entry, err := fetchLatestNasdaqRegSHO(context.Background(), now)
	if err != nil {
		t.Fatalf("dated walk should land on the prior day's file: %v", err)
	}
	if _, ok := entry.Symbols["CRWV"]; !ok {
		t.Fatalf("expected the 2026-06-09 file's symbols, got %+v", entry.Symbols)
	}
}
