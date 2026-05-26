package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/marketcal"
	"github.com/osauer/ibkr/internal/rpc"
)

// defaultExpiryIVCap is how many expiries get IV by default — the front
// weeklies, the next few monthlies, plus the next quarterly. Anything
// further out is rarely on the decision path and would burn slot budget
// on every chain refresh. Callers can override via params.AllExpiries.
const defaultExpiryIVCap = 12

// chainExpiryWorkers bounds the per-expiry ATM IV fetcher fan-out.
// The IBKR market-data farm throttles aggressive subscribe churn; 4
// concurrent legs is the documented safe ceiling we already use for the
// chain strikes loop (see handleChainFetch). Higher values trigger
// "market data subscription limit reached" against the entitled slot
// count well before the fan-out wins anything.
const chainExpiryWorkers = 4

// handleChainExpiries returns the sorted, deduped option expiries for the
// underlying. WithIV (default-on via CLI) fetches per-expiry ATM implied
// volatility through a bounded worker pool, with daemon-side caching so
// the second invocation within the TTL is instant. AllExpiries lifts the
// default 12-expiry cap. On any per-strike error the row keeps IV=nil
// with IVStatus="timeout"|"unavailable" — never fail the whole call.
func (s *Server) handleChainExpiries(ctx context.Context, req *rpc.Request) (*rpc.ChainExpiriesResult, error) {
	var p rpc.ChainExpiriesParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	sym := normSym(p.Symbol)
	if sym == "" {
		return nil, errBadRequest("symbol required")
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}

	// Per-stage budget visibility. Captured at INFO so an off-hours
	// timeout shows immediately where the 25 s budget went — "spent 8 s
	// in expiries+strikes, 0 ms in IV fan-out" tells investigators the
	// SECDEF farm is sick, not the IV path.
	start := time.Now()
	var spotMs, expiriesMs, fanoutMs int64
	defer func() {
		s.logger.Infof("chain.expiries %s done in %dms (expiries+strikes=%dms, spot=%dms, iv-fanout=%dms)",
			sym, time.Since(start).Milliseconds(), expiriesMs, spotMs, fanoutMs)
	}()

	tExpiries := time.Now()
	expiries, strikesByExpiry, err := fetchExpiriesAndStrikes(c, sym, 12*time.Second)
	expiriesMs = time.Since(tExpiries).Milliseconds()
	if err != nil {
		return nil, wrapChainExpiriesErr(sym, err)
	}

	res := &rpc.ChainExpiriesResult{
		Symbol:   sym,
		AsOf:     time.Now(),
		Expiries: make([]rpc.ChainExpiry, 0, len(expiries)),
	}

	if !p.WithIV {
		today := todayLocal()
		for _, e := range expiries {
			res.Expiries = append(res.Expiries, rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)})
		}
		return res, nil
	}

	// Cap the expiry list before IV fetch so the slow path stays bounded.
	// expiries is already sorted ascending by fetchExpiriesAndStrikes, so
	// slicing from the front picks the nearest expiries which is what
	// users actually evaluate.
	work := expiries
	if !p.AllExpiries && len(work) > defaultExpiryIVCap {
		work = work[:defaultExpiryIVCap]
	}

	// Spot is required to pick the ATM strike. A single brief subscribe
	// shared across all expiries — pre-fix this ran once before the loop
	// already; only the loop changed shape (parallel + cached).
	tSpot := time.Now()
	spot, _ := s.briefSnapshotPriceHeld(ctx, c, sym, 5*time.Second)
	spotMs = time.Since(tSpot).Milliseconds()
	if spot > 0 {
		res.Spot = spot
	}

	now := time.Now()
	today := todayLocal()
	rows := make([]rpc.ChainExpiry, len(work))
	type job struct {
		idx       int
		expiry    string
		expiryYMD string
		atm       float64
	}
	var jobs []job
	for i, e := range work {
		row := rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)}
		// Cache lookup first — a hit avoids the round-trip entirely.
		if cached, ok := s.expiryIVs.get(sym, e, now); ok {
			if cached.iv > 0 {
				v := cached.iv
				row.IV = &v
			}
			row.IVStatus = cached.status
			rows[i] = row
			continue
		}
		strikes := strikesByExpiry[e]
		if spot <= 0 || len(strikes) == 0 {
			row.IVStatus = "unavailable"
			rows[i] = row
			// Negative-cache so we don't re-poll every refresh.
			s.expiryIVs.put(sym, e, expiryIVEntry{status: "unavailable"}, now)
			continue
		}
		atm := closestStrike(strikes, spot)
		expiryYMD := strings.ReplaceAll(e, "-", "")
		rows[i] = row // populate placeholder; worker will overwrite IV/IVStatus
		jobs = append(jobs, job{idx: i, expiry: e, expiryYMD: expiryYMD, atm: atm})
	}

	// Workers write index-disjoint rows[j.idx], so no per-write mutex is
	// needed — wg.Wait inside runBounded provides happens-before to the
	// caller. The expiryIVs cache is responsible for its own locking.
	tFanout := time.Now()
	runBounded(jobs, chainExpiryWorkers, func(j job) {
		if ctx.Err() != nil {
			return
		}
		iv, status := collectExpiryATMIV(ctx, c, sym, j.expiryYMD, j.atm, 2*time.Second)
		entry := expiryIVEntry{status: status}
		if iv != nil {
			entry.iv = *iv
		}
		s.expiryIVs.put(sym, j.expiry, entry, time.Now())
		if iv != nil {
			rows[j.idx].IV = iv
		}
		rows[j.idx].IVStatus = status
	})
	fanoutMs = time.Since(tFanout).Milliseconds()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decorate each row with the 1-σ implied move now that IV is settled.
	// Pure derivation from spot + IV + DTE — no extra round trips. Skips
	// rows missing any of the three so the field stays nil rather than
	// silently absorbing a zero.
	for i := range rows {
		if mv, mvPct, ok := computeImpliedMove(spot, rows[i].IV, rows[i].DTE); ok {
			rows[i].ImpliedMove = &mv
			rows[i].ImpliedMovePct = &mvPct
		}
	}

	// Append the working set, then the rest (without IV) when caller
	// asked for the full list. AllExpiries=false drops the tail.
	res.Expiries = append(res.Expiries, rows...)
	if p.AllExpiries && len(expiries) > len(work) {
		for _, e := range expiries[len(work):] {
			res.Expiries = append(res.Expiries, rpc.ChainExpiry{Date: e, DTE: dteFromDate(today, e)})
		}
	}
	return res, nil
}

