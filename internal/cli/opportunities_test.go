package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestOpportunitiesSubcommandIndexFindsHoistedSubcommand(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{args: []string{"--json", "list"}, want: 1},
		{args: []string{"preview", "--quantity", "1", "key", "rev"}, want: 0},
		{args: []string{"--timeout", "5s", "exercise", "key", "rev"}, want: 2},
		{args: []string{"--json"}, want: -1},
	}
	for _, tc := range cases {
		if got := opportunitiesSubcommandIndex(tc.args); got != tc.want {
			t.Fatalf("opportunitiesSubcommandIndex(%v)=%d, want %d", tc.args, got, tc.want)
		}
	}
}

func TestRenderOpportunitiesText(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderOpportunitiesText(env, &rpc.OpportunitySnapshot{
		Revision:      "sha256:rev",
		PolicyID:      "opportunity-option-exercise-mvp",
		PolicyVersion: 1,
		Counts: rpc.OpportunityCounts{
			Total:                1,
			Actionable:           1,
			ExpectedGain:         100,
			ExpectedGainCurrency: "USD",
		},
		Opportunities: []rpc.Opportunity{{
			Key:                  "option_exercise:key",
			Bucket:               rpc.OpportunityBucketOptionExercise,
			Action:               rpc.OpportunityActionExercise,
			Quantity:             1,
			Symbol:               "AAPL",
			PositionEffect:       rpc.ExercisePositionEffectClose,
			ExpectedGain:         100,
			ExpectedGainCurrency: "USD",
			Contract:             rpc.ContractParams{Right: "C", Strike: 100},
			Details:              []string{"intrinsic 300.00 USD"},
		}},
	})
	got := stdout.String()
	for _, want := range []string{
		"IBKR Opportunities  1 actionable / 1 total",
		"Expected gain",
		"option_exercise:key",
		"gain=$ 100.00",
		"intrinsic 300.00 USD",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("opportunities render missing %q:\n%s", want, got)
		}
	}
}

func TestRenderOpportunityPreviewText(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderOpportunityPreviewText(env, &rpc.OpportunityExercisePreviewResult{
		Accepted:       true,
		SubmitEligible: false,
		PreviewTokenID: "opprev-1",
		AsOf:           time.Now(),
		Opportunity: rpc.Opportunity{
			Key:                      "option_exercise:key",
			Action:                   rpc.OpportunityActionExercise,
			Quantity:                 1,
			Symbol:                   "AAPL",
			Contract:                 rpc.ContractParams{Right: "C", Strike: 100, Expiry: "20260619"},
			ExpectedGain:             100,
			ExpectedGainCurrency:     "USD",
			UnderlyingQuantityBefore: -100,
			UnderlyingQuantityAfter:  0,
			PositionEffect:           rpc.ExercisePositionEffectClose,
		},
		Blockers: []rpc.TradingBlocker{{Code: "live_agent_origin_blocked", Message: "live broker writes from agent-origin callers are blocked"}},
	})
	got := stdout.String()
	for _, want := range []string{
		"Opportunity Exercise Preview  accepted=true submit_eligible=false",
		"Token ID",
		"opprev-1",
		"Expected gain",
		"live_agent_origin_blocked: live broker writes from agent-origin callers are blocked",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("opportunity preview render missing %q:\n%s", want, got)
		}
	}
}
