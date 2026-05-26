package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestOptionLegDataStatus(t *testing.T) {
	t.Parallel()
	delta := 0.42
	cases := []struct {
		name string
		bid  float64
		ask  float64
		last float64
		prev float64
		iv   float64
		d    *float64
		want string
	}{
		{name: "quoted bid", bid: 1.2, want: "quoted"},
		{name: "quoted ask", ask: 1.3, want: "quoted"},
		{name: "quoted last", last: 1.25, want: "quoted"},
		{name: "previous close price anchor", prev: 1.12, want: "prev_close"},
		{name: "model IV only", iv: 0.31, want: "model_only"},
		{name: "model delta only", d: &delta, want: "model_only"},
		{name: "no data", want: "no_quote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := optionLegDataStatus(tc.bid, tc.ask, tc.last, tc.prev, tc.iv, tc.d); got != tc.want {
				t.Fatalf("optionLegDataStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChainWarningDetailsSummarizesUnavailableLegs(t *testing.T) {
	t.Parallel()
	res := &rpc.ChainResult{
		Symbol:   "BB",
		DataType: rpc.MarketDataClosed,
		Strikes: []rpc.ChainStrike{
			{CallDataStatus: "prev_close", CallIVStatus: "ok", CallOIStatus: "unavailable"},
			{CallDataStatus: "model_only", CallIVStatus: "ok", CallOIStatus: "unavailable"},
			{CallDataStatus: "no_quote", CallIVStatus: "unavailable", CallOIStatus: "unavailable"},
		},
	}

	got := chainWarningDetails(res, true, false)

	for _, want := range []string{"options_closed", "prev_close_legs", "model_only_legs", "oi_unavailable", "iv_unavailable"} {
		if !hasWarningCode(got, want) {
			t.Fatalf("warning %q missing from %+v", want, got)
		}
	}
	if !strings.Contains(warningMessage(got, "oi_unavailable"), "3 option legs") {
		t.Fatalf("oi warning should summarize all requested legs: %+v", got)
	}
}

func hasWarningCode(warnings []rpc.DataWarning, code string) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func warningMessage(warnings []rpc.DataWarning, code string) string {
	for _, w := range warnings {
		if w.Code == code {
			return w.Message
		}
	}
	return ""
}
