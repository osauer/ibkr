package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/discover"
	"github.com/osauer/ibkr/internal/rpc"
)

func TestPreviewLimitPriceDefaultsPatientLimit(t *testing.T) {
	t.Parallel()
	bid, ask := 100.10, 100.15
	quote := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}

	got, err := previewLimitPrice(rpc.OrderActionBuy, rpc.OrderStrategyPatientLimit, nil, quote)
	if err != nil {
		t.Fatalf("previewLimitPrice buy: %v", err)
	}
	if got != 100.12 {
		t.Fatalf("buy patient-limit = %.4f, want 100.1200", got)
	}
	got, err = previewLimitPrice(rpc.OrderActionSell, rpc.OrderStrategyPatientLimit, nil, quote)
	if err != nil {
		t.Fatalf("previewLimitPrice sell: %v", err)
	}
	if got != 100.13 {
		t.Fatalf("sell patient-limit = %.4f, want 100.1300", got)
	}
}

func TestPreviewLimitRejectsDelayedPatientLimit(t *testing.T) {
	t.Parallel()
	bid, ask := 100.10, 100.15
	quote := rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataDelayed}

	if _, err := previewLimitPrice(rpc.OrderActionBuy, rpc.OrderStrategyPatientLimit, nil, quote); err == nil {
		t.Fatal("patient-limit on delayed data should fail")
	}
}

func TestClassifyPositionEffectBlocksShortFlip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		before float64
		after  float64
		want   string
		block  bool
	}{
		{0, 10, rpc.OrderPositionEffectOpen, false},
		{10, 3, rpc.OrderPositionEffectReduce, false},
		{10, 0, rpc.OrderPositionEffectClose, false},
		{10, -2, rpc.OrderPositionEffectFlip, true},
		{0, -1, rpc.OrderPositionEffectOpenShort, true},
	}
	for _, tc := range cases {
		got := classifyPositionEffect(tc.before, tc.after)
		if got != tc.want {
			t.Fatalf("classifyPositionEffect(%v,%v) = %q, want %q", tc.before, tc.after, got, tc.want)
		}
		if stockShortOrFlip(got) != tc.block {
			t.Fatalf("stockShortOrFlip(%q) = %v, want %v", got, stockShortOrFlip(got), tc.block)
		}
	}
}

func TestOrderTokenSignerBindsDraft(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 8, 30, 0, 0, time.UTC)
	signer, err := newOrderTokenSigner(t.TempDir()+"/order-preview-key", func() time.Time { return now })
	if err != nil {
		t.Fatalf("newOrderTokenSigner: %v", err)
	}
	draft := rpc.OrderDraft{
		Action:     rpc.OrderActionBuy,
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		Quantity:   10,
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: 100.12,
		TIF:        rpc.OrderTIFDay,
		Strategy:   rpc.OrderStrategyPatientLimit,
		OrderRef:   "ibkr-20260528-083000",
	}
	token, tokenID, expiresAt, err := signer.mint(orderPreviewTokenPayload{
		Mode:         "paper",
		Account:      "DU1234567",
		Endpoint:     "127.0.0.1:4002",
		ClientID:     31,
		Draft:        draft,
		Quote:        rpc.OrderQuoteSnapshot{Symbol: "AAPL"},
		Position:     rpc.OrderPositionImpact{Before: 0, After: 10, Effect: rpc.OrderPositionEffectOpen},
		Notional:     1001.20,
		WhatIf:       previewWhatIfUnavailable(),
		WhatIfStatus: rpc.OrderWhatIfStatusUnavailable,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tokenID == "" || token == "" {
		t.Fatalf("mint returned empty token or id")
	}
	if !expiresAt.Equal(now.Add(orderPreviewTokenTTL)) {
		t.Fatalf("expiresAt = %s, want %s", expiresAt, now.Add(orderPreviewTokenTTL))
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != orderPreviewTokenPrefix {
		t.Fatalf("token shape = %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload orderPreviewTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.TokenID != tokenID || payload.Draft.Contract.Symbol != "AAPL" || payload.Draft.LimitPrice != 100.12 {
		t.Fatalf("payload did not bind draft/token: %+v", payload)
	}
	if payload.Quote.Symbol != "AAPL" || payload.Position.Effect != rpc.OrderPositionEffectOpen || payload.Notional != 1001.20 || payload.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable {
		t.Fatalf("payload did not bind preview evidence: %+v", payload)
	}
	verified, err := signer.verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.TokenID != tokenID || verified.Draft.Contract.Symbol != "AAPL" {
		t.Fatalf("verified payload mismatch: %+v", verified)
	}
}

func TestOrderTokenSignerRejectsTamperedAndExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 8, 30, 0, 0, time.UTC)
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), func() time.Time { return now })
	if err != nil {
		t.Fatalf("newOrderTokenSigner: %v", err)
	}
	token, _, _, err := signer.mint(orderPreviewTokenPayload{
		ExpiresAt:    now.Add(time.Minute),
		Mode:         "paper",
		Account:      "DU1234567",
		Endpoint:     "127.0.0.1:4002",
		ClientID:     31,
		Draft:        rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"}, Quantity: 1, OrderType: rpc.OrderTypeLMT, LimitPrice: 1, TIF: rpc.OrderTIFDay},
		WhatIf:       previewWhatIfUnavailable(),
		WhatIfStatus: rpc.OrderWhatIfStatusUnavailable,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := signer.verify(token + "x"); err == nil {
		t.Fatal("tampered token should fail verification")
	}

	expiredSigner, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), func() time.Time { return now.Add(2 * time.Minute) })
	if err != nil {
		t.Fatalf("new expired signer: %v", err)
	}
	expiredSigner.key = signer.key
	if _, err := expiredSigner.verify(token); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token err = %v, want expired", err)
	}
}

