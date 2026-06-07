package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

func TestPurgeExecuteUsesFreshStockQuantity(t *testing.T) {
	t.Parallel()
	srv := newPurgeExecuteTestServer(t)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{{
			Contract: ibkrlib.Contract{ConID: 111, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD", Multiplier: 1},
			Position: 1,
		}}, nil
	}
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	var sent *ibkrlib.RawOrder
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
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
			Code:    "live_not_allowed",
			Message: "live trading requires an explicit local override",
			Action:  "Set [trading].allow_live = true.",
		}},
	})
	if !hasBlocker(blockers, "live_not_allowed") {
		t.Fatalf("blockers = %+v, want live_not_allowed", blockers)
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

func newPurgeExecuteTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderWritesEnabled = func() bool { return true }
	nextID := 1000
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		nextID++
		return nextID, nil
	}
	return srv
}
