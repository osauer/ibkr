package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestRenderOrderPreviewShowsTokenAndSubmitEligibility(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}

	renderOrderPreviewText(env, &rpc.OrderPreviewResult{
		PreviewToken:          "ibkrp1.body.sig",
		PreviewTokenID:        "tok_123",
		PreviewTokenExpiresAt: time.Date(2026, 5, 30, 9, 30, 0, 0, time.UTC),
		TokenMinted:           true,
		SubmitEligible:        false,
		Executable:            false,
		Mode:                  "paper",
		Account:               "DU1234567",
		Endpoint:              "127.0.0.1:4002",
		ClientID:              31,
		Draft: rpc.OrderDraft{
			Action:     rpc.OrderActionBuy,
			Contract:   rpc.ContractParams{Symbol: "AAPL"},
			Quantity:   10,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: 100.12,
			TIF:        rpc.OrderTIFDay,
			Strategy:   rpc.OrderStrategyPatientLimit,
		},
		Quote: rpc.OrderQuoteSnapshot{
			Symbol:       "AAPL",
			DataType:     rpc.MarketDataLive,
			QuoteQuality: "firm",
		},
		Position: rpc.OrderPositionImpact{Before: 0, After: 10, Effect: rpc.OrderPositionEffectOpen},
		Notional: 1001.20,
		WhatIf: rpc.OrderWhatIfResult{
			Status:            rpc.OrderWhatIfStatusUnavailable,
			RequiredForSubmit: true,
		},
	})

	got := stdout.String()
	for _, want := range []string{
		"Token minted   true",
		"Submit eligible false",
		"WhatIf         unavailable (required=true)",
		"Token ID       tok_123",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("order preview missing %q:\n%s", want, got)
		}
	}
}

func TestPreviewCLIOrderTypeAndTrailDraftSummary(t *testing.T) {
	t.Parallel()
	got, err := previewCLIOrderType("", true, true)
	if err != nil {
		t.Fatalf("previewCLIOrderType: %v", err)
	}
	if got != rpc.OrderTypeTRAILLIMIT {
		t.Fatalf("default trail-limit order type = %q, want TRAIL LIMIT", got)
	}
	got, err = previewCLIOrderType("trail-limit", false, false)
	if err != nil {
		t.Fatalf("previewCLIOrderType trail-limit: %v", err)
	}
	if got != rpc.OrderTypeTRAILLIMIT {
		t.Fatalf("normalized trail-limit order type = %q, want TRAIL LIMIT", got)
	}
	if _, err := previewCLIOrderType("LMT", true, false); err == nil || !strings.Contains(err.Error(), "cannot include trail") {
		t.Fatalf("previewCLIOrderType LMT+trail err = %v, want contradiction", err)
	}

	pct, offset := 2.0, 0.05
	summary := formatOrderDraftSummary(rpc.OrderDraft{
		Action:    rpc.OrderActionSell,
		Contract:  rpc.ContractParams{Symbol: "SPY"},
		Quantity:  10,
		OrderType: rpc.OrderTypeTRAILLIMIT,
		TIF:       rpc.OrderTIFDay,
		Trail: &rpc.OrderTrailSpec{
			TrailingPercent:  &pct,
			InitialStopPrice: 98,
			LimitOffset:      &offset,
		},
	})
	for _, want := range []string{"SELL 10 SPY TRAIL LIMIT", "trail 2%", "stop 98.0000", "limit_offset 0.0500"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("draft summary %q missing %q", summary, want)
		}
	}
}

func TestHoistFlagsKeepsReplaceOrderValue(t *testing.T) {
	t.Parallel()
	got := hoistFlags([]string{"preview", "--replace-order", "6", "--market", "de", "buy", "MBG", "1"})
	want := []string{"--replace-order", "6", "--market", "de", "preview", "buy", "MBG", "1"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("hoistFlags = %#v, want %#v", got, want)
	}
}
