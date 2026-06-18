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
			Reason:    "broker-side trailing stop",
			TrailSizing: &rpc.TradeProposalTrailSizing{
				Fallback:          true,
				ChosenPct:         10,
				PolicyFallbackPct: 10,
				PolicyMinPct:      6,
				PolicyMaxPct:      15,
			},
		}},
	})

	got := stdout.String()
	for _, want := range []string{
		"Trail sizing:",
		"10.0% fallback trail used",
		"dynamic stop unavailable",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered proposals missing %q:\n%s", want, got)
		}
	}
}
