package ibkr

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseContractDetailsLiteVersion1(t *testing.T) {
	fields := []string{
		"10", // message id
		"1",  // reqID (version omitted because serverVersion >= size rules)
		"GLD",
		"STK",
		"",  // last trade date / contract month
		"",  // strike
		"0", // right
		"",  // empty exchange placeholder (older payloads)
		"SMART",
		"USD",
		"GLD",
		"GLD",
		"GLD",
		"51529211",
		"0.01",
		"",                           // md size multiplier (deprecated)
		"ACTIVETIM,AD,ADDONT,ADJUST", // order types (partial)
	}

	lite, ok := parseContractDetailsLite(fields, 1, 203)
	if !ok {
		t.Fatalf("expected parseContractDetailsLite to succeed")
	}
	if lite.ConID != 51529211 {
		t.Fatalf("expected conID 51529211, got %d", lite.ConID)
	}
	if lite.LocalSymbol != "GLD" {
		t.Fatalf("expected local symbol GLD, got %q", lite.LocalSymbol)
	}
}

func TestNormalizeEquityRoutingUsesSmartExchange(t *testing.T) {
	contract := Contract{
		SecType:  "STK",
		Exchange: "NASDAQ",
	}

	normalizeEquityRouting(&contract, "")

	if contract.Exchange != "SMART" {
		t.Fatalf("expected exchange SMART, got %q", contract.Exchange)
	}
	if contract.PrimaryExch != "NASDAQ" {
		t.Fatalf("expected primary NASDAQ, got %q", contract.PrimaryExch)
	}
}

func TestNormalizeEquityRoutingRespectsFallback(t *testing.T) {
	contract := Contract{
		SecType:  "STK",
		Exchange: "SMART",
	}

	normalizeEquityRouting(&contract, "ARCA")

	if contract.Exchange != "SMART" {
		t.Fatalf("expected exchange SMART, got %q", contract.Exchange)
	}
	if contract.PrimaryExch != "ARCA" {
		t.Fatalf("expected primary ARCA, got %q", contract.PrimaryExch)
	}
}

func TestExactOrderContractFiltersStockListingAndPreservesRoute(t *testing.T) {
	request := Contract{Symbol: "abc", SecType: "STK", Exchange: "smart", PrimaryExch: "nasdaq", Currency: "usd"}
	resolved, err := exactOrderContract(request, []ContractDetailsLite{
		{ConID: 22, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD"},
		{ConID: 11, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", LocalSymbol: "ABC", TradingClass: "NMS"},
	})
	if err != nil {
		t.Fatalf("exactOrderContract: %v", err)
	}
	if resolved.Contract.ConID != 11 || resolved.Contract.Exchange != "SMART" || resolved.Contract.PrimaryExch != "NASDAQ" {
		t.Fatalf("resolved contract = %+v", resolved.Contract)
	}
}

func TestExactOrderContractRequiresSuppliedStockPrimaryExchange(t *testing.T) {
	request := Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	for name, detail := range map[string]ContractDetailsLite{
		"missing":  {ConID: 11, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		"conflict": {ConID: 11, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := exactOrderContract(request, []ContractDetailsLite{detail}); err == nil {
				t.Fatal("expected supplied primary exchange mismatch to fail closed")
			}
		})
	}
}

func TestExactOrderContractFiltersDirectExchange(t *testing.T) {
	request := Contract{Symbol: "ABC", SecType: "STK", Exchange: "IBIS", Currency: "EUR"}
	resolved, err := exactOrderContract(request, []ContractDetailsLite{
		{ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR"},
		{ConID: 2, Symbol: "ABC", SecType: "STK", Exchange: "IBIS", PrimaryExch: "IBIS", Currency: "EUR"},
	})
	if err != nil {
		t.Fatalf("exactOrderContract: %v", err)
	}
	if resolved.Contract.ConID != 2 || resolved.Contract.Exchange != "IBIS" {
		t.Fatalf("resolved contract = %+v", resolved.Contract)
	}
}

func TestExactOrderContractRequiresSuppliedQualifiers(t *testing.T) {
	base := Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "ABC.N", TradingClass: "NMS"}
	for name, detail := range map[string]ContractDetailsLite{
		"local missing":  {ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", TradingClass: "NMS"},
		"local conflict": {ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "OTHER", TradingClass: "NMS"},
		"class missing":  {ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "ABC.N"},
		"class conflict": {ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "ABC.N", TradingClass: "OTHER"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := exactOrderContract(base, []ContractDetailsLite{detail}); err == nil {
				t.Fatal("expected supplied qualifier mismatch to fail closed")
			}
		})
	}
}

func TestExactOrderContractOptionMultiplierAndPrimaryHint(t *testing.T) {
	request := Contract{Symbol: "ABC", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C", Multiplier: 100, Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	if _, err := exactOrderContract(request, []ContractDetailsLite{{
		ConID: 8, Symbol: "ABC", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C", Multiplier: 10, Exchange: "SMART", Currency: "USD",
	}}); err == nil {
		t.Fatal("expected nonstandard multiplier mismatch to fail closed")
	}
	resolved, err := exactOrderContract(request, []ContractDetailsLite{{
		ConID: 9, Symbol: "ABC", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C", Multiplier: 100, Exchange: "SMART", Currency: "USD",
	}})
	if err != nil {
		t.Fatalf("exactOrderContract: %v", err)
	}
	if resolved.Contract.ConID != 9 || resolved.Contract.Multiplier != 100 || resolved.Contract.PrimaryExch != "NASDAQ" {
		t.Fatalf("resolved option = %+v", resolved.Contract)
	}
}

