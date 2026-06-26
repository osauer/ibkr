package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestRunProposalsGroupHelp(t *testing.T) {
	t.Parallel()
	for _, help := range []string{"--help", "-h", "-help", "help"} {
		t.Run(help, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &stderr}
			if code := Run(context.Background(), env, "proposals", []string{help}); code != 0 {
				t.Fatalf("Run(proposals, %s)=%d, want 0", help, code)
			}
			got := stdout.String()
			for _, want := range []string{
				"ibkr proposals",
				"Daemon-owned close/reduce-only protection proposals",
				"status|refresh|list|preview|submit|ignore",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("proposals help missing %q:\n%s", want, got)
				}
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q, want empty", stderr.String())
			}
		})
	}
}

func TestRenderProposalsTextShowsPositionContext(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	thetaPct, thetaDay := 1.2, 1719.79
	groupPct, groupDay, groupDayPct := 34.2, 14227.98, 19.2
	renderProposalsText(env, &rpc.TradeProposalSnapshot{
		Revision: "sha256:test", PolicyID: "default", PolicyVersion: 3,
		Counts: rpc.TradeProposalCounts{Total: 2, Actionable: 2},
		Proposals: []rpc.TradeProposal{
			{
				Key: "theta_hygiene:NOW", Bucket: rpc.TradeProposalBucketThetaHygiene,
				Action: rpc.OrderActionSell, Quantity: 20, PositionQuantity: 20, Symbol: "NOW",
				SecType: "OPT", OrderType: rpc.OrderTypeLMT, Reason: "time value bleed",
				Contract:               rpc.ContractParams{Currency: "USD"},
				PositionMarketValue:    2919.79,
				MarketValuePctNLV:      &thetaPct,
				PositionDayChangeMoney: &thetaDay, PositionDayChangeCurrency: "USD",
			},
			{
				Key: "risk_reduction:NOW", Bucket: rpc.TradeProposalBucketRiskReduction,
				Action: rpc.OrderActionSell, Quantity: 103, PositionQuantity: 400, Symbol: "NOW",
				SecType: "STK", OrderType: rpc.OrderTypeLMT, Reason: "34% of NLV",
				Contract:               rpc.ContractParams{Currency: "USD"},
				PositionMarketValue:    84501.67,
				MarketValuePctNLV:      &groupPct,
				PositionDayChangeMoney: &groupDay, PositionDayChangeCurrency: "EUR",
				PositionDayChangePct: &groupDayPct,
			},
		},
	})
	got := stdout.String()
	for _, want := range []string{
		"held 20 ct",  // theta shows the exact contract count
		"(1.2% NLV)",  // exposure context
		"today +$",    // today's move carries an explicit sign
		"(34.2% NLV)", // risk-reduction group exposure
		"today +€",    // group P&L reported in base currency
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("position context missing %q:\n%s", want, got)
		}
	}
	// Risk reduction acts on a group; a single leg's share count would mislead.
	if strings.Contains(got, "held 400 sh") {
		t.Fatalf("risk reduction must not print a misleading leg share count:\n%s", got)
	}
}

func TestRenderProposalsTextShowsTrailSizingFallback(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	trailAmount, ref, stop, distance, distancePct, loss, gapPrice, gapLoss, ladderStop := 10.0, 100.0, 90.0, 10.0, 10.0, 2000.0, 85.5, 2900.0, 95.0
	renderProposalsText(env, &rpc.TradeProposalSnapshot{
		Revision:      "sha256:test",
		PolicyID:      "default",
		PolicyVersion: 1,
		Counts:        rpc.TradeProposalCounts{Total: 1, Actionable: 1},
		Proposals: []rpc.TradeProposal{{
			Key:       "trailing_stop:PBLS",
			Bucket:    rpc.TradeProposalBucketTrailingStop,
			Action:    rpc.OrderActionSell,
			Quantity:  200,
			Symbol:    "PBLS",
			OrderType: rpc.OrderTypeTRAIL,
			TIF:       rpc.OrderTIFGTC,
			Reason:    "broker-side trailing stop",
			Trail: &rpc.OrderTrailSpec{
				OffsetType:       rpc.OrderTrailOffsetAmount,
				TrailingAmount:   &trailAmount,
				InitialStopPrice: stop,
			},
			TrailSizing: &rpc.TradeProposalTrailSizing{
				Fallback:          true,
				ChosenPct:         10,
				PolicyFallbackPct: 10,
				PolicyMinPct:      6,
				PolicyMaxPct:      15,
			},
			ExecutionSemantics: &rpc.TradeProposalExecutionSemantics{
				ReferenceSide:      "bid",
				ReferencePrice:     &ref,
				TriggerMethod:      rpc.OrderTriggerMethodLast,
				TriggerMethodLabel: "last",
				TriggerEffect:      "market_order_when_triggered",
				PriceGuarantee:     "stop_price_is_not_execution_price",
			},
			StopRisk: &rpc.TradeProposalStopRisk{
				ReferencePrice: &ref,
				StopPrice:      &stop,
				Distance:       &distance,
				DistancePct:    &distancePct,
				EstimatedLoss:  &loss,
				Currency:       "USD",
				GapScenario: &rpc.TradeProposalStopRiskGap{
					GapPct:                5,
					AssumedExecutionPrice: &gapPrice,
					EstimatedLoss:         &gapLoss,
				},
			},
			StopLadder: []rpc.TradeProposalStopLadderStep{{
				Label:     "5%",
				StopPrice: &ladderStop,
			}},
		}},
	})

	got := stdout.String()
	for _, want := range []string{
		"Trail sizing:",
		"10.0% fallback trail used",
		"dynamic stop unavailable",
		"Risk ticket",
		"stop 90.00",
		"trigger last on bid 100.00",
		"Estimated loss",
		"$ 2,000.00",
		"Gap scenario",
		"5.0% beyond stop at $ 85.50",
		"Stop ladder",
		"TRAIL converts to a market order",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered proposals missing %q:\n%s", want, got)
		}
	}
}
