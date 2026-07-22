package ibkr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWSHWireEncodingUsesMetadataThenFilteredEarningsRequest(t *testing.T) {
	connector, conn, out := newWSHTestConnector(t)
	connector.resolveWSHExactContract = func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error) {
		t.Fatal("legacy API must not invoke exact-ConID resolver")
		return nil, nil
	}
	connector.wshNow = func() time.Time { return time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC) }

	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		metaReqID := waitForHandlerReqID(t, conn, msgWSHMetaData)
		deliverWSHData(conn, msgWSHMetaData, metaReqID, `{"event_types":[{"code":"wsh_ed"}]}`)
		eventReqID := waitForHandlerReqIDAfter(t, conn, msgWSHEventData, metaReqID)
		deliverWSHData(conn, msgWSHEventData, eventReqID, `{"events":[{"date":"2026-08-01"}]}`)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := connector.FetchWSHEarnings(ctx, "AAPL")
	if err != nil {
		t.Fatalf("FetchWSHEarnings: %v", err)
	}
	<-responderDone
	if got != `{"events":[{"date":"2026-08-01"}]}` {
		t.Fatalf("event JSON = %q", got)
	}

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	metadata := findOutboundFrame(t, frames, reqWSHMetaData)
	if len(metadata) != 3 { // decoder retains the trailing NUL field
		t.Fatalf("metadata fields = %#v", metadata)
	}
	event := findOutboundFrame(t, frames, reqWSHEventData)
	assertField(t, event, 2, "0", "event conId")
	assertField(t, event, 4, "0", "fillWatchlist")
	assertField(t, event, 5, "0", "fillPortfolio")
	assertField(t, event, 6, "0", "fillCompetitors")
	assertField(t, event, 7, "", "startDate")
	assertField(t, event, 8, "", "endDate")
	assertField(t, event, 9, strconv.Itoa(wshRequestLimit), "totalLimit")

	var filter map[string]any
	if err := json.Unmarshal([]byte(event[3]), &filter); err != nil {
		t.Fatalf("event filter JSON: %v; raw=%q", err, event[3])
	}
	watchlist, ok := filter["watchlist"].([]any)
	if !ok || len(watchlist) != 1 || watchlist[0] != "8314" {
		t.Fatalf("filter watchlist = %#v", filter["watchlist"])
	}
	if filter["wsh_ed"] != "true" {
		t.Fatalf("filter wsh_ed = %#v", filter["wsh_ed"])
	}
	if filter["country"] != "All" || filter["limit_region"] != float64(wshRequestLimit) {
		t.Fatalf("required WSH filter scope missing: %#v", filter)
	}
}

func TestSendWSHEventDataServer171OmitsDateFields(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	setServerVersionReady(conn, minServerVerWSHEventFilters)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.sendWSHEventDataRequest(ctx, 77, wshEventRequest{
		conID: 8314, limit: 10,
	}); err != nil {
		t.Fatalf("sendWSHEventDataRequest: %v", err)
	}
	fields := onlyOutboundFrame(t, conn, out.Bytes())
	if len(fields) != 8 { // seven encoded values plus the trailing NUL field
		t.Fatalf("server 171 event fields = %#v", fields)
	}
	assertField(t, fields, 3, `{"country":"All","limit":10,"limit_region":10,"watchlist":["8314"],"wsh_ed":"true"}`, "filter")
}

