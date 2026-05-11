package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/rpc"
)

// defaultCoalesceInterval is the cadence at which the per-symbol tick loop
// reads the IBKR market-data cache and (on change) emits a frame to its
// taps. Matches the value used by the pre-fan-out implementation; chosen
// to amortize render churn without lagging visibly behind the gateway.
const defaultCoalesceInterval = 150 * time.Millisecond

// ibkrMarketConnector is the slice of *ibkrlib.Connector that subManager
// touches. Defining it as an interface lets unit tests drive the manager
// with a fake instead of a real gateway connection — required by the
// project's "no mocks for daemon-internal data, but transport seams are
// fair game" rule.
type ibkrMarketConnector interface {
	SubscribeMarketData(symbol string, fields []string) error
	UnsubscribeMarketData(symbol string) error
	GetMarketData() map[string]*ibkrlib.MarketData
	GetMarketDataTypeForSymbol(symbol string) int
}

// subManager owns the daemon's per-symbol market-data subscriptions and
// fans tick frames out to multiple consumers (CLI watch + MCP subscribers
// + concurrent snapshot polls). At most one IBKR market-data line is held
// per symbol regardless of consumer count; the line is released the moment
// the last consumer goes away.
type subManager struct {
	mu       sync.Mutex
	subs     map[string]*subEntry
	coalesce time.Duration

	// connector is re-fetched on every tick so a daemon-side reconnect
	// (gatewayConnector returning a fresh *Connector) is observed without
	// having to thread the new pointer through every active subscription.
	// nil return means the gateway is currently unavailable and tick loops
	// translate that into a terminal gateway_lost frame.
	connector func() ibkrMarketConnector
}

type subEntry struct {
	sym      string
	refcount int
	taps     map[*frameTap]struct{}
	stop     chan struct{}

	// Cached last-emitted state for change-detection. Mutated only by the
	// tick loop while holding subManager.mu, same as taps.
	lastBid, lastAsk, lastLast float64
	lastBidSize, lastAskSize   int
	lastDataType               string
	emitted                    bool
}

type frameTap struct {
	ch chan rpc.Frame
}

func newSubManager(connector func() ibkrMarketConnector) *subManager {
	return &subManager{
		subs:      map[string]*subEntry{},
		coalesce:  defaultCoalesceInterval,
		connector: connector,
	}
}

// Subscribe acquires a market-data reference for sym and returns a frame
// channel that delivers coalesced ticks until release is called or a
// terminal error frame arrives. release is always safe to call exactly
// once (typically via defer).
//
// When this is the first reference to sym, an IBKR market-data line is
// opened and the per-symbol tick loop spins up. Subsequent Subscribe
// callers attach a new tap onto the existing fan-out.
func (m *subManager) Subscribe(sym string) (<-chan rpc.Frame, func(), error) {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" {
		return nil, func() {}, errors.New("subscribe: symbol required")
	}
	c := m.connector()
	if c == nil {
		return nil, func() {}, ibkrlib.ErrIBKRUnavailable
	}

	tap := &frameTap{ch: make(chan rpc.Frame, 16)}

	m.mu.Lock()
	e, exists := m.subs[sym]
	if !exists {
		// First reference for this symbol — open the IBKR line. pkg/ibkr's
		// SubscribeMarketData is itself idempotent, so a duplicate call from
		// a stale prior session resolves without surfacing an error here.
		if err := c.SubscribeMarketData(sym, []string{"100", "101", "104"}); err != nil {
			m.mu.Unlock()
			return nil, func() {}, fmt.Errorf("subscribe %s: %w", sym, err)
		}
		e = &subEntry{
			sym:  sym,
			taps: map[*frameTap]struct{}{},
			stop: make(chan struct{}),
		}
		m.subs[sym] = e
		go m.tickLoop(e)
	}
	e.taps[tap] = struct{}{}
	e.refcount++
	m.mu.Unlock()

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		m.releaseTap(sym, tap)
	}
	return tap.ch, release, nil
}