func TestOrderPreviewDisabledGateFailsBeforeMarketData(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: false, Mode: config.TradingModePaper})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("quote hook should not be called while trading is disabled")
		return rpc.OrderQuoteSnapshot{}, nil
	}

	_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:   "buy",
		Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity: 1,
	})
	if !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("previewOrder err = %v, want ErrTradingDisabled", err)
	}
}

func TestOrderPreviewPaperMintsTokenAndJournal(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	bid, ask := 100.10, 100.15
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		if c.Symbol != "AAPL" || c.Exchange != "SMART" || c.Currency != "USD" {
			t.Fatalf("contract = %+v, want SMART/USD AAPL", c)
		}
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{
			Symbol:       "AAPL",
			Bid:          &bid,
			Ask:          &ask,
			Midpoint:     &mid,
			DataType:     rpc.MarketDataLive,
			QuoteQuality: "firm",
			AsOf:         srv.now(),
		}, nil
	}
	srv.orderPreviewPositionImpact = func(_ context.Context, symbol, action string, qty int) (rpc.OrderPositionImpact, error) {
		if symbol != "AAPL" || action != rpc.OrderActionBuy || qty != 10 {
			t.Fatalf("position hook args = %s %s %d", symbol, action, qty)
		}
		return rpc.OrderPositionImpact{Before: 0, After: 10, Effect: rpc.OrderPositionEffectOpen}, nil
	}

	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:   "buy",
		Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity: 10,
	})
	if err != nil {
		t.Fatalf("previewOrder: %v", err)
	}
	if !res.TokenMinted || res.PreviewToken == "" || res.PreviewTokenID == "" {
		t.Fatalf("preview token not minted: %+v", res)
	}
	if res.SubmitEligible || res.Executable {
		t.Fatalf("unavailable WhatIf preview should not be submit eligible: %+v", res)
	}
	if res.Draft.Strategy != rpc.OrderStrategyPatientLimit || res.Draft.TIF != rpc.OrderTIFDay || res.Draft.OutsideRTH {
		t.Fatalf("unexpected defaults in draft: %+v", res.Draft)
	}
	if res.Draft.LimitPrice != 100.12 || res.Notional != 1001.20 {
		t.Fatalf("pricing/notional = %.4f %.4f, want 100.12 1001.20", res.Draft.LimitPrice, res.Notional)
	}
	if res.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable || !res.WhatIf.RequiredForSubmit {
		t.Fatalf("WhatIf = %+v, want unavailable required", res.WhatIf)
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if payload.TokenID != res.PreviewTokenID || payload.Account != "DU1234567" || payload.Endpoint != "127.0.0.1:4002" || payload.ClientID != 31 {
		t.Fatalf("token binding mismatch: %+v", payload)
	}
	if payload.Draft.LimitPrice != res.Draft.LimitPrice || payload.WhatIfStatus != rpc.OrderWhatIfStatusUnavailable {
		t.Fatalf("token draft mismatch: %+v", payload)
	}
	if payload.Quote.Symbol != "AAPL" || payload.Position.Effect != rpc.OrderPositionEffectOpen || payload.Notional != res.Notional || payload.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable {
		t.Fatalf("token preview evidence mismatch: %+v", payload)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("journal events = %d, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Type != orderJournalEventPreviewed || ev.PreviewTokenID != res.PreviewTokenID || ev.OrderRef != res.Draft.OrderRef || ev.Action != rpc.OrderActionBuy {
		t.Fatalf("journal event mismatch: %+v", ev)
	}
}

func TestOrderPreviewBindsAcceptedBrokerWhatIf(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	commission := 1.25
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{
			Status:            rpc.OrderWhatIfStatusAccepted,
			Available:         true,
			RequiredForSubmit: false,
			Margin: &rpc.OrderMarginImpact{
				Currency:           "USD",
				Commission:         &commission,
				CommissionCurrency: "USD",
			},
		}, nil
	}

	limit := 100.0
	res, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:     "buy",
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:   1,
		LimitPrice: &limit,
	})
	if err != nil {
		t.Fatalf("previewOrder: %v", err)
	}
	if res.WhatIf.Status != rpc.OrderWhatIfStatusAccepted || res.WhatIf.RequiredForSubmit {
		t.Fatalf("WhatIf = %+v, want accepted submit-ready", res.WhatIf)
	}
	if !res.TokenMinted || !res.SubmitEligible || !res.Executable {
		t.Fatalf("accepted WhatIf preview should be submit eligible: %+v", res)
	}
	for _, w := range res.Warnings {
		if w.Code == "broker_what_if_unavailable" {
			t.Fatalf("accepted WhatIf should not carry unavailable warning: %+v", res.Warnings)
		}
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if payload.WhatIf.Status != rpc.OrderWhatIfStatusAccepted || payload.WhatIf.Margin == nil || payload.WhatIf.Margin.Commission == nil || *payload.WhatIf.Margin.Commission != commission {
		t.Fatalf("token did not bind accepted WhatIf margin: %+v", payload.WhatIf)
	}
}