func TestWSHWireRejectsProtocolNewerThanAdvertisedClient(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	setServerVersionReady(conn, maxClientVersion+1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := conn.sendWSHEventDataRequest(ctx, 77, wshEventRequest{conID: 8314, limit: 10})
	assertWSHError(t, err, WSHErrorUnsupportedProtocol, "event_data", 0)
	if out.Len() != 0 {
		t.Fatalf("unsupported newer protocol emitted %d wire bytes", out.Len())
	}
}

func TestWSHMetadataReadinessIsConnectionScopedAndExpires(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	conn := connector.conn
	other := NewConnection(nil)
	t.Cleanup(func() {
		conn.rateLimiter.Stop()
		other.rateLimiter.Stop()
	})
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	connector.markWSHMetadataReady(conn, now)
	if !connector.wshMetadataReady(conn, now.Add(23*time.Hour)) {
		t.Fatal("metadata readiness should remain valid within 24 hours")
	}
	if connector.wshMetadataReady(conn, now.Add(wshMetadataReadyTTL)) {
		t.Fatal("metadata readiness must expire at 24 hours")
	}
	if connector.wshMetadataReady(other, now.Add(time.Hour)) {
		t.Fatal("metadata readiness must not transfer to another connection")
	}
	connector.resetWSHMetadataReadiness()
	if connector.wshMetadataReady(conn, now.Add(time.Hour)) {
		t.Fatal("reset must clear metadata readiness")
	}
}

func TestWSHMetadataSelectsCurrentEarningsTag(t *testing.T) {
	raw := `{"meta_data":{"event_types":[{"tag":"wshe_ed"}]}}`
	if tag, ok := wshMetadataEarningsEventTag(raw); !ok || tag != "wshe_ed" {
		t.Fatalf("metadata tag=(%q,%v), want wshe_ed", tag, ok)
	}
	conn, out := newReadyWireTestConnection(t)
	setServerVersionReady(conn, maxClientVersion)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.sendWSHEventDataRequest(ctx, 77, wshEventRequest{conID: 8314, limit: 10, eventTag: "wshe_ed"}); err != nil {
		t.Fatal(err)
	}
	fields := onlyOutboundFrame(t, conn, out.Bytes())
	var filter map[string]any
	if err := json.Unmarshal([]byte(fields[3]), &filter); err != nil {
		t.Fatal(err)
	}
	if filter["wshe_ed"] != "true" || filter["wsh_ed"] != nil {
		t.Fatalf("metadata-selected filter=%#v", filter)
	}
}

func TestFetchWSHEarningsClassifiesLegacyEntitlementErrorWithoutGatewayText(t *testing.T) {
	connector, conn, _ := newWSHTestConnector(t)
	rawMessage := "account ABC123 is not subscribed; token secret-value"
	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		reqID := waitForHandlerReqID(t, conn, msgWSHMetaData)
		fields := []string{strconv.Itoa(msgErrMsg), "2", strconv.Itoa(reqID), "10089", rawMessage}
		for _, handler := range conn.snapshotHandlers(msgErrMsg) {
			handler(fields)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := connector.FetchWSHEarnings(ctx, "AAPL")
	<-responderDone
	assertWSHError(t, err, WSHErrorEntitlementRequired, "metadata", 10089)
	if strings.Contains(err.Error(), "ABC123") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("sanitized error leaked gateway text: %v", err)
	}
}

func TestFetchWSHEarningsClassifiesSystemNotificationError(t *testing.T) {
	connector, conn, _ := newWSHTestConnector(t)
	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		reqID := waitForHandlerReqID(t, conn, msgWSHMetaData)
		conn.processMessage(encodeSystemNotificationForTest(reqID, 10279, "provider internals and account text", ""))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := connector.FetchWSHEarnings(ctx, "AAPL")
	<-responderDone
	assertWSHError(t, err, WSHErrorProviderFailure, "metadata", 10279)
}

func TestWSHUnknownCodeIgnoresAdversarialEntitlementProse(t *testing.T) {
	err := classifyWSHBrokerError("event_data", 19999, "subscription required; do not retry")
	assertWSHError(t, err, WSHErrorProviderFailure, "event_data", 19999)
}

func TestFetchWSHEarningsRefreshesMetadataOnceWhenTWSRequiresIt(t *testing.T) {
	connector, conn, out := newWSHTestConnector(t)
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	connector.wshNow = func() time.Time { return now }
	connector.markWSHMetadataReady(conn, now, "wsh_ed")

	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		firstEvent := waitForHandlerReqID(t, conn, msgWSHEventData)
		fields := []string{strconv.Itoa(msgErrMsg), "2", strconv.Itoa(firstEvent), "10282", "metadata required"}
		for _, handler := range conn.snapshotHandlers(msgErrMsg) {
			handler(fields)
		}
		metadata := waitForHandlerReqIDAfter(t, conn, msgWSHMetaData, firstEvent)
		deliverWSHData(conn, msgWSHMetaData, metadata, `{"meta_data":{"event_types":[{"tag":"wshe_ed"}]}}`)
		secondEvent := waitForHandlerReqIDAfter(t, conn, msgWSHEventData, metadata)
		deliverWSHData(conn, msgWSHEventData, secondEvent, `{"events":[]}`)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := connector.FetchWSHEarnings(ctx, "AAPL")
	<-responderDone
	if err != nil || got != `{"events":[]}` {
		t.Fatalf("recovered result=%q err=%v", got, err)
	}
	if countOutboundFrames(t, conn, out.Bytes(), reqWSHEventData) != 2 || countOutboundFrames(t, conn, out.Bytes(), reqWSHMetaData) != 1 {
		t.Fatalf("unexpected recovery wire sequence")
	}
	if tag, ok := connector.wshMetadataEarningsTag(conn, now); !ok || tag != "wshe_ed" {
		t.Fatalf("refreshed metadata tag=(%q,%v)", tag, ok)
	}
}

func TestFetchWSHEarningsTimeoutCancelsPendingMetadata(t *testing.T) {
	connector, conn, out := newWSHTestConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := connector.FetchWSHEarnings(ctx, "AAPL")
	assertWSHError(t, err, WSHErrorTimeout, "metadata", 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must unwrap context.DeadlineExceeded: %v", err)
	}

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	request := findOutboundFrame(t, frames, reqWSHMetaData)
	cancelFrame := findOutboundFrame(t, frames, cancelWSHMetaData)
	assertField(t, cancelFrame, 1, request[1], "cancel reqID")
}

func TestFetchWSHEarningsCancellationCancelsPendingEventData(t *testing.T) {
	connector, conn, out := newWSHTestConnector(t)
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := connector.FetchWSHEarnings(ctx, "AAPL")
		resultCh <- err
	}()
	metaReqID := waitForHandlerReqID(t, conn, msgWSHMetaData)
	deliverWSHData(conn, msgWSHMetaData, metaReqID, `{"event_types":["wsh_ed"]}`)
	eventReqID := waitForHandlerReqIDAfter(t, conn, msgWSHEventData, metaReqID)
	cancel()
	err := <-resultCh
	assertWSHError(t, err, WSHErrorCanceled, "event_data", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must unwrap context.Canceled: %v", err)
	}

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	cancelFrame := findOutboundFrame(t, frames, cancelWSHEventData)
	assertField(t, cancelFrame, 1, strconv.Itoa(eventReqID), "cancel event reqID")
}

