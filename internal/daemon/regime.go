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
		SpecDoc: "docs/specs/risk-regime-dashboard.md",
	}
	deps := productionRegimeDeps(c, s.logger.Warnf)

	// All five fetches in parallel. The slowest one bounds the wall
	// clock; on a warm daemon all five complete within a few seconds
	// (gamma returns from cache after the first call of the day).
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); res.VIXTermStructure = fetchRegimeVIXTerm(ctx, deps) }()
	go func() { defer wg.Done(); res.HYGSPYDivergence = fetchRegimeHYGSPY(ctx, deps) }()
	go func() { defer wg.Done(); res.USDJPY = fetchRegimeUSDJPY(ctx, deps) }()
	go func() { defer wg.Done(); res.GammaZero = fetchRegimeGamma(ctx, s) }()
	go func() { defer wg.Done(); res.Breadth = fetchRegimeBreadth(ctx, s) }()
	wg.Wait()

	res.AsOf = time.Now()
	return res, nil
}

// regimeDeps is the dependency surface the three quote-and-history
// indicators (VIX, HYG/SPY, USD/JPY) share. It exists for two
// concrete reasons:
//
//  1. The three fetchers all call briefSnapshotPrice +
//     FetchHistoricalDailyBars + GetMarketData lookups, so a single
//     struct keeps the call sites uniform.
//  2. The unit tests need to drive each fetcher with canned data
//     without spinning up a real daemon or gateway connection.
//
// logWarnf is the operator-visible signal for partial failures:
// history-bar fetch errors and insufficient-bar truncations land here
// rather than getting silently swallowed. A null sub-field in the
// returned envelope only tells the consumer *that* a field is missing;
// the daemon log tells them *why*. Tests inject a capture closure to
// assert the right diagnostic landed.
//
// snapshotWith52WHigh is the SPY-specific seam: the default snapshot
// path returns on the first price tick, too fast for IBKR's Misc
// Stats tick 165 (Week-range highs/lows) to arrive. The HYG/SPY
// indicator needs the 52w high to evaluate the spec's yellow-band
// trigger; without it the indicator drops to a 2-state signal.
//
// Indicators 4 (gamma) and 5 (breadth) already delegate to their own
// handlers (handleGammaZeroSPX, handleBreadthSPX); they don't need a
// deps struct because they already have a server-level seam.
type regimeDeps struct {
	snapshot            func(ctx context.Context, sym string, timeout time.Duration) (price float64, dataType string)
	snapshotWith52WHigh func(ctx context.Context, sym string, timeout time.Duration) (price float64, week52High float64, dataType string)
	history             func(sym string, days int, timeout time.Duration) ([]ibkrlib.HistoricalBar, error)
	logWarnf            func(format string, args ...any)
}

// productionRegimeDeps wires the deps struct to the live connector.
// Tests pass a hand-rolled regimeDeps with closures returning canned
// values instead.
func productionRegimeDeps(c *ibkrlib.Connector, logWarnf func(format string, args ...any)) *regimeDeps {
	return &regimeDeps{
		snapshot: func(ctx context.Context, sym string, timeout time.Duration) (float64, string) {
			return briefSnapshotPrice(ctx, c, sym, timeout)
		},
		snapshotWith52WHigh: func(ctx context.Context, sym string, timeout time.Duration) (float64, float64, string) {
			return briefSnapshotPriceWith52WHigh(ctx, c, sym, timeout)
		},
		history: func(sym string, days int, timeout time.Duration) ([]ibkrlib.HistoricalBar, error) {
			return c.FetchHistoricalDailyBars(sym, days, timeout)
		},
		logWarnf: logWarnf,
	}
}

// ----------------------------------------------------------------------------
// Per-indicator fetchers. Each one returns a fully-populated row even on
// failure — the regime envelope never carries nil sub-objects.

const vixTermNotes = "VIX (30-day implied vol) divided by VIX3M (3-month implied vol). Spec thresholds: <0.92 green (healthy contango), 0.92-1.00 yellow (flattening), >1.00 red (backwardation — acute stress pricing). Signal requires sustained inversion over 2-3 sessions, not a single Fed-day spike."

