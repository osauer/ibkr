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
