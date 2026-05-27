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

func TestFilterScanRowsUsesAverageDollarVolumeWhenLiveVolumeMissing(t *testing.T) {
	t.Parallel()
	rows := []rpc.ScanRow{
		{Symbol: "LIVE", Last: ptrIfPos(10.00), Volume: ptrIfPos[int64](6_000_000), DataType: rpc.MarketDataLive},
		{Symbol: "AVG_DOLLAR", Last: ptrIfPos(25.00), AvgDollarVolume20D: ptrIfPos(75_000_000.0), DataType: rpc.MarketDataLive},
		{Symbol: "AVG_SHARES", Last: ptrIfPos(40.00), AvgVolume20D: ptrIfPos[int64](2_000_000), DataType: rpc.MarketDataLive},
		{Symbol: "TOO_THIN", Last: ptrIfPos(12.00), AvgDollarVolume20D: ptrIfPos(25_000_000.0), DataType: rpc.MarketDataLive},
	}

	got := filterScanRows(rows, rpc.ScanRunParams{
		MinDollarVolume: 50_000_000,
		RequireLive:     true,
	})

	if len(got) != 3 {
		t.Fatalf("filtered row count = %d, want 3: %+v", len(got), got)
	}
	for i, want := range []string{"LIVE", "AVG_DOLLAR", "AVG_SHARES"} {
		if got[i].Symbol != want {
			t.Fatalf("row %d = %s, want %s", i, got[i].Symbol, want)
		}
	}
}
