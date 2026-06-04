package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

func TestPurgeRestorePreviewIsAllOrNone(t *testing.T) {
	t.Parallel()

	srv := newPurgeRestoreTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	aapl := purgeLedgerTestStockContract()
	msft := aapl
	msft.ConID = 272093
	msft.Symbol = "MSFT"
	seedPurgeLedgerFill(t, srv.purgeLedger, "purge-test", "leg-aapl", aapl, rpc.OrderActionSell, 1, 100)
	seedPurgeLedgerFill(t, srv.purgeLedger, "purge-test", "leg-msft", msft, rpc.OrderActionSell, 1, 200)

	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) { return nil, nil }
	srv.orderPreviewQuote = fixedPreviewQuote(99, 101)
	srv.orderPreviewWhatIf = func(_ context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		if strings.EqualFold(draft.Contract.Symbol, "MSFT") {
			return rpc.OrderWhatIfResult{
				Status:            rpc.OrderWhatIfStatusRejected,
				Available:         true,
				RequiredForSubmit: true,
				Message:           "broker rejected",
			}, nil
		}
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		t.Fatal("all-or-none restore should not reserve an order id when one leg fails WhatIf")
		return 0, nil
	}
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("all-or-none restore should not send any broker order when one leg fails WhatIf")
		return nil
	}

	res, err := srv.executePurgeRestore(context.Background(), rpc.PurgeRestoreParams{All: true, Scale: 1, WaitMs: 1})
	if err != nil {
		t.Fatalf("executePurgeRestore: %v", err)
	}
	if res.Status != purgeRestoreStatusBlocked || res.SelectedLegs != 2 || res.SubmittedLegs != 0 || len(res.Blockers) == 0 {
		t.Fatalf("restore result = %+v, want blocked all-or-none preflight", res)
	}
	rows, totals, err := srv.purgeLedger.Snapshot("", "")
	if err != nil {
		t.Fatalf("snapshot after blocked restore: %v", err)
	}
	if totals.ActiveRows != 2 || totals.RestoredQuantity != 0 || len(rows) != 2 {
		t.Fatalf("blocked restore mutated ledger rows=%+v totals=%+v", rows, totals)
	}
}

func TestPurgeRestoreExecuteRecomputesWhatIfAndSendFailureLeavesLedger(t *testing.T) {
	t.Parallel()

	srv := newPurgeRestoreTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	contract := purgeLedgerTestStockContract()
	seedPurgeLedgerFill(t, srv.purgeLedger, "purge-test", "leg-aapl", contract, rpc.OrderActionSell, 1, 100)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) { return nil, nil }
	srv.orderPreviewQuote = fixedPreviewQuote(99, 101)
	var whatIfCalls int
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		whatIfCalls++
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		return errors.New("send failed")
	}

	preview, err := srv.previewPurgeRestore(context.Background(), rpc.PurgeRestoreParams{Symbols: []string{"AAPL"}, Scale: 1})
	if err != nil {
		t.Fatalf("previewPurgeRestore: %v", err)
	}
	if preview.Status != purgeRestoreStatusPreview || preview.SelectedLegs != 1 {
		t.Fatalf("preview = %+v, want accepted preview", preview)
	}
	executed, err := srv.executePurgeRestore(context.Background(), rpc.PurgeRestoreParams{Symbols: []string{"AAPL"}, Scale: 1, WaitMs: 1})
	if err != nil {
		t.Fatalf("executePurgeRestore: %v", err)
	}
	if whatIfCalls != 2 {
		t.Fatalf("WhatIf calls = %d, want preview + fresh execute preflight", whatIfCalls)
	}
	if executed.Status != purgeRestoreStatusError || executed.SubmittedLegs != 0 || executed.ErrorLegs != 1 {
		t.Fatalf("execute result = %+v, want send error without submission", executed)
	}
	rows, totals, err := srv.purgeLedger.Snapshot("", "")
	if err != nil {
		t.Fatalf("snapshot after send failure: %v", err)
	}
	if len(rows) != 1 || rows[0].RemainingQuantity != 1 || rows[0].RestoredQuantity != 0 || totals.ActiveRows != 1 {
		t.Fatalf("send failure mutated ledger rows=%+v totals=%+v", rows, totals)
	}
}