func TestFetchWSHEarningsSerializesCompleteWSHSequence(t *testing.T) {
	connector, conn, out := newWSHTestConnector(t)
	type result struct {
		data string
		err  error
	}
	results := make(chan result, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		data, err := connector.FetchWSHEarnings(ctx, "AAPL")
		results <- result{data: data, err: err}
	}()
	firstMeta := waitForHandlerReqID(t, conn, msgWSHMetaData)
	deliverWSHData(conn, msgWSHMetaData, firstMeta, `{"event_types":["wsh_ed"]}`)
	firstEvent := waitForHandlerReqIDAfter(t, conn, msgWSHEventData, firstMeta)

	go func() {
		data, err := connector.FetchWSHEarnings(ctx, "MSFT")
		results <- result{data: data, err: err}
	}()
	time.Sleep(30 * time.Millisecond)
	if got := countOutboundFrames(t, conn, out.Bytes(), reqWSHMetaData); got != 1 {
		t.Fatalf("metadata requests while first event pending = %d, want 1", got)
	}
	deliverWSHData(conn, msgWSHEventData, firstEvent, `{"events":[]}`)

	secondEvent := waitForHandlerReqIDAfter(t, conn, msgWSHEventData, firstEvent)
	deliverWSHData(conn, msgWSHEventData, secondEvent, `{"events":[]}`)

	for range 2 {
		got := <-results
		if got.err != nil || got.data != `{"events":[]}` {
			t.Fatalf("serialized result = %#v", got)
		}
	}
	if got := countOutboundFrames(t, conn, out.Bytes(), reqWSHMetaData); got != 1 {
		t.Fatalf("metadata requests = %d, want 1 session-ready request", got)
	}
}