func TestConfirmPreviewTokenForPlaceRequiresAcceptedWhatIf(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	token := mintPreviewTokenForConfirmTest(t, srv, previewWhatIfUnavailable())

	_, err := srv.confirmPreviewTokenForPlace(token)
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "accepted broker WhatIf") {
		t.Fatalf("confirmPreviewTokenForPlace err = %v, want accepted WhatIf blocker", err)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("journal events = %+v, want none on rejected confirmation", events)
	}
}

func TestConfirmPreviewTokenForPlaceIsSingleUse(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:            rpc.OrderWhatIfStatusAccepted,
		Available:         true,
		RequiredForSubmit: false,
	})

	payload, err := srv.confirmPreviewTokenForPlace(token)
	if err != nil {
		t.Fatalf("confirmPreviewTokenForPlace first use: %v", err)
	}
	if payload.TokenID == "" || payload.Draft.Contract.Symbol == "" {
		t.Fatalf("confirmed payload missing token/draft: %+v", payload)
	}
	_, err = srv.confirmPreviewTokenForPlace(token)
	if !errors.Is(err, ErrTradingDisabled) || !errors.Is(err, errOrderPreviewTokenAlreadyUsed) {
		t.Fatalf("confirmPreviewTokenForPlace second use err = %v, want token-used blocker", err)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != orderJournalEventTokenConfirmed || events[0].PreviewTokenID != payload.TokenID {
		t.Fatalf("journal events = %+v, want one token-confirmed event", events)
	}
}

func TestConfirmPreviewTokenForPlaceRejectsGateMismatch(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:            rpc.OrderWhatIfStatusAccepted,
		Available:         true,
		RequiredForSubmit: false,
	})
	srv.endpoint.Account = "DU7654321"

	_, err := srv.confirmPreviewTokenForPlace(token)
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "preview token was minted for") {
		t.Fatalf("confirmPreviewTokenForPlace err = %v, want gate mismatch", err)
	}
}