func fetchRegimeVIXTerm(ctx context.Context, deps *regimeDeps) rpc.RegimeVIXTerm {
	out := rpc.RegimeVIXTerm{Notes: vixTermNotes}
	now := time.Now()

	// VIX itself usually delivers a live mark (tick 37) even off-hours.
	// VIX3M is a thinner CBOE index whose calculation only updates with
	// active SPX option flow; pre-open it routinely emits no live ticks
	// at all and the snapshot helper falls back to the previous
	// regular-session close (tick 9) so the ratio still ranks. The
	// data-type field honestly reports "frozen" in that case so the
	// renderer dims the row instead of pretending it's live.
	vix, vixDT := deps.snapshot(ctx, "VIX", 5*time.Second)
	if vix <= 0 {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "VIX: no spot tick"
		return out
	}
	// 8 s budget (vs 5 s for VIX) because VIX3M is a much thinner
	// CBOE index: off-hours the gateway sometimes takes longer than
	// the VIX leg to push the close tick, and 5 s reliably lost it on
	// cold-frozen-mode calls even with a warm contract cache. 8 s
	// matches the SPY 52w-high budget for the same reason.
	vix3m, vix3mDT := deps.snapshot(ctx, "VIX3M", 8*time.Second)
	if vix3m <= 0 {
		// One arm of the pair is enough to be informative, but the
		// ratio cannot be computed; surface VIX alone with an
		// error_message so the consumer knows the ratio is missing.
		out.VIX = new(vix)
		out.VIXQuality = firmTickQuality(now, vixDT, "VIX tick")
		out.DataType = vixDT
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "VIX3M: no tick within budget (thin CBOE index, common off-hours)"
		return out
	}

	out.VIX = new(vix)
	out.VIX3M = new(vix3m)
	out.VIXQuality = firmTickQuality(now, vixDT, "VIX tick")
	out.VIX3MQuality = firmTickQuality(now, vix3mDT, "VIX3M tick (thin CBOE; off-hours typically frozen)")
	r := vix / vix3m
	out.Ratio = &r
	// The ratio is only as fresh as the staler leg. Both must be live
	// to call the whole row "live".
	out.DataType = vixDT
	if !rpc.IsLiveDataType(vix3mDT) {
		out.DataType = vix3mDT
	}
	if rpc.IsLiveDataType(out.DataType) {
		out.Status = rpc.RegimeStatusOK
	} else {
		out.Status = rpc.RegimeStatusStale
	}
	return out
}

const hygSpyNotes = "HYG (high-yield corporate bond ETF) vs SPY context. Spec thresholds: green when both trending up and HYG above 50-day SMA; yellow when HYG breaks 50-day SMA while SPY within 3% of 52-week high; red when HYG in clear downtrend (5+ sessions below 50-day) while SPY at/near highs. Daemon returns raw measurements — consumer compares HYG vs hyg_50dma and SPY vs spy_52w_high. Observation window 2-4 weeks; single-day moves are noise."