func TestFetchWSHEarningsWaitingCallerHonorsContext(t *testing.T) {
	connector, conn, out := newWSHTestConnector(t)
	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := connector.FetchWSHEarnings(firstCtx, "AAPL")
		firstDone <- err
	}()
	_ = waitForHandlerReqID(t, conn, msgWSHMetaData)

	queuedCtx, queuedCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer queuedCancel()
	_, err := connector.FetchWSHEarnings(queuedCtx, "MSFT")
	assertWSHError(t, err, WSHErrorTimeout, "acquire", 0)
	if got := countOutboundFrames(t, conn, out.Bytes(), reqWSHMetaData); got != 1 {
		t.Fatalf("queued timed-out call emitted %d metadata requests, want 1", got)
	}
	firstCancel()
	<-firstDone
}

func TestFetchWSHEarningsRejectsUnsupportedSecurityBeforeWire(t *testing.T) {
	connector, _, out := newWSHTestConnector(t)
	_, err := connector.FetchWSHEarnings(context.Background(), "SPX")
	assertWSHError(t, err, WSHErrorUnsupportedSecurity, "resolve_contract", 0)
	if out.Len() != 0 {
		t.Fatalf("unsupported security emitted wire traffic: %d bytes", out.Len())
	}
}

func TestFetchWSHEarningsKnownInactiveRecoversAfterReconnect(t *testing.T) {
	connector, conn, out := newWSHTestConnector(t)
	const testName = "TESTQ"
	connector.inactiveMu.Lock()
	if connector.inactiveSymbols == nil {
		connector.inactiveSymbols = make(map[string]inactiveSymbolState)
	}
	connector.inactiveSymbols[testName] = inactiveSymbolState{reason: "inactive", markedAt: time.Now()}
	connector.inactiveMu.Unlock()
	connector.contractMu.Lock()
	connector.contractCache[testName] = ContractDetailsLite{Symbol: testName, SecType: "STK", ConID: 991001}
	connector.contractMu.Unlock()

	resolveCalls := 0
	connector.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		resolveCalls++
		return &ContractDetailsLite{Symbol: testName, SecType: "STK", ConID: 991001}, nil
	}

	_, err := connector.FetchWSHEarnings(context.Background(), testName)
	assertWSHError(t, err, WSHErrorConnectorInactive, "resolve_contract", 0)
	if resolveCalls != 0 {
		t.Fatalf("known-inactive contract resolution calls = %d, want 0", resolveCalls)
	}
	if out.Len() != 0 {
		t.Fatalf("known-inactive request emitted %d wire bytes, want 0", out.Len())
	}

	// A successor connector session drops every unstamped observation,
	// including the temporary inactive mark. The next WSH attempt must resolve
	// and reach the provider instead of retaining an unsupported verdict.
	connector.invalidateUnstampedConnectorObservations(conn)
	if connector.IsSymbolInactive(testName) {
		t.Fatal("reconnect did not clear the temporary inactive mark")
	}
	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		metaReqID := waitForHandlerReqID(t, conn, msgWSHMetaData)
		deliverWSHData(conn, msgWSHMetaData, metaReqID, `{"event_types":["wsh_ed"]}`)
		eventReqID := waitForHandlerReqIDAfter(t, conn, msgWSHEventData, metaReqID)
		deliverWSHData(conn, msgWSHEventData, eventReqID, `{"events":[]}`)
	}()
	got, err := connector.FetchWSHEarnings(context.Background(), testName)
	<-responderDone
	if err != nil || got != `{"events":[]}` {
		t.Fatalf("post-reconnect WSH result=%q err=%v", got, err)
	}
	if resolveCalls != 1 {
		t.Fatalf("post-reconnect contract resolution calls = %d, want 1", resolveCalls)
	}
}

