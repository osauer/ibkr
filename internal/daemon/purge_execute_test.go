package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/config"
	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

func TestPurgeExecuteUsesFreshStockQuantity(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{{
			Contract: ibkrlib.Contract{ConID: 111, Symbol: "SAP", SecType: "STK", Exchange: "IBIS", Currency: "EUR", Multiplier: 100},
			Position: 1,
		}}, nil
	}
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		if c.Exchange != "SMART" || c.PrimaryExch != "IBIS" || c.Currency != "EUR" || c.Multiplier != 1 {
			t.Fatalf("quote contract = %+v, want SMART/IBIS EUR stock multiplier 1", c)
		}
		bid, ask := 100.0, 101.0
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}, nil
	}
	var sent *ibkrlib.RawOrder
	var sentContract *ibkrlib.Contract
	srv.orderPlaceBroker = func(_ context.Context, contract *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		contractCopy := *contract
		sentContract = &contractCopy
		copy := *order
		sent = &copy
		return nil
	}

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{
		PurgeID: "purge_test",
		All:     true,
		WaitMs:  1,
	})
	if err != nil {
		t.Fatalf("executePurge: %v", err)
	}
	if res.Status != purgeExecuteStatusSubmitted || res.SelectedLegs != 1 || res.SubmittedLegs != 1 || res.ErrorLegs != 0 {
		t.Fatalf("result = %+v, want submitted", res)
	}
	if sent == nil || sent.TotalQty != 1 || sent.Action != rpc.OrderActionSell || sent.OpenClose != "C" {
		t.Fatalf("sent order = %+v, want fresh qty 1 sell close", sent)
	}
	if sentContract == nil || sentContract.Exchange != "SMART" || sentContract.PrimaryExch != "IBIS" || sentContract.Multiplier != 0 {
		t.Fatalf("sent stock contract = %+v, want SMART/IBIS with omitted multiplier", sentContract)
	}
	if got := res.Orders[0].Contract.Multiplier; got != 1 {
		t.Fatalf("purge result stock multiplier = %d, want 1", got)
	}
	if got := res.Orders[0].Contract.Exchange; got != "SMART" {
		t.Fatalf("purge result stock exchange = %q, want SMART", got)
	}
	if got := res.Orders[0].Contract.PrimaryExch; got != "IBIS" {
		t.Fatalf("purge result stock primary exchange = %q, want IBIS", got)
	}
}

func TestPurgeExecuteBlockersUseBrokerWriteAuthorization(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	blockers := srv.purgeExecuteBlockers(rpc.TradingStatus{Mode: config.TradingModeLive,
		CanPreview: true,
		CanWrite:   false,
		Account:    "U1234567",
		Endpoint:   "127.0.0.1:4001",
		ClientID:   31,
		Blockers: []rpc.TradingBlocker{{
			Code:    "gateway_port_unpinned",
			Message: "order submission requires a pinned gateway port",
			Action:  "Set [gateway].port.",
		}},
	})
	if !hasBlocker(blockers, "gateway_port_unpinned") {
		t.Fatalf("blockers = %+v, want gateway_port_unpinned", blockers)
	}
	if hasBlocker(blockers, "paper_writes_only") {
		t.Fatalf("blockers = %+v, did not expect a paper-only gate", blockers)
	}
}

func TestPurgeExecuteFlatWhenNoCurrentPositionsMatch(t *testing.T) {
	t.Parallel()
	flat := newPurgeExecuteTestServer(t)
	flat.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) { return nil, nil }
	res, err := flat.executePurge(context.Background(), rpc.PurgeExecuteParams{
		PurgeID: "purge_test",
		Symbols: []string{"AAPL"},
		WaitMs:  1,
	})
	if err != nil {
		t.Fatalf("flat executePurge: %v", err)
	}
	if res.Status != purgeExecuteStatusFlat || res.SelectedLegs != 0 || len(res.Skipped) != 0 {
		t.Fatalf("flat result = %+v", res)
	}
}

