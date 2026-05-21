package ibkr

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// optionExpiryFetch coalesces the per-exchange msg-75 frames that IBKR emits
// in response to a single reqSecDefOptParams (78). The gateway sends one frame
// per (exchange, trading-class) pair followed by a single msg-76 end marker.
//
// Two views are maintained side-by-side. The legacy `strikes` map dedupes
// across BOTH exchange and trading class for back-compat with single-class
// callers (SPY, equities — `FetchOptionExpiryStrikes`). The `classed` map
// preserves the trading-class qualifier so multi-class callers (SPX's
// `SPX` AM-monthlies + `SPXW` PM-weeklies) can disambiguate same-date
// contracts via `FetchOptionExpiryStrikesClassed`.
type optionExpiryFetch struct {
	mu          sync.Mutex
	expirations map[string]struct{}                        // YYYYMMDD set, deduped across exchanges and classes
	strikes     map[string]map[float64]struct{}            // YYYYMMDD -> set of strikes (legacy, any class)
	classed     map[string]map[string]map[float64]struct{} // YYYYMMDD -> tradingClass -> set of strikes
	done        chan struct{}
}

func newOptionExpiryFetch() *optionExpiryFetch {
	return &optionExpiryFetch{
		expirations: make(map[string]struct{}),
		strikes:     make(map[string]map[float64]struct{}),
		classed:     make(map[string]map[string]map[float64]struct{}),
		done:        make(chan struct{}),
	}
}

// ExpiryClassedStrikes is the strike grid for one (expiry, trading-class)
// pair. SPX's third-Friday cycle lists BOTH an AM-settled SPX contract and a
// PM-settled SPXW contract on the same date, with distinct ConIDs and (in
// general) distinct strike grids — the TradingClass field is the
// discriminator. For single-class underlyings (SPY, equities) the slice
// returned by FetchOptionExpiryStrikesClassed always has length 1.
type ExpiryClassedStrikes struct {
	TradingClass string    `json:"trading_class"`
	Strikes      []float64 `json:"strikes"`
}