func TestFetchWSHEarningsResolutionInactiveMarkSkipsProviderWire(t *testing.T) {
	connector, _, out := newWSHTestConnector(t)
	const testName = "TESTQ"
	resolveCalls := 0
	connector.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		resolveCalls++
		connector.inactiveMu.Lock()
		if connector.inactiveSymbols == nil {
			connector.inactiveSymbols = make(map[string]inactiveSymbolState)
		}
		connector.inactiveSymbols[testName] = inactiveSymbolState{reason: "inactive", markedAt: time.Now()}
		connector.inactiveMu.Unlock()
		return nil, errors.New("contract resolution failed")
	}

	_, err := connector.FetchWSHEarnings(context.Background(), testName)
	assertWSHError(t, err, WSHErrorConnectorInactive, "resolve_contract", 0)
	if resolveCalls != 1 {
		t.Fatalf("contract resolution calls = %d, want 1 confirmation", resolveCalls)
	}
	if out.Len() != 0 {
		t.Fatalf("new inactive mark emitted %d WSH wire bytes, want 0", out.Len())
	}
}

func TestResolveWSHStockIdentityBypassesInactiveAndSanitizesFailure(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	t.Cleanup(func() { connector.conn.rateLimiter.Stop() })
	connector.inactiveMu.Lock()
	if connector.inactiveSymbols == nil {
		connector.inactiveSymbols = make(map[string]inactiveSymbolState)
	}
	connector.inactiveSymbols["SYNTH1"] = inactiveSymbolState{reason: "typed_test", markedAt: time.Now()}
	connector.inactiveMu.Unlock()
	called := 0
	connector.resolveWSHExactContract = func(_ context.Context, symbol string, conID int, _ time.Duration) (*ContractDetailsLite, error) {
		called++
		if symbol != "SYNTH1" || conID != 424242 {
			t.Fatalf("exact resolver args symbol=%q conID=%d", symbol, conID)
		}
		return nil, errors.New("SENSITIVE_SENTINEL")
	}
	identity, err := connector.ResolveWSHStockIdentity(context.Background(), "SYNTH1", 424242)
	if identity != nil {
		t.Fatalf("failed exact lookup returned identity: %+v", *identity)
	}
	assertWSHError(t, err, WSHErrorContractResolution, "resolve_contract", 0)
	if strings.Contains(err.Error(), "SENSITIVE_SENTINEL") {
		t.Fatalf("sanitized exact-resolution error leaked source text: %v", err)
	}
	if called != 1 {
		t.Fatalf("exact resolver calls=%d, want 1 despite inactive symbol cache", called)
	}
}

func TestFetchWSHEarningsWithIdentityBypassesOnlySymbolInactiveCache(t *testing.T) {
	connector, conn, out := newWSHTestConnector(t)
	const (
		symbol = "SYNTH1"
		conID  = 424242
	)
	connector.inactiveMu.Lock()
	if connector.inactiveSymbols == nil {
		connector.inactiveSymbols = make(map[string]inactiveSymbolState)
	}
	connector.inactiveSymbols[symbol] = inactiveSymbolState{reason: "typed_test", markedAt: time.Now()}
	connector.inactiveMu.Unlock()
	connector.contractMu.Lock()
	connector.contractCache[symbol] = ContractDetailsLite{Symbol: symbol, SecType: "STK", ConID: conID, Currency: "USD"}
	connector.contractMu.Unlock()

	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		detailReqID := waitForHandlerReqID(t, conn, msgContractData)
		fields := syntheticStockContractDetailsFields(maxClientVersion, "TYPE_CODE")
		fields[1] = strconv.Itoa(detailReqID)
		for _, handler := range conn.snapshotHandlers(msgContractData) {
			handler(fields)
		}
		metaReqID := waitForHandlerReqIDAfter(t, conn, msgWSHMetaData, detailReqID)
		deliverWSHData(conn, msgWSHMetaData, metaReqID, `{"event_types":["wsh_ed"]}`)
		eventReqID := waitForHandlerReqIDAfter(t, conn, msgWSHEventData, metaReqID)
		deliverWSHData(conn, msgWSHEventData, eventReqID, `{"events":[]}`)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := connector.FetchWSHEarningsWithIdentity(ctx, symbol, conID)
	<-responderDone
	if err != nil {
		t.Fatalf("exact WSH fetch: %v", err)
	}
	if result.EventJSON != `{"events":[]}` || result.StockIdentity == nil {
		t.Fatalf("exact WSH result event=%q identity_present=%v", result.EventJSON, result.StockIdentity != nil)
	}
	if result.StockIdentity.ConID != conID || result.StockIdentity.SecType != "STK" || result.StockIdentity.StockType != "TYPE_CODE" {
		t.Fatalf("exact identity did not retain typed broker fields: %+v", *result.StockIdentity)
	}
	if !connector.IsSymbolInactive(symbol) {
		t.Fatal("exact identity read cleared the independent symbol inactive cache")
	}

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	detailRequest := findOutboundFrame(t, frames, reqContractData)
	assertField(t, detailRequest, 3, strconv.Itoa(conID), "exact contract conID")
	assertField(t, detailRequest, 4, "", "exact contract symbol")
	assertField(t, detailRequest, 5, "STK", "exact contract security type")
	eventRequest := findOutboundFrame(t, frames, reqWSHEventData)
	var filter map[string]any
	if err := json.Unmarshal([]byte(eventRequest[3]), &filter); err != nil {
		t.Fatal(err)
	}
	watchlist, _ := filter["watchlist"].([]any)
	if len(watchlist) != 1 || watchlist[0] != strconv.Itoa(conID) {
		t.Fatalf("event request did not use supplied exact conID: %#v", filter["watchlist"])
	}
}

