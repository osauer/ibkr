package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// handleRegimeSnapshot fans out fetches for all five risk-regime
// dashboard indicators in parallel and assembles one consolidated
// envelope. Per-indicator failures are localised — a stale VIX feed
// doesn't fail the whole call; the affected row carries
// Status="error" or "unavailable" with a notes string the consumer
// can render.
//
// This is the surface the dashboard generator and the MCP
// natural-language interface call. The daemon does NOT derive
// green/yellow/red status from raw values: the spec explicitly calls
// those thresholds user-tunable, and bundling threshold logic into
// the daemon would force every renderer to share the daemon's edit
// cycle. Instead each row's Notes field embeds the spec's threshold
// language verbatim, giving an LLM consumer enough context to
// interpret without reading the methodology doc.
//
// Indicator 4 (gamma) is auto-kicked: the first regime snapshot of
// the NY trading session triggers the heavy compute, returns
// Status="computing" + an ETA. Subsequent calls within the day
// return the cached result instantly via the existing
// gammaZeroCache singleflight.
//
// Indicators 3 (USD/JPY) and 5 (breadth) may surface
// Status="unavailable" depending on classifySymbol coverage at
// snapshot time — see the per-indicator notes for the disposition.
func (s *Server) handleRegimeSnapshot(ctx context.Context, _ *rpc.Request) (*rpc.RegimeSnapshotResult, error) {
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}

	res := &rpc.RegimeSnapshotResult{
		AsOf:    time.Now(),
		SpecDoc: "docs/specs/risk-regime-dashboard.md",
	}

	// All five fetches in parallel. The slowest one bounds the wall
	// clock; on a warm daemon all five complete within a few seconds
	// (gamma returns from cache after the first call of the day).
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); res.VIXTermStructure = fetchRegimeVIXTerm(ctx, c) }()
	go func() { defer wg.Done(); res.HYGSPYDivergence = fetchRegimeHYGSPY(ctx, c) }()
	go func() { defer wg.Done(); res.USDJPY = fetchRegimeUSDJPY(ctx, c) }()
	go func() { defer wg.Done(); res.GammaZero = fetchRegimeGamma(ctx, s) }()
	go func() { defer wg.Done(); res.Breadth = fetchRegimeBreadth(ctx, s) }()
	wg.Wait()

	res.AsOf = time.Now()
	return res, nil
}

// ----------------------------------------------------------------------------
// Per-indicator fetchers. Each one returns a fully-populated row even on
// failure — the regime envelope never carries nil sub-objects.

const vixTermNotes = "VIX (30-day implied vol) divided by VIX3M (3-month implied vol). Spec thresholds: <0.92 green (healthy contango), 0.92-1.00 yellow (flattening), >1.00 red (backwardation — acute stress pricing). Signal requires sustained inversion over 2-3 sessions, not a single Fed-day spike."

func fetchRegimeVIXTerm(ctx context.Context, c *ibkrlib.Connector) rpc.RegimeVIXTerm {
	out := rpc.RegimeVIXTerm{Notes: vixTermNotes}

	vix, vixDT := briefSnapshotPrice(ctx, c, "VIX", 5*time.Second)
	if vix <= 0 {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "VIX: no spot tick"
		return out
	}
	vix3m, _ := briefSnapshotPrice(ctx, c, "VIX3M", 5*time.Second)
	if vix3m <= 0 {
		// One arm of the pair is enough to be informative, but the
		// ratio cannot be computed; surface VIX alone with an
		// error_message so the consumer knows the ratio is missing.
		// VIX3M will fail until classifySymbol routes it to CBOE/IND;
		// without that the gateway returns "no security definition".
		out.VIX = new(vix)
		out.DataType = vixDT
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "VIX3M: no spot tick (classifySymbol entry may be missing)"
		return out
	}

	out.VIX = new(vix)
	out.VIX3M = new(vix3m)
	r := vix / vix3m
	out.Ratio = &r
	out.DataType = vixDT
	if isLiveDataTypeWire(vixDT) {
		out.Status = rpc.RegimeStatusOK
	} else {
		out.Status = rpc.RegimeStatusStale
	}
	return out
}

