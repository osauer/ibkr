// Command _preview renders the three signature CLI screens with realistic
// synthetic fixture data — used for social-preview screenshots, README
// updates, and visual review without exposing a live account.
//
// The leading underscore in the directory name makes `go build ./...`
// skip this tool, so it never lands in release tarballs. Invoke by file
// path:
//
//	go run cmd/_preview/main.go            # all three screens
//	go run cmd/_preview/main.go account    # one screen
//	go run cmd/_preview/main.go positions
//	go run cmd/_preview/main.go chain
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
	switch which {
	case "account":
		cli.PreviewRenderAccount(env, fixtureAccount())
	case "positions":
		cli.PreviewRenderPositionsByUnderlying(env, fixturePositions())
	case "chain":
		cli.PreviewRenderChainExpiries(env, fixtureChain(), true)
	case "all":
		cli.PreviewRenderAccount(env, fixtureAccount())
		fmt.Fprintln(os.Stdout)
		cli.PreviewRenderPositionsByUnderlying(env, fixturePositions())
		fmt.Fprintln(os.Stdout)
		cli.PreviewRenderChainExpiries(env, fixtureChain(), true)
	default:
		fmt.Fprintf(os.Stderr, "unknown preview: %q (want account | positions | chain | all)\n", which)
		os.Exit(2)
	}
}

func fixtureAccount() *rpc.AccountResult {
	return &rpc.AccountResult{
		AccountID:         "U7842931",
		BaseCurrency:      "EUR",
		NetLiquidation:    248310.42,
		BuyingPower:       992841.68,
		AvailableFunds:    124055.21,
		ExcessLiquidity:   124055.21,
		TotalCash:         119084.21,
		MaintenanceMargin: 24318.10,
		InitialMargin:     29182.32,
		CurrencyExposure: []rpc.CurrencyExposure{
			{Currency: "USD", NetLiquidationCcy: 92418.07, ExchangeRate: 1.0823, NetLiquidationBase: 85398.92},
			{Currency: "GBP", NetLiquidationCcy: 12061.40, ExchangeRate: 1.1718, NetLiquidationBase: 14034.83},
		},
		DataType: "live",
		AsOf:     time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

func fixturePositions() *rpc.PositionsResult {
	// AAPL: long stock plus a covered call and a protective put.
	aaplStock := rpc.PositionView{
		Symbol: "AAPL", Quantity: 100, AvgCost: 192.10, Mark: 207.42,
		DayChange: f64(1.32), DayChangePct: f64(0.64),
		MarketValue: 20742.00, UnrealizedPnL: 1532.00,
	}
	aaplCall := rpc.PositionView{
		Symbol: "AAPL", Right: "C", Strike: 210, Expiry: "20251219",
		Quantity: 2, AvgCost: 5.10, Mark: 7.85,
		MarketValue: 1570.00, UnrealizedPnL: 550.00,
		Delta: f64(0.42), Gamma: f64(0.018), Theta: f64(-0.08), Vega: f64(0.42),
	}
	aaplPut := rpc.PositionView{
		Symbol: "AAPL", Right: "P", Strike: 195, Expiry: "20251219",
		Quantity: -1, AvgCost: 3.85, Mark: 3.21,
		MarketValue: -321.00, UnrealizedPnL: 64.00,
		Delta: f64(-0.18), Gamma: f64(0.024), Theta: f64(-0.06), Vega: f64(0.31),
	}

	// NVDA: long stock plus a long upside call.
	nvdaStock := rpc.PositionView{
		Symbol: "NVDA", Quantity: 250, AvgCost: 119.05, Mark: 128.54,
		DayChange: f64(-0.98), DayChangePct: f64(-0.77),
		MarketValue: 32135.00, UnrealizedPnL: 2372.50,
	}
	nvdaCall := rpc.PositionView{
		Symbol: "NVDA", Right: "C", Strike: 135, Expiry: "20250621",
		Quantity: 5, AvgCost: 6.20, Mark: 4.80,
		MarketValue: 2400.00, UnrealizedPnL: -700.00,
		Delta: f64(0.31), Gamma: f64(0.029), Theta: f64(-0.12), Vega: f64(0.55),
	}

	// SPY: pure-options hedge — a long downside put pair.
	spyPut1 := rpc.PositionView{
		Symbol: "SPY", Right: "P", Strike: 560, Expiry: "20260619",
		Quantity: 3, AvgCost: 8.40, Mark: 6.75,
		MarketValue: 2025.00, UnrealizedPnL: -495.00,
		Delta: f64(-0.22), Gamma: f64(0.008), Theta: f64(-0.11), Vega: f64(0.68),
	}
	spyPut2 := rpc.PositionView{
		Symbol: "SPY", Right: "P", Strike: 540, Expiry: "20260619",
		Quantity: 2, AvgCost: 4.10, Mark: 3.05,
		MarketValue: 610.00, UnrealizedPnL: -210.00,
		Delta: f64(-0.14), Gamma: f64(0.005), Theta: f64(-0.09), Vega: f64(0.61),
	}

	return &rpc.PositionsResult{
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
			DollarDeltaCurrency: "EUR",
			DailyTheta:          f64(-42.18),
			Gamma:               f64(12.4),
			Vega:                f64(1205.00),
			GreeksCoverage:      5,
			GreeksTotal:         5,
			FXSensitivityPerPct: f64(-854.32),
			FXBaseCurrency:      "EUR",
		},
		AsOf:     time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
		DataType: "live",
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