// todayLocal returns today's date at midnight local time. Surfaced as a
// helper so dteFromDate and the no-IV / AllExpiries-tail paths agree on
// the reference instant — they all read the same wall clock at handler
// entry.
func todayLocal() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// dteFromDate returns the calendar day count from today's local date to
// the YYYY-MM-DD expiry. Same-day returns 0; one calendar day out returns
// 1. Returns 0 on parse failure or expired dates — callers treat 0 as
// "intraday" and downstream math (sqrt(DTE/365)) safely yields 0 too.
func dteFromDate(today time.Time, expiry string) int {
	t, err := time.ParseInLocation("2006-01-02", expiry, today.Location())
	if err != nil {
		return 0
	}
	days := int(t.Sub(today).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// computeImpliedMove returns the 1-σ expected dollar move by expiration,
// computed from spot × IV × √(DTE/365). Industry-standard "expected move
// by expiry" formula — same shape the CBOE option calculator uses.
//
// Returns (move, movePct, true) when spot > 0, IV is non-nil and > 0,
// and DTE >= 0. The percent value is `move / spot` (a fraction, so 0.042
// means 4.2%). A DTE of 0 yields a zero move, which is correct: at expiry
// the option's time value collapses to intrinsic.
func computeImpliedMove(spot float64, iv *float64, dte int) (float64, float64, bool) {
	if spot <= 0 || iv == nil || *iv <= 0 || dte < 0 {
		return 0, 0, false
	}
	mv := spot * (*iv) * math.Sqrt(float64(dte)/365.0)
	return mv, mv / spot, true
}

// fetchExpiriesAndStrikes is a small seam for tests — the connector's
// FetchOptionExpiries and FetchOptionExpiryStrikes share an internal fetcher,
// but the daemon needs both halves and the connector public surface returns
// them via separate calls. We do one round trip via the strikes path (which
// is a superset) and derive the sorted-expiry list from the map keys.
var fetchExpiriesAndStrikes = func(connector chainExpiriesConnector, symbol string, timeout time.Duration) ([]string, map[string][]float64, error) {
	strikes, err := connector.FetchOptionExpiryStrikes(symbol, timeout)
	if err != nil {
		return nil, nil, err
	}
	expiries := make([]string, 0, len(strikes))
	for k := range strikes {
		expiries = append(expiries, k)
	}
	slices.Sort(expiries)
	return expiries, strikes, nil
}

// chainExpiriesConnector is the narrow connector surface handleChainExpiries
// uses. Defined here (not in pkg/ibkr) so tests can stub the daemon side
// without lifting the dependency back into the library.
type chainExpiriesConnector interface {
	FetchOptionExpiryStrikes(symbol string, timeout time.Duration) (map[string][]float64, error)
}

// wrapChainExpiriesErr turns the low-level pkg/ibkr errors that surface from
// the chain-expiries fetch into something a user can act on. The big one:
// ErrContractDetailsTimeout, which happens when the IBKR security-definition
// data farm is degraded (often pre-market or just after gateway start). The
// underlying quote subscription typically works in this state — the chain
// path is a separate gateway request that depends on contract resolution.
// Surfacing a generic "internal: timeout" leaves the user guessing whether
// it's a bug, a bad symbol, or a transient gateway condition.
func wrapChainExpiriesErr(symbol string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ibkrlib.ErrContractDetailsTimeout) {
		return &chainContractTimeoutError{symbol: symbol, cause: err}
	}
	return err
}

// chainContractTimeoutError carries the gateway-side "no security definitions"
// case. classifyError maps it to rpc.CodeTimeout so the CLI can recognise it
// distinctly from CodeInternal. The Error() text is the user-facing message —
// keep it short, name the symbol, and point at a concrete next step.
type chainContractTimeoutError struct {
	symbol string
	cause  error
}

func (e *chainContractTimeoutError) Error() string {
	return fmt.Sprintf("option chain unavailable for %s: gateway did not deliver security definitions in time. This is usually transient — try again in a moment, or run `ibkr status` to verify the gateway connection.", e.symbol)
}

func (e *chainContractTimeoutError) Unwrap() error { return e.cause }

// closestStrike picks the strike closest to spot. For ties (which only happens
// when strikes straddle spot equidistantly) the lower strike wins for
// determinism — IBKR's IV surface is symmetric enough that this rarely matters.
func closestStrike(strikes []float64, spot float64) float64 {
	best := strikes[0]
	bestDist := math.Abs(best - spot)
	for _, k := range strikes[1:] {
		d := math.Abs(k - spot)
		if d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}

// collectExpiryATMIV subscribes to the ATM option for one expiry, polls the
// connector's IV cache for up to perStrikeTimeout, then unsubscribes. Returns
// (iv, "ok"), (nil, "timeout"), or (nil, "unavailable") on subscribe failure.
// Takes the connector as an argument so the caller's snapshot is reused —
// avoids re-reading s.connector from inside a per-strike loop where a
// concurrent stopConnector would cause a nil deref.
func collectExpiryATMIV(ctx context.Context, c *ibkrlib.Connector, symbol, expiryYMD string, strike float64, perStrikeTimeout time.Duration) (*float64, string) {
	expiryT, err := time.Parse("20060102", expiryYMD)
	if err != nil {
		return nil, "unavailable"
	}
	reqID, err := c.SubscribeOptionIV(ctx, symbol, expiryT, strike, "C")
	if err != nil {
		return nil, "unavailable"
	}
	// reqID-scoped cancel: the 4-worker fan-out at collectExpiryIVs runs
	// multiple expiries against the same underlier concurrently. A
	// symbol-scoped UnsubscribeMarketData here would either no-op (the
	// common case, since SubscribeOptionIV doesn't install a streaming
	// entry under the symbol) or — worse — tear down an unrelated
	// quote --watch subscription.
	defer c.CancelOptionIV(reqID)

	deadline := time.Now().Add(perStrikeTimeout)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	for {
		if iv, ok := c.GetOptionIV(symbol); ok && iv > 0 {
			v := iv
			return &v, "ok"
		}
		if time.Now().After(deadline) {
			return nil, "timeout"
		}
		select {
		case <-ctx.Done():
			return nil, "timeout"
		case <-poll.C:
		}
	}
}

// handleChainFetch returns ATM ± width strikes for the specified expiry.
// Greeks are populated only when IBKR delivers them.
func (s *Server) handleChainFetch(ctx context.Context, req *rpc.Request) (*rpc.ChainResult, error) {
	var p rpc.ChainFetchParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Symbol == "" {
		return nil, errBadRequest("symbol required")
	}
	if p.Width < 0 {
		return nil, errBadRequest("width must be >= 0")
	}
	if p.Side == "" {
		p.Side = "both"
	}
	if !validChainSide(p.Side) {
		return nil, errBadRequest("side must be calls, puts, or both")
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	expiryYMD, err := normalizeExpiry(p.Expiry)
	if err != nil {
		return nil, err
	}
	dte := daysUntil(expiryYMD)

	// Per-stage budget visibility (same rationale as handleChainExpiries).
	// "snapshot=4s, legs=21s" tells investigators where the 25 s budget
	// went without re-running with debug logging.
	start := time.Now()
	sym := strings.ToUpper(p.Symbol)
	var snapshotMs, legsMs int64
	defer func() {
		s.logger.Infof("chain.fetch %s %s done in %dms (snapshot=%dms, legs=%dms)",
			sym, expiryYMD, time.Since(start).Milliseconds(), snapshotMs, legsMs)
	}()

	tSnapshot := time.Now()
	spot, dataType := s.briefSnapshotPriceHeld(ctx, c, p.Symbol, 5*time.Second)
	if spot <= 0 {
		spot, dataType = chainHistoricalSpotFallback(ctx, c, sym, 5*time.Second)
	}
	snapshotMs = time.Since(tSnapshot).Milliseconds()
	if spot <= 0 {
		if s.gatewayConnector() == nil {
			return nil, ibkrlib.ErrIBKRUnavailable
		}
		return nil, fmt.Errorf("no spot price available for %s (market closed or symbol inactive)", p.Symbol)
	}
	step := strikeStep(spot)
	atm := math.Round(spot/step) * step

	res := &rpc.ChainResult{
		Symbol:       strings.ToUpper(p.Symbol),
		Spot:         spot,
		Expiry:       expiryYMD[:4] + "-" + expiryYMD[4:6] + "-" + expiryYMD[6:8],
		DTE:          dte,
		DataType:     dataType,
		SessionState: rpc.ClassifySession(time.Now()).String(),
		AsOf:         time.Now(),
	}
	if !rpc.IsOptionRTH(res.AsOf) {
		if dataType != "" {
			res.FeedType = dataType
		}
		res.DataType = rpc.MarketDataClosed
	}

	wantCalls := p.Side == "calls" || p.Side == "both"
	wantPuts := p.Side == "puts" || p.Side == "both"

	// Pre-build the strike grid in display order, then fan the per-leg
	// fills out to a bounded worker pool. Pre-fix the loop was sequential
	// — for width=5 both sides that's 22 legs × ~2.5s each ≈ 55s,
	// reliably blowing past the CLI's 60s budget. 4 concurrent legs is
	// the documented safe gateway throttle (v0.2 backlog notes); the
	// gateway-side rate limiter (AcquireMarketDataSlot) serialises
	// further if we'd exceed the entitled slot count.
	n := 2*p.Width + 1
	res.Strikes = make([]rpc.ChainStrike, n)
	for i := -p.Width; i <= p.Width; i++ {
		idx := i + p.Width
		res.Strikes[idx] = rpc.ChainStrike{Strike: atm + float64(i)*step, IsATM: i == 0}
	}

	type job struct {
		idx   int
		right string
	}
	var jobs []job
	for idx := range n {
		if wantCalls {
			jobs = append(jobs, job{idx: idx, right: "C"})
		}
		if wantPuts {
			jobs = append(jobs, job{idx: idx, right: "P"})
		}
	}

	// Two workers can target the same strike (one C-leg, one P-leg)
	// writing disjoint fields. Go's memory model still requires a
	// happens-before for the publish, so one mutex around mergeStrikeSide
	// is plenty — contention is bounded at one merge per leg.
	var mergeMu sync.Mutex
	tLegs := time.Now()
	runBounded(jobs, 4, func(j job) {
		if ctx.Err() != nil {
			return
		}
		var local rpc.ChainStrike
		local.Strike = res.Strikes[j.idx].Strike
		fillOptionLeg(ctx, c, &local, p.Symbol, expiryYMD, local.Strike, j.right)
		mergeMu.Lock()
		mergeStrikeSide(&res.Strikes[j.idx], &local, j.right)
		mergeMu.Unlock()
	})
	legsMs = time.Since(tLegs).Milliseconds()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res.WarningDetails = chainWarningDetails(res, wantCalls, wantPuts)
	return res, nil
}

func chainHistoricalSpotFallback(ctx context.Context, c *ibkrlib.Connector, symbol string, timeout time.Duration) (float64, string) {
	if c == nil || !chainCanUseHistoricalSpot(marketcal.MarketUSEquity, time.Now()) {
		return 0, ""
	}
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	fallbackCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bars, err := c.FetchHistoricalDailyBarsCtx(fallbackCtx, symbol, 10)
	if err != nil {
		return 0, ""
	}
	return chainHistoricalSpotFromBars(bars)
}

func chainCanUseHistoricalSpot(market marketcal.Market, at time.Time) bool {
	session, err := marketcal.New().SessionAt(market, at)
	if err != nil || session.State == marketcal.StateUnknown {
		return false
	}
	return !session.IsOpen
}

func chainHistoricalSpotFromBars(bars []ibkrlib.HistoricalBar) (float64, string) {
	for _, bar := range slices.Backward(bars) {
		if bar.Close > 0 {
			return bar.Close, rpc.MarketDataFrozen
		}
	}
	return 0, ""
}

// mergeStrikeSide copies the side-specific fields (call or put)
// populated by a worker into the shared row. Disjoint by construction
// — the C worker only writes Call*, the P worker only writes Put* —
// but go through one helper so the field list stays in one place.
func mergeStrikeSide(dst, src *rpc.ChainStrike, right string) {
	if right == "C" {
		dst.CallBid = src.CallBid
		dst.CallAsk = src.CallAsk
		dst.CallLast = src.CallLast
		dst.CallPrevClose = src.CallPrevClose
		dst.CallIV = src.CallIV
		dst.CallDelta = src.CallDelta
		dst.CallOI = src.CallOI
		dst.CallAsOf = src.CallAsOf
		dst.CallDataStatus = src.CallDataStatus
		dst.CallIVStatus = src.CallIVStatus
		dst.CallOIStatus = src.CallOIStatus
		return
	}
	dst.PutBid = src.PutBid
	dst.PutAsk = src.PutAsk
	dst.PutLast = src.PutLast
	dst.PutPrevClose = src.PutPrevClose
	dst.PutIV = src.PutIV
	dst.PutDelta = src.PutDelta
	dst.PutOI = src.PutOI
	dst.PutAsOf = src.PutAsOf
	dst.PutDataStatus = src.PutDataStatus
	dst.PutIVStatus = src.PutIVStatus
	dst.PutOIStatus = src.PutOIStatus
}

func fillOptionLeg(ctx context.Context, c *ibkrlib.Connector, row *rpc.ChainStrike, symbol, expiryYMD string, strike float64, right string) {
	// Trading class defaults to the symbol for single-class chain
	// callers (chain.go fetches one underlying at a time and doesn't
	// today distinguish SPX vs SPXW; SubscribeOption's empty-class
	// normalisation matches the SPY pattern). SPX classed enumeration
	// would extend this in step 6 of the gamma SPX coverage arc.
	key, _, err := c.SubscribeOption(ctx, symbol, symbol, expiryYMD, strike, right)
	if err != nil {
		setOptionLegUnavailable(row, right, "subscribe_error")
		return
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	asOf := time.Now()
	deadline := time.Now().Add(2500 * time.Millisecond)
	var bid, ask, last float64
	if err := pollMarketData(ctx, c, key, deadline, func(d *ibkrlib.MarketData) bool {
		asOf = time.Now()
		if d.Bid > 0 || d.Ask > 0 || d.Last > 0 {
			bid, ask, last = d.Bid, d.Ask, d.Last
			return true
		}
		return false
	}); err != nil && err != context.DeadlineExceeded {
		return
	}
	// Tick 13 (model option computation) typically arrives a beat after
	// the first bid/ask print. IV gets its own 1 s budget, capped to
	// whatever's left of the leg's overall deadline so a slow quote can't
	// outlive the per-request budget.
	//
	// Pre-market / after-hours, the option book may have no bid/ask/last
	// but IBKR can still deliver IV via model computation. The poll runs
	// unconditionally so those fills land — costs at most one extra 1 s
	// per dead leg, bounded by the per-request budget.
	var iv float64
	ivDeadline := time.Now().Add(1 * time.Second)
	if ivDeadline.After(deadline) {
		ivDeadline = deadline
	}
	if err := pollUntil(ctx, ivDeadline, func() bool {
		v, ok := c.GetOptionIV(key)
		if ok && v > 0 {
			iv = v
			asOf = time.Now()
			return true
		}
		return false
	}); err != nil && err != context.DeadlineExceeded {
		return
	}
	// Greeks: the same SubscribeOption path drives msg-21 model-
	// computation ticks, so by the time we have IV the per-leg Delta
	// is typically already cached. No extra wait — we just read what
	// landed. Gamma/Theta/Vega aren't surfaced on the chain wire
	// shape today; if a future chain consumer wants them we extend
	// ChainStrike rather than fold them into the same fields.
	var delta *float64
	if g, ok := c.GetOptionGreeks(key); ok {
		// GetOptionGreeks' ok flag is the "at least one field populated
		// from a valid model-computation tick" gate; genuine zero delta
		// (far-OTM near expiry) must surface as a non-nil pointer per
		// the wire contract.
		d := g.Delta
		delta = &d
	}
	var prevClose float64
	if px, ok := c.GetOptionPrevClose(key); ok && px > 0 {
		prevClose = px
	}
	// Opportunistic OI read off the same subscription. Tick types 27
	// (callOpenInterest) and 28 (putOpenInterest) land on the cached
	// MarketData.OpenInt — same pattern gamma uses
	// (internal/daemon/gamma_zero_compute.go:352-357). May be zero off-
	// hours or for illiquid strikes; nil-vs-zero distinction stays on
	// the wire so renderers can differentiate "not delivered" from
	// "genuinely no open interest".
	var oi *int64
	if d, ok := c.GetMarketData()[key]; ok && d.OpenInt > 0 {
		v := d.OpenInt
		oi = &v
	}
	dataStatus := optionLegDataStatus(bid, ask, last, prevClose, iv, delta)
	ivStatus := "unavailable"
	if iv > 0 {
		ivStatus = "ok"
	}
	oiStatus := "unavailable"
	if oi != nil {
		oiStatus = "ok"
	}
	if right == "C" {
		if bid > 0 {
			v := bid
			row.CallBid = &v
		}
		if ask > 0 {
			v := ask
			row.CallAsk = &v
		}
		if last > 0 {
			v := last
			row.CallLast = &v
		}
		if prevClose > 0 {
			v := prevClose
			row.CallPrevClose = &v
		}
		if iv > 0 {
			v := iv
			row.CallIV = &v
		}
		row.CallDelta = delta
		row.CallOI = oi
		row.CallAsOf = asOf
		row.CallDataStatus = dataStatus
		row.CallIVStatus = ivStatus
		row.CallOIStatus = oiStatus
		return
	}
	if bid > 0 {
		v := bid
		row.PutBid = &v
	}
	if ask > 0 {
		v := ask
		row.PutAsk = &v
	}
	if last > 0 {
		v := last
		row.PutLast = &v
	}
	if prevClose > 0 {
		v := prevClose
		row.PutPrevClose = &v
	}
	if iv > 0 {
		v := iv
		row.PutIV = &v
	}
	row.PutDelta = delta
	row.PutOI = oi
	row.PutAsOf = asOf
	row.PutDataStatus = dataStatus
	row.PutIVStatus = ivStatus
	row.PutOIStatus = oiStatus
}

func optionLegDataStatus(bid, ask, last, prevClose, iv float64, delta *float64) string {
	switch {
	case bid > 0 || ask > 0 || last > 0:
		return "quoted"
	case prevClose > 0:
		return "prev_close"
	case iv > 0 || delta != nil:
		return "model_only"
	default:
		return "no_quote"
	}
}

func setOptionLegUnavailable(row *rpc.ChainStrike, right, status string) {
	if status == "" {
		status = "unavailable"
	}
	now := time.Now()
	if right == "C" {
		row.CallAsOf = now
		row.CallDataStatus = status
		row.CallIVStatus = "unavailable"
		row.CallOIStatus = "unavailable"
		return
	}
	row.PutAsOf = now
	row.PutDataStatus = status
	row.PutIVStatus = "unavailable"
	row.PutOIStatus = "unavailable"
}

func chainWarningDetails(res *rpc.ChainResult, wantCalls, wantPuts bool) []rpc.DataWarning {
	if res == nil {
		return nil
	}
	var out []rpc.DataWarning
	if res.DataType == rpc.MarketDataClosed {
		out = append(out, rpc.DataWarning{
			Code:     "options_closed",
			Scope:    res.Symbol,
			Severity: "info",
			Message:  "U.S. listed options are outside regular trading hours.",
			Impact:   "Bid/ask/last and open interest may be unavailable; model IV can still populate from IBKR's option model.",
			Action:   "Retry during 09:30-16:00 ET for executable option quotes.",
		})
	}
	var prevCloseOnly, modelOnly, missingOI, missingIV int
	for _, row := range res.Strikes {
		if wantCalls {
			if row.CallDataStatus == "prev_close" {
				prevCloseOnly++
			}
			if row.CallDataStatus == "model_only" {
				modelOnly++
			}
			if row.CallOIStatus == "unavailable" {
				missingOI++
			}
			if row.CallIVStatus == "unavailable" {
				missingIV++
			}
		}
		if wantPuts {
			if row.PutDataStatus == "prev_close" {
				prevCloseOnly++
			}
			if row.PutDataStatus == "model_only" {
				modelOnly++
			}
			if row.PutOIStatus == "unavailable" {
				missingOI++
			}
			if row.PutIVStatus == "unavailable" {
				missingIV++
			}
		}
	}
	if prevCloseOnly > 0 {
		out = append(out, rpc.DataWarning{
			Code:     "prev_close_legs",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("%d option legs used prior-session close as the only price anchor.", prevCloseOnly),
			Impact:   "The leg has price context but no executable bid/ask/last quote within the fill window.",
			Action:   "Use call_prev_close/put_prev_close only as stale context, not a live fill price.",
		})
	}
	if modelOnly > 0 {
		out = append(out, rpc.DataWarning{
			Code:     "model_only_legs",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("%d option legs returned model data without bid/ask/last.", modelOnly),
			Impact:   "IV/delta may be usable for context, but the legs were not quotable within the fill window.",
		})
	}
	if missingOI > 0 {
		out = append(out, rpc.DataWarning{
			Code:     "oi_unavailable",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("Open interest was unavailable for %d option legs.", missingOI),
			Impact:   "Liquidity and gamma filters should not assume missing OI is zero.",
		})
	}
	if missingIV > 0 {
		out = append(out, rpc.DataWarning{
			Code:     "iv_unavailable",
			Scope:    res.Symbol,
			Severity: "data_quality",
			Message:  fmt.Sprintf("Implied volatility was unavailable for %d option legs.", missingIV),
			Impact:   "The gateway did not deliver a model fit for those strikes within the chain budget.",
		})
	}
	return out
}