func fetchRegimeHYGSPY(ctx context.Context, deps *regimeDeps) rpc.RegimeHYGSPYDivergence {
	out := rpc.RegimeHYGSPYDivergence{Notes: hygSpyNotes}
	now := time.Now()

	hyg, hygDT := deps.snapshot(ctx, "HYG", 5*time.Second)
	if hyg <= 0 {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "HYG: no spot tick"
		return out
	}
	out.HYGPrice = new(hyg)
	out.HYGDataType = hygDT
	out.HYGQuality = firmTickQuality(now, hygDT, "HYG tick (ARCA)")

	// SPY: pull spot + 52-week high in one combined subscribe so tick
	// 165 (Misc Stats) has time to land. Either field may still come
	// back zero — the predicate inside snapshotWith52WHigh returns
	// partial results on timeout so a cold-start gateway still
	// surfaces what it had. 8s budget (vs 5s for plain snapshots)
	// because the Misc-Stats tick reliably arrives later than the
	// price triple in observed traces.
	spy, spy52, spyDT := deps.snapshotWith52WHigh(ctx, "SPY", 8*time.Second)
	if spy > 0 {
		out.SPYPrice = new(spy)
		out.SPYQuality = firmTickQuality(now, spyDT, "SPY tick")
	}
	if spy52 > 0 {
		out.SPY52WHigh = new(spy52)
		out.SPY52WHighQuality = firmTickQuality(now, spyDT, "SPY tick 165 (Misc Stats)")
	} else {
		// Frozen-mode fallback: in MarketDataType=2 the gateway sends
		// the price triple as one static snapshot then goes silent —
		// tick 165 (Misc Stats) never arrives, no matter the budget.
		// Compute max(High) over ~1 trading year of daily bars instead,
		// so the indicator stays 3-state at all hours rather than
		// dropping to 2-state every time the market is closed. The
		// live tick is still primary above; this branch fires only
		// when the gateway didn't supply a value.
		//
		// 365 calendar days yields ~252 trading bars after weekends and
		// the 9-10 US holidays per year; FetchHistoricalDailyBars maps
		// >365 to "1 Y" anyway, so 365 is the exact knee.
		spyBars, err := deps.history("SPY", 365, 20*time.Second)
		switch {
		case err != nil:
			warnDeps(deps, "regime: SPY 52w high history fetch failed: %v", err)
		case len(spyBars) < 50:
			// 50 is a soft floor — any shorter window doesn't
			// meaningfully approximate a 52w high. Stay symmetric
			// with HYG 50DMA's diagnostic shape.
			warnDeps(deps, "regime: SPY 52w high insufficient bars: got %d, want ~252", len(spyBars))
		default:
			hi := maxHigh(spyBars, 252)
			if hi > 0 {
				out.SPY52WHigh = new(hi)
				out.SPY52WHighQuality = derivedQuality(now, "SPY 252d max(High) fallback")
			}
		}
	}

	// 50-day SMA on HYG. 50 trading days ≈ 70 calendar days when
	// the window has zero holidays; the US market closes 9-10 days
	// per year, so a 70-day window can come up short on the wrong
	// side of Memorial Day / Labor Day / Thanksgiving. 90 calendar
	// days gives ~10 days of slack — the IBKR HMDS API only bills
	// the call, not the bar count, so this is free.
	bars, err := deps.history("HYG", 90, 20*time.Second)
	switch {
	case err != nil:
		warnDeps(deps, "regime: HYG 50DMA history fetch failed: %v", err)
	case len(bars) < 50:
		warnDeps(deps, "regime: HYG 50DMA insufficient bars: got %d, need 50", len(bars))
	default:
		sma := averageClose(bars, 50)
		if sma > 0 {
			out.HYG50DMA = new(sma)
			out.HYG50DMAQuality = derivedQuality(now, "HYG 50-bar SMA")
		}
	}

	if out.HYGPrice == nil || out.SPYPrice == nil {
		out.Status = rpc.RegimeStatusError
		out.ErrorMessage = "HYG or SPY spot missing"
		return out
	}
	out.Status = rpc.RegimeStatusOK
	if !rpc.IsLiveDataType(hygDT) {
		out.Status = rpc.RegimeStatusStale
	}
	// Advisory sub-field annotations — the row's primary measurements
	// landed, but a renderer may want to dim "52w-high" or "50DMA"
	// cells that didn't.
	if out.SPY52WHigh == nil {
		out.FieldsMissing = append(out.FieldsMissing, "spy_52w_high")
	}
	if out.HYG50DMA == nil {
		out.FieldsMissing = append(out.FieldsMissing, "hyg_50dma")
	}
	return out
}

const usdJpyNotes = "USD/JPY exchange rate. Spec thresholds: stable or <1% weekly move (green); 1-2% weekly yen strength i.e. USD/JPY falling (yellow); >2% in 3 days or >3% in a week (red). Speed of move matters more than absolute level; August 2024 carry unwind played out in 3 sessions. Daemon returns last + close 7 trading days ago so the consumer can compute weekly_change_pct themselves. Source: IBKR CASH/IDEALPRO FX (Symbol=USD, Currency=JPY, SecType=CASH) — routed via the dotted-pair classifier; the row surfaces Status=unavailable when the gateway has no FX ticks (typically: account lacks IDEALPRO market-data subscription, or markets closed with no frozen tick to fall back on)."