func TestPurgeRestorePaperNeutralStockAndOption(t *testing.T) {
	t.Parallel()

	srv := newPurgeRestoreTestServer(t, config.Trading{
		Enabled:               true,
		Mode:                  config.TradingModePaper,
		AllowOptionSellToOpen: true,
		MaxNotional:           100_000,
		MaxOptionContracts:    10,
	})
	stock := purgeLedgerTestStockContract()
	option := purgeLedgerTestOptionContract()
	seedPurgeLedgerFill(t, srv.purgeLedger, "purge-neutral", "leg-aapl-stock", stock, rpc.OrderActionSell, 10, 100)
	seedPurgeLedgerFill(t, srv.purgeLedger, "purge-neutral", "leg-spy-option", option, rpc.OrderActionBuy, 2, 3.50)
	srv.purgeRefreshPositions = func() ([]*ibkrlib.RawPosition, error) { return nil, nil }
	srv.orderPreviewQuote = func(_ context.Context, contract rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		if strings.EqualFold(contract.SecType, "OPT") {
			return purgeRestoreQuote(3.00, 3.20), nil
		}
		return purgeRestoreQuote(99, 101), nil
	}
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	nextID := 1000
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		nextID++
		return nextID, nil
	}
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error { return nil }

	res, err := srv.executePurgeRestore(context.Background(), rpc.PurgeRestoreParams{All: true, Scale: 1, WaitMs: 1})
	if err != nil {
		t.Fatalf("executePurgeRestore neutral: %v", err)
	}
	if res.Status != purgeRestoreStatusSubmitted || res.SelectedLegs != 2 || res.SubmittedLegs != 2 {
		t.Fatalf("neutral execute result = %+v, want two submitted restore orders", res)
	}
	afterBySymbol := map[string]float64{}
	for _, leg := range res.Legs {
		afterBySymbol[leg.Symbol+"|"+leg.SecType] = leg.Position.After
	}
	if afterBySymbol["AAPL|STK"] != 10 || afterBySymbol["SPY|OPT"] != -2 {
		t.Fatalf("restore position impacts = %+v, want original signed sizes", afterBySymbol)
	}

	for _, order := range res.Orders {
		srv.appendOrderLifecycleEvent(ibkrlib.OrderLifecycleEvent{
			Type:         ibkrlib.OrderLifecycleEventStatus,
			OrderID:      order.ReservedOrderID,
			PermID:       order.ReservedOrderID + 9000,
			ClientID:     31,
			Status:       "Filled",
			Filled:       float64(order.Quantity),
			Remaining:    0,
			AvgFillPrice: order.LimitPrice,
		})
	}
	rows, totals, err := srv.purgeLedger.Snapshot("", "purge-neutral")
	if err != nil {
		t.Fatalf("neutral snapshot: %v", err)
	}
	if len(rows) != 2 || totals.ActiveRows != 0 || totals.RestoredRows != 2 || totals.RemainingQuantity != 0 {
		t.Fatalf("neutral ledger rows=%+v totals=%+v, want retained restored rows", rows, totals)
	}
}

func newPurgeRestoreTestServer(t *testing.T, trading config.Trading) *Server {
	t.Helper()
	srv := newPurgeExecuteTestServer(t)
	trading = trading.WithDefaults()
	srv.cfg.Trading = trading
	srv.orderWritesEnabled = func() bool { return true }
	srv.purgeLedger = newPurgeLedgerStore(filepath.Join(t.TempDir(), "purge-ledger.json"), srv.now)
	return srv
}

func seedPurgeLedgerFill(t *testing.T, store *purgeLedgerStore, purgeID, legID string, contract rpc.ContractParams, action string, quantity float64, avgFillPrice float64) {
	t.Helper()
	ev := purgeLedgerFillEvent(purgeExecuteSource, "purge-"+legID, purgeID, legID, contract, action, quantity, quantity, avgFillPrice)
	ev.Account = "DU1234567"
	if err := store.ApplyOrderFill(ev); err != nil {
		t.Fatalf("seed purge ledger %s: %v", legID, err)
	}
}

func purgeRestoreQuote(bid, ask float64) rpc.OrderQuoteSnapshot {
	mid := (bid + ask) / 2
	return rpc.OrderQuoteSnapshot{
		Bid:          &bid,
		Ask:          &ask,
		Midpoint:     &mid,
		DataType:     rpc.MarketDataLive,
		QuoteQuality: "firm",
	}
}

func purgeLedgerTestOptionContract() rpc.ContractParams {
	return rpc.ContractParams{
		ConID:        777001,
		Symbol:       "SPY",
		SecType:      "OPT",
		Exchange:     "SMART",
		Currency:     "USD",
		LocalSymbol:  "SPY  260619C00520000",
		TradingClass: "SPY",
		Expiry:       "20260619",
		Strike:       520,
		Right:        "C",
		Multiplier:   100,
	}
}
