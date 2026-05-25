// Command _preview renders the user-facing CLI screens with realistic
// synthetic fixture data — used for social-preview screenshots, README
// updates, and visual review without exposing a live account.
//
// The leading underscore in the directory name makes `go build ./...`
// skip this tool, so it never lands in release tarballs. Invoke by file
// path:
//
//	go run cmd/_preview/main.go              # every screen, top-to-bottom
//	go run cmd/_preview/main.go account      # one screen
//	go run cmd/_preview/main.go positions    # positions --by underlying
//	go run cmd/_preview/main.go positions-flat
//	go run cmd/_preview/main.go chain        # chain SYM (expiry list)
//	go run cmd/_preview/main.go chain-strikes
//	go run cmd/_preview/main.go quote
//	go run cmd/_preview/main.go history
//	go run cmd/_preview/main.go scan
//	go run cmd/_preview/main.go size
//	go run cmd/_preview/main.go status
//
// Color is forced on so a tee'd capture (`… | tee /tmp/preview.txt`) keeps
// the ANSI escapes that screenshot tools like freezer.dev / ray.so /
// carbon.now.sh need to render the right bold / dim / sign-color layout.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/osauer/ibkr/internal/cli"
	"github.com/osauer/ibkr/internal/rpc"
)

func main() {
	which := "all"
	if len(os.Args) > 1 {
		which = os.Args[1]
	}
	env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Color: true}
	screens := map[string]func(){
		"account":        func() { cli.PreviewRenderAccount(env, fixtureAccount()) },
		"positions":      func() { cli.PreviewRenderPositionsByUnderlying(env, fixturePositions()) },
		"positions-flat": func() { cli.PreviewRenderPositions(env, fixturePositions()) },
		"chain":          func() { cli.PreviewRenderChainExpiries(env, fixtureChain(), true) },
		"chain-strikes":  func() { cli.PreviewRenderChainStrikes(env, fixtureChainStrikes()) },
		"quote":          func() { cli.PreviewRenderQuoteSnapshot(env, fixtureQuotes()) },
		"history":        func() { cli.PreviewRenderHistory(env, fixtureHistory()) },
		"scan":           func() { cli.PreviewRenderScan(env, fixtureScan()) },
		"size":           func() { cli.PreviewRenderSize(env, fixtureSize()) },
		"status":         func() { cli.PreviewRenderStatus(env, fixtureStatus()) },
		"regime":         func() { cli.PreviewRenderRegime(env, fixtureRegime()) },
	}
	order := []string{"status", "account", "positions", "positions-flat", "chain", "chain-strikes", "quote", "history", "scan", "size", "regime"}

	if which == "all" {
		for i, key := range order {
			if i > 0 {
				fmt.Fprintln(os.Stdout)
			}
			screens[key]()
		}
		return
	}
	fn, ok := screens[which]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown preview: %q\n", which)
		fmt.Fprintln(os.Stderr, "screens: account | positions | positions-flat | chain | chain-strikes | quote | history | scan | size | status | regime | all")
		os.Exit(2)
	}
	fn()
}