func TestPurgeExecuteSelectsOnlyCurrentTargetSymbol(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{
			{
				Contract: ibkrlib.Contract{Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD", Multiplier: 1},
				Position: 2,
			},
			{
				Contract: ibkrlib.Contract{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD", Multiplier: 1},
				Position: 4,
			},
		}, nil
	}
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	var sent int
	srv.orderPlaceBroker = func(_ context.Context, contract *ibkrlib.Contract, _ *ibkrlib.RawOrder) error {
		sent++
		if contract.Symbol != "MSFT" {
			t.Fatalf("sent contract = %+v, want only MSFT", contract)
		}
		return nil
	}

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{
		PurgeID: "purge_test",
		Symbols: []string{"MSFT"},
		WaitMs:  1,
	})
	if err != nil {
		t.Fatalf("executePurge target: %v", err)
	}
	if res.Status != purgeExecuteStatusSubmitted || res.SelectedLegs != 1 || sent != 1 {
		t.Fatalf("target result = %+v, sent=%d", res, sent)
	}
}

func TestPurgeExecuteMatchesOptionByConID(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{{
			Contract: ibkrlib.Contract{ConID: 222, Symbol: "SPY", SecType: "OPT", Expiry: "20260619", Strike: 520, Right: "C", Exchange: "SMART", Currency: "USD", LocalSymbol: "SPY  260619C00520000", TradingClass: "SPY", Multiplier: 100},
			Position: -2,
		}}, nil
	}
	bid := 2.05
	ask := 2.15
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		if c.ConID != 222 || c.SecType != "OPT" || c.Expiry != "20260619" || c.Right != "C" || c.Multiplier != 100 {
			t.Fatalf("quote contract = %+v, want exact option identity", c)
		}
		return rpc.OrderQuoteSnapshot{Symbol: "SPY_20260619C520", Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}, nil
	}
	var sentContract *ibkrlib.Contract
	var sentOrder *ibkrlib.RawOrder
	srv.orderPlaceBroker = func(_ context.Context, contract *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		contractCopy := *contract
		orderCopy := *order
		sentContract = &contractCopy
		sentOrder = &orderCopy
		return nil
	}

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{
		PurgeID: "purge_test",
		All:     true,
		WaitMs:  1,
	})
	if err != nil {
		t.Fatalf("executePurge option: %v", err)
	}
	if res.Status != purgeExecuteStatusSubmitted || sentOrder == nil || sentOrder.TotalQty != 2 || sentOrder.Action != rpc.OrderActionBuy || sentOrder.OpenClose != "C" {
		t.Fatalf("option result/order = %+v / %+v", res, sentOrder)
	}
	if sentContract == nil || sentContract.ConID != 222 || sentContract.SecType != "OPT" || sentContract.Expiry != "20260619" || sentContract.Multiplier != 100 {
		t.Fatalf("sent contract = %+v, want exact option contract", sentContract)
	}
}

func TestPurgeAggressiveLimitAlignsToObservedStockQuoteGrid(t *testing.T) {
	t.Parallel()

	bid, ask := 159.56, 159.58
	sell, err := purgeAggressiveLimit(rpc.OrderActionSell, rpc.ContractParams{Symbol: "SAP", SecType: "STK"}, rpc.OrderQuoteSnapshot{
		Bid:      &bid,
		Ask:      &ask,
		DataType: rpc.MarketDataLive,
	})
	if err != nil {
		t.Fatalf("purgeAggressiveLimit SAP sell: %v", err)
	}
	if sell != 159.52 {
		t.Fatalf("SAP sell limit = %.4f, want 159.5200", sell)
	}

	bid, ask = 47.62, 47.625
	buy, err := purgeAggressiveLimit(rpc.OrderActionBuy, rpc.ContractParams{Symbol: "MBG", SecType: "STK"}, rpc.OrderQuoteSnapshot{
		Bid:      &bid,
		Ask:      &ask,
		DataType: rpc.MarketDataLive,
	})
	if err != nil {
		t.Fatalf("purgeAggressiveLimit MBG buy: %v", err)
	}
	if buy != 47.635 {
		t.Fatalf("MBG buy limit = %.4f, want 47.6350", buy)
	}
}