func fetchRegimeUSDJPY(ctx context.Context, deps *regimeDeps) rpc.RegimeUSDJPY {
	out := rpc.RegimeUSDJPY{
		Symbol: "USD.JPY",
		Notes:  usdJpyNotes,
	}
	now := time.Now()

	// briefSnapshotPrice routes "USD.JPY" through pkg/ibkr.classifySymbol
	// to CASH/IDEALPRO/JPY (see commit 6ac583c). A 0 result here means
	// either the gateway has no FX entitlement for this account or
	// there's no frozen tick to fall back on; either way, surface as
	// unavailable rather than faking a value.
	last, dt := deps.snapshot(ctx, "USD.JPY", 5*time.Second)
	if last <= 0 {
		out.Status = rpc.RegimeStatusUnavailable
		out.ErrorMessage = "USD.JPY: gateway delivered no FX tick (check IDEALPRO entitlement)"
		return out
	}
	out.Last = new(last)
	out.LastQuality = firmTickQuality(now, dt, "USD.JPY CASH tick (IDEALPRO)")
	out.DataType = dt

	// 7-trading-days-ago close. FX history uses MIDPOINT bars
	// (defaultHistoricalWhat for CASH); FX trades 24/5 so 7 trading
	// days = 7 weekday FX sessions. 14 calendar days covers 7 FX
	// sessions even when a Monday or Friday bank holiday interrupts
	// the count (US: MLK Day, Memorial Day, Labor Day, Thanksgiving,
	// etc. all fall on Mondays and clip one US-tradable FX day).
	bars, err := deps.history("USD.JPY", 14, 20*time.Second)
	switch {
	case err != nil:
		warnDeps(deps, "regime: USD.JPY history fetch failed: %v", err)
	case len(bars) < 8:
		warnDeps(deps, "regime: USD.JPY history insufficient bars: got %d, need 8", len(bars))
	default:
		// bars are oldest-first; pick the close from 7 trading days
		// before the most recent close.
		idx := len(bars) - 8
		if idx >= 0 {
			c7 := bars[idx].Close
			if c7 > 0 {
				out.Close7DAgo = new(c7)
				out.Close7DAgoQuality = derivedQuality(now, "USD.JPY MIDPOINT bar t-7")
				chg := (last - c7) / c7 * 100
				out.WeeklyChange = &chg
			}
		}
	}

	if rpc.IsLiveDataType(dt) {
		out.Status = rpc.RegimeStatusOK
	} else {
		out.Status = rpc.RegimeStatusStale
	}
	if out.Close7DAgo == nil {
		out.FieldsMissing = append(out.FieldsMissing, "close_7d_ago")
	}
	if out.WeeklyChange == nil {
		out.FieldsMissing = append(out.FieldsMissing, "weekly_change_pct")
	}
	return out
}

const gammaNotes = "SPY dealer γ-zero level (the spot where dealer net gamma crosses zero). Spec thresholds: SPY >2% above γ-zero (green, stabilizing); within 2% (yellow, regime can flip on a single session); below (red, amplifying). The crossing itself is the regime event — no waiting period. Underlying is SPY (the S&P 500 ETF) rather than SPX (the index) so the compute is robust off-hours: SPY trades extended hours with continuous market-maker quotes and a single trading class, which keeps option IV ticks flowing and the chain enumeration clean; SPX has no spot trading outside RTH and IBKR's model-computation engine doesn't push IV ticks for SPX options pre-market. The regime signal is unchanged — SPY dealer gamma tracks SPX dealer gamma closely — but the absolute level is SPY-scale (~SPX/10). Methodology: Perfiliev BS-sweep over 6 nearest non-0DTE-post-settlement expirations × ±10% strikes; sign convention assumes 2018-era dealers-long-calls-short-puts (regime hint, not precise level; documented caveats around covered-call ETFs, autocallables, sticky IV). Pre-market: when the gateway's model-computation engine is idle, the compute falls back to Black-Scholes Newton-Raphson on each option's prior-session close to back-solve IV; legs using the fallback are counted in derived_iv_legs and surfaced in the row's source disclosure. First regime call of an NY trading day auto-kicks the heavy compute; subsequent calls return the cached result. The envelope's gamma_total_abs + top_strikes give the sign-agnostic magnitude signal which is more robust than the signed γ-zero level when positioning is unusual."

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
		if envelope.Result != nil {
			// Both scalars derive from the same compute, so AsOf is the
			// compute's completion timestamp. ZeroGamma is modelled (the
			// BS sweep's interpolation); GammaTotalAbs is the firmer
			// sign-agnostic notional aggregated from OI+IV observations
			// — still an estimate because per-leg coverage varies.
			//
			// When DerivedIVLegs == LegCount, every IV in the compute came
			// from the BS-IV Newton-Raphson fallback against
			// prior-session prices (typical pre-market). The Source
			// string disclosure surfaces that to the --explain reader so
			// the prior-prices anchor is visible without re-reading the
			// methodology spec.
			source := envelope.Result.Method
			if r := envelope.Result; r.DerivedIVLegs > 0 && r.DerivedIVLegs == r.LegCount {
				source = r.Method + " · BS-IV from prior-session last price"
			}
			out.ZeroGammaQuality = modelledQuality(envelope.Result.AsOf, source)
			out.GammaTotalAbsQuality = derivedQuality(envelope.Result.AsOf, "BS-sweep |Γ|·OI·spot²")
		}
	case rpc.GammaZeroStatusComputing:
		out.Status = rpc.RegimeStatusComputing
	case rpc.GammaZeroStatusError:
		out.Status = rpc.RegimeStatusError
	default:
		out.Status = rpc.RegimeStatusError
	}
	return out
}

