package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

	deps := productionRegimeDeps(c, s.logger.Warnf)
	res := runRegimeFanout(
		ctx,
		func(c context.Context) rpc.RegimeVIXTerm { return fetchRegimeVIXTerm(c, deps) },
		func(c context.Context) rpc.RegimeHYGSPYDivergence { return fetchRegimeHYGSPY(c, deps) },
		func(c context.Context) rpc.RegimeUSDJPY { return fetchRegimeUSDJPY(c, deps) },
		func(c context.Context) rpc.RegimeGammaZero { return fetchRegimeGamma(c, s) },
		func(c context.Context) rpc.RegimeBreadth { return fetchRegimeBreadth(c, s) },
		s.regimeContentionMessage,
	)
	// Tick the streak counters after the fan-out completes. The store
	// classifies each indicator's band using the spec's default
	// thresholds (a slight violation of the wire-shape posture, accepted
	// because streak persistence requires a stable daemon-side
	// classification — see regime_streaks.go for the rationale). Each
	// indicator's StreakInfo is attached to its row before returning.
	s.populateStreaks(res)
	return res, nil
}

// populateStreaks ticks the streak counter for each regime row and
// attaches the resulting *rpc.StreakInfo. Nil-safe on the store side
// (the field stays nil when streaks aren't persisted), and nil-safe on
// the band side (Tick freezes the counter when band="").
func (s *Server) populateStreaks(res *rpc.RegimeSnapshotResult) {
	if s.streaks == nil || res == nil {
		return
	}
	now := nyDateNow()
	// VIX/VIX3M ratio band.
	{
		band := ""
		var value float64
		if res.VIXTermStructure.Status == rpc.RegimeStatusOK || res.VIXTermStructure.Status == rpc.RegimeStatusStale {
			band = classifyVIXTermBand(res.VIXTermStructure.Ratio)
			if res.VIXTermStructure.Ratio != nil {
				value = *res.VIXTermStructure.Ratio
			}
		}
		res.VIXTermStructure.Streak = s.streaks.Tick(StreakKeyVIXTerm, value, band, now)
	}
	// HYG vs SPY band.
	{
		band := ""
		var value float64
		if res.HYGSPYDivergence.Status == rpc.RegimeStatusOK || res.HYGSPYDivergence.Status == rpc.RegimeStatusStale {
			band = classifyHYGSPYBand(res.HYGSPYDivergence)
			if res.HYGSPYDivergence.HYGPrice != nil {
				value = *res.HYGSPYDivergence.HYGPrice
			}
		}
		res.HYGSPYDivergence.Streak = s.streaks.Tick(StreakKeyHYGSPY, value, band, now)
	}
	// USD/JPY weekly-change band.
	{
		band := ""
		var value float64
		if res.USDJPY.Status == rpc.RegimeStatusOK || res.USDJPY.Status == rpc.RegimeStatusStale {
			band = classifyUSDJPYBand(res.USDJPY.WeeklyChange)
			if res.USDJPY.WeeklyChange != nil {
				value = *res.USDJPY.WeeklyChange
			}
		}
		res.USDJPY.Streak = s.streaks.Tick(StreakKeyUSDJPY, value, band, now)
	}
	// Gamma band (only when the envelope landed a ready result).
	{
		band := ""
		var value float64
		if res.GammaZero.Status == rpc.RegimeStatusOK && res.GammaZero.Envelope.Result != nil {
			c := res.GammaZero.Envelope.Result
			band = classifyGammaBand(c.GapPct, c.GammaSign)
			if c.GapPct != nil {
				value = *c.GapPct
			}
		}
		res.GammaZero.Streak = s.streaks.Tick(StreakKeyGammaZero, value, band, now)
	}
	// Breadth band (simplified value-only classification for streak
	// purposes — see regime_streaks.go for the rationale).
	{
		band := ""
		var value float64
		if (res.Breadth.Status == rpc.RegimeStatusOK || res.Breadth.Status == rpc.RegimeStatusStale) && res.Breadth.Envelope.State == rpc.BreadthStateReady {
			value = res.Breadth.Envelope.PctAbove50DMA
			band = classifyBreadthBand(value)
		}
		res.Breadth.Streak = s.streaks.Tick(StreakKeyBreadth, value, band, now)
	}
}