const hygSpyNotes = "HYG (high-yield corporate bond ETF) vs SPY context. Spec thresholds: green when both trending up and HYG above 50-day SMA; yellow when HYG breaks 50-day SMA while SPY within 3% of 52-week high; red when HYG in clear downtrend (5+ sessions below 50-day) while SPY at/near highs. Daemon returns raw measurements — consumer compares HYG vs hyg_50dma and SPY vs spy_52w_high. Observation window 2-4 weeks; single-day moves are noise."

func fetchRegimeHYGSPY(ctx context.Context, c *ibkrlib.Connector) rpc.RegimeHYGSPYDivergence {
	out := rpc.RegimeHYGSPYDivergence{Notes: hygSpyNotes}

	hyg, hygDT := briefSnapshotPrice(ctx, c, "HYG", 5*time.Second)
	if hyg <= 0 {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "HYG: no spot tick"
		return out
	}
	out.HYGPrice = new(hyg)
	out.HYGDataType = hygDT

	// SPY: pull spot + 52-week high in one snapshot. The streaming
	// subscribe path delivers Misc Stats tick 165 for week_52_high,
	// but briefSnapshotPrice only returns the price triple. For v1
	// the SPY 52w high is exposed via GetMarketData after the
	// subscription closes — we accept what's in the cache. A
	// fancier renderer can pull the 52w high separately if needed.
	spy, _ := briefSnapshotPrice(ctx, c, "SPY", 5*time.Second)
	if spy > 0 {
		out.SPYPrice = new(spy)
	}
	if md := c.GetMarketData()["SPY"]; md != nil && md.Week52High > 0 {
		out.SPY52WHigh = new(md.Week52High)
	}

	// 50-day SMA on HYG. Pull ~70 calendar days to account for
	// non-trading-day shrinkage, average the last 50 closes.
	bars, err := c.FetchHistoricalDailyBars("HYG", 70, 20*time.Second)
	if err == nil {
		sma := averageClose(bars, 50)
		if sma > 0 {
			out.HYG50DMA = new(sma)
		}
	}

	if out.HYGPrice != nil && out.SPYPrice != nil {
		out.Status = rpc.RegimeStatusOK
		if !isLiveDataTypeWire(hygDT) {
			out.Status = rpc.RegimeStatusStale
		}
	} else {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "HYG or SPY spot missing"
	}
	return out
}

const usdJpyNotes = "USD/JPY exchange rate. Spec thresholds: stable or <1% weekly move (green); 1-2% weekly yen strength i.e. USD/JPY falling (yellow); >2% in 3 days or >3% in a week (red). Speed of move matters more than absolute level; August 2024 carry unwind played out in 3 sessions. Daemon returns last + close 7 trading days ago so the consumer can compute weekly_change_pct themselves. Source: IBKR CASH/IDEALPRO FX (Symbol=USD, Currency=JPY, SecType=CASH) — routing arrives in a sibling commit; until then this row surfaces Status=unavailable."

func fetchRegimeUSDJPY(ctx context.Context, c *ibkrlib.Connector) rpc.RegimeUSDJPY {
	out := rpc.RegimeUSDJPY{
		Symbol: "USD.JPY",
		Notes:  usdJpyNotes,
	}

	// Native FX routing depends on classifySymbol learning the dotted
	// FX-pair pattern (sibling commit). Until then briefSnapshotPrice
	// will route USD.JPY as STK and the gateway returns "no security
	// definition". Surface that as unavailable rather than faking.
	last, dt := briefSnapshotPrice(ctx, c, "USD.JPY", 5*time.Second)
	if last <= 0 {
		out.Status = rpc.RegimeStatusUnavailable
		out.ErrorMessage = "USD.JPY routing not yet available (CASH/IDEALPRO classifier pending)"
		return out
	}
	out.Last = new(last)
	out.DataType = dt

	// 7-trading-days-ago close. FX history uses MIDPOINT bars
	// (defaultHistoricalWhat for CASH); pull ~12 calendar days to
	// span 7 trading days even across a weekend / holiday.
	bars, err := c.FetchHistoricalDailyBars("USD.JPY", 12, 20*time.Second)
	if err == nil && len(bars) >= 8 {
		// bars are oldest-first; pick the close from 7 trading days
		// before the most recent close.
		idx := len(bars) - 8
		if idx >= 0 {
			c7 := bars[idx].Close
			if c7 > 0 {
				out.Close7DAgo = new(c7)
				chg := (last - c7) / c7 * 100
				out.WeeklyChange = &chg
			}
		}
	}

	if isLiveDataTypeWire(dt) {
		out.Status = rpc.RegimeStatusOK
	} else {
		out.Status = rpc.RegimeStatusStale
	}
	return out
}

