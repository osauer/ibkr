package daemon

import (
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestFilterScanRowsDropsIlliquidOffHoursRows(t *testing.T) {
	t.Parallel()
	rows := []rpc.ScanRow{
		{Symbol: "MICRO", Last: ptrIfPos(1.14), Volume: ptrIfPos[int64](11), DataType: rpc.MarketDataLive},
		{Symbol: "STALE", Last: ptrIfPos(12.00), Volume: ptrIfPos[int64](1_000_000), DataType: rpc.MarketDataLive, WarningDetails: []rpc.DataWarning{{Code: "off_hours_quote"}}},
		{Symbol: "GOOD", Last: ptrIfPos(25.00), Volume: ptrIfPos[int64](3_000_000), DataType: rpc.MarketDataLive},
	}

	got := filterScanRows(rows, rpc.ScanRunParams{
		ExcludePenny:    true,
		MinDollarVolume: 50_000_000,
		RequireLive:     true,
	})

	if len(got) != 1 || got[0].Symbol != "GOOD" {
		t.Fatalf("filtered rows = %+v, want only GOOD", got)
	}
}
