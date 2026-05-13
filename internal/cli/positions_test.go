package cli

import (
	"math"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

// IBKR's averageCost convention differs by SecType: per-share for STK,
// per-contract (multiplier-inclusive) for OPT. The CLI renders AvgCost
// alongside a per-share Mark, so OPT rows need normalisation to read
// correctly. JSON output stays IBKR-faithful — only the renderer changes.
func TestAvgCostPerShare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.PositionView
		want float64
	}{
		{
			name: "stock returns raw avg cost (already per-share)",
			in:   rpc.PositionView{SecType: "STK", Multiplier: 1, AvgCost: 192.10},
			want: 192.10,
		},
		{
			name: "option with mult=100 divides by multiplier",
			in:   rpc.PositionView{SecType: "OPT", Multiplier: 100, AvgCost: 510.00},
			want: 5.10,
		},
		{
			name: "option with mult=1000 (some index options) divides correctly",
			in:   rpc.PositionView{SecType: "OPT", Multiplier: 1000, AvgCost: 4200.00},
			want: 4.20,
		},
		{
			name: "option with unknown multiplier (0) returns raw — better than div-by-zero",
			in:   rpc.PositionView{SecType: "OPT", Multiplier: 0, AvgCost: 510.00},
			want: 510.00,
		},
		{
			name: "stock with omitted multiplier returns raw",
			in:   rpc.PositionView{SecType: "STK", AvgCost: 192.10},
			want: 192.10,
		},
		{
			name: "empty SecType (defensive) returns raw — no OPT assumption",
			in:   rpc.PositionView{Multiplier: 100, AvgCost: 510.00},
			want: 510.00,
		},
		{
			name: "negative avg cost (theoretically possible on short premium) is preserved through division",
			in:   rpc.PositionView{SecType: "OPT", Multiplier: 100, AvgCost: -300.00},
			want: -3.00,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := avgCostPerShare(tc.in)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("avgCostPerShare = %v, want %v", got, tc.want)
			}
		})
	}
}
