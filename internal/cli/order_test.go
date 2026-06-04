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