// fetchOptionExpiriesData runs one reqSecDefOptParams round trip and returns
// the deduped expirations (YYYY-MM-DD), the per-expiry strike sets (merged
// across classes for back-compat), and the per-expiry per-class strike sets
// for callers that need the class qualifier. Internal helper shared by
// FetchOptionExpiries, FetchOptionExpiryStrikes, and
// FetchOptionExpiryStrikesClassed so the three public APIs share one round
// trip.
func (c *Connector) fetchOptionExpiriesData(symbol string, timeout time.Duration) ([]string, map[string][]float64, map[string][]ExpiryClassedStrikes, error) {
	if !c.IsReady() {
		return nil, nil, nil, ErrIBKRUnavailable
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, nil, nil, fmt.Errorf("FetchOptionExpiries: symbol required")
	}
	if _, inactive := c.inactiveReason(symbol); inactive {
		return nil, nil, nil, ErrSymbolInactive
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return nil, nil, nil, ErrIBKRUnavailable
	}

	// Resolve the underlying conID via the existing contract cache. The chain
	// command and the rest of the daemon already drive contracts through this
	// path; reuse it so option-chain lookups share the same warm cache.
	detail, err := c.ensureContractDetails(symbol, 5*time.Second)
	if err != nil || detail == nil || detail.ConID == 0 {
		// Fall back to the late-arrival grace window historical uses.
		grace := contractDetailsLateGrace
		if half := timeout / 2; half > 0 && half < grace {
			grace = half
		}
		late := c.awaitContractDetail(symbol, grace)
		if late == nil || late.ConID == 0 {
			if err == nil {
				err = fmt.Errorf("contract details unresolved for %s", symbol)
			}
			return nil, nil, nil, err
		}
		detail = late
	}
	secType, _, _, _ := classifySymbol(symbol)
	if secType == "" {
		secType = "STK"
	}

	fetch := newOptionExpiryFetch()
	var (
		registeredReqID    int
		dataHandlerID      uint64
		endHandlerID       uint64
		registeredHandlers bool
	)

	// Register handlers BEFORE the request goes on the wire, but key on the
	// reqID so other in-flight reqSecDefOptParams calls (if any) don't fan in.
	beforeSend := func(reqID int) {
		registeredReqID = reqID
		dataHandlerID = conn.RegisterHandler(msgSecurityDefinitionOptionalParameter, func(fields []string) {
			c.handleSecDefOptParam(reqID, fetch, fields)
		})
		endHandlerID = conn.RegisterHandler(msgSecurityDefinitionOptionalParameterEnd, func(fields []string) {
			c.handleSecDefOptParamEnd(reqID, fetch, fields)
		})
		registeredHandlers = true
	}

	_, err = conn.RequestSecDefOptParams(symbol, "", secType, detail.ConID, beforeSend)
	if err != nil {
		if registeredHandlers {
			conn.UnregisterHandler(msgSecurityDefinitionOptionalParameter, dataHandlerID)
			conn.UnregisterHandler(msgSecurityDefinitionOptionalParameterEnd, endHandlerID)
		}
		return nil, nil, nil, fmt.Errorf("reqSecDefOptParams: %w", err)
	}
	defer func() {
		conn.UnregisterHandler(msgSecurityDefinitionOptionalParameter, dataHandlerID)
		conn.UnregisterHandler(msgSecurityDefinitionOptionalParameterEnd, endHandlerID)
	}()
	_ = registeredReqID

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Wait for end marker or timeout. On timeout we still return whatever we
	// observed across exchanges — partial data is more useful than nothing for
	// the listing UX, and the IBKR-spec end marker is best-effort during
	// degraded data-farm conditions.
	timedOut := false
	select {
	case <-fetch.done:
	case <-timer.C:
		timedOut = true
	}

	expiries, strikes, classed := fetch.snapshot()
	if len(expiries) == 0 && timedOut {
		return nil, nil, nil, fmt.Errorf("option expiries timeout for %s after %s", symbol, timeout)
	}
	return expiries, strikes, classed, nil
}

// FetchOptionExpiries returns the sorted, deduped list of available option
// expiries for the given equity underlying, in YYYY-MM-DD form. It issues
// reqSecDefOptParams (msg 78) and aggregates all per-exchange
// SecurityDefinitionOptionalParameter (msg 75) responses until the end marker
// (msg 76) arrives or the timeout fires. On timeout with at least one frame
// already observed it returns the partial slice; on a fully empty timeout it
// returns an error so callers can surface it as gateway_unavailable.
func (c *Connector) FetchOptionExpiries(symbol string, timeout time.Duration) ([]string, error) {
	expiries, _, _, err := c.fetchOptionExpiriesData(symbol, timeout)
	if err != nil {
		return nil, err
	}
	return expiries, nil
}

// FetchOptionExpiryStrikes returns strikes per expiry as observed across
// exchanges AND trading classes. Used by --with-iv to pick the ATM strike
// per expiry without re-issuing the request. The map key is YYYY-MM-DD.
//
// For multi-class underlyings (SPX has both `SPX` AM-settled monthlies and
// `SPXW` PM-settled weeklies, distinct on third-Fridays), this merges the
// classes into one strike grid per date. Callers that need to keep the
// classes separated use FetchOptionExpiryStrikesClassed.
func (c *Connector) FetchOptionExpiryStrikes(symbol string, timeout time.Duration) (map[string][]float64, error) {
	_, strikes, _, err := c.fetchOptionExpiriesData(symbol, timeout)
	if err != nil {
		return nil, err
	}
	return strikes, nil
}