// Hold acquires a market-data reference without subscribing to the frame
// fan-out. Used by the snapshot path: it wants the IBKR line to stay open
// while it polls the cached tick state via Connector.GetMarketData(), but
// it doesn't consume per-tick frames.
func (m *subManager) Hold(sym string) (func(), error) {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" {
		return func() {}, errors.New("hold: symbol required")
	}
	c := m.connector()
	if c == nil {
		return func() {}, ibkrlib.ErrIBKRUnavailable
	}

	m.mu.Lock()
	e, exists := m.subs[sym]
	if !exists {
		if err := c.SubscribeMarketData(sym, []string{"100", "101", "104"}); err != nil {
			m.mu.Unlock()
			return func() {}, fmt.Errorf("subscribe %s: %w", sym, err)
		}
		e = &subEntry{
			sym:  sym,
			taps: map[*frameTap]struct{}{},
			stop: make(chan struct{}),
		}
		m.subs[sym] = e
		go m.tickLoop(e)
	}
	e.refcount++
	m.mu.Unlock()

	released := false
	return func() {
		if released {
			return
		}
		released = true
		m.releaseHold(sym)
	}, nil
}

// releaseTap drops a tap from sym's entry, decrements the refcount, and
// (on the last reference) tears down the IBKR line and the tick loop.
func (m *subManager) releaseTap(sym string, tap *frameTap) {
	m.mu.Lock()
	e, ok := m.subs[sym]
	if !ok {
		m.mu.Unlock()
		return
	}
	if _, present := e.taps[tap]; present {
		delete(e.taps, tap)
		// Closing the tap's channel signals the consumer's range loop to
		// exit. Safe because no other goroutine sends on tap.ch.
		close(tap.ch)
	}
	teardown := false
	e.refcount--
	if e.refcount <= 0 {
		delete(m.subs, sym)
		teardown = true
	}
	m.mu.Unlock()
	if teardown {
		m.teardown(e, sym)
	}
}

// releaseHold drops a hold, decrementing the refcount. Symmetric with
// releaseTap minus the tap-channel close (the holder never had one).
func (m *subManager) releaseHold(sym string) {
	m.mu.Lock()
	e, ok := m.subs[sym]
	if !ok {
		m.mu.Unlock()
		return
	}
	teardown := false
	e.refcount--
	if e.refcount <= 0 {
		delete(m.subs, sym)
		teardown = true
	}
	m.mu.Unlock()
	if teardown {
		m.teardown(e, sym)
	}
}

// teardown stops the tick loop and unsubscribes the IBKR line. Caller has
// already removed the entry from m.subs under the lock; this happens
// outside the lock so the IBKR call (which can block) doesn't serialize
// other Subscribe/Hold calls behind it.
func (m *subManager) teardown(e *subEntry, sym string) {
	close(e.stop)
	if c := m.connector(); c != nil {
		_ = c.UnsubscribeMarketData(sym)
	}
}