func TestPurgeExecuteIsIdempotentWithOpenPurgeOrder(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	contract := purgeLedgerTestStockContract()
	legID := purgeLegIDForContract(contract)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{{
			Contract: ibkrlib.Contract{ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType, Exchange: contract.Exchange, Currency: contract.Currency, Multiplier: contract.Multiplier},
			Position: 2,
		}}, nil
	}
	if err := srv.orderJournal.Append(purgeLedgerEventWithContract(orderJournalEvent{
		At:              srv.orderNow(),
		Type:            orderJournalEventSendAttempted,
		Source:          purgeExecuteSource,
		PurgeID:         "purge-test",
		LegID:           legID,
		OrderRef:        "purge-open",
		ReservedOrderID: 1001,
		ClientID:        31,
		Account:         "DU1234567",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        2,
		LimitPrice:      99,
		SendState:       orderSendStateSendAttempted,
	}, contract)); err != nil {
		t.Fatalf("append open purge order: %v", err)
	}
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("retry with an open purge order must not fetch quotes or reserve/send")
		return rpc.OrderQuoteSnapshot{}, nil
	}

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{All: true, WaitMs: 1})
	if err != nil {
		t.Fatalf("executePurge retry: %v", err)
	}
	if res.Status != purgeExecuteStatusFlat || res.SubmittedLegs != 0 || len(res.Skipped) != 1 {
		t.Fatalf("retry result = %+v, want idempotent flat skip", res)
	}
	if res.Skipped[0].Reason != "open purge/restore order exists for this ledger row" {
		t.Fatalf("skip reason = %q", res.Skipped[0].Reason)
	}
}

func TestPurgeExecuteIsIdempotentWhenActiveLedgerCoversStalePosition(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	contract := purgeLedgerTestStockContract()
	seedPurgeLedgerFill(t, srv.purgeLedger, "purge-test", purgeLegacyLegIDForContract(contract), contract, rpc.OrderActionSell, 2, 100)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{{
			Contract: ibkrlib.Contract{ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType, Exchange: contract.Exchange, Currency: contract.Currency, Multiplier: contract.Multiplier},
			Position: 2,
		}}, nil
	}
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("stale position already covered by active purge ledger must not fetch quotes or reserve/send")
		return rpc.OrderQuoteSnapshot{}, nil
	}

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{All: true, WaitMs: 1})
	if err != nil {
		t.Fatalf("executePurge covered stale position: %v", err)
	}
	if res.Status != purgeExecuteStatusFlat || res.SubmittedLegs != 0 || len(res.Skipped) != 1 {
		t.Fatalf("covered result = %+v, want idempotent flat skip", res)
	}
	if res.Skipped[0].Reason != "current quantity already covered by active purge ledger" {
		t.Fatalf("skip reason = %q", res.Skipped[0].Reason)
	}
}

func TestPurgeExecuteSubtractsActiveLedgerCoverage(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	contract := purgeLedgerTestStockContract()
	seedPurgeLedgerFill(t, srv.purgeLedger, "purge-test", purgeLegIDForContract(contract), contract, rpc.OrderActionSell, 2, 100)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{{
			Contract: ibkrlib.Contract{ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType, Exchange: contract.Exchange, Currency: contract.Currency, Multiplier: contract.Multiplier},
			Position: 5,
		}}, nil
	}
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	var sentQty int
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		sentQty = order.TotalQty
		return nil
	}

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{All: true, WaitMs: 1})
	if err != nil {
		t.Fatalf("executePurge uncovered remainder: %v", err)
	}
	if res.Status != purgeExecuteStatusSubmitted || res.SubmittedLegs != 1 || sentQty != 3 {
		t.Fatalf("uncovered result = %+v sentQty=%d, want only 3 uncovered shares submitted", res, sentQty)
	}
}

func TestPurgeOptionFallbackIdentityRequiresMultiplier(t *testing.T) {
	t.Parallel()
	want := purgeLedgerTestOptionContract()
	want.ConID = 0
	mini := want
	mini.Multiplier = 10
	pos := ibkrlib.RawPosition{
		Contract: ibkrlib.Contract{
			Symbol:       mini.Symbol,
			SecType:      mini.SecType,
			Expiry:       mini.Expiry,
			Strike:       mini.Strike,
			Right:        mini.Right,
			Exchange:     mini.Exchange,
			Currency:     mini.Currency,
			LocalSymbol:  mini.LocalSymbol,
			TradingClass: mini.TradingClass,
			Multiplier:   mini.Multiplier,
		},
		Position: 1,
	}
	leg := rpc.PurgeExecuteLeg{Symbol: want.Symbol, SecType: want.SecType, Contract: want, OriginalSide: purgeOriginalSideLong}
	if purgePositionMatchesLeg(pos, leg) {
		t.Fatalf("standard option leg matched mini-option position without ConID")
	}
	if qty := positionQuantityForContract([]*ibkrlib.RawPosition{&pos}, want); qty != 0 {
		t.Fatalf("positionQuantityForContract mini mismatch = %.2f, want 0", qty)
	}
	pos.Contract.Multiplier = want.Multiplier
	if !purgePositionMatchesLeg(pos, leg) {
		t.Fatalf("matching option multiplier did not match without ConID")
	}
	if qty := positionQuantityForContract([]*ibkrlib.RawPosition{&pos}, want); qty != 1 {
		t.Fatalf("positionQuantityForContract matching multiplier = %.2f, want 1", qty)
	}
}