func fixtureAccount() *rpc.AccountResult {
	return &rpc.AccountResult{
		AccountID:            "DU0000000",
		AccountType:          "IB-MARGIN",
		BaseCurrency:         "EUR",
		NetLiquidation:       248310.42,
		BuyingPower:          992841.68,
		AvailableFunds:       124055.21,
		ExcessLiquidity:      124055.21,
		TotalCash:            119084.21,
		MaintenanceMargin:    24318.10,
		InitialMargin:        29182.32,
		GrossPositionValue:   188420.10,
		UnrealizedPnL:        2418.07,
		RealizedPnL:          -312.50,
		Cushion:              0.50,
		LookAheadInitMargin:  29182.32,
		LookAheadMaintMargin: 24318.10,
		LookAheadAvailable:   124055.21,
		LookAheadExcess:      124055.21,
		DailyPnL:             f64(1247.30),
		DailyPnLUnrealized:   f64(962.10),
		DailyPnLRealized:     f64(285.20),
		CurrencyExposure: []rpc.CurrencyExposure{
			{Currency: "USD", NetLiquidationCcy: 92418.07, ExchangeRate: 1.0823, NetLiquidationBase: 85398.92},
			{Currency: "GBP", NetLiquidationCcy: 12061.40, ExchangeRate: 1.1718, NetLiquidationBase: 14034.83},
		},
		// AccountResult.DataType is omitempty-emit-empty per the v0.15.0
		// wire-honesty pass; the field exists on the struct for renderer-
		// fallback compatibility but the daemon never sets it. Mirror that
		// here so the screenshots match the real binary's output.
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

func fixturePositions() *rpc.PositionsResult {
	ny := time.FixedZone("EDT", -4*60*60)
	// AAPL: long stock plus a covered call and a protective put.
	aaplStock := rpc.PositionView{
		Symbol: "AAPL", SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1,
		Quantity: 100, AvgCost: 192.10, Mark: 207.42,
		DataType: rpc.MarketDataLive, PriceSource: "last",
		PrevClose: f64(206.10), DayChange: f64(1.32), DayChangePct: f64(0.64), DayChangeMoney: f64(132.00),
		DayLow: f64(205.85), DayHigh: f64(209.80), Week52Low: f64(164.08), Week52High: f64(237.49),
		Volume: ptrInt64(41_762_007), AvgVolume: ptrInt64(58_900_000),
		PriceAt: time.Date(2026, 5, 13, 10, 32, 18, 0, ny),
		SessionContext: &rpc.MarketSession{
			Timezone: "America/New_York",
			State:    "regular",
			IsOpen:   true,
		},
		MarketValue: 20742.00, UnrealizedPnL: 1532.00, DailyPnL: f64(132.00),
	}
	// AvgCost is per-contract on OPT (multiplier-inclusive) — mirrors what
	// IBKR sends over the wire. The CLI normalises to per-share on display.
	aaplCall := rpc.PositionView{
		Symbol: "AAPL", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "C", Strike: 210, Expiry: "20251219",
		Quantity: 2, AvgCost: 510.00, Mark: 7.85,
		MarketValue: 1570.00, UnrealizedPnL: 550.00,
		Delta: f64(0.42), Gamma: f64(0.018), Theta: f64(-0.08), Vega: f64(0.42),
	}
	aaplPut := rpc.PositionView{
		Symbol: "AAPL", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "P", Strike: 195, Expiry: "20251219",
		Quantity: -1, AvgCost: 385.00, Mark: 3.21,
		MarketValue: -321.00, UnrealizedPnL: 64.00,
		Delta: f64(-0.18), Gamma: f64(0.024), Theta: f64(-0.06), Vega: f64(0.31),
	}

	// NVDA: long stock plus a long upside call.
	nvdaStock := rpc.PositionView{
		Symbol: "NVDA", SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1,
		Quantity: 250, AvgCost: 119.05, Mark: 128.54,
		DataType: rpc.MarketDataLive, PriceSource: "last",
		PrevClose: f64(129.52), DayChange: f64(-0.98), DayChangePct: f64(-0.77), DayChangeMoney: f64(-245.00),
		DayLow: f64(126.70), DayHigh: f64(130.12), Week52Low: f64(86.62), Week52High: f64(153.13),
		Volume: ptrInt64(248_000_000), AvgVolume: ptrInt64(212_400_000),
		PriceAt: time.Date(2026, 5, 13, 10, 32, 18, 0, ny),
		SessionContext: &rpc.MarketSession{
			Timezone: "America/New_York",
			State:    "regular",
			IsOpen:   true,
		},
		MarketValue: 32135.00, UnrealizedPnL: 2372.50, DailyPnL: f64(-245.00),
	}
	nvdaCall := rpc.PositionView{
		Symbol: "NVDA", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "C", Strike: 135, Expiry: "20250621",
		Quantity: 5, AvgCost: 620.00, Mark: 4.80,
		MarketValue: 2400.00, UnrealizedPnL: -700.00,
		Delta: f64(0.31), Gamma: f64(0.029), Theta: f64(-0.12), Vega: f64(0.55),
	}

	// SPY: pure-options hedge — a long downside put pair.
	spyPut1 := rpc.PositionView{
		Symbol: "SPY", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "P", Strike: 560, Expiry: "20260619",
		Quantity: 3, AvgCost: 840.00, Mark: 6.75,
		MarketValue: 2025.00, UnrealizedPnL: -495.00,
		Delta: f64(-0.22), Gamma: f64(0.008), Theta: f64(-0.11), Vega: f64(0.68),
	}
	spyPut2 := rpc.PositionView{
		Symbol: "SPY", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "P", Strike: 540, Expiry: "20260619",
		Quantity: 2, AvgCost: 410.00, Mark: 3.05,
		MarketValue: 610.00, UnrealizedPnL: -210.00,
		Delta: f64(-0.14), Gamma: f64(0.005), Theta: f64(-0.09), Vega: f64(0.61),
	}

	return &rpc.PositionsResult{
		Stocks:  []rpc.PositionView{aaplStock, nvdaStock},
		Options: []rpc.PositionView{aaplCall, aaplPut, nvdaCall, spyPut1, spyPut2},
		ByUnderlying: []rpc.PositionGroup{
			{
				Underlying:         "AAPL",
				Stock:              &aaplStock,
				Options:            []rpc.PositionView{aaplCall, aaplPut},
				GroupMarketValue:   21991.00,
				GroupUnrealizedPnL: 2146.00,
			},
			{
				Underlying:         "NVDA",
				Stock:              &nvdaStock,
				Options:            []rpc.PositionView{nvdaCall},
				GroupMarketValue:   34535.00,
				GroupUnrealizedPnL: 1672.50,
			},
			{
				Underlying:         "SPY",
				Options:            []rpc.PositionView{spyPut1, spyPut2},
				GroupMarketValue:   2635.00,
				GroupUnrealizedPnL: -705.00,
			},
		},
		Portfolio: &rpc.PositionsPortfolio{
			EffectiveDelta:      f64(1847.0),
			DollarDelta:         f64(326584.50),
			DollarDeltaCurrency: "USD",
			DailyTheta:          f64(-42.18),
			DailyThetaCurrency:  "USD",
			Gamma:               f64(12.4),
			Vega:                f64(1205.00),
			GreeksCoverage:      5,
			GreeksTotal:         5,
			FXSensitivityPerPct: f64(-854.32),
			FXBaseCurrency:      "EUR",
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
		// PositionsResult.DataType is omitempty-emit-empty per v0.15.0.
	}
}

// fixtureChainStrikes is a 5-strike SPY June chain centered on the spot at
// 583.18. The ATM row carries only the strike (no quotes) to exercise the
// em-dash placeholder path — illiquid wings on real symbols look the same.
func fixtureChainStrikes() *rpc.ChainResult {
	mk := func(b, a, l, iv float64) (*float64, *float64, *float64, *float64) {
		return f64(b), f64(a), f64(l), f64(iv)
	}
	cb1, ca1, cl1, civ1 := mk(20.10, 20.35, 20.20, 0.182)
	pb1, pa1, pl1, piv1 := mk(0.42, 0.51, 0.47, 0.241)
	cb2, ca2, cl2, civ2 := mk(11.20, 11.45, 11.30, 0.171)
	pb2, pa2, pl2, piv2 := mk(1.85, 1.95, 1.90, 0.205)
	cb4, ca4, cl4, civ4 := mk(0.95, 1.05, 1.00, 0.215)
	pb4, pa4, pl4, piv4 := mk(11.05, 11.30, 11.15, 0.182)
	cb5, ca5, cl5, civ5 := mk(0.18, 0.24, 0.20, 0.238)
	pb5, pa5, pl5, piv5 := mk(20.10, 20.40, 20.25, 0.198)
	return &rpc.ChainResult{
		Symbol:   "SPY",
		Spot:     583.18,
		Expiry:   "2026-06-20",
		DTE:      38,
		DataType: "live",
		Strikes: []rpc.ChainStrike{
			{Strike: 565, CallBid: cb1, CallAsk: ca1, CallLast: cl1, CallIV: civ1, PutBid: pb1, PutAsk: pa1, PutLast: pl1, PutIV: piv1},
			{Strike: 575, CallBid: cb2, CallAsk: ca2, CallLast: cl2, CallIV: civ2, PutBid: pb2, PutAsk: pa2, PutLast: pl2, PutIV: piv2},
			{Strike: 585, IsATM: true},
			{Strike: 595, CallBid: cb4, CallAsk: ca4, CallLast: cl4, CallIV: civ4, PutBid: pb4, PutAsk: pa4, PutLast: pl4, PutIV: piv4},
			{Strike: 605, CallBid: cb5, CallAsk: ca5, CallLast: cl5, CallIV: civ5, PutBid: pb5, PutAsk: pa5, PutLast: pl5, PutIV: piv5},
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

// fixtureQuotes is a 3-symbol snapshot mixing a live quote (full data),
// a partial quote (Last only, no previous close — pre-market case), and
// a delayed quote (entitlement-limited).
func fixtureQuotes() []rpc.Quote {
	// change_pct is in percent (0.70 = 0.70%), matching the position view's
	// day-change-pct convention and how the daemon serializes the tick.
	bid1, ask1, last1 := 207.82, 207.88, 207.85
	prev1, chg1, pct1 := 206.40, 1.45, 0.70
	bidSz1, askSz1 := 240, 510
	vol1 := int64(38_400_000)
	iv1 := 0.247

	last2 := 128.54

	bid3, ask3, last3 := 461.04, 461.20, 461.12
	prev3, chg3, pct3 := 463.50, -2.38, -0.51
	vol3 := int64(8_120_000)

	return []rpc.Quote{
		{
			Symbol: "AAPL", Bid: &bid1, Ask: &ask1, Last: &last1,
			PrevClose: &prev1, Change: &chg1, ChangePct: &pct1,
			BidSize: &bidSz1, AskSize: &askSz1, Volume: &vol1,
			IV: &iv1, DataType: "live",
		},
		{Symbol: "NVDA", Last: &last2, DataType: "live"},
		{
			Symbol: "SPY", Bid: &bid3, Ask: &ask3, Last: &last3,
			PrevClose: &prev3, Change: &chg3, ChangePct: &pct3,
			Volume:   &vol3,
			DataType: "delayed",
		},
	}
}

func fixtureHistory() *rpc.HistoryDailyResult {
	// HistoryDailyResult.DataType is omitempty-emit-empty per v0.15.0.
	return &rpc.HistoryDailyResult{
		Symbol: "AAPL",
		Days:   5,
		Bars: []rpc.HistoryBar{
			{Date: "2026-05-06", Open: 204.30, High: 207.10, Low: 203.85, Close: 206.40, Volume: 41_200_000},
			{Date: "2026-05-07", Open: 206.50, High: 208.95, Low: 205.70, Close: 207.85, Volume: 36_500_000},
			{Date: "2026-05-08", Open: 207.90, High: 210.40, Low: 207.10, Close: 209.55, Volume: 44_800_000},
			{Date: "2026-05-09", Open: 209.20, High: 210.05, Low: 206.30, Close: 207.42, Volume: 39_100_000},
			{Date: "2026-05-12", Open: 207.55, High: 209.80, Low: 206.95, Close: 208.91, Volume: 31_700_000},
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

// fixtureScan is a 6-row mixed-exchange screen — USD-quoted US rows,
// one EUR row, one HKD row, plus a partial row (no IV, no 52w range,
// no Currency) so the em-dash + fallback paths both render. Exercises
// the per-row Currency rendering introduced in v0.13.
func fixtureScan() *rpc.ScanResult {
	mkRow := func(rank int, sym, ccy string, last, pct, iv, lo, hi float64, vol int64) rpc.ScanRow {
		return rpc.ScanRow{
			Rank: rank, Symbol: sym, Currency: ccy,
			Last: f64(last), ChangePct: f64(pct), IV: f64(iv),
			Week52Low: f64(lo), Week52High: f64(hi),
			Volume: &vol,
		}
	}
	// CHG% is in percent units (7.21 = 7.21%) — formatChangePct prints
	// the value with a trailing % without multiplying.
	partial := rpc.ScanRow{Rank: 6, Symbol: "ZZZ", Last: f64(12.34), ChangePct: f64(4.10)}
	return &rpc.ScanResult{
		Preset: "top_pct_gain",
		Type:   "TOP_PERC_GAIN",
		Rows: []rpc.ScanRow{
			mkRow(1, "AAPL", "USD", 207.85, 7.21, 0.247, 142.10, 219.50, 38_400_000),
			mkRow(2, "NVDA", "USD", 128.54, 5.12, 0.382, 75.20, 145.80, 248_000_000),
			mkRow(3, "AMD", "USD", 145.22, 4.28, 0.318, 89.40, 178.90, 62_500_000),
			mkRow(4, "SAP", "EUR", 174.50, 2.85, 0.221, 130.20, 188.40, 4_100_000),
			mkRow(5, "0700", "HKD", 412.20, 3.42, 0.298, 285.00, 458.80, 18_400_000),
			partial,
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

// fixtureSize mirrors a typical EUR account sizing a long AAPL trade
// with a 2R target — covers the full screen including the reward block.
func fixtureSize() *cli.SizeResult {
	tgt, r, reward, be := 215.50, 2.0, 4000.0, 1.0/3.0
	return &cli.SizeResult{
		Symbol: "AAPL", Side: "long",
		Entry: 207.50, Stop: 202.50, Target: &tgt,
		RiskPct: 1.0, Lot: 1, FX: 1.0,
		NLV: 248310.42, BaseCurrency: "EUR",
		RiskBase: 2483.10, RiskQuote: 2483.10,
		PerShareRisk: 5.00,
		Shares:       496, Notional: 102920.00, MaxLoss: 2480.00,
		R: &r, RewardQuote: &reward, BreakevenWinRate: &be,
		Status: "ok",
	}
}

// fixtureStatus is a healthy connected daemon — the happy-path screen
// the user sees after `ibkr status` on a working setup.
func fixtureStatus() *rpc.HealthResult {
	return &rpc.HealthResult{
		DaemonVersion: "v1.0.0",
		UptimeSeconds: 1842,
		Account:       "DU0000000",
		GatewayHost:   "127.0.0.1",
		GatewayPort:   4001,
		GatewayTLS:    false,
		NegotiatedTLS: false,
		PortOrigin:    "discovered",
		ClientID:      17,
		Connected:     true,
		ServerVersion: 178,
		Members: rpc.MembersHealth{
			Source:       "cache",
			AsOf:         time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
			Count:        503,
			RefreshState: "healthy",
		},
		// HealthResult.DataType is omitempty-emit-empty post-v0.16.0
		// (the daemon stopped hardcoding "live" — status has no per-
		// reqID feed type to honestly report). Renderer falls back to
		// MarketDataLive when empty.
	}
}

func fixtureChain() *rpc.ChainExpiriesResult {
	// All ImpliedMove values satisfy spot × IV × √(DTE/365), so the
	// formula caption isn't decorative — the screen is self-consistent.
	expiry := func(date string, dte int, iv, move, pct float64) rpc.ChainExpiry {
		return rpc.ChainExpiry{
			Date: date, DTE: dte,
			IV: f64(iv), IVStatus: "ok",
			ImpliedMove: f64(move), ImpliedMovePct: f64(pct),
		}
	}
	return &rpc.ChainExpiriesResult{
		Symbol: "SPY",
		Spot:   583.18,
		Expiries: []rpc.ChainExpiry{
			expiry("2026-05-16", 3, 0.142, 7.51, 0.0129),
			expiry("2026-05-23", 10, 0.168, 16.21, 0.0278),
			expiry("2026-05-30", 17, 0.181, 22.78, 0.0391),
			expiry("2026-06-20", 38, 0.194, 36.51, 0.0626),
			expiry("2026-07-18", 66, 0.207, 51.36, 0.0881),
			expiry("2026-08-15", 94, 0.214, 63.31, 0.1086),
			expiry("2026-09-19", 129, 0.218, 75.60, 0.1297),
			expiry("2026-12-19", 220, 0.221, 100.04, 0.1716),
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

func f64(v float64) *float64 { return &v }

func ptrInt64(v int64) *int64 { return &v }

// fixtureRegime returns a realistic mid-session regime envelope that
// exercises every render branch: one OK-live row, one stale row with a
// missing sub-field, one OK row with weekly change, one row in the
// computing state, and one structurally-unavailable row. Numbers track
// the spec's "Live-test result on 2026-05-17" snippet so the screen and
// the spec doc agree.
func fixtureRegime() *rpc.RegimeSnapshotResult {
	return &rpc.RegimeSnapshotResult{
		AsOf:    time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC),
		SpecDoc: "docs/specs/risk-regime-dashboard.md",
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status:       rpc.RegimeStatusOK,
			VIX:          f64(18.43),
			VIX3M:        f64(21.36),
			Ratio:        f64(18.43 / 21.36),
			VIXPrevClose: f64(18.85),
			VIXChangePct: f64((18.43 - 18.85) / 18.85 * 100),
			DataType:     rpc.MarketDataLive,
			Notes:        "VIX (30-day implied vol) divided by VIX3M (3-month implied vol). Spec thresholds: <0.92 green (healthy contango), 0.92-1.00 yellow (flattening), >1.00 red (backwardation — acute stress pricing).",
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:       rpc.RegimeStatusStale,
			HYGPrice:     f64(79.55),
			HYG50DMA:     f64(80.10),
			SPYPrice:     f64(737.34),
			SPY52WHigh:   f64(749.30),
			SPYPrevClose: f64(736.14),
			SPYChange:    f64(737.34 - 736.14),
			SPYChangePct: f64((737.34 - 736.14) / 736.14 * 100),
			HYGDataType:  rpc.MarketDataDelayedFrozen,
			Notes:        "HYG (high-yield corporate bond ETF) vs SPY context. Spec thresholds: green when both trending up and HYG above 50-day SMA; yellow when HYG breaks 50-day SMA while SPY within 3% of 52-week high.",
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status:       rpc.RegimeStatusOK,
			Symbol:       "USD.JPY",
			Last:         f64(158.7285),
			Close7DAgo:   f64(158.05),
			WeeklyChange: f64(0.43),
			DataType:     rpc.MarketDataLive,
			Notes:        "USD/JPY exchange rate. Spec thresholds: stable or <1% weekly move (green); 1-2% weekly yen strength (yellow); >2% in 3 days or >3% in a week (red).",
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusComputing,
			Envelope: rpc.GammaZeroSPXResult{
				Status:     rpc.GammaZeroStatusComputing,
				EtaSeconds: 42,
				Progress:   40,
			},
			Notes: "SPY dealer zero-gamma flip level. Spec thresholds: SPY >2% above zero_gamma (green); within 2% (yellow); below (red). Methodology: Perfiliev BS-sweep; see spec for limitations and the calibration ritual.",
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.BreadthSPXResult{
				State:          rpc.BreadthStateReady,
				PctAbove50DMA:  61.8,
				PctAbove200DMA: 68.2,
				NewHighsToday:  27,
				NewLowsToday:   8,
				NetNewHighsPct: 3.8,
				Source:         "Computed from S&P-500 constituent daily bars (IBKR HMDS)",
				Method:         "constituent-fanout-50/200dma-hl",
				AsOf:           time.Date(2026, 5, 16, 20, 35, 0, 0, time.UTC),
			},
			PctAbove50DMA:  61.8,
			PctAbove200DMA: 68.2,
			NewHighsToday:  27,
			NewLowsToday:   8,
			NetNewHighsPct: 3.8,
			Notes:          "% S&P 500 stocks above their 50-day SMA. Spec thresholds: >55 green (healthy participation); 40-55 yellow; <40 with SPX within 3% of 52-week high is the textbook late-cycle divergence (red). IBKR doesn't redistribute S&P DJI's S5FI index on retail subscriptions, so the daemon computes the same number locally from the 500 constituent daily closes (once-daily refresh post-close).",
		},
	}
}