func TestExactOrderContractUsesBrokerStockTypeForETF(t *testing.T) {
	resolved, err := exactOrderContract(Contract{Symbol: "HYG", SecType: "ETF", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"}, []ContractDetailsLite{{
		ConID: 43652089, Symbol: "HYG", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD",
	}})
	if err != nil {
		t.Fatalf("exactOrderContract: %v", err)
	}
	if resolved.Contract.SecType != "STK" {
		t.Fatalf("resolved SecType = %q, want broker STK", resolved.Contract.SecType)
	}
}

func TestExactOrderContractRejectsAmbiguityAndNonPositiveIdentity(t *testing.T) {
	request := Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	if _, err := exactOrderContract(request, []ContractDetailsLite{
		{ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"},
		{ConID: 2, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD"},
	}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguity error = %v", err)
	}
	if _, err := exactOrderContract(request, []ContractDetailsLite{{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"}}); err == nil {
		t.Fatal("expected non-positive identity to fail closed")
	}
}

func TestResolveOrderContractForSessionBrokerizesETFAndReturnsExactIdentity(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session")
	}
	type outcome struct {
		resolved ResolvedOrderContract
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		resolved, err := connector.ResolveOrderContractForSession(context.Background(), binding, Contract{
			Symbol: "HYG", SecType: "ETF", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD",
		}, time.Second)
		done <- outcome{resolved: resolved, err: err}
	}()
	reqID := waitForHandlerReqID(t, conn, msgContractData)
	requestFields := onlyOutboundFrame(t, conn, socket.Bytes())
	assertField(t, requestFields, 5, "STK", "contractDetails broker secType")

	frame := orderContractDetailsFrame(reqID, 43652089, "HYG", "STK", "SMART", "ARCA", "USD", "HYG", "HYG", 0)
	conn.dispatchHandlers(msgContractData, frame, binding.epoch)
	conn.dispatchHandlers(msgContractDataEnd, []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(reqID)}, binding.epoch)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("ResolveOrderContractForSession: %v", got.err)
		}
		if got.resolved.Contract.ConID != 43652089 || got.resolved.Contract.SecType != "STK" || got.resolved.Contract.PrimaryExch != "ARCA" {
			t.Fatalf("resolved = %+v", got.resolved)
		}
	case <-time.After(time.Second):
		t.Fatal("resolver did not complete")
	}
}

func TestResolveOrderContractForSessionRejectsRetiredCallbacksAndDoesNotCompleteSuccessor(t *testing.T) {
	conn, connector, _, _, _ := newQueuedInstructionReconnectFixture(t)
	bindingA, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session A")
	}
	type outcome struct {
		resolved ResolvedOrderContract
		err      error
	}
	doneA := make(chan outcome, 1)
	go func() {
		resolved, err := connector.ResolveOrderContractForSession(context.Background(), bindingA, Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"}, time.Second)
		doneA <- outcome{resolved: resolved, err: err}
	}()
	reqA := waitForHandlerReqID(t, conn, msgContractData)
	conn.resetOrderIDReadiness()
	epochB := conn.BrokerSessionEpoch()
	conn.observeNextValidOrderIDAtEpoch(500, epochB)
	bindingB, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session B")
	}
	doneB := make(chan outcome, 1)
	go func() {
		resolved, err := connector.ResolveOrderContractForSession(context.Background(), bindingB, Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"}, time.Second)
		doneB <- outcome{resolved: resolved, err: err}
	}()
	reqB := waitForHandlerReqIDAfter(t, conn, msgContractData, reqA)

	staleFrame := orderContractDetailsFrame(reqA, 1, "ABC", "STK", "SMART", "NASDAQ", "USD", "ABC", "NMS", 0)
	conn.dispatchHandlers(msgContractData, staleFrame, bindingA.epoch)
	conn.dispatchHandlers(msgContractDataEnd, []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(reqA)}, bindingA.epoch)
	select {
	case got := <-doneB:
		t.Fatalf("stale A callback completed B: %+v", got)
	case <-time.After(25 * time.Millisecond):
	}

	currentFrame := orderContractDetailsFrame(reqB, 2, "ABC", "STK", "SMART", "NASDAQ", "USD", "ABC", "NMS", 0)
	conn.dispatchHandlers(msgContractData, currentFrame, bindingB.epoch)
	conn.dispatchHandlers(msgContractDataEnd, []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(reqB)}, bindingB.epoch)
	select {
	case got := <-doneB:
		if got.err != nil || got.resolved.Contract.ConID != 2 {
			t.Fatalf("B result = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("B resolver did not complete")
	}
	select {
	case got := <-doneA:
		if got.err == nil {
			t.Fatalf("retired A resolver succeeded: %+v", got.resolved)
		}
	case <-time.After(time.Second):
		t.Fatal("retired A resolver did not fail")
	}
}

func orderContractDetailsFrame(reqID, conID int, symbol, secType, exchange, primary, currency, localSymbol, tradingClass string, multiplier int) []string {
	frame := make([]string, 29)
	frame[0] = strconv.Itoa(msgContractData)
	frame[1] = strconv.Itoa(reqID)
	frame[2] = symbol
	frame[3] = secType
	frame[8] = exchange
	frame[9] = currency
	frame[10] = localSymbol
	frame[12] = tradingClass
	frame[13] = strconv.Itoa(conID)
	frame[14] = "0.01"
	if multiplier > 0 {
		frame[15] = strconv.Itoa(multiplier)
	}
	frame[21] = primary
	return frame
}