func TestPurgeLegIDIncludesOptionMultiplier(t *testing.T) {
	t.Parallel()
	standard := purgeLedgerTestOptionContract()
	standard.ConID = 0
	mini := standard
	mini.Multiplier = 10
	if purgeLegIDForContract(standard) == purgeLegIDForContract(mini) {
		t.Fatalf("option leg id should include multiplier when ConID is unavailable")
	}
}

func newPurgeExecuteTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderWritesEnabled = func() bool { return true }
	srv.purgeLedger = newPurgeLedgerStore(filepath.Join(t.TempDir(), "purge-ledger.json"), srv.now)
	nextID := 1000
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		nextID++
		return nextID, nil
	}
	return srv
}

func TestPurgeExecuteSkipsStaleAndClosedSessionQuotes(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{
			{Contract: ibkrlib.Contract{ConID: 111, Symbol: "AAA", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Position: 1},
			{Contract: ibkrlib.Contract{ConID: 222, Symbol: "BBB", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Position: 2},
		}, nil
	}
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 100.0, 101.0
		if c.Symbol == "AAA" {
			return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, Stale: true, StaleReason: "no tick in 120s"}, nil
		}
		// Empty DataType passes rpc.IsLiveDataType; the session context is
		// the only honest signal that this book is unpriceable.
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, SessionContext: &rpc.MarketSession{Market: "us_equities", IsOpen: false, Reason: "weekend"}}, nil
	}
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("no broker order may be sent off a stale or session-closed quote")
		return nil
	}

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{All: true, WaitMs: 1})
	if err != nil {
		t.Fatalf("executePurge: %v", err)
	}
	if res.Status != purgeExecuteStatusBlocked || res.SubmittedLegs != 0 || res.SkippedLegs != 2 || res.ErrorLegs != 0 {
		t.Fatalf("result = %+v, want blocked with 2 skipped legs", res)
	}
	reasons := map[string]string{}
	for _, skipped := range res.Skipped {
		reasons[skipped.Symbol] = skipped.Reason
	}
	if reasons["AAA"] != "quote is stale: no tick in 120s" {
		t.Fatalf("AAA skip reason = %q, want stale reason", reasons["AAA"])
	}
	if reasons["BBB"] != "market session is closed: weekend" {
		t.Fatalf("BBB skip reason = %q, want session-closed reason", reasons["BBB"])
	}
}

func TestPurgeExecutePrefetchesQuotesWithBoundedFanOut(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	const legCount = 12
	positions := make([]*ibkrlib.RawPosition, 0, legCount)
	for i := range legCount {
		positions = append(positions, &ibkrlib.RawPosition{
			Contract: ibkrlib.Contract{ConID: 1000 + i, Symbol: fmt.Sprintf("SYM%02d", i), SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Position: 1,
		})
	}
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) { return positions, nil }
	var inFlight, maxInFlight, calls atomic.Int32
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		calls.Add(1)
		cur := inFlight.Add(1)
		for {
			seen := maxInFlight.Load()
			if cur <= seen || maxInFlight.CompareAndSwap(seen, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		bid, ask := 100.0, 101.0
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}, nil
	}
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error { return nil }

	res, err := srv.executePurge(context.Background(), rpc.PurgeExecuteParams{All: true, WaitMs: 1})
	if err != nil {
		t.Fatalf("executePurge: %v", err)
	}
	if res.Status != purgeExecuteStatusSubmitted || res.SubmittedLegs != legCount {
		t.Fatalf("result = %+v, want %d submitted legs", res, legCount)
	}
	if got := calls.Load(); got != legCount {
		t.Fatalf("quote fetches = %d, want exactly %d (one prefetch per leg, no per-leg refetch)", got, legCount)
	}
	if got := maxInFlight.Load(); got < 2 || got > purgeQuoteWorkers {
		t.Fatalf("max concurrent quote fetches = %d, want 2..%d", got, purgeQuoteWorkers)
	}
}
