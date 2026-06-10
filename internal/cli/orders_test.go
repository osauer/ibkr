package cli

import (
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestFormatOrderViewTitleByOrderShape(t *testing.T) {
	cases := []struct {
		name  string
		order rpc.OrderView
		want  string
	}{
		{
			name: "trail amount shows offset and stop, not limit price",
			order: rpc.OrderView{
				OrderRef: "ibkr-1", Action: "SELL", Quantity: 1, Symbol: "MBG",
				OrderType: "TRAIL", TIF: "DAY",
				Trail: &rpc.OrderTrailSpec{
					OffsetType:       rpc.OrderTrailOffsetAmount,
					TrailingAmount:   new(3.83),
					InitialStopPrice: 44.04,
				},
			},
			want: "ibkr-1  SELL 1 MBG TRAIL trail 3.8300 stop 44.0400 DAY",
		},
		{
			name: "trail limit percent shows percent, stop, and limit offset",
			order: rpc.OrderView{
				OrderRef: "ibkr-2", Action: "SELL", Quantity: 2, Symbol: "SAP",
				OrderType: "TRAIL LIMIT", TIF: "GTC",
				Trail: &rpc.OrderTrailSpec{
					OffsetType:       rpc.OrderTrailOffsetPercent,
					TrailingPercent:  new(8.0),
					InitialStopPrice: 142.41,
					LimitOffset:      new(0.1),
				},
			},
			want: "ibkr-2  SELL 2 SAP TRAIL LIMIT trail 8% stop 142.4100 limit_offset 0.1000 GTC",
		},
		{
			name: "limit order keeps limit price",
			order: rpc.OrderView{
				OrderRef: "ibkr-3", Action: "BUY", Quantity: 1, Symbol: "SAP",
				OrderType: "LMT", TIF: "DAY", LimitPrice: 1,
			},
			want: "ibkr-3  BUY 1 SAP LMT 1.0000 DAY",
		},
		{
			name: "market order omits the meaningless zero price",
			order: rpc.OrderView{
				OrderRef: "ibkr-4", Action: "SELL", Quantity: 1, Symbol: "DTE",
				OrderType: "MKT", TIF: "DAY",
			},
			want: "ibkr-4  SELL 1 DTE MKT DAY",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatOrderViewTitle(tc.order); got != tc.want {
				t.Fatalf("formatOrderViewTitle = %q, want %q", got, tc.want)
			}
		})
	}
}