const breadthNotes = "% S&P 500 stocks above their 50-day SMA. Spec thresholds: >55 green (healthy participation); 40-55 yellow; <40 with SPX within 3% of 52-week high is the textbook late-cycle divergence (red). IBKR does not redistribute S&P DJI's S5FI index on retail subscriptions, so the daemon computes the same number locally from the 500 constituent daily closes — refresh runs once per US trading day post-close (16:35 ET). Method token: constituent-fanout-50dma."

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

	// State on the envelope is the single source of truth — replaces
	// the pre-v0.27.3 side-channel that called s.breadth.IsRefreshing()
	// separately and tried to disambiguate (value==0 AND history==[])
	// heuristically. That heuristic mis-classified a poisoned
	// Coverage=0 snapshot (history len 1, value 0) as "ok" — the bug
	// that produced three patch releases in one day. Reading
	// envelope.State directly makes the classification mechanical.
	switch envelope.State {
	case rpc.BreadthStateComputing:
		out.Status = rpc.RegimeStatusComputing
		return out
	case rpc.BreadthStateCold, rpc.BreadthStateDegraded:
		out.Status = rpc.RegimeStatusUnavailable
		return out
	}
	// State == "ready" — fall through to the populated-envelope path.

	// The value is computed (not a live gateway tick). derivedQuality
	// is the right shelf — it tags FreshnessClass=derived,
	// Confidence=estimate so renderers don't mistake this for a
	// firm-tick reading.
	out.ValueQuality = derivedQuality(envelope.AsOf, "constituent-fanout-50dma")
	out.Status = rpc.RegimeStatusOK
	// "Stale" only applies once we're a full session past the AsOf
	// stamp — the engine refreshes daily, so anything more than ~30 h
	// old is a missed cycle worth flagging.
	if time.Since(envelope.AsOf) > 30*time.Hour {
		out.Status = rpc.RegimeStatusStale
	}
	return out
}

// ----------------------------------------------------------------------------
// Helpers shared across the per-indicator fetchers.

// warnDeps is the per-deps log shim. Production deps wire logWarnf to
// the daemon logger; tests inject a capture closure; nil is a no-op
// for the rare caller that doesn't care.
func warnDeps(d *regimeDeps, format string, args ...any) {
	if d == nil || d.logWarnf == nil {
		return
	}
	d.logWarnf(format, args...)
}

// firmTickQuality builds a Quality for a value that came directly from
// a gateway tick. FreshnessClass tracks live vs frozen based on the
// data-type the gateway labelled the subscription with; Confidence is
// "firm" because the value is a direct gateway measurement (not
// computed from history or a model).
func firmTickQuality(at time.Time, dataType, source string) *rpc.Quality {
	cls := rpc.FreshnessLive
	if !rpc.IsLiveDataType(dataType) {
		cls = rpc.FreshnessFrozen
	}
	return &rpc.Quality{
		AsOf:           at,
		FreshnessClass: cls,
		Confidence:     rpc.ConfidenceFirm,
		Source:         source,
	}
}

// derivedQuality builds a Quality for a value computed from historical
// bars (e.g. a 50-day SMA or a 252-bar max). The freshness class is
// "derived" because the value reflects the most recent close anchoring
// the bar fetch, not a live tick; confidence is "estimate" — a fallback
// when a firm tick was unavailable or always-derived by methodology.
func derivedQuality(at time.Time, source string) *rpc.Quality {
	return &rpc.Quality{
		AsOf:           at,
		FreshnessClass: rpc.FreshnessDerived,
		Confidence:     rpc.ConfidenceEstimate,
		Source:         source,
	}
}

// modelledQuality builds a Quality for a value produced by a model
// (currently only the gamma compute's zero-flip estimate). The Source
// field carries the method token so consumers can deep-link to the
// methodology disclosure without re-reading the spec doc.
func modelledQuality(at time.Time, method string) *rpc.Quality {
	return &rpc.Quality{
		AsOf:           at,
		FreshnessClass: rpc.FreshnessModelled,
		Confidence:     rpc.ConfidenceProxy,
		Source:         method,
	}
}

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

// maxHigh returns the largest High over the last N daily bars
// (oldest-first). If the slice has fewer than N rows the whole slice
// is scanned — partial data is still useful for the 52w-high fallback
// where the indicator needs a best-effort upper bound. Returns 0 only
// on an empty slice.
func maxHigh(bars []ibkrlib.HistoricalBar, n int) float64 {
	if len(bars) == 0 {
		return 0
	}
	tail := bars
	if len(bars) > n {
		tail = bars[len(bars)-n:]
	}
	hi := 0.0
	for _, b := range tail {
		if b.High > hi {
			hi = b.High
		}
	}
	return hi
}