func TestFetchWSHEarningsWithIdentityMismatchesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		detail ContractDetailsLite
	}{
		{name: "conid", detail: ContractDetailsLite{Symbol: "SYNTH1", SecType: "STK", ConID: 424243, StockType: "TYPE_CODE"}},
		{name: "security_type", detail: ContractDetailsLite{Symbol: "SYNTH1", SecType: "FUND", ConID: 424242, StockType: "TYPE_CODE"}},
		{name: "symbol", detail: ContractDetailsLite{Symbol: "SYNTH2", SecType: "STK", ConID: 424242, StockType: "TYPE_CODE"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			connector, conn, _ := newWSHTestConnector(t)
			now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
			connector.wshNow = func() time.Time { return now }
			connector.markWSHMetadataReady(conn, now, "wsh_ed")
			connector.resolveWSHExactContract = func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error) {
				copy := test.detail
				return &copy, nil
			}
			go func() {
				reqID := waitForHandlerReqID(t, conn, msgWSHEventData)
				deliverWSHData(conn, msgWSHEventData, reqID, `{"events":[]}`)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := connector.FetchWSHEarningsWithIdentity(ctx, "SYNTH1", 424242)
			if err != nil || result.EventJSON != `{"events":[]}` {
				t.Fatalf("event result=%q err=%v", result.EventJSON, err)
			}
			if result.StockIdentity != nil {
				t.Fatalf("mismatched detail became identity: %+v", *result.StockIdentity)
			}
		})
	}
}

func TestFetchWSHEarningsWithIdentityDoesNotPromotePartialCache(t *testing.T) {
	connector, conn, _ := newWSHTestConnector(t)
	const (
		symbol = "SYNTH1"
		conID  = 424242
	)
	connector.contractMu.Lock()
	connector.contractCache[symbol] = ContractDetailsLite{
		Symbol: symbol, SecType: "STK", ConID: conID, Currency: "USD", StockType: "CACHE_ONLY_TYPE",
	}
	connector.contractMu.Unlock()
	connector.resolveWSHExactContract = func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error) {
		return nil, errors.New("synthetic")
	}
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	connector.wshNow = func() time.Time { return now }
	connector.markWSHMetadataReady(conn, now, "wsh_ed")
	go func() {
		reqID := waitForHandlerReqID(t, conn, msgWSHEventData)
		deliverWSHData(conn, msgWSHEventData, reqID, `{"events":[]}`)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := connector.FetchWSHEarningsWithIdentity(ctx, symbol, conID)
	if err != nil || result.EventJSON != `{"events":[]}` {
		t.Fatalf("event result=%q err=%v", result.EventJSON, err)
	}
	if result.StockIdentity != nil {
		t.Fatalf("cache-only stock type was promoted: %+v", *result.StockIdentity)
	}
}

