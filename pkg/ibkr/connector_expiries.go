package ibkr

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// optionExpiryFetch coalesces the per-exchange msg-75 frames that IBKR emits
// in response to a single reqSecDefOptParams (78). The gateway sends one frame
// per exchange (SMART, AMEX, CBOE, …) followed by a single msg-76 end marker.
type optionExpiryFetch struct {
	mu          sync.Mutex
	expirations map[string]struct{}             // YYYYMMDD set, deduped across exchanges
	strikes     map[string]map[float64]struct{} // YYYYMMDD -> set of strikes (any exchange)
	done        chan struct{}
}

func newOptionExpiryFetch() *optionExpiryFetch {
	return &optionExpiryFetch{
		expirations: make(map[string]struct{}),
		strikes:     make(map[string]map[float64]struct{}),
		done:        make(chan struct{}),
	}
}

// fetchOptionExpiriesData runs one reqSecDefOptParams round trip and returns
// the deduped expirations (YYYY-MM-DD) and the per-expiry strike sets observed
// across exchanges. Internal helper shared by FetchOptionExpiries and
// FetchOptionExpiryStrikes so callers don't pay for two round trips.
func (c *Connector) fetchOptionExpiriesData(symbol string, timeout time.Duration) ([]string, map[string][]float64, error) {
	if !c.IsReady() {
		return nil, nil, ErrIBKRUnavailable
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, nil, fmt.Errorf("FetchOptionExpiries: symbol required")
	}
	if _, inactive := c.inactiveReason(symbol); inactive {
		return nil, nil, ErrSymbolInactive
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return nil, nil, ErrIBKRUnavailable
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
			return nil, nil, err
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
		return nil, nil, fmt.Errorf("reqSecDefOptParams: %w", err)
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

	expiries, strikes := fetch.snapshot()
	if len(expiries) == 0 && timedOut {
		return nil, nil, fmt.Errorf("option expiries timeout for %s after %s", symbol, timeout)
	}
	return expiries, strikes, nil
}

// FetchOptionExpiries returns the sorted, deduped list of available option
// expiries for the given equity underlying, in YYYY-MM-DD form. It issues
// reqSecDefOptParams (msg 78) and aggregates all per-exchange
// SecurityDefinitionOptionalParameter (msg 75) responses until the end marker
// (msg 76) arrives or the timeout fires. On timeout with at least one frame
// already observed it returns the partial slice; on a fully empty timeout it
// returns an error so callers can surface it as gateway_unavailable.
func (c *Connector) FetchOptionExpiries(symbol string, timeout time.Duration) ([]string, error) {
	expiries, _, err := c.fetchOptionExpiriesData(symbol, timeout)
	if err != nil {
		return nil, err
	}
	return expiries, nil
}

// FetchOptionExpiryStrikes returns strikes per expiry as observed across
// exchanges. Used by --with-iv to pick the ATM strike per expiry without
// re-issuing the request. The map key is YYYY-MM-DD.
func (c *Connector) FetchOptionExpiryStrikes(symbol string, timeout time.Duration) (map[string][]float64, error) {
	_, strikes, err := c.fetchOptionExpiriesData(symbol, timeout)
	if err != nil {
		return nil, err
	}
	return strikes, nil
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
		set, ok := fetch.strikes[exp]
		if !ok {
			set = make(map[float64]struct{})
			fetch.strikes[exp] = set
		}
		for _, k := range parsedStrikes {
			set[k] = struct{}{}
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

// snapshot returns the deduped, normalised expiry list (YYYY-MM-DD, ascending)
// and the per-expiry sorted strike list. Safe to call multiple times.
func (f *optionExpiryFetch) snapshot() ([]string, map[string][]float64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	expiries := make([]string, 0, len(f.expirations))
	for raw := range f.expirations {
		if normalised, ok := normaliseExpiry8(raw); ok {
			expiries = append(expiries, normalised)
		}
	}
	sort.Strings(expiries)

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
		sort.Float64s(out)
		// Multiple raw expiries from different exchanges can normalise to the
		// same key; merge instead of overwriting.
		if existing, ok := strikes[normalised]; ok {
			merged := append(existing, out...)
			sort.Float64s(merged)
			strikes[normalised] = dedupeFloats(merged)
		} else {
			strikes[normalised] = out
		}
	}
	return expiries, strikes
}

// normaliseExpiry8 converts IBKR's YYYYMMDD wire form into the YYYY-MM-DD
// canonical form used elsewhere in the project (matches barDate). Returns
// false for malformed input so callers can drop the row.
func normaliseExpiry8(raw string) (string, bool) {
	if len(raw) != 8 {
		return "", false
	}
	for i := 0; i < 8; i++ {
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