// FetchOptionExpiryStrikesClassed returns the strike grid for each
// (expiry, tradingClass) pair the gateway lists. SPY-style underlyings
// with a single trading class return one ExpiryClassedStrikes per expiry
// date; SPX returns multiple per-date entries on third-Fridays — one for
// the AM-settled `SPX` monthly and one for the PM-settled `SPXW` weekly
// that happens to share the third-Friday slot.
//
// The class qualifier matters for the gamma compute: same date, same
// strike, two contracts, two ConIDs, two settlement times. Without the
// class the entries collide in the option-contract cache and the gamma
// compute mis-prices half a day of time-to-expiry.
func (c *Connector) FetchOptionExpiryStrikesClassed(symbol string, timeout time.Duration) (map[string][]ExpiryClassedStrikes, error) {
	_, _, classed, err := c.fetchOptionExpiriesData(symbol, timeout)
	if err != nil {
		return nil, err
	}
	return classed, nil
}

// handleSecDefOptParam decodes one msg-75 frame and merges its expirations
// and strikes into the shared fetch state. Per the IBKR Python ibapi
// reference (decoder.processSecurityDefinitionOptionParameterMsg), the wire
// fields after the msgID are:
//
//	[reqID, exchange, underlyingConId, tradingClass, multiplier,
//	 expCount, expirations*expCount, strikeCount, strikes*strikeCount]
//
// Note: there is no version field — msg 78 was added after IBKR moved to the
// versionless request-numbered protocol.
//
// We capture `tradingClass` from fields[4] and write the strike grid into
// BOTH the legacy `strikes` map (deduped across classes, for back-compat) and
// the new `classed` map (per-class, for the SPX path). The dual write keeps
// FetchOptionExpiryStrikes byte-for-byte unchanged while
// FetchOptionExpiryStrikesClassed surfaces the SPX vs SPXW split.
func (c *Connector) handleSecDefOptParam(expectedReqID int, fetch *optionExpiryFetch, fields []string) {
	if len(fields) < 7 {
		return
	}
	// fields[0] = msgID
	rid, err := strconv.Atoi(fields[1])
	if err != nil || rid != expectedReqID {
		return
	}
	// fields[2] = exchange (we keep it implicit — dedupe across all exchanges)
	// fields[3] = underlyingConId, fields[4] = tradingClass, fields[5] = multiplier
	tradingClass := strings.TrimSpace(fields[4])
	idx := 6
	expCount, err := strconv.Atoi(fields[idx])
	if err != nil || expCount < 0 {
		return
	}
	idx++
	if idx+expCount > len(fields) {
		return
	}
	expirations := fields[idx : idx+expCount]
	idx += expCount

	if idx >= len(fields) {
		return
	}
	strikeCount, err := strconv.Atoi(fields[idx])
	if err != nil || strikeCount < 0 {
		return
	}
	idx++
	if idx+strikeCount > len(fields) {
		return
	}
	strikeStrings := fields[idx : idx+strikeCount]

	parsedStrikes := make([]float64, 0, strikeCount)
	for _, s := range strikeStrings {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		parsedStrikes = append(parsedStrikes, v)
	}

	fetch.mu.Lock()
	defer fetch.mu.Unlock()
	for _, exp := range expirations {
		exp = strings.TrimSpace(exp)
		if exp == "" {
			continue
		}
		fetch.expirations[exp] = struct{}{}
		// Legacy: deduped across all classes — back-compat for SPY callers.
		set, ok := fetch.strikes[exp]
		if !ok {
			set = make(map[float64]struct{})
			fetch.strikes[exp] = set
		}
		for _, k := range parsedStrikes {
			set[k] = struct{}{}
		}
		// Classed: keyed by tradingClass so SPX vs SPXW stay separated.
		// Empty tradingClass (unexpected — IBKR always fills it in
		// practice) buckets under "" rather than merging into a sibling.
		byClass, ok := fetch.classed[exp]
		if !ok {
			byClass = make(map[string]map[float64]struct{})
			fetch.classed[exp] = byClass
		}
		classSet, ok := byClass[tradingClass]
		if !ok {
			classSet = make(map[float64]struct{})
			byClass[tradingClass] = classSet
		}
		for _, k := range parsedStrikes {
			classSet[k] = struct{}{}
		}
	}
}

