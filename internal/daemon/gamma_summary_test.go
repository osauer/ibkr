package daemon

import (
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestGammaZeroStatusAndRegimeUsesGapForCrossing(t *testing.T) {
	cases := []struct {
		name string
		gap  float64
		want string
	}{
		{"above_zero_long_gamma", 3.0, "long_gamma"},
		{"near_zero_transition", 0.5, "transition_gamma"},
		{"below_zero_short_gamma", -3.0, "short_gamma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zero := 100.0
			c := &rpc.GammaZeroComputed{ZeroGamma: &zero, GapPct: &tc.gap}
			status, regime := gammaZeroStatusAndRegime(c)
			if status != "crossing" {
				t.Fatalf("status = %q, want crossing", status)
			}
			if regime != tc.want {
				t.Fatalf("regime = %q, want %q", regime, tc.want)
			}
		})
	}
}