const gammaNotes = "SPX dealer zero-gamma flip level. Spec thresholds: SPX >2% above zero_gamma (green); within 2% (yellow); below (red). The flip itself is the regime event — no waiting period. Methodology: Perfiliev BS-sweep over 6 nearest non-0DTE-post-settlement expirations × ±10% strikes; sign convention assumes 2018-era dealers-long-calls-short-puts (regime hint, not precise level; documented caveats around covered-call ETFs, autocallables, sticky IV). First regime call of an NY trading day auto-kicks the heavy compute; subsequent calls return the cached result. The envelope's gamma_total_abs + top_strikes give the sign-agnostic magnitude signal which is more robust than the signed flip level when positioning is unusual."

func fetchRegimeGamma(ctx context.Context, s *Server) rpc.RegimeGammaZero {
	out := rpc.RegimeGammaZero{Notes: gammaNotes}
	// Reuse the existing handler — auto-kick via the cache's
	// kickOrJoin happens inside. WaitMs=0 means we get whatever
	// state is current; subsequent regime calls within the day
	// will see status="ready" once the bg compute finishes.
	envelope, err := s.handleGammaZeroSPX(ctx, &rpc.Request{
		Method: rpc.MethodGammaZeroSPX,
		Params: json.RawMessage(`{}`),
	})
	if err != nil {
		out.Status = rpc.RegimeStatusError
		return out
	}
	out.Envelope = *envelope
	switch envelope.Status {
	case rpc.GammaZeroStatusReady:
		out.Status = rpc.RegimeStatusOK
	case rpc.GammaZeroStatusComputing:
		out.Status = rpc.RegimeStatusComputing
	case rpc.GammaZeroStatusError:
		out.Status = rpc.RegimeStatusError
	default:
		out.Status = rpc.RegimeStatusError
	}
	return out
}

const breadthNotes = "% S&P 500 stocks above their 50-day SMA. Spec thresholds: >55 green (healthy participation); 40-55 yellow; <40 with SPX within 3% of 52-week high is the textbook late-cycle divergence (red). IBKR does not catalogue the S&P-DJI breadth index (S5FI / MMFI / SPXA50R / BPSPX variants) on retail subscriptions — confirmed via reqContractDetails probe against the CBOE US Indexes feed. In v1 this indicator is unavailable: consumers either compute it from the 500 constituent daily bars (~85 min cold refresh) or treat it as a manual-entry slot per the original dashboard spec."

func fetchRegimeBreadth(ctx context.Context, s *Server) rpc.RegimeBreadth {
	out := rpc.RegimeBreadth{Notes: breadthNotes}
	envelope, err := s.handleBreadthSPX(ctx, &rpc.Request{
		Method: rpc.MethodBreadthSPX,
		Params: json.RawMessage(`{}`),
	})
	if err != nil {
		out.Status = rpc.RegimeStatusError
		return out
	}
	out.Envelope = *envelope
	// Today's reality: the IBKR S5FI subscribe returns no ticks and
	// no historical bars, so envelope.Value is 0 with an empty
	// History. Surface as unavailable rather than ok-with-zero.
	if envelope.Value == 0 && len(envelope.History) == 0 {
		out.Status = rpc.RegimeStatusUnavailable
		return out
	}
	out.Status = rpc.RegimeStatusOK
	if !isLiveDataTypeWire(envelope.DataType) {
		out.Status = rpc.RegimeStatusStale
	}
	return out
}

// ----------------------------------------------------------------------------
// Helpers shared across the per-indicator fetchers.

// averageClose returns the simple average of the last N daily closes
// from a bars slice (oldest-first). Returns 0 if the slice has
// fewer than N rows so the caller can distinguish "computed" from
// "insufficient data."
func averageClose(bars []ibkrlib.HistoricalBar, n int) float64 {
	if len(bars) < n {
		return 0
	}
	sum := 0.0
	tail := bars[len(bars)-n:]
	for _, b := range tail {
		sum += b.Close
	}
	return sum / float64(n)
}

// isLiveDataTypeWire is the wire-level "live or pending" check the
// renderer would apply. Kept local to avoid a cross-package import
// just for one helper.
func isLiveDataTypeWire(dt string) bool {
	return dt == "" || dt == rpc.MarketDataLive
}