// handleSecDefOptParamEnd closes the fetch's done channel exactly once.
// IBKR sends one msg-76 per request; we tolerate a duplicate as a no-op.
func (c *Connector) handleSecDefOptParamEnd(expectedReqID int, fetch *optionExpiryFetch, fields []string) {
	if len(fields) < 2 {
		return
	}
	rid, err := strconv.Atoi(fields[1])
	if err != nil || rid != expectedReqID {
		return
	}
	fetch.mu.Lock()
	defer fetch.mu.Unlock()
	select {
	case <-fetch.done:
		return
	default:
		close(fetch.done)
	}
}

// snapshot returns the deduped, normalised expiry list (YYYY-MM-DD,
// ascending), the per-expiry sorted strike list (legacy, merged across
// classes), and the per-expiry per-class slice (new, SPX-aware). Safe to
// call multiple times.
func (f *optionExpiryFetch) snapshot() ([]string, map[string][]float64, map[string][]ExpiryClassedStrikes) {
	f.mu.Lock()
	defer f.mu.Unlock()

	expiries := make([]string, 0, len(f.expirations))
	for raw := range f.expirations {
		if normalised, ok := normaliseExpiry8(raw); ok {
			expiries = append(expiries, normalised)
		}
	}
	slices.Sort(expiries)

	strikes := make(map[string][]float64, len(f.strikes))
	for raw, set := range f.strikes {
		normalised, ok := normaliseExpiry8(raw)
		if !ok {
			continue
		}
		out := make([]float64, 0, len(set))
		for k := range set {
			out = append(out, k)
		}
		slices.Sort(out)
		// Multiple raw expiries from different exchanges can normalise to the
		// same key; merge instead of overwriting.
		if existing, ok := strikes[normalised]; ok {
			merged := append(existing, out...)
			slices.Sort(merged)
			strikes[normalised] = dedupeFloats(merged)
		} else {
			strikes[normalised] = out
		}
	}

	classed := make(map[string][]ExpiryClassedStrikes, len(f.classed))
	for raw, byClass := range f.classed {
		normalised, ok := normaliseExpiry8(raw)
		if !ok {
			continue
		}
		// Stable class ordering so the gamma compute's two-pass prewarm
		// sees the same class order across runs; aids debugging.
		classNames := make([]string, 0, len(byClass))
		for cls := range byClass {
			classNames = append(classNames, cls)
		}
		slices.Sort(classNames)
		for _, cls := range classNames {
			set := byClass[cls]
			out := make([]float64, 0, len(set))
			for k := range set {
				out = append(out, k)
			}
			slices.Sort(out)
			classed[normalised] = append(classed[normalised], ExpiryClassedStrikes{
				TradingClass: cls,
				Strikes:      out,
			})
		}
	}
	return expiries, strikes, classed
}

// normaliseExpiry8 converts IBKR's YYYYMMDD wire form into the YYYY-MM-DD
// canonical form used elsewhere in the project (matches barDate). Returns
// false for malformed input so callers can drop the row.
func normaliseExpiry8(raw string) (string, bool) {
	if len(raw) != 8 {
		return "", false
	}
	for i := range 8 {
		if raw[i] < '0' || raw[i] > '9' {
			return "", false
		}
	}
	return raw[:4] + "-" + raw[4:6] + "-" + raw[6:], true
}

func dedupeFloats(in []float64) []float64 {
	if len(in) <= 1 {
		return in
	}
	out := in[:1]
	for i := 1; i < len(in); i++ {
		if in[i] != out[len(out)-1] {
			out = append(out, in[i])
		}
	}
	return out
}