// tickLoop reads the IBKR market-data cache on the coalesce cadence and
// fans out a Frame to every tap whenever a tick field, the size, or the
// data-type-notice changes. Exits when sub.stop is closed.
//
// Gateway-loss detection is the connector() returning nil mid-stream: in
// that case the loop emits a terminal gateway_lost frame to every tap,
// closes them, and returns. tearing-down state is done under the lock to
// match Subscribe/Hold's invariants.
func (m *subManager) tickLoop(e *subEntry) {
	coalesce := m.coalesce
	if coalesce <= 0 {
		coalesce = defaultCoalesceInterval
	}
	t := time.NewTicker(coalesce)
	defer t.Stop()

	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			c := m.connector()
			if c == nil {
				m.emitError(e.sym, rpc.FrameErrGatewayLost,
					"IB Gateway connection dropped during streaming subscription")
				return
			}
			data := c.GetMarketData()
			md, ok := data[e.sym]
			if !ok {
				continue
			}
			dt := marketDataTypeName(c.GetMarketDataTypeForSymbol(e.sym))

			m.mu.Lock()
			// Re-check that the entry is still alive — a concurrent
			// teardown can race with a tick: refcount went to zero, the
			// last release ran, then the tick fires before the loop sees
			// the stop signal. Drop the tick rather than fanning out to
			// already-closed channels.
			if _, alive := m.subs[e.sym]; !alive {
				m.mu.Unlock()
				return
			}
			if e.emitted &&
				md.Bid == e.lastBid && md.Ask == e.lastAsk && md.Last == e.lastLast &&
				md.BidSize == e.lastBidSize && md.AskSize == e.lastAskSize &&
				dt == e.lastDataType {
				m.mu.Unlock()
				continue
			}
			frame := buildFrame(md, dt)
			for tap := range e.taps {
				select {
				case tap.ch <- frame:
				default:
					// Backpressure: tap is full (consumer not draining
					// fast enough). Drop this frame for that tap; the
					// next change-tick will retry. Honest > stalling.
				}
			}
			e.lastBid, e.lastAsk, e.lastLast = md.Bid, md.Ask, md.Last
			e.lastBidSize, e.lastAskSize = md.BidSize, md.AskSize
			e.lastDataType = dt
			e.emitted = true
			m.mu.Unlock()
		}
	}
}

// emitError sends a terminal error frame to every tap on sym, closes the
// tap channels, removes the entry, and unsubscribes the IBKR line. Idempotent
// against double-call: a second invocation against an already-torn-down sym
// is a no-op.
func (m *subManager) emitError(sym string, code, message string) {
	frame := rpc.Frame{
		T:     time.Now(),
		Error: &rpc.FrameError{Code: code, Message: message},
	}
	m.mu.Lock()
	e, ok := m.subs[sym]
	if !ok {
		m.mu.Unlock()
		return
	}
	for tap := range e.taps {
		select {
		case tap.ch <- frame:
		default:
			// Consumer hasn't drained the buffer — fall back to closing
			// without the explicit frame. The closed channel still signals
			// "stream over"; the consumer just won't see the structured
			// reason. Better than blocking the teardown.
		}
		close(tap.ch)
	}
	delete(m.subs, sym)
	m.mu.Unlock()
	m.teardown(e, sym)
}

// Close emits a daemon_shutdown frame to every active subscription and
// tears them down. Called from Server.Stop so MCP clients and CLI watchers
// see a structured terminal frame instead of an opaque socket close.
func (m *subManager) Close() {
	m.mu.Lock()
	syms := make([]string, 0, len(m.subs))
	for sym := range m.subs {
		syms = append(syms, sym)
	}
	m.mu.Unlock()
	for _, sym := range syms {
		m.emitError(sym, rpc.FrameErrDaemonShutdown, "ibkr daemon shutting down")
	}
}

// activeCount reports the number of distinct symbols currently held by
// the manager. Used by tests; safe to call concurrently.
func (m *subManager) activeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

// buildFrame projects an *ibkrlib.MarketData snapshot into the wire frame
// shape, using nil pointers for fields the gateway hasn't delivered yet.
func buildFrame(md *ibkrlib.MarketData, dt string) rpc.Frame {
	f := rpc.Frame{T: time.Now(), DataType: dt}
	if md.Bid != 0 {
		v := md.Bid
		f.Bid = &v
	}
	if md.Ask != 0 {
		v := md.Ask
		f.Ask = &v
	}
	if md.Last != 0 {
		v := md.Last
		f.Last = &v
	}
	if md.BidSize != 0 {
		v := md.BidSize
		f.BidSize = &v
	}
	if md.AskSize != 0 {
		v := md.AskSize
		f.AskSize = &v
	}
	return f
}