func TestOrderPreviewRejectsMaxNotional(t *testing.T) {
	t.Parallel()
	tr := config.Trading{Enabled: true, Mode: config.TradingModePaper, MaxNotional: 500}
	srv := newOrderPreviewTestServer(t, tr)
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 6, rpc.OrderPositionEffectOpen)

	limit := 100.0
	_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:     "buy",
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:   6,
		LimitPrice: &limit,
	})
	var bad *badRequestError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), "max_notional") {
		t.Fatalf("previewOrder err = %v, want max_notional bad request", err)
	}
}

func TestOrderPreviewRejectsStockFlipUnlessEnabled(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, -1, rpc.OrderPositionEffectFlip)

	limit := 100.0
	_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action:     "sell",
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		Quantity:   2,
		LimitPrice: &limit,
	})
	var bad *badRequestError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), "allow_stock_short") {
		t.Fatalf("previewOrder err = %v, want stock short/flip bad request", err)
	}
}

func TestOrderPreviewRejectsOptionsBeforeQuote(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Enabled: true, Mode: config.TradingModePaper})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("quote hook should not be called for option preview rejection")
		return rpc.OrderQuoteSnapshot{}, nil
	}

	limit := 2.10
	_, err := srv.previewOrder(context.Background(), rpc.OrderPreviewParams{
		Action: "buy",
		Contract: rpc.ContractParams{
			Symbol: "SPY", SecType: "OPT", Expiry: "20260619", Right: "C", Strike: 520,
		},
		Quantity:   1,
		LimitPrice: &limit,
	})
	var bad *badRequestError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), "single-leg options remain disabled") {
		t.Fatalf("previewOrder err = %v, want option-disabled bad request", err)
	}
}

func newOrderPreviewTestServer(t *testing.T, trading config.Trading) *Server {
	t.Helper()
	now := time.Date(2026, 5, 28, 8, 45, 0, 0, time.UTC)
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), func() time.Time { return now })
	if err != nil {
		t.Fatalf("newOrderTokenSigner: %v", err)
	}
	trading = trading.WithDefaults()
	return &Server{
		cfg: &config.Resolved{
			Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(31), Account: "DU1234567"},
			Trading: trading,
		},
		endpoint:     discover.Endpoint{Host: "127.0.0.1", Port: 4002, ClientID: 31, Account: "DU1234567", PortOrigin: discover.OriginPinned},
		now:          func() time.Time { return now },
		orderJournal: newOrderJournalStore(filepath.Join(t.TempDir(), "order-journal.jsonl")),
		orderTokens:  signer,
	}
}

func fixedPreviewQuote(bid, ask float64) func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
	return func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{
			Symbol:       "AAPL",
			Bid:          &bid,
			Ask:          &ask,
			Midpoint:     &mid,
			DataType:     rpc.MarketDataLive,
			QuoteQuality: "firm",
		}, nil
	}
}

func fixedPreviewPosition(before, after float64, effect string) func(context.Context, string, string, int) (rpc.OrderPositionImpact, error) {
	return func(context.Context, string, string, int) (rpc.OrderPositionImpact, error) {
		return rpc.OrderPositionImpact{Before: before, After: after, Effect: effect}, nil
	}
}

func mintPreviewTokenForConfirmTest(t *testing.T, srv *Server, whatIf rpc.OrderWhatIfResult) string {
	t.Helper()
	if whatIf.Status == "" {
		whatIf = previewWhatIfUnavailable()
	}
	draft := rpc.OrderDraft{
		Action:     rpc.OrderActionBuy,
		Contract:   rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		Quantity:   1,
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: 100,
		TIF:        rpc.OrderTIFDay,
		Strategy:   rpc.OrderStrategyExplicitLimit,
		OrderRef:   "ibkr-20260528-084500",
	}
	token, _, _, err := srv.orderTokens.mint(orderPreviewTokenPayload{
		Mode:     "paper",
		Account:  "DU1234567",
		Endpoint: "127.0.0.1:4002",
		ClientID: 31,
		Draft:    draft,
		Quote:    rpc.OrderQuoteSnapshot{Symbol: "AAPL"},
		Position: rpc.OrderPositionImpact{
			Before: 0,
			After:  1,
			Effect: rpc.OrderPositionEffectOpen,
		},
		Notional:     100,
		WhatIf:       whatIf,
		WhatIfStatus: whatIf.Status,
	})
	if err != nil {
		t.Fatalf("mint preview token: %v", err)
	}
	return token
}