// regimeContentionMessage produces the partial-envelope ErrorMessage
// for the regime fan-out's deadline-fired branch. Reads
// s.backgroundTasks() so the message names the daemon-internal task
// that was running when the deadline fired, rather than the generic
// v0.27.6 hedge "concurrent breadth/gamma work".
//
// Called fresh at deadline-fired time so the names reflect the state
// at that moment, not a stale snapshot from handler entry. The
// empty-list case falls through to a gateway-side hedge — the
// daemon couldn't identify an internal cause, so the contention is
// somewhere else (rate-limit headroom, market-data farm).
func (s *Server) regimeContentionMessage() string {
	tasks := s.backgroundTasks()
	if len(tasks) == 0 {
		return "regime fan-out exceeded handler deadline (gateway-side timeout; no daemon-internal contention detected)"
	}
	names := make([]string, len(tasks))
	for i, t := range tasks {
		names[i] = t.Name
	}
	return fmt.Sprintf("regime fan-out exceeded handler deadline (contended with daemon-internal task(s): %s)", strings.Join(names, ", "))
}

// runRegimeFanout drives the five regime fetchers in parallel and
// returns a consolidated envelope. The function honours ctx's deadline:
// any fetcher that hasn't returned by ctx.Done is surfaced as
// Status=error in the envelope rather than blocking the handler.
//
// Why this exists — pre-v0.27.6 the orchestration used a plain
// wg.Wait() which would hang the handler indefinitely if any one
// fetcher's goroutine blocked past the ctx deadline (e.g. an HMDS
// history fetch queued behind breadth's cold-start fan-out, since the
// legacy FetchHistoricalDailyBars didn't honour parent ctx). The CLI
// then timed out at its own 60 s budget and the user saw
// "regime: context deadline exceeded" — the symptom reported on
// 2026-05-19 that motivated v0.27.6.
//
// Lingering goroutines exit cleanly: the buffered results channel
// (cap 5) accepts late sends without blocking; the late values are
// garbage-collected once the caller has returned. Gateway slots stay
// held only as long as the per-call timeouts the fetchers already set
// on their own derived contexts (productionRegimeDeps uses
// FetchHistoricalDailyBarsCtx, which respects them).
//
// contentionMsg is called fresh at the deadline-fired branch to
// produce the partial-envelope ErrorMessage. Production wires it to
// Server.regimeContentionMessage so the message names the daemon-
// internal task(s) running at deadline time; tests pass a fixed
// closure.
//
// The function is package-private and takes the closures so tests
// can drive it without constructing a full Server fixture — see
// TestRunRegimeFanout_ReturnsOnCtxDoneWithPartialEnvelope and
// TestRunRegimeFanout_PartialEnvelopeUsesContentionMessage.
func runRegimeFanout(
	ctx context.Context,
	vix func(context.Context) rpc.RegimeVIXTerm,
	hyg func(context.Context) rpc.RegimeHYGSPYDivergence,
	usdjpy func(context.Context) rpc.RegimeUSDJPY,
	gamma func(context.Context) rpc.RegimeGammaZero,
	breadth func(context.Context) rpc.RegimeBreadth,
	contentionMsg func() string,
) *rpc.RegimeSnapshotResult {
	res := &rpc.RegimeSnapshotResult{
		SpecDoc: "docs/specs/risk-regime-dashboard.md",
	}

	type regimeRow struct {
		kind string
		v    any
	}
	results := make(chan regimeRow, 5)
	go func() { results <- regimeRow{"vix", vix(ctx)} }()
	go func() { results <- regimeRow{"hyg", hyg(ctx)} }()
	go func() { results <- regimeRow{"usdjpy", usdjpy(ctx)} }()
	go func() { results <- regimeRow{"gamma", gamma(ctx)} }()
	go func() { results <- regimeRow{"breadth", breadth(ctx)} }()

	received := make(map[string]bool, 5)
	deadlineFired := false
	for len(received) < 5 && !deadlineFired {
		select {
		case r := <-results:
			switch r.kind {
			case "vix":
				res.VIXTermStructure = r.v.(rpc.RegimeVIXTerm)
			case "hyg":
				res.HYGSPYDivergence = r.v.(rpc.RegimeHYGSPYDivergence)
			case "usdjpy":
				res.USDJPY = r.v.(rpc.RegimeUSDJPY)
			case "gamma":
				res.GammaZero = r.v.(rpc.RegimeGammaZero)
			case "breadth":
				res.Breadth = r.v.(rpc.RegimeBreadth)
			}
			received[r.kind] = true
		case <-ctx.Done():
			deadlineFired = true
		}
	}
	if deadlineFired {
		// Fill any rows that didn't complete with an honest error
		// envelope so the wire payload is never half-filled. In
		// practice the laggard is one of vix/hyg/usdjpy — gamma and
		// breadth read in-memory state and shouldn't be missing here —
		// but we cover all five defensively.
		exceededMsg := contentionMsg()
		if !received["vix"] {
			res.VIXTermStructure = rpc.RegimeVIXTerm{Notes: vixTermNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["hyg"] {
			res.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{Notes: hygSpyNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["usdjpy"] {
			res.USDJPY = rpc.RegimeUSDJPY{Symbol: "USD.JPY", Notes: usdJpyNotes, Status: rpc.RegimeStatusError, ErrorMessage: exceededMsg}
		}
		if !received["gamma"] {
			res.GammaZero = rpc.RegimeGammaZero{Notes: gammaNotes, Status: rpc.RegimeStatusError}
		}
		if !received["breadth"] {
			res.Breadth = rpc.RegimeBreadth{Notes: breadthNotes, Status: rpc.RegimeStatusError}
		}
	}

	res.AsOf = time.Now()
	return res
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
	// snapshot returns price + previous regular-session close (tick 9) +
	// gateway data-type. PrevClose is the same anchor tick 9 emits
	// alongside the price triple — surfacing it here lets the dashboard
	// header carry day-over-day change for SPY and VIX without a second
	// subscribe. PrevClose is 0 when the gateway didn't deliver tick 9 in
	// the budget; callers must distinguish "not arrived" from "zero".
	snapshot            func(ctx context.Context, sym string, timeout time.Duration) (price, prevClose float64, dataType string)
	snapshotWith52WHigh func(ctx context.Context, sym string, timeout time.Duration) (price, prevClose, week52High float64, dataType string)
	// history takes ctx instead of an explicit timeout so cancellation
	// from handleRegimeSnapshot's outer deadline propagates into the
	// HMDS fetch. The fetcher wraps each call in context.WithTimeout
	// for its own per-call budget; canceling either the parent ctx or
	// the per-call ctx unblocks the call. See v0.27.6 changelog for
	// the bug class this guards against.
	history  func(ctx context.Context, sym string, days int) ([]ibkrlib.HistoricalBar, error)
	logWarnf func(format string, args ...any)
}

// productionRegimeDeps wires the deps struct to the live connector.
// Tests pass a hand-rolled regimeDeps with closures returning canned
// values instead.
func productionRegimeDeps(c *ibkrlib.Connector, logWarnf func(format string, args ...any)) *regimeDeps {
	return &regimeDeps{
		snapshot: func(ctx context.Context, sym string, timeout time.Duration) (float64, float64, string) {
			return briefSnapshotPriceWithClose(ctx, c, sym, timeout)
		},
		snapshotWith52WHigh: func(ctx context.Context, sym string, timeout time.Duration) (float64, float64, float64, string) {
			return briefSnapshotPriceWith52WHigh(ctx, c, sym, timeout)
		},
		history: func(ctx context.Context, sym string, days int) ([]ibkrlib.HistoricalBar, error) {
			return c.FetchHistoricalDailyBarsCtx(ctx, sym, days)
		},
		logWarnf: logWarnf,
	}
}

// ----------------------------------------------------------------------------
// Per-indicator fetchers. Each one returns a fully-populated row even on
// failure — the regime envelope never carries nil sub-objects.

// boundedSnapshot bounds the wall time of deps.snapshot to ~budget+1s,
// regardless of whether deps.snapshot itself honours ctx all the way
// down. Kept as defense-in-depth after F-26 closed the structural gap
// that originally motivated it.
//
// History:
//
//   - v0.27.5 fixed a hard hang in SubscribeMarketData.
//   - v0.27.6 stopped a 45s envelope-level deadline from clobbering one-row
//     errors so a slow leg surfaced cleanly.
//   - v0.27.9 added this wrapper because the inner pkg/ibkr.acquireMarketDataSlot
//     used Connection.ctx, not the caller's ctx — a fetcher that hit slot
//     exhaustion would block past its 5s budget (the inner pollUntil never
//     ran because SubscribeMarketData never returned) and only bail at the
//     orchestrator's 45s handler ctx. The wrapper races deps.snapshot in a
//     goroutine and returns zeros after budget+1s regardless of inner ctx
//     honouring.
//   - F-26 (v0.27.11) threaded ctx through SubscribeMarketData →
//     RequestMarketDataWithContract → acquireMarketDataSlot so the budget
//     is enforced at the slot-acquire layer. The inner code now honours
//     ctx end-to-end and this wrapper is no longer load-bearing.
//
// We keep the wrapper anyway: it costs nothing in the happy path (the
// timer fires only after budget+1s, well past inner completion) and
// catches future regressions in either the slot path or any other
// inner code that might block past its declared budget.
//
// If the goroutine times out it leaks until it returns naturally;
// callers map zero values to a row-level "no spot tick" status.
func boundedSnapshot(ctx context.Context, deps *regimeDeps, sym string, budget time.Duration) (price, prevClose float64, dataType string) {
	type r struct {
		price, prevClose float64
		dt               string
	}
	resCh := make(chan r, 1)
	go func() {
		p, pc, d := deps.snapshot(ctx, sym, budget)
		resCh <- r{p, pc, d}
	}()
	// One-second slack over budget so deps.snapshot has a chance to
	// return its own deadline error before we bail. The slack matters
	// when the inner code DOES honour ctx — without it, we'd race the
	// inner deadline and lose, returning zeros instead of the inner
	// path's classified result.
	select {
	case got := <-resCh:
		return got.price, got.prevClose, got.dt
	case <-time.After(budget + time.Second):
		return 0, 0, ""
	case <-ctx.Done():
		return 0, 0, ""
	}
}

// boundedSnapshotWith52WHigh is the boundedSnapshot wrapper for the
// snapshotWith52WHigh dep variant. Same rationale and structure.
func boundedSnapshotWith52WHigh(ctx context.Context, deps *regimeDeps, sym string, budget time.Duration) (price, prevClose, week52High float64, dataType string) {
	type r struct {
		price, prevClose, week52High float64
		dt                           string
	}
	resCh := make(chan r, 1)
	go func() {
		p, pc, w, d := deps.snapshotWith52WHigh(ctx, sym, budget)
		resCh <- r{p, pc, w, d}
	}()
	select {
	case got := <-resCh:
		return got.price, got.prevClose, got.week52High, got.dt
	case <-time.After(budget + time.Second):
		return 0, 0, 0, ""
	case <-ctx.Done():
		return 0, 0, 0, ""
	}
}

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
	vix, vixPrev, vixDT := boundedSnapshot(ctx, deps, "VIX", 5*time.Second)
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
	vix3m, _, vix3mDT := boundedSnapshot(ctx, deps, "VIX3M", 8*time.Second)
	// Populate the VIX day-change anchor as soon as the close lands —
	// independent of whether VIX3M arrives. The dashboard header is
	// useful even when the ratio leg fails.
	if vixPrev > 0 {
		out.VIXPrevClose = new(vixPrev)
		chg := (vix - vixPrev) / vixPrev * 100
		out.VIXChangePct = &chg
	}
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

// HYGLookbackDays is the calendar-day window passed to the HMDS
// history fetch when computing HYG's 50-day SMA. 50 trading days ≈ 70
// calendar days when the window has zero holidays; the US market
// closes 9-10 days per year, so a 70-day window can come up short on
// the wrong side of Memorial Day / Labor Day / Thanksgiving. 90
// calendar days gives ~10 days of slack — the IBKR HMDS API only
// bills the call, not the bar count, so this is free. Widened from
// 70 to 90 in v0.23.0 (commit 02aba13).
const HYGLookbackDays = 90

func fetchRegimeHYGSPY(ctx context.Context, deps *regimeDeps) rpc.RegimeHYGSPYDivergence {
	out := rpc.RegimeHYGSPYDivergence{Notes: hygSpyNotes}
	now := time.Now()

	hyg, _, hygDT := boundedSnapshot(ctx, deps, "HYG", 5*time.Second)
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
	spy, spyPrev, spy52, spyDT := boundedSnapshotWith52WHigh(ctx, deps, "SPY", 8*time.Second)
	if spy > 0 {
		out.SPYPrice = new(spy)
		out.SPYQuality = firmTickQuality(now, spyDT, "SPY tick")
	}
	// SPY day-change anchor: same tick-9 close the subscribe captures
	// alongside the price triple. Surfaces to the dashboard header so
	// the reader sees "SPY 530.42 +1.20 (+0.23%)" at the top without a
	// separate quote call.
	if spy > 0 && spyPrev > 0 {
		out.SPYPrevClose = new(spyPrev)
		diff := spy - spyPrev
		out.SPYChange = &diff
		pct := diff / spyPrev * 100
		out.SPYChangePct = &pct
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
		hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
		spyBars, err := deps.history(hctx, "SPY", 365)
		hcancel()
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

	// 50-day SMA on HYG. See HYGLookbackDays for the
	// calendar-day window's holiday-clipping rationale.
	hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
	bars, err := deps.history(hctx, "HYG", HYGLookbackDays)
	hcancel()
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

// USDJPYLookbackDays is the calendar-day window passed to the HMDS
// history fetch when computing the 7-trading-day close for USD/JPY.
// FX trades 24/5 so 7 trading days = 7 weekday FX sessions. 14
// calendar days covers 7 FX sessions even when a Monday or Friday
// bank holiday interrupts the count (US: MLK Day, Memorial Day,
// Labor Day, Thanksgiving, etc. all fall on Mondays and clip one
// US-tradable FX day). Widened from 12 to 14 in v0.23.0
// (commit 02aba13).
const USDJPYLookbackDays = 14

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
	last, _, dt := boundedSnapshot(ctx, deps, "USD.JPY", 5*time.Second)
	if last <= 0 {
		out.Status = rpc.RegimeStatusUnavailable
		out.ErrorMessage = "USD.JPY: gateway delivered no FX tick (check IDEALPRO entitlement)"
		return out
	}
	out.Last = new(last)
	out.LastQuality = firmTickQuality(now, dt, "USD.JPY CASH tick (IDEALPRO)")
	out.DataType = dt

	// 7-trading-days-ago close. FX history uses MIDPOINT bars
	// (defaultHistoricalWhat for CASH). See USDJPYLookbackDays for
	// the calendar-day window's holiday-clipping rationale.
	hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
	bars, err := deps.history(hctx, "USD.JPY", USDJPYLookbackDays)
	hcancel()
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

const gammaNotes = "SPY dealer γ-zero level (the spot where dealer net gamma crosses zero). Spec thresholds: SPY >2% above γ-zero (green, stabilizing); within 2% (yellow, regime can flip on a single session); below (red, amplifying). The crossing itself is the regime event — no waiting period. Underlying is SPY (the S&P 500 ETF) rather than SPX (the index) so the compute is robust off-hours: SPY trades extended hours with continuous market-maker quotes and a single trading class, which keeps option IV ticks flowing and the chain enumeration clean. The regime signal tracks SPX dealer gamma closely; absolute level is SPY-scale (~SPX/10). Methodology v2 (`perfiliev-bs-sweep-v2-stickymoneyness`): Perfiliev BS-sweep over 6 nearest non-0DTE-post-settlement expirations × ±10% strikes. The sweep now reprices each leg's IV at the scenario-spot's moneyness via a per-expiry quadratic skew curve fitted at snapshot time — sticky-moneyness rather than sticky-IV — so the zero-gamma estimate shifts ~30-80 SPX points relative to the v1 recipe and tracks SpotGamma's posted numbers materially better. Curves that fail to fit (fewer than 3 IV samples or degenerate solve) fall back to sticky-IV for that expiry only; surface as `skew_fallback:YYYYMMDD` warnings. The envelope also carries separate γ-zero readings for the near bucket (DTE ≤ 7 days; ~59% of 2025 SPX volume is 0DTE) and the term bucket (DTE > 7); the `horizon_agreement` field flags `diverge` when the two readings land on opposite sides of spot — the high-information case. Sign convention assumes 2018-era dealers-long-calls-short-puts (regime hint, not precise level; documented caveats around covered-call ETFs and autocallable hedging). Pre-market: when the gateway's model-computation engine is idle, the compute falls back to Black-Scholes Newton-Raphson on each option's prior-session close to back-solve IV; legs using the fallback are counted in derived_iv_legs. First regime call of an NY trading day auto-kicks the heavy compute; subsequent calls return the cached result. The envelope's gamma_total_abs + top_strikes give the sign-agnostic magnitude signal which is more robust than the signed γ-zero level when positioning is unusual."

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
			out.HorizonAgreement = classifyHorizonAgreement(envelope.Result)
		}
	case rpc.GammaZeroStatusComputing:
		out.Status = rpc.RegimeStatusComputing
	case rpc.GammaZeroStatusCold:
		// Cold means no compute has ever been kicked this session. In
		// practice handleGammaZeroSPX auto-kicks on every call so a
		// regime fetch typically transitions Cold → Computing inside
		// the same call. Map to Unavailable for the rare interleaving
		// where the snapshot races a kick — mirrors breadth's Cold →
		// Unavailable mapping below at fetchRegimeBreadth.
		out.Status = rpc.RegimeStatusUnavailable
	case rpc.GammaZeroStatusError:
		out.Status = rpc.RegimeStatusError
	default:
		out.Status = rpc.RegimeStatusError
	}
	return out
}

const breadthNotes = "S&P 500 breadth — the daemon computes two SMA readings and the new-52-week-highs/lows count locally from the 500 constituent daily closes (IBKR doesn't redistribute the underlying S&P DJI / NYSE breadth indices on retail subscriptions). Refresh runs once per US trading day post-close (16:35 ET). Method token: constituent-fanout-50/200dma-hl. The 50-day reading (`pct_above_50dma`) keeps the spec's bands: >55 green / 40-55 yellow / <40 with SPX within 3% of 52-week high is the textbook late-cycle divergence (red). The 200-day reading (`pct_above_200dma`) uses 60/40 bands calibrated to the post-Mag-7 era: >60 green / 40-60 yellow / <40 red (the StockCharts 70/30 default fires red far too often in this regime). New-highs/lows surface as a sub-signal: when SPX is near highs and `net_new_highs_pct` is near zero or negative, that's the classic narrow-rally pattern — a small set of mega-caps carrying the index while the median name is rolling over."

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
	out.ValueQuality = derivedQuality(envelope.AsOf, envelope.Method)
	// Echo the four sub-fields onto the regime row so a consumer
	// doesn't have to dig into the nested envelope for the standard
	// breadth view that informs the band.
	out.PctAbove50DMA = envelope.PctAbove50DMA
	out.PctAbove200DMA = envelope.PctAbove200DMA
	out.NewHighsToday = envelope.NewHighsToday
	out.NewLowsToday = envelope.NewLowsToday
	out.NetNewHighsPct = envelope.NetNewHighsPct
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

// classifyHorizonAgreement compares the gamma compute's near (DTE ≤ 7)
// and term (DTE > 7) zero-gamma readings and names how they relate.
// Returns one of the documented HorizonAgreement strings — see
// rpc.RegimeGammaZero.HorizonAgreement for the meanings. Empty string
// when both buckets are no-crossing or no-data (the headline already
// carries that case).
func classifyHorizonAgreement(c *rpc.GammaZeroComputed) string {
	if c == nil || c.SpotUnderlying <= 0 {
		return ""
	}
	nearAvail := c.ZeroGammaNear != nil
	termAvail := c.ZeroGammaTerm != nil
	switch {
	case nearAvail && termAvail:
		spotAboveNear := c.SpotUnderlying > *c.ZeroGammaNear
		spotAboveTerm := c.SpotUnderlying > *c.ZeroGammaTerm
		switch {
		case spotAboveNear && spotAboveTerm:
			return "both_above"
		case !spotAboveNear && !spotAboveTerm:
			return "both_below"
		default:
			return "diverge"
		}
	case nearAvail:
		return "near_only"
	case termAvail:
		return "term_only"
	}
	return ""
}