func TestFetchWSHEarningsWithIdentityReturnsBrokerIdentityWithProviderFailure(t *testing.T) {
	connector, conn, _ := newWSHTestConnector(t)
	connector.resolveWSHExactContract = func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{Symbol: "SYNTH1", SecType: "STK", ConID: 424242, StockType: "TYPE_CODE"}, nil
	}
	go func() {
		reqID := waitForHandlerReqID(t, conn, msgWSHMetaData)
		fields := []string{strconv.Itoa(msgErrMsg), "2", strconv.Itoa(reqID), "10089", "SYNTHETIC"}
		for _, handler := range conn.snapshotHandlers(msgErrMsg) {
			handler(fields)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := connector.FetchWSHEarningsWithIdentity(ctx, "SYNTH1", 424242)
	assertWSHError(t, err, WSHErrorEntitlementRequired, "metadata", 10089)
	if result.StockIdentity == nil || result.StockIdentity.StockType != "TYPE_CODE" {
		t.Fatalf("provider failure discarded exact identity: %+v", result.StockIdentity)
	}
}

func TestFetchWSHEarningsWithIdentityRejectsMissingConIDBeforeWire(t *testing.T) {
	connector, _, out := newWSHTestConnector(t)
	_, err := connector.FetchWSHEarningsWithIdentity(context.Background(), "SYNTH1", 0)
	assertWSHError(t, err, WSHErrorUnsupportedSecurity, "resolve_contract", 0)
	if out.Len() != 0 {
		t.Fatalf("missing exact conID emitted %d wire bytes", out.Len())
	}
}

func TestFetchWSHEarningsRejectsMetadataWithoutEarningsEventType(t *testing.T) {
	connector, conn, _ := newWSHTestConnector(t)
	go func() {
		reqID := waitForHandlerReqID(t, conn, msgWSHMetaData)
		deliverWSHData(conn, msgWSHMetaData, reqID, `{"event_types":["wshe_bod"]}`)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := connector.FetchWSHEarnings(ctx, "AAPL")
	assertWSHError(t, err, WSHErrorEventTypeUnavailable, "metadata", 0)
}

func TestWSHMetadataRequiresEarningsInsideEventTypes(t *testing.T) {
	if wshMetadataSupportsEarnings(`{"filters":["wsh_ed"],"event_types":["wshe_bod"]}`) {
		t.Fatal("wsh_ed outside event_types must not authorize the earnings filter")
	}
	if wshMetadataSupportsEarnings(`{"event_types":[{"description":"wsh_ed","tag":"wshe_bod"}]}`) {
		t.Fatal("untyped description text must not authorize the earnings filter")
	}
	if wshMetadataSupportsEarnings(`{"event_types":[{"tag":"wsh_ed","code":"wshe_ed"}]}`) {
		t.Fatal("conflicting typed metadata fields must not authorize the earnings filter")
	}
	if !wshMetadataSupportsEarnings(`{"event_types":[{"code":"wsh_ed"}]}`) {
		t.Fatal("wsh_ed event type should be accepted")
	}
}

func newWSHTestConnector(t *testing.T) (*Connector, *Connection, *safeBuffer) {
	t.Helper()
	connector := NewConnector(&ConnectorConfig{})
	conn := connector.conn
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	out := &safeBuffer{}
	conn.writer = bufio.NewWriter(out)
	connector.running = true
	connector.ready = true
	connector.resolveWSHContract = func(context.Context, string, time.Duration) (*ContractDetailsLite, error) {
		return &ContractDetailsLite{Symbol: "AAPL", SecType: "STK", ConID: 8314}, nil
	}
	return connector, conn, out
}

func deliverWSHData(conn *Connection, msgID, reqID int, data string) {
	conn.processMessage(conn.encodeMsg(msgID, reqID, data))
}

func countOutboundFrames(t *testing.T, conn *Connection, payload []byte, msgID int) int {
	t.Helper()
	want := strconv.Itoa(msgID)
	count := 0
	for _, frame := range decodeOutboundFrames(t, conn, payload) {
		if len(frame) > 0 && frame[0] == want {
			count++
		}
	}
	return count
}

func assertWSHError(t *testing.T, err error, kind WSHErrorKind, operation string, code int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected WSH error %s", kind)
	}
	var target *WSHError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *WSHError: %v", err, err)
	}
	if target.Kind != kind || target.Operation != operation || target.Code != code {
		t.Fatalf("WSH error = %+v, want kind=%s operation=%s code=%d", target, kind, operation, code)
	}
}
