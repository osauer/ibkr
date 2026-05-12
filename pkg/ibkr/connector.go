package ibkr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/pkg/ibkr/internal/logging"
)

var connectorLogger = logging.Component("IBKR Connector")
var marketDataLogger = logging.Component("IBKR MarketData")

// ErrSymbolInactive indicates IBKR has reported the contract is unavailable (e.g., delisted).
var ErrSymbolInactive = errors.New("symbol marked inactive")

// ErrContractDetailsTimeout marks the case where reqContractData was sent but
// IBKR never emitted contractDetailsEnd within the budget. Gateway-side
// condition: usually the security-definition data farm is degraded
// (intermittent pre-market, after maintenance windows, or when account
// permissions for a venue lapse). Callers wrap this with context — the
// daemon turns it into a CLI-friendly "option chain unavailable" hint.
var ErrContractDetailsTimeout = errors.New("timeout waiting for contract details")

// Connector is the main IBKR connector component
type Connector struct {
	name   string
	config *ConnectorConfig
	pool   *ConnectionPool
	lease  *ConnectionLease
	conn   *Connection

	gatewayBootstrapper GatewayBootstrapper

	fetchContractDetails func(string, time.Duration) ([]ContractDetailsLite, error)
	contractTimingHook   func(string, time.Duration, bool)

	// Component state
	running bool
	mu      sync.RWMutex
	ready   bool // true after handlers registered and startup completes

	// Market data subscriptions
	subscriptions map[string]*Subscription
	reqIDMap      map[int]string // Maps request IDs to symbols
	subMu         sync.RWMutex

	// Order management
	openOrders       map[string]*Order
	brokerOrderIndex map[string]string // IB order ID -> internal order ID
	orderUpdates     []Order
	orderMu          sync.RWMutex

	// Execution and commission listeners (fan-out to sinks like ExecutionSink)
	execMu        sync.RWMutex
	execListeners []func(*ExecutionReport)
	commMu        sync.RWMutex
	commListeners []func(*CommissionReport)

	// Lightweight contract details cache to improve routing during OOH sessions
	contractMu         sync.RWMutex
	contractCache      map[string]ContractDetailsLite
	inactiveMu         sync.RWMutex
	inactiveSymbols    map[string]inactiveSymbolState
	inactiveCandidates map[string]inactiveCandidateState
	inactiveStore      InactiveSymbolStore

	// Option IV tracking (by underlying symbol)
	optMu sync.RWMutex
	optIV map[string]float64 // last observed implied vol (fraction, e.g., 0.30)
	// Option quote tracking for derivation when 106 absent
	optReqIDs       map[int]string      // option reqID -> underlying symbol
	optQuoteBid     map[string]float64  // last observed option bid per underlying
	optQuoteAsk     map[string]float64  // last observed option ask per underlying
	optQuoteMid     map[string]float64  // derived option mid per underlying
	optGreeks       map[string]Greeks   // last observed model-computation greeks per option key
	optUnderlyingPx map[string]float64  // model-computation underlying price per option key
	optParams       map[string]struct { // subscription params used
		strike float64
		expiry time.Time
		right  string
	}

	// Error tracking for runtime stability reporting
	errMu     sync.Mutex
	errTotals map[int]int64
	errEvents []struct {
		code int
		t    time.Time
	}

	// Historical data requests (HMDS)
	historicalMu      sync.Mutex
	historicalReqs    map[int]*historicalRequest
	historicalBackoff map[string]int
}

// ConnectorConfig holds configuration for the IBKR connector
type ConnectorConfig struct {
	ServiceName       string
	PreferredClientID int
	PoolConfig        *PoolConfig
	GatewayBootstrap  GatewayBootstrapper
}

// Subscription represents a market data subscription.
//
// PrevClose holds tick type 9 (previous regular-session close), which IBKR
// emits automatically on any reqMktData. It's the anchor for change-vs-
// prev-close calculations rendered by quote and positions — pre-market /
// after-hours it's the only price worth comparing against because the
// live ticks (bid/ask/last) may not be flowing yet.
//
// Open / High / Low (ticks 14/6/7) are captured for completeness but not
// currently rendered; they're cheap to plumb and ready for a future
// `quote --verbose` view.
type Subscription struct {
	Symbol    string
	ReqID     int
	Fields    []string
	LastPrice float64
	Bid       float64
	Ask       float64
	BidSize   int64
	AskSize   int64
	Volume    int64
	PrevClose float64
	Open      float64
	High      float64
	Low       float64
	// Week-range highs/lows arrive via generic tick 165 (Misc Stats) as
	// tickPrice messages with tick types 15-20. Captured here so consumers
	// (notably scan-row enrichment, where 52w range is a standard column)
	// can read them without a separate market-data call.
	Week13Low  float64
	Week13High float64
	Week26Low  float64
	Week26High float64
	Week52Low  float64
	Week52High float64
	// IV is the option implied volatility tick (generic tick 106), present
	// only when the streaming subscribe requested it. Stored as a fraction
	// (0.234 == 23.4%); the gateway sometimes emits the percent form, which
	// the handler normalizes.
	IV       float64
	LastTime time.Time
	Observed bool // true once we receive any tick for this reqID
}

const (
	contractHydrationWait  = 2 * time.Second
	contractHydrationPoll  = 25 * time.Millisecond
	contractHydrationGrace = 5 * time.Second
)

// HistoricalBar represents a single OHLC bar returned by IBKR HMDS.
// The Time field is parsed best-effort from the gateway response; when parsing
// fails it will be the zero value while Date retains the original string.
type HistoricalBar struct {
	Time     time.Time
	Date     string
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   int64
	Average  float64
	BarCount int
}

const minServerVerHistoricalDataEnd = 196

type historicalResult struct {
	start string
	end   string
	bars  []HistoricalBar
	err   error
}

type historicalRequest struct {
	symbol string
	result chan historicalResult
}

// HistoricalRequestError encapsulates IBKR historical data error codes and retry hints.
type HistoricalRequestError struct {
	Code       int
	Message    string
	RetryAfter time.Duration
}

func (e *HistoricalRequestError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("historical data error %d", e.Code)
}

// NewConnector creates a new IBKR connector
func NewConnector(config *ConnectorConfig) *Connector {
	if config == nil {
		config = &ConnectorConfig{
			ServiceName: "regime-connector",
			PoolConfig:  DefaultPoolConfig(),
		}
	}
	if config.PoolConfig == nil {
		config.PoolConfig = DefaultPoolConfig()
	}
	if config.PoolConfig.BaseConfig == nil {
		config.PoolConfig.BaseConfig = DefaultConfig()
	}

	if template := strings.TrimSpace(os.Getenv("IBKR_PACKET_LOG_TEMPLATE")); template != "" && config.PoolConfig.BaseConfig.PacketLogPath == "" {
		if strings.HasSuffix(template, string(os.PathSeparator)) {
			template = filepath.Join(template, "ibkr_client_%d.log")
		} else if !strings.Contains(template, "%d") {
			template = template + "_%d.log"
		}
		config.PoolConfig.BaseConfig.PacketLogPath = template
	}

	conn := &Connector{
		name:             "IBKRConnector",
		config:           config,
		pool:             NewConnectionPool(config.PoolConfig),
		subscriptions:    make(map[string]*Subscription),
		reqIDMap:         make(map[int]string),
		openOrders:       make(map[string]*Order),
		brokerOrderIndex: make(map[string]string),
		orderUpdates:     make([]Order, 0, 8),
		contractCache:    make(map[string]ContractDetailsLite),
		optIV:            make(map[string]float64),
		optReqIDs:        make(map[int]string),
		optQuoteBid:      make(map[string]float64),
		optQuoteAsk:      make(map[string]float64),
		optQuoteMid:      make(map[string]float64),
		optGreeks:        make(map[string]Greeks),
		optUnderlyingPx:  make(map[string]float64),
		optParams: make(map[string]struct {
			strike float64
			expiry time.Time
			right  string
		}),
		gatewayBootstrapper: config.GatewayBootstrap,
		errTotals:           make(map[int]int64),
		historicalReqs:      make(map[int]*historicalRequest),
		historicalBackoff:   make(map[string]int),
	}
	conn.fetchContractDetails = conn.FetchContractDetails
	return conn
}

// UseInactiveSymbolStore preloads the connector with persisted inactive symbols.
func (c *Connector) UseInactiveSymbolStore(ctx context.Context, store InactiveSymbolStore) error {
	if store == nil {
		return nil
	}

	records, err := store.LoadInactiveSymbols(ctx)
	if err != nil {
		return err
	}

	var loaded int
	c.inactiveMu.Lock()
	c.inactiveStore = store
	if c.inactiveSymbols == nil {
		c.inactiveSymbols = make(map[string]inactiveSymbolState, len(records))
	}
	for sym, state := range records {
		upper := strings.ToUpper(strings.TrimSpace(sym))
		if upper == "" {
			continue
		}
		c.inactiveSymbols[upper] = state
		loaded++
	}
	c.inactiveMu.Unlock()

	if loaded > 0 {
		c.logInfo("Loaded %d inactive symbols from store", loaded)
	}
	return nil
}

// Name returns the component name
func (c *Connector) Name() string {
	return c.name
}

func (c *Connector) logInfo(format string, args ...interface{}) {
	connectorLogger.Infof("%s: "+format, append([]interface{}{c.name}, args...)...)
}

func (c *Connector) logWarn(format string, args ...interface{}) {
	connectorLogger.Warnf("%s: "+format, append([]interface{}{c.name}, args...)...)
}

func (c *Connector) logError(format string, args ...interface{}) {
	connectorLogger.Errorf("%s: "+format, append([]interface{}{c.name}, args...)...)
}

func (c *Connector) logDebug(format string, args ...interface{}) {
	connectorLogger.Debugf("%s: "+format, append([]interface{}{c.name}, args...)...)
}

func (c *Connector) recordContractTiming(symbol string, elapsed time.Duration, resolved bool) {
	if symbol == "" || elapsed <= 0 {
		return
	}
	if c.contractTimingHook != nil {
		c.contractTimingHook(symbol, elapsed, resolved)
	}
	if c.conn != nil {
		c.conn.observeContractTiming(symbol, elapsed, resolved)
	}
}

func (c *Connector) inactiveReason(symbol string) (string, bool) {
	c.inactiveMu.RLock()
	state, ok := c.inactiveSymbols[symbol]
	c.inactiveMu.RUnlock()
	if !ok {
		return "", false
	}
	return state.reason, true
}

// InactiveReason reports the stored inactivity reason for a symbol, if any.
func (c *Connector) InactiveReason(symbol string) (string, bool) {
	if symbol == "" {
		return "", false
	}
	if reason, ok := c.inactiveReason(symbol); ok {
		return reason, true
	}
	upper := strings.ToUpper(symbol)
	if upper != symbol {
		return c.inactiveReason(upper)
	}
	return "", false
}

// IsSymbolInactive returns true when the symbol has been marked inactive via IBKR error handling.
func (c *Connector) IsSymbolInactive(symbol string) bool {
	_, inactive := c.InactiveReason(symbol)
	return inactive
}

func (c *Connector) hasActiveContract(symbol string) bool {
	symbol = strings.ToUpper(symbol)
	c.contractMu.RLock()
	detail, ok := c.contractCache[symbol]
	c.contractMu.RUnlock()
	return ok && detail.ConID != 0
}

func (c *Connector) clearInactiveCandidate(symbol string) {
	c.inactiveMu.Lock()
	if c.inactiveCandidates != nil {
		delete(c.inactiveCandidates, strings.ToUpper(symbol))
	}
	c.inactiveMu.Unlock()
}

func (c *Connector) registerInactiveCandidate(symbol, reason string) bool {
	if symbol == "" {
		return false
	}
	symbol = strings.ToUpper(symbol)

	forceInactive := false
	upperReason := strings.ToUpper(reason)
	if c.hasActiveContract(symbol) {
		if strings.Contains(upperReason, "NO SECURITY DEFINITION") || strings.Contains(upperReason, "NO DATA") {
			forceInactive = true
		} else {
			c.clearInactiveCandidate(symbol)
			return false
		}
	}

	reason = strings.TrimSpace(reason)
	c.inactiveMu.Lock()
	if c.inactiveSymbols != nil {
		if _, exists := c.inactiveSymbols[symbol]; exists {
			c.inactiveMu.Unlock()
			return true
		}
	}
	if c.inactiveCandidates == nil {
		c.inactiveCandidates = make(map[string]inactiveCandidateState)
	}
	state := c.inactiveCandidates[symbol]
	state.count++
	state.lastReason = reason
	state.lastUpdated = time.Now()
	shouldMark := forceInactive || state.count >= 1
	if shouldMark {
		delete(c.inactiveCandidates, symbol)
	}
	if !shouldMark {
		c.inactiveCandidates[symbol] = state
	}
	c.inactiveMu.Unlock()

	if shouldMark {
		c.markSymbolInactive(symbol, reason)
		return true
	}
	return false
}

func (c *Connector) markSymbolInactive(symbol, reason string) {
	if symbol == "" {
		return
	}
	symbol = strings.ToUpper(symbol)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "inactive"
	}

	c.inactiveMu.Lock()
	if c.inactiveSymbols == nil {
		c.inactiveSymbols = make(map[string]inactiveSymbolState)
	}
	if c.inactiveCandidates != nil {
		delete(c.inactiveCandidates, symbol)
	}
	if _, exists := c.inactiveSymbols[symbol]; exists {
		c.inactiveMu.Unlock()
		return
	}
	state := inactiveSymbolState{
		reason:   reason,
		markedAt: time.Now(),
	}
	c.inactiveSymbols[symbol] = state
	c.inactiveMu.Unlock()

	c.dropSubscription(symbol)
	c.logInfo("Suppressing market data for %s (inactive: %s)", symbol, reason)
	c.persistInactiveSymbol(symbol, state)
}

func (c *Connector) processSystemNotice(alias reqAliasEntry, note *systemNotification) {
	if note == nil {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(alias.symbol))
	if symbol == "" {
		return
	}

	secType := strings.ToUpper(strings.TrimSpace(alias.secType))
	isDerivative := secType == "OPT" || secType == "FOP" || secType == "WAR" || secType == "BAG"

	upperMsg := strings.ToUpper(note.message)
	switch note.code {
	case 200:
		if strings.Contains(upperMsg, "NO SECURITY DEFINITION") {
			if isDerivative {
				c.logDebug("Ignoring system notice code %d for derivative request %s (%s): %s", note.code, symbol, alias.localSymbol, note.message)
				return
			}
			c.registerInactiveCandidate(symbol, note.message)
		}
	case 162:
		if strings.Contains(upperMsg, "NO DATA") {
			c.registerInactiveCandidate(symbol, note.message)
		}
	case 366:
		c.registerInactiveCandidate(symbol, note.message)
	}
}

func (c *Connector) dropSubscription(symbol string) {
	if symbol == "" {
		return
	}
	upper := strings.ToUpper(symbol)

	c.subMu.Lock()
	if sub, ok := c.subscriptions[upper]; ok {
		if conn := c.conn; conn != nil && conn.IsConnected() && sub.ReqID != 0 {
			_ = conn.CancelMarketData(sub.ReqID)
		}
		delete(c.subscriptions, upper)
	}
	for reqID, sym := range c.reqIDMap {
		if strings.EqualFold(sym, upper) {
			delete(c.reqIDMap, reqID)
		}
	}
	c.subMu.Unlock()

	c.optMu.Lock()
	for reqID, sym := range c.optReqIDs {
		if strings.EqualFold(sym, upper) {
			delete(c.optReqIDs, reqID)
		}
	}
	delete(c.optParams, upper)
	c.optMu.Unlock()
}

// SetMarketDataType proxies to the underlying connection to control data type.
// dataType: 1=Live, 2=Frozen, 3=Delayed, 4=DelayedFrozen
func (c *Connector) SetMarketDataType(dataType int) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return fmt.Errorf("IBKR connection not available")
	}
	if dataType == 1 && conn.HasCompetingLiveSession() {
		connectorLogger.Warnf("%s: Live market data blocked by competing session; forcing delayed mode", c.name)
		dataType = 3
	}
	return conn.SetMarketDataType(dataType)
}

// Start initializes the IBKR connector
func (c *Connector) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return fmt.Errorf("connector already running")
	}
	c.mu.Unlock()

	c.logInfo("Starting IBKR connector")

	// Start connection pool
	if err := c.pool.Start(ctx); err != nil {
		return fmt.Errorf("failed to start connection pool: %w", err)
	}

	// Log summary of established IBKR connections and their client IDs
	{
		status := c.pool.GetPoolStatus()
		total := status["total_connections"]
		connected := status["connected_count"]
		details, _ := status["connections"].(map[int]map[string]interface{})
		// Fallback if type cast fails due to interface{} map types
		if details == nil {
			if m, ok := status["connections"].(map[interface{}]interface{}); ok {
				// Convert to a readable list
				var list []string
				for k, v := range m {
					cm, _ := v.(map[string]interface{})
					if cm != nil {
						list = append(list, fmt.Sprintf("%v(%v)", k, cm["status"]))
					} else {
						list = append(list, fmt.Sprintf("%v", k))
					}
				}
				c.logInfo("IBKR pool total=%v connected=%v clients=%v", total, connected, strings.Join(list, ", "))
			}
		} else {
			// Build list of configured->actual client IDs and statuses
			var list []string
			for cid, info := range details {
				st, _ := info["status"].(string)
				actualAny, ok := info["client_id"]
				actual := cid
				if ok {
					switch v := actualAny.(type) {
					case int:
						actual = v
					case int64:
						actual = int(v)
					case float64:
						actual = int(v)
					}
				}
				if actual != cid {
					list = append(list, fmt.Sprintf("%d->%d(%s)", cid, actual, st))
				} else {
					list = append(list, fmt.Sprintf("%d(%s)", cid, st))
				}
			}
			c.logInfo("IBKR pool total=%v connected=%v clients=%s", total, connected, strings.Join(list, ", "))
		}
	}

	// Request a connection lease
	lease, err := c.pool.RequestLease(ctx, c.config.ServiceName, c.config.PreferredClientID)
	if err != nil && c.gatewayBootstrapper != nil {
		c.logInfo("Failed to acquire IBKR lease: %v", err)
		c.logInfo("Attempting to bootstrap IBKR gateway before retrying lease request")
		if bootErr := c.gatewayBootstrapper.EnsureGateway(ctx); bootErr != nil {
			c.logWarn("Gateway bootstrap attempt failed: %v", bootErr)
		} else {
			// Allow gateway a brief moment to come online before retrying
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
			}
			lease, err = c.pool.RequestLease(ctx, c.config.ServiceName, c.config.PreferredClientID)
		}
	}
	if err != nil {
		// Try to continue without IBKR for resilience
		c.logWarn("Failed to get connection lease: %v", err)
		c.logWarn("Running in degraded mode without IBKR connection")
		c.mu.Lock()
		c.running = true
		c.mu.Unlock()
		return nil
	}

	c.mu.Lock()
	c.lease = lease
	c.mu.Unlock()

	// Get the actual connection and attach hooks before the connection thread starts.
	conn, err := c.pool.GetConnectionPrepared(lease.LeaseID, func(conn *Connection) {
		c.attachConnectionHooks(conn)
	})
	if err != nil {
		c.logWarn("Failed to get connection: %v", err)
		c.mu.Lock()
		c.running = true
		c.mu.Unlock()
		return nil
	}

	c.mu.Lock()
	c.conn = conn
	c.running = true
	c.mu.Unlock()

	// Start lease heartbeat
	go c.maintainLease()

	// Report both lease (configured) and actual connection client IDs
	actualID := lease.ClientID
	if info := conn.GetConnectionInfo(); info != nil {
		if v, ok := info["client_id"]; ok {
			switch t := v.(type) {
			case int:
				actualID = t
			case int64:
				actualID = int(t)
			case float64:
				actualID = int(t)
			}
		}
	}
	c.logInfo("IBKR connector started successfully (lease_client_id: %d, actual_client_id: %d)",
		lease.ClientID, actualID)

	return nil
}

func (c *Connector) attachConnectionHooks(conn *Connection) {
	conn.SetOnConnect(func() {
		c.onConnectionEstablished(conn)
	})
	conn.SetOnDisconnect(func(err error) {
		c.onConnectionLost(conn)
	})

	// If the connection is already established (e.g., eager pool), ensure
	// handlers are registered immediately so the connector is ready.
	if conn.IsConnected() {
		c.onConnectionEstablished(conn)
	}
}

func (c *Connector) onConnectionEstablished(conn *Connection) {
	// Ensure the connector references the active connection while handlers register.
	c.mu.Lock()
	c.conn = conn
	c.ready = false
	c.mu.Unlock()

	c.registerHandlers(conn)

	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
}

func (c *Connector) onConnectionLost(conn *Connection) {
	if conn != nil {
		conn.SetSystemNoticeHandler(nil)
	}
	c.mu.Lock()
	if c.conn == conn {
		c.ready = false
	}
	c.mu.Unlock()
}

// GetStatus returns a compact status map for API consumption (system/status endpoint)
func (c *Connector) GetStatus() map[string]interface{} {
	c.mu.RLock()
	conn := c.conn
	running := c.running
	c.mu.RUnlock()

	total, observed := c.GetSubscriptionStats()
	totals, last5m := c.GetErrorStats(5 * time.Minute)
	m := map[string]interface{}{
		"running":          running,
		"connected":        conn != nil && conn.IsConnected(),
		"subscriptions":    total,
		"subscriptions_ok": observed,
		"errors_total":     totals,
		"errors_last5m":    last5m,
	}
	if conn != nil {
		m["connection"] = conn.GetConnectionInfo()
	}
	return m
}

// GetMarketDataTypeForSymbol returns the gateway's per-reqID
// market-data-type notice for the symbol's active subscription:
// 1=RealTime, 2=Frozen, 3=Delayed, 4=DelayedFrozen, 0=unknown (no
// subscription, or notice not yet received). The CLI uses this to
// render an after-hours data-type badge — frozen mode delivers a single
// snapshot per request and then goes silent, so the user needs to know
// rather than watch a dead stream.
func (c *Connector) GetMarketDataTypeForSymbol(symbol string) int {
	c.subMu.RLock()
	sub, ok := c.subscriptions[strings.ToUpper(symbol)]
	c.subMu.RUnlock()
	if !ok || sub.ReqID == 0 {
		return 0
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return 0
	}
	return conn.GetMarketDataType(sub.ReqID)
}

// GetSubscriptionStats returns (total, observed) subscription counts.
func (c *Connector) GetSubscriptionStats() (int, int) {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	total := len(c.subscriptions)
	observed := 0
	for _, s := range c.subscriptions {
		if s != nil && s.Observed {
			observed++
		}
	}
	return total, observed
}

// recordError note an IBKR error code for stability reporting.
func (c *Connector) recordError(code int) {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	c.errTotals[code]++
	// keep a bounded timeline (max 2000 events) for windowed stats
	c.errEvents = append(c.errEvents, struct {
		code int
		t    time.Time
	}{code: code, t: time.Now()})
	if len(c.errEvents) > 2000 {
		c.errEvents = c.errEvents[len(c.errEvents)-1500:]
	}
}

// GetErrorStats returns total counts and counts within a window.
func (c *Connector) GetErrorStats(window time.Duration) (map[int]int64, map[int]int64) {
	cutoff := time.Now().Add(-window)
	c.errMu.Lock()
	defer c.errMu.Unlock()
	totals := make(map[int]int64, len(c.errTotals))
	for k, v := range c.errTotals {
		totals[k] = v
	}
	last := map[int]int64{}
	// prune and count window
	j := 0
	for _, ev := range c.errEvents {
		if ev.t.After(cutoff) {
			last[ev.code]++
			c.errEvents[j] = ev
			j++
		}
	}
	c.errEvents = c.errEvents[:j]
	return totals, last
}

// ContractDetailsLite contains the subset of details needed for calendar building
type ContractDetailsLite struct {
	ReqID        int
	Symbol       string
	Exchange     string
	PrimaryExch  string
	ConID        int
	LocalSymbol  string
	TradingClass string
	TimeZoneID   string
	TradingHours string
	LiquidHours  string
}

func mergeContractDetailsLite(base, incoming ContractDetailsLite) ContractDetailsLite {
	if base.Symbol == "" {
		base.Symbol = incoming.Symbol
	}
	if base.ConID == 0 {
		base.ConID = incoming.ConID
	}
	if base.Exchange == "" && incoming.Exchange != "" {
		base.Exchange = incoming.Exchange
	}
	if base.PrimaryExch == "" && incoming.PrimaryExch != "" {
		base.PrimaryExch = incoming.PrimaryExch
	}
	if base.LocalSymbol == "" && incoming.LocalSymbol != "" {
		base.LocalSymbol = incoming.LocalSymbol
	}
	if base.TradingClass == "" && incoming.TradingClass != "" {
		base.TradingClass = incoming.TradingClass
	}
	if base.TimeZoneID == "" && incoming.TimeZoneID != "" {
		base.TimeZoneID = incoming.TimeZoneID
	}
	if base.TradingHours == "" && incoming.TradingHours != "" {
		base.TradingHours = incoming.TradingHours
	}
	if base.LiquidHours == "" && incoming.LiquidHours != "" {
		base.LiquidHours = incoming.LiquidHours
	}
	return base
}

type inactiveSymbolState struct {
	reason   string
	markedAt time.Time
}

type inactiveCandidateState struct {
	count       int
	lastReason  string
	lastUpdated time.Time
}

func shouldPersistInactiveReason(reason string) bool {
	if reason == "" {
		return false
	}
	upper := strings.ToUpper(reason)
	if strings.Contains(upper, "NO SECURITY DEFINITION") {
		return true
	}
	if strings.Contains(upper, "DELIST") {
		return true
	}
	return false
}

func (c *Connector) getInactiveStore() InactiveSymbolStore {
	c.inactiveMu.RLock()
	store := c.inactiveStore
	c.inactiveMu.RUnlock()
	return store
}

func (c *Connector) persistInactiveSymbol(symbol string, state inactiveSymbolState) {
	store := c.getInactiveStore()
	if store == nil || !shouldPersistInactiveReason(state.reason) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.SaveInactiveSymbol(ctx, symbol, state); err != nil {
		c.logWarn("Failed to persist inactive symbol %s: %v", symbol, err)
	}
}

func (c *Connector) applyContractDetail(detail ContractDetailsLite, contract *Contract) bool {
	if detail.Exchange != "" {
		contract.Exchange = detail.Exchange
	}
	if detail.PrimaryExch != "" {
		contract.PrimaryExch = detail.PrimaryExch
	}
	if detail.ConID != 0 {
		contract.ConID = detail.ConID
	}
	if detail.LocalSymbol != "" {
		contract.LocalSymbol = detail.LocalSymbol
	} else if detail.ConID != 0 {
		connectorLogger.Debugf("Contract detail for %s (conID=%d) missing local symbol", detail.Symbol, detail.ConID)
	}
	if detail.TradingClass != "" {
		contract.TradingClass = detail.TradingClass
	}
	return contract.ConID != 0
}

func normalizeEquityRouting(contract *Contract, fallbackPrimary string) {
	if contract == nil || contract.SecType != "STK" {
		return
	}

	if contract.PrimaryExch == "" {
		contract.PrimaryExch = fallbackPrimary
	}
	if contract.PrimaryExch == "" && contract.Exchange != "" && !strings.EqualFold(contract.Exchange, "SMART") {
		contract.PrimaryExch = contract.Exchange
	}
	if contract.PrimaryExch != "" && strings.EqualFold(contract.PrimaryExch, "SMART") {
		contract.PrimaryExch = ""
	}
	if contract.PrimaryExch != "" {
		if strings.EqualFold(contract.Exchange, contract.PrimaryExch) || contract.Exchange == "" {
			contract.Exchange = "SMART"
		}
	}
}

func (c *Connector) prepareContract(symbol string, fetchTimeout time.Duration, asyncWarm bool) (Contract, bool) {
	start := time.Now()
	upper := strings.ToUpper(symbol)
	secType, exchange, currency, primary := classifySymbol(upper)
	localSymbol, tradingClass := contractDisplayHints(upper, secType)

	contract := Contract{
		Symbol:       upper,
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  primary,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClass,
	}

	if reason, inactive := c.inactiveReason(upper); inactive {
		c.logDebug("Skipping contract hydration for inactive symbol %s (%s)", upper, reason)
		return contract, false
	}

	var hasDetail bool

	c.contractMu.RLock()
	if detail, ok := c.contractCache[upper]; ok {
		hasDetail = c.applyContractDetail(detail, &contract)
	}
	c.contractMu.RUnlock()

	if !hasDetail && fetchTimeout > 0 && c.conn != nil && c.conn.IsConnected() {
		fetch := c.fetchContractDetails
		if fetch == nil {
			fetch = c.FetchContractDetails
		}
		if details, err := fetch(upper, fetchTimeout); err == nil && len(details) > 0 {
			detail := details[0]
			c.contractMu.Lock()
			c.contractCache[upper] = detail
			c.contractMu.Unlock()
			hasDetail = c.applyContractDetail(detail, &contract)
		}
	}

	if !hasDetail && asyncWarm {
		go c.asyncWarmContractDetails(upper, fetchTimeout)
	}

	elapsed := time.Since(start)
	c.recordContractTiming(symbol, elapsed, hasDetail && contract.ConID != 0)
	normalizeEquityRouting(&contract, primary)

	return contract, hasDetail
}

func (c *Connector) waitForContractDetails(symbol string, base Contract, detailsReady bool) (Contract, bool) {
	upper := strings.ToUpper(symbol)
	if (detailsReady && base.ConID != 0) || base.Symbol == "" {
		return base, detailsReady || base.ConID != 0
	}
	deadline := time.Now().Add(contractHydrationWait)
	contract := base
	for contract.ConID == 0 && time.Now().Before(deadline) {
		time.Sleep(contractHydrationPoll)
		c.contractMu.RLock()
		detail, ok := c.contractCache[upper]
		c.contractMu.RUnlock()
		if !ok {
			continue
		}
		contractCopy := contract
		if c.applyContractDetail(detail, &contractCopy) && contractCopy.ConID != 0 {
			normalizeEquityRouting(&contractCopy, contract.PrimaryExch)
			return contractCopy, true
		}
	}
	return contract, detailsReady || contract.ConID != 0
}

func (c *Connector) asyncWarmContractDetails(symbol string, timeout time.Duration) {
	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		return
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if details, err := c.FetchContractDetails(symbol, timeout); err == nil && len(details) > 0 {
		c.contractMu.Lock()
		c.contractCache[symbol] = details[0]
		c.contractMu.Unlock()
		c.clearInactiveCandidate(symbol)
		c.logInfo("Cached contract details for %s (PrimaryExch=%s)", symbol, details[0].PrimaryExch)
	}
}

const (
	minServerVerMdSizeMultiplier = 110
	minServerVerSizeRules        = 164
	minServerVerLastTradeDate    = 182
)

// FetchContractDetails requests contract details for a symbol and waits for completion.
// NOTE: This implementation assumes single-flight usage per process due to global handlers.
func (c *Connector) FetchContractDetails(symbol string, timeout time.Duration) ([]ContractDetailsLite, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	if _, inactive := c.inactiveReason(symbol); inactive {
		c.logDebug("Contract details fetch skipped for inactive symbol %s", symbol)
		return nil, ErrSymbolInactive
	}
	if cached := c.cachedContractDetail(symbol); cached != nil && cached.ConID != 0 {
		c.logDebug("Contract details fetch satisfied from cache symbol=%s conID=%d", symbol, cached.ConID)
		return []ContractDetailsLite{*cached}, nil
	}
	if !c.isConnected() {
		return nil, fmt.Errorf("IBKR connection not available")
	}
	// Prepare contract using the same classification as market data
	secType, exchange, currency, primary := classifySymbol(symbol)
	contract := Contract{Symbol: symbol, SecType: secType, Exchange: exchange, Currency: currency}
	if primary != "" {
		contract.PrimaryExch = primary
	}
	detailsCh := make(chan ContractDetailsLite, 10)
	doneCh := make(chan struct{})
	serverVersion := c.conn.serverVersion
	reqID := c.conn.GetNextRequestID()

	c.logDebug("Contract details fetch start reqID=%d symbol=%s secType=%s exch=%s primary=%s currency=%s", reqID, symbol, contract.SecType, contract.Exchange, contract.PrimaryExch, contract.Currency)

	// Register temporary handlers
	dataHandlerID := c.conn.RegisterHandler(msgContractData, func(fields []string) {
		if lite, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			detailsCh <- *lite
		}
	})

	endHandlerID := c.conn.RegisterHandler(msgContractDataEnd, func(fields []string) {
		if len(fields) < 3 {
			return
		}
		rid, _ := strconv.Atoi(safeGet(fields, 2))
		if rid == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})

	if err := c.conn.sendContractDetailsRequest(contract, reqID); err != nil {
		c.conn.UnregisterHandler(msgContractData, dataHandlerID)
		c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
		return nil, err
	}

	// Wait for completion
	var results []ContractDetailsLite
	deadline := time.After(timeout)
	for {
		select {
		case d := <-detailsCh:
			results = append(results, d)
		case <-doneCh:
			c.conn.UnregisterHandler(msgContractData, dataHandlerID)
			c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
			if len(results) == 0 {
				c.logDebug("Contract details fetch complete reqID=%d symbol=%s (0 rows)", reqID, symbol)
			} else {
				c.clearInactiveCandidate(symbol)
				first := results[0]
				c.logDebug("Contract details fetch success reqID=%d symbol=%s count=%d conID=%d exch=%s primary=%s local=%s class=%s",
					reqID, symbol, len(results), first.ConID, first.Exchange, first.PrimaryExch, first.LocalSymbol, first.TradingClass)
			}
			return results, nil
		case <-deadline:
			c.deferContractDetailsCleanup(symbol, reqID, detailsCh, doneCh, dataHandlerID, endHandlerID)
			c.logDebug("Contract details fetch timeout reqID=%d symbol=%s received=%d", reqID, symbol, len(results))
			return results, ErrContractDetailsTimeout
		}
	}
}

const contractDetailsLateGrace = 3 * time.Second

func (c *Connector) deferContractDetailsCleanup(symbol string, reqID int, detailsCh <-chan ContractDetailsLite, doneCh <-chan struct{}, dataHandlerID, endHandlerID uint64) {
	go func() {
		timer := time.NewTimer(contractDetailsLateGrace)
		defer timer.Stop()

		var cachedDetail *ContractDetailsLite

	forLoop:
		for {
			select {
			case detail := <-detailsCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(contractDetailsLateGrace)

				if detail.Symbol != "" {
					d := detail // copy
					cachedDetail = &d
					key := strings.ToUpper(detail.Symbol)
					c.contractMu.Lock()
					if existing, ok := c.contractCache[key]; !ok || existing.ConID == 0 {
						c.contractCache[key] = detail
					}
					c.contractMu.Unlock()
				}
			case <-doneCh:
				break forLoop
			case <-timer.C:
				break forLoop
			}
		}

		c.conn.UnregisterHandler(msgContractData, dataHandlerID)
		c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)

		if cachedDetail != nil {
			connectorLogger.Infof("[INFO] Contract details for %s arrived after timeout (reqID=%d, conID=%d)", symbol, reqID, cachedDetail.ConID)
		}

		for {
			select {
			case <-detailsCh:
			default:
				return
			}
		}
	}()
}

func (c *Connector) ensureContractDetails(symbol string, timeout time.Duration) (*ContractDetailsLite, error) {
	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		return nil, ErrSymbolInactive
	}

	c.contractMu.RLock()
	if cached, ok := c.contractCache[symbol]; ok && cached.ConID != 0 {
		c.contractMu.RUnlock()
		return &cached, nil
	}
	c.contractMu.RUnlock()

	fetch := c.fetchContractDetails
	if fetch == nil {
		fetch = c.FetchContractDetails
	}
	details, err := fetch(symbol, timeout)
	if err != nil {
		return nil, err
	}
	if len(details) == 0 {
		return nil, fmt.Errorf("contract details unavailable for %s", symbol)
	}
	primary := details[0]
	c.contractMu.Lock()
	c.contractCache[symbol] = primary
	c.contractMu.Unlock()
	return &primary, nil
}

func (c *Connector) cachedContractDetail(symbol string) *ContractDetailsLite {
	symbol = strings.ToUpper(symbol)
	c.contractMu.RLock()
	defer c.contractMu.RUnlock()
	if detail, ok := c.contractCache[symbol]; ok {
		d := detail
		return &d
	}
	return nil
}

func (c *Connector) awaitContractDetail(symbol string, wait time.Duration) *ContractDetailsLite {
	if wait <= 0 {
		return nil
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if detail := c.cachedContractDetail(symbol); detail != nil && detail.ConID != 0 {
			return detail
		}
		time.Sleep(contractHydrationPoll)
	}
	return nil
}

func safeGet(a []string, i int) string {
	if i >= 0 && i < len(a) {
		return a[i]
	}
	return ""
}

func parseIntSafe(s string) int {
	if s == "" {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

func parseContractDetailsLite(fields []string, expectedReqID int, serverVersion int) (*ContractDetailsLite, bool) {
	if len(fields) <= 1 {
		return nil, false
	}

	idx := 1
	version := 8
	if serverVersion < minServerVerSizeRules {
		version = parseIntSafe(safeGet(fields, idx))
		idx++
	}

	reqID := expectedReqID
	if version >= 3 {
		parsedReqID := parseIntSafe(safeGet(fields, idx))
		idx++
		if parsedReqID != 0 {
			reqID = parsedReqID
		}
	}
	if expectedReqID != 0 && reqID != expectedReqID {
		return nil, false
	}

	symbol := strings.TrimSpace(safeGet(fields, idx))
	idx++
	secType := safeGet(fields, idx)
	idx++
	_ = secType

	// Last trade date / contract month
	_ = safeGet(fields, idx)
	idx++
	if serverVersion >= minServerVerLastTradeDate {
		idx++
	}

	// Strike and right
	_ = safeGet(fields, idx)
	idx++
	_ = safeGet(fields, idx)
	idx++

	exchange := strings.TrimSpace(safeGet(fields, idx))
	idx++
	currency := safeGet(fields, idx)
	idx++
	_ = currency

	localSymbol := strings.TrimSpace(safeGet(fields, idx))
	idx++
	_ = safeGet(fields, idx) // market name
	idx++
	tradingClass := strings.TrimSpace(safeGet(fields, idx))
	idx++

	conID := parseIntSafe(safeGet(fields, idx))
	idx++
	idx++ // min tick

	if serverVersion >= minServerVerMdSizeMultiplier && serverVersion < minServerVerSizeRules {
		idx++ // md size multiplier (deprecated)
	}

	_ = safeGet(fields, idx) // multiplier
	idx++
	_ = safeGet(fields, idx) // order types
	idx++
	_ = safeGet(fields, idx) // valid exchanges
	idx++

	if version >= 2 {
		idx++ // price magnifier
	}
	if version >= 4 {
		idx++ // underConId
	}

	primaryExch := ""
	if version >= 5 {
		idx++ // long name
		primaryExch = strings.TrimSpace(safeGet(fields, idx))
		idx++
	}

	timeZoneID := ""
	tradingHours := ""
	liquidHours := ""
	if version >= 6 {
		idx += 4 // contractMonth, industry, category, subcategory
		timeZoneID = safeGet(fields, idx)
		idx++
		tradingHours = safeGet(fields, idx)
		idx++
		liquidHours = safeGet(fields, idx)
		idx++
	}

	return &ContractDetailsLite{
		ReqID:        reqID,
		Symbol:       symbol,
		Exchange:     exchange,
		PrimaryExch:  primaryExch,
		ConID:        conID,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClass,
		TimeZoneID:   timeZoneID,
		TradingHours: tradingHours,
		LiquidHours:  liquidHours,
	}, true
}

// Stop gracefully shuts down the IBKR connector.
//
// Lifecycle contract for callers: callers in internal/daemon may still
// hold a *Connector reference and invoke methods on it after Stop
// returns (the daemon's gatewayConnector() snapshot is mu-guarded for
// the pointer but is released before the handler runs). Method calls
// on a stopped connector must therefore remain memory-safe — return
// errors or no-op, never panic or read freed memory. Do not introduce
// sync.Pool reuse, runtime finalizers, or any teardown that frees the
// *Connector struct itself without first refactoring the daemon to
// hold a lifecycle lock across handler calls.
func (c *Connector) Stop() error {
	// Drop c.mu BEFORE calling into pool.ReleaseLease — that path fires
	// the registered onDisconnect callback (attachConnectionHooks.func2),
	// which calls back into onConnectionLost and tries to acquire c.mu.
	// Holding mu across the callback would deadlock the shutdown path,
	// hanging the daemon process indefinitely after idle timeout / SIGTERM
	// because main goroutine is blocked unwinding stopConnector.
	//
	// Marking running=false before releasing the lock is the right
	// boundary: any reentrant caller that re-checks c.running mid-shutdown
	// sees a stopped connector and no-ops, and we don't have to invent a
	// second-state flag for "in the middle of shutting down".
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil
	}
	lease := c.lease
	pool := c.pool
	c.running = false
	c.mu.Unlock()

	c.logInfo("Stopping IBKR connector")

	// Release lease if we have one (fires onDisconnect → onConnectionLost
	// callback chain; safe now that c.mu is released).
	if lease != nil {
		if err := pool.ReleaseLease(lease.LeaseID); err != nil {
			c.logWarn("Error releasing lease: %v", err)
		}
	}

	// Stop connection pool
	if err := pool.Stop(); err != nil {
		c.logWarn("Error stopping connection pool: %v", err)
	}

	c.logInfo("IBKR connector stopped")

	return nil
}

// SubscribeMarketData subscribes to market data for a symbol. Idempotent:
// re-subscribing to the same symbol is a no-op (returns nil), so callers
// that race or run sequentially do not need to coordinate. Unsubscribe is
// the symmetric tear-down.
func (c *Connector) SubscribeMarketData(symbol string, fields []string) error {
	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		c.logDebug("Skipping SubscribeMarketData for inactive symbol %s", symbol)
		return nil
	}
	c.subMu.RLock()
	if sub, exists := c.subscriptions[symbol]; exists {
		c.subMu.RUnlock()
		marketDataLogger.Debugf("%s: SubscribeMarketData(%s) is a no-op; existing subscription reqID=%d", c.name, symbol, sub.ReqID)
		return nil
	}
	c.subMu.RUnlock()

	reqID := 0
	if c.conn != nil && c.conn.IsConnected() {
		contract, ready := c.prepareContract(symbol, 2*time.Second, true)
		contract, ready = c.waitForContractDetails(symbol, contract, ready)

		var err error
		switch {
		case ready:
			reqID, err = c.conn.RequestMarketDataWithContract(contract, "100,101,104,106,165,221,233", false, false)
		case contract.PrimaryExch != "":
			reqID, err = c.conn.RequestMarketDataWithPrimary(symbol, contract.PrimaryExch)
		default:
			reqID, err = c.conn.RequestMarketData(symbol)
		}
		if err != nil {
			c.logWarn("Failed to request market data for %s: %v", symbol, err)
			reqID = 0
		}
	}

	c.subMu.Lock()
	defer c.subMu.Unlock()

	// Race protection: another goroutine may have raced past the first
	// idempotency check. If we issued a reqID to IBKR, cancel it so we
	// don't leak a gateway-side subscription.
	if _, exists := c.subscriptions[symbol]; exists {
		if reqID != 0 && c.conn != nil && c.conn.IsConnected() {
			_ = c.conn.CancelMarketData(reqID)
		}
		marketDataLogger.Debugf("%s: SubscribeMarketData(%s) raced; reusing existing subscription", c.name, symbol)
		return nil
	}

	if reqID != 0 {
		c.reqIDMap[reqID] = symbol
	}

	c.subscriptions[symbol] = &Subscription{
		Symbol:   symbol,
		ReqID:    reqID,
		Fields:   fields,
		LastTime: time.Now(),
	}

	marketDataLogger.Infof("%s: Subscribed to market data for %s (ReqID: %d)", c.name, symbol, reqID)

	return nil
}

// EnsureMarketDataSubscription ensures there is an active, fresh subscription for a symbol.
// If a subscription exists but appears stale (no ticks in staleAfter), it will request again.
// Returns true if a request was sent (new or refreshed).
func (c *Connector) EnsureMarketDataSubscription(symbol string, fields []string, staleAfter time.Duration) (bool, error) {
	symbol = strings.ToUpper(symbol)
	if reason, inactive := c.inactiveReason(symbol); inactive {
		if reason == "" {
			reason = "inactive"
		}
		c.logDebug("Skipping EnsureMarketDataSubscription for inactive symbol %s (%s)", symbol, reason)
		return false, ErrSymbolInactive
	}
	c.subMu.Lock()
	defer c.subMu.Unlock()

	// Helper to (re)request from IBKR, mapping reqID
	request := func() (int, error) {
		if !c.IsReady() {
			return 0, fmt.Errorf("IBKR connection not ready")
		}

		contract, hasDetail := c.prepareContract(symbol, 2*time.Second, true)
		contract, hasDetail = c.waitForContractDetails(symbol, contract, hasDetail)
		if contract.SecType == "STK" && !hasDetail && contract.ConID == 0 {
			contract.PrimaryExch = ""
		}
		hydrated := hasDetail || contract.ConID != 0
		if !hydrated {
			if late := c.awaitContractDetail(symbol, contractHydrationGrace); late != nil {
				if c.applyContractDetail(*late, &contract) && contract.ConID != 0 {
					hydrated = true
				}
			}
		}
		if !hydrated {
			return 0, fmt.Errorf("contract details pending for %s", symbol)
		}

		var (
			reqID int
			err   error
		)

		reqID, err = c.conn.RequestMarketDataWithContract(contract, "100,101,104,106,165,221,233", false, false)
		if err != nil {
			return 0, err
		}
		c.reqIDMap[reqID] = symbol
		return reqID, nil
	}

	if sub, exists := c.subscriptions[symbol]; exists {
		// Refresh if stale
		if staleAfter > 0 && time.Since(sub.LastTime) >= staleAfter {
			if sub.ReqID != 0 {
				if conn := c.conn; conn != nil && conn.IsConnected() {
					if err := conn.CancelMarketData(sub.ReqID); err != nil {
						marketDataLogger.Warnf("%s: Failed to cancel stale market data for %s (ReqID: %d): %v", c.name, symbol, sub.ReqID, err)
					}
				} else if conn != nil && conn.rateLimiter != nil {
					// Connection not available – ensure local slot accounting stays in sync
					conn.rateLimiter.ReleaseMarketDataSlot()
				}
				// Reset subscription metadata so the new request can cleanly re-register
				sub.ReqID = 0
				sub.Observed = false
			}
			reqID, err := request()
			if err != nil {
				marketDataLogger.Warnf("%s: Failed to refresh market data for %s: %v", c.name, symbol, err)
				return false, err
			}
			sub.ReqID = reqID
			marketDataLogger.Infof("%s: Refreshed market data subscription for %s (ReqID: %d)", c.name, symbol, reqID)
			return true, nil
		}
		// Already subscribed and fresh enough
		return false, nil
	}

	// No existing subscription: create and request
	reqID := 0
	if c.IsReady() {
		if rid, err := request(); err == nil {
			reqID = rid
		} else {
			marketDataLogger.Warnf("%s: Failed to request market data for %s: %v", c.name, symbol, err)
			return false, err
		}
	} else {
		return false, fmt.Errorf("IBKR connection not ready")
	}

	sub := &Subscription{
		Symbol:   symbol,
		ReqID:    reqID,
		Fields:   fields,
		LastTime: time.Now(),
	}
	c.subscriptions[symbol] = sub
	marketDataLogger.Infof("%s: Subscribed to market data for %s (ReqID: %d)", c.name, symbol, reqID)
	return true, nil
}

// UnsubscribeMarketData unsubscribes from market data
func (c *Connector) UnsubscribeMarketData(symbol string) error {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	sub, exists := c.subscriptions[symbol]
	if !exists {
		// Make this idempotent; no-op if not found
		marketDataLogger.Debugf("%s: Unsubscribe requested for %s but no active subscription found", c.name, symbol)
		return nil
	}

	delete(c.subscriptions, symbol)

	// Best-effort cancel with IBKR if we have a reqID and the subscription was observed (to avoid 300 spam on shutdown)
	if c.conn != nil && c.conn.IsConnected() && sub.ReqID != 0 && sub.Observed {
		if err := c.conn.CancelMarketData(sub.ReqID); err != nil {
			marketDataLogger.Warnf("%s: Failed to cancel market data %s (ReqID: %d): %v", c.name, symbol, sub.ReqID, err)
		}
	}

	marketDataLogger.Infof("%s: Unsubscribed from market data for %s", c.name, symbol)
	return nil
}

// PlaceOrder sends an order to IBKR
func (c *Connector) PlaceOrder(order *Order) error {
	if !c.isConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	c.orderMu.Lock()
	defer c.orderMu.Unlock()

	// Validate order
	if err := c.validateOrder(order); err != nil {
		return fmt.Errorf("order validation failed: %w", err)
	}

	// Store order
	c.openOrders[order.ID] = order

	// TODO: Send order to IBKR via TCP connection
	c.logInfo("Placing order: %s %s %.0f %s @ %s",
		order.ID, order.Side, order.Quantity, order.Symbol, order.OrderType)

	// For now, simulate order placement
	order.Status = OrderStatusSubmitted
	order.CreatedAt = time.Now()

	return nil
}

// RawOrder represents an IBKR order
type RawOrder struct {
	OrderID    int
	ClientID   int
	PermID     int
	Action     string // BUY or SELL
	TotalQty   int
	OrderType  string // MKT, LMT, STP, etc.
	LmtPrice   float64
	AuxPrice   float64 // Stop price for stop orders
	TIF        string  // Time in force: DAY, GTC, IOC, etc.
	Account    string
	OrderRef   string // Our internal order ID
	OutsideRth bool   // Allow execution outside regular trading hours
}

// SubmitOrder submits an order to IBKR
func (c *Connector) SubmitOrder(contract *Contract, order *RawOrder) error {
	if !c.isConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	// Get connection from pool
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("no active connection")
	}

	// Convert to IBKROrder for the connection
	ibkrOrder := &IBKROrder{
		ConID:        contract.ConID,
		Symbol:       contract.Symbol,
		SecType:      contract.SecType,
		Expiry:       contract.Expiry,
		Strike:       contract.Strike,
		Right:        contract.Right,
		Multiplier:   multiplierToString(contract.Multiplier),
		Exchange:     contract.Exchange,
		PrimaryExch:  contract.PrimaryExch,
		Currency:     contract.Currency,
		LocalSymbol:  contract.LocalSymbol,
		TradingClass: contract.TradingClass,
		Action:       order.Action,
		TotalQty:     order.TotalQty,
		OrderType:    order.OrderType,
		LmtPrice:     order.LmtPrice,
		AuxPrice:     order.AuxPrice,
		TIF:          order.TIF,
		OrderRef:     order.OrderRef,
		OutsideRth:   order.OutsideRth,
		Account:      order.Account,
		Transmit:     true,
		OpenClose:    "O",
		Origin:       0,
	}

	// Place the order through the connection
	err := conn.PlaceOrder(ibkrOrder)
	if err != nil {
		return fmt.Errorf("failed to place order: %w", err)
	}

	// Track it locally as well
	brokerID := strconv.Itoa(ibkrOrder.OrderID)
	now := time.Now()
	coreOrder := &Order{
		ID:              order.OrderRef,
		BrokerID:        brokerID,
		Symbol:          contract.Symbol,
		Side:            OrderSide(order.Action),
		Quantity:        float64(order.TotalQty),
		OrderType:       mapIBOrderType(order.OrderType),
		LimitPrice:      order.LmtPrice,
		StopPrice:       order.AuxPrice,
		TimeInForce:     mapIBTimeInForce(order.TIF),
		Status:          OrderStatusPending,
		CreatedAt:       now,
		UpdatedAt:       now,
		AllowOutsideRth: order.OutsideRth,
	}

	c.orderMu.Lock()
	c.openOrders[order.OrderRef] = coreOrder
	c.brokerOrderIndex[brokerID] = order.OrderRef
	c.orderMu.Unlock()

	c.logInfo("Order submitted: ID=%d, %s %s %d @ %.2f (TIF=%s, OutsideRth=%v)",
		ibkrOrder.OrderID, order.Action, contract.Symbol, order.TotalQty,
		order.LmtPrice, order.TIF, order.OutsideRth)

	return nil
}

func multiplierToString(mult int) string {
	if mult <= 0 {
		return ""
	}
	return strconv.Itoa(mult)
}

// CancelOrder cancels an open order by its internal ID
func (c *Connector) CancelOrder(orderID int) error {
	if !c.isConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	// Get connection
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("no active connection")
	}

	// Send cancel request through connection
	err := conn.CancelOrder(orderID)
	if err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}

	// Update local tracking
	c.orderMu.Lock()
	orderIDStr := fmt.Sprintf("%d", orderID)
	if order, exists := c.openOrders[orderIDStr]; exists {
		order.Status = OrderStatusCancelled
		now := time.Now()
		order.CancelledAt = &now
	}
	c.orderMu.Unlock()

	c.logInfo("Order cancel request sent for ID: %d", orderID)

	return nil
}

// GetPositions retrieves current positions from IBKR
func (c *Connector) GetPositions() ([]*Position, error) {
	if !c.isConnected() {
		c.logWarn("GetPositions: Not connected (running=%v, lease=%v, conn=%v)",
			c.running, c.lease != nil, c.conn != nil)
		// Return empty positions in degraded mode
		return []*Position{}, nil
	}

	// Get connection from pool using our lease
	c.mu.RLock()
	lease := c.lease
	c.mu.RUnlock()

	if lease == nil {
		return nil, fmt.Errorf("no active lease")
	}

	conn, err := c.pool.GetConnection(lease.LeaseID)
	if err != nil {
		return nil, fmt.Errorf("failed to get connection: %w", err)
	}

	// Request latest positions
	if err := conn.RequestPositions(); err != nil {
		c.logWarn("Failed to request positions: %v", err)
		return nil, err
	}

	// Wait for positions to complete with a generous timeout
	if err := conn.WaitForPositionsEnd(15 * time.Second); err != nil {
		c.logWarn("Warning: %v (proceeding with partial data)", err)
		// Continue with whatever positions we have
	}

	// Get positions from connection
	ibkrPositions := conn.GetPositions()

	// Seed contract cache with any identifiers reported alongside positions so we
	// avoid redundant contract lookups during startup warm-ups.
	c.seedContractCacheFromPositions(ibkrPositions)

	// Convert IBKR positions to core positions
	result := make([]*Position, 0, len(ibkrPositions))
	for key, pos := range ibkrPositions {
		if pos == nil {
			continue
		}
		if pos.Contract.SecType == "STK" && pos.Contract.ConID == 0 {
			c.registerInactiveCandidate(pos.Contract.Symbol, "position missing contract id")
			c.logInfo("Skipping position for %s: missing contract ID (likely inactive)", pos.Contract.Symbol)
			continue
		}
		// Determine asset type
		assetType := AssetTypeStock
		if pos.Contract.SecType == "OPT" {
			assetType = AssetTypeOption
		}

		if pos.Contract.SecType == "STK" && c.IsSymbolInactive(pos.Contract.Symbol) {
			c.logDebug("Skipping position for inactive symbol %s (qty=%.2f)", pos.Contract.Symbol, pos.Position)
			continue
		}

		// For options, include strike and expiry in the symbol
		symbol := pos.Contract.Symbol
		if pos.Contract.SecType == "OPT" {
			// Format: SPY_231215_P440
			symbol = fmt.Sprintf("%s_%s_%s%.0f",
				pos.Contract.Symbol,
				pos.Contract.Expiry,
				pos.Contract.Right,
				pos.Contract.Strike)
		}

		currentPrice := pos.MarketPrice
		if currentPrice == 0 && pos.Position != 0 {
			denom := pos.Position * float64(pos.Contract.Multiplier)
			if denom != 0 && pos.MarketValue != 0 {
				currentPrice = math.Abs(pos.MarketValue / denom)
			}
		}

		unrealizedPnL := pos.UnrealizedPNL
		if unrealizedPnL == 0 && currentPrice != 0 && pos.AverageCost != 0 {
			delta := currentPrice - pos.AverageCost
			unrealizedPnL = delta * pos.Position * float64(pos.Contract.Multiplier)
		}

		assetMultiplier := pos.Contract.Multiplier
		if assetType == AssetTypeStock && assetMultiplier == 100 {
			assetMultiplier = 1
		}

		// Create core position
		corePos := &Position{
			ID: fmt.Sprintf("pos-%s-%d", key, time.Now().Unix()),
			Asset: Asset{
				Symbol:     symbol,
				Exchange:   pos.Contract.Exchange,
				AssetType:  assetType,
				Multiplier: assetMultiplier,
				Currency:   pos.Contract.Currency,
			},
			Quantity:      pos.Position,
			EntryPrice:    pos.AverageCost,
			CurrentPrice:  currentPrice,
			UnrealizedPnL: unrealizedPnL,
			RealizedPnL:   pos.RealizedPNL,
			OpenedAt:      time.Now(), // IBKR doesn't provide this
			UpdatedAt:     time.Now(),
		}

		result = append(result, corePos)
	}

	c.logInfo("Retrieved %d positions from IBKR", len(result))
	return result, nil
}

func (c *Connector) seedContractCacheFromPositions(positions map[string]*RawPosition) {
	if len(positions) == 0 {
		return
	}

	hints := make(map[string]ContractDetailsLite, len(positions))
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		contract := pos.Contract
		if contract.ConID == 0 {
			continue
		}
		// Only seed bare-symbol cache entries from stock positions. The
		// cache is indexed by the underlying ticker (e.g. "SPY") and is
		// later read by prepareContract when resolving stock quote
		// requests; if a held option (`SPY 700P 2026-06-18`, secType=OPT)
		// is allowed to seed under the bare key, prepareContract picks
		// up the option's ConID and `quote SPY` returns the option's
		// pricing instead of the ETF's. Filter to STK only.
		if !strings.EqualFold(contract.SecType, "STK") {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(contract.Symbol))
		if symbol == "" {
			continue
		}

		detail := ContractDetailsLite{
			Symbol:       symbol,
			Exchange:     strings.TrimSpace(contract.Exchange),
			PrimaryExch:  strings.TrimSpace(contract.PrimaryExch),
			ConID:        contract.ConID,
			LocalSymbol:  strings.TrimSpace(contract.LocalSymbol),
			TradingClass: strings.TrimSpace(contract.TradingClass),
		}

		if existing, ok := hints[symbol]; ok {
			hints[symbol] = mergeContractDetailsLite(existing, detail)
		} else {
			hints[symbol] = detail
		}
	}

	if len(hints) == 0 {
		return
	}

	c.contractMu.Lock()
	for symbol, hint := range hints {
		if cached, ok := c.contractCache[symbol]; ok {
			c.contractCache[symbol] = mergeContractDetailsLite(cached, hint)
		} else {
			c.contractCache[symbol] = hint
		}
	}
	c.contractMu.Unlock()
}

// GetAccountSummary retrieves account information from IBKR
func (c *Connector) GetAccountSummary() (*AccountSummary, error) {
	if !c.isConnected() {
		c.logWarn("GetAccountSummary: Not connected (running=%v, lease=%v, conn=%v)",
			c.running, c.lease != nil, c.conn != nil)
		// No connection - return error
		return nil, fmt.Errorf("IBKR connection not available")
	}

	// Get connection from pool using our lease
	c.mu.RLock()
	lease := c.lease
	c.mu.RUnlock()

	if lease == nil {
		return nil, fmt.Errorf("no active lease")
	}

	conn, err := c.pool.GetConnection(lease.LeaseID)
	if err != nil {
		return nil, fmt.Errorf("failed to get connection: %w", err)
	}

	// Request account summary with a unique request ID
	reqID := conn.GetNextRequestID()
	if err := conn.RequestAccountSummary(reqID, ""); err != nil {
		c.logWarn("Failed to request account summary: %v", err)
		return nil, err
	}

	// Wait for account summary to complete with a generous timeout
	if err := conn.WaitForAccountSummaryEnd(15 * time.Second); err != nil {
		c.logWarn("Warning: %v (proceeding with partial data)", err)
		// Continue with whatever account data we have
	}

	// Get account summary from connection
	accountData := conn.GetAccountSummary()

	summary := buildAccountSummary(accountData, conn.GetAccountCode())

	// Cancel the subscription to avoid continuous updates
	conn.CancelAccountSummary(reqID)

	c.logInfo("Account Summary: NetLiq=%.2f, Buying=%.2f, ExcessLiq=%.2f, MaintMargin=%.2f",
		summary.NetLiquidation, summary.BuyingPower, summary.ExcessLiquidity, summary.MaintenanceMargin)

	return summary, nil
}

func buildAccountSummary(accountData map[string]string, accountCode string) *AccountSummary {
	parseFloat := func(keys ...string) float64 {
		suffixes := []string{"_USD", "_BASE", "_EUR", "_S", "_C", "_CUR"}
		for _, key := range keys {
			for _, suffix := range suffixes {
				if val, exists := accountData[key+suffix]; exists {
					if f, err := strconv.ParseFloat(val, 64); err == nil {
						return f
					}
				}
			}
			if val, exists := accountData[key]; exists {
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					return f
				}
			}
		}
		return 0
	}

	netLiq := parseFloat("NetLiquidation", "NetLiquidationValue")
	availableFunds := parseFloat("AvailableFunds")
	excessLiquidity := parseFloat("ExcessLiquidity")
	maintMargin := parseFloat("MaintMarginReq", "FullMaintMarginReq")
	initMargin := parseFloat("InitMarginReq", "FullInitMarginReq")
	buyingPower := parseFloat("BuyingPower")
	if buyingPower == 0 {
		buyingPower = availableFunds
	}
	if buyingPower == 0 && excessLiquidity > 0 {
		buyingPower = excessLiquidity
	}
	if buyingPower == 0 && maintMargin > 0 {
		buyingPower = math.Max(netLiq-maintMargin, 0)
	}
	cashBalance := parseFloat("TotalCashValue", "CashBalance")
	realized := parseFloat("RealizedPnL")
	unrealized := parseFloat("UnrealizedPnL")
	grossPositionValue := parseFloat("GrossPositionValue")
	equityWithLoan := parseFloat("EquityWithLoanValue")

	if maintMargin == 0 && netLiq > 0 && excessLiquidity > 0 {
		maintMargin = netLiq - excessLiquidity
	}
	if excessLiquidity == 0 && netLiq > 0 && maintMargin > 0 {
		excessLiquidity = netLiq - maintMargin
	}

	return &AccountSummary{
		AccountID:          accountCode,
		NetLiquidation:     netLiq,
		BuyingPower:        buyingPower,
		CashBalance:        cashBalance,
		RealizedPNL:        realized,
		UnrealizedPNL:      unrealized,
		AvailableFunds:     availableFunds,
		ExcessLiquidity:    excessLiquidity,
		MaintenanceMargin:  maintMargin,
		InitialMargin:      initMargin,
		GrossPositionValue: grossPositionValue,
		EquityWithLoan:     equityWithLoan,
	}
}

// GetPool returns the connection pool for testing
func (c *Connector) GetPool() *ConnectionPool {
	return c.pool
}

// maintainLease sends heartbeats to keep the lease active
func (c *Connector) maintainLease() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		c.mu.RLock()
		running := c.running
		lease := c.lease
		c.mu.RUnlock()

		if !running || lease == nil {
			return
		}

		if err := c.pool.HeartbeatLease(lease.LeaseID); err != nil {
			c.logWarn("Failed to heartbeat lease: %v", err)
			// Try to get a new lease
			c.reconnect()
		}
	}
}

// reconnect attempts to get a new connection lease
func (c *Connector) reconnect() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease, err := c.pool.RequestLease(ctx, c.config.ServiceName, c.config.PreferredClientID)
	if err != nil {
		c.logWarn("Failed to get new lease: %v", err)
		return
	}

	conn, err := c.pool.GetConnection(lease.LeaseID)
	if err != nil {
		c.logWarn("Failed to get connection for new lease: %v", err)
		return
	}

	c.mu.Lock()
	c.lease = lease
	c.conn = conn
	c.mu.Unlock()

	c.logInfo("Reconnected with new lease (Client ID: %d)", lease.ClientID)
}

// isConnected checks if we have an active IBKR connection
func (c *Connector) isConnected() bool {
	c.mu.RLock()
	hasLease := c.lease != nil
	hasPool := c.pool != nil
	hasConn := c.conn != nil
	c.mu.RUnlock()

	// If we don't have basic requirements, not connected
	if !hasLease || !hasPool || !hasConn {
		return false
	}

	// Check if the connection is actually connected
	return c.conn.IsConnected()
}

// IsConnected exposes connection status for API/monitoring layers
func (c *Connector) IsConnected() bool { return c.isConnected() }

// UsingTLS reports the TLS mode the active session negotiated. False when
// disconnected or when a non-TLS handshake succeeded (possibly via fallback).
func (c *Connector) UsingTLS() bool {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return false
	}
	return conn.UsingTLS()
}

// IsReady indicates connection established and handlers registered
func (c *Connector) IsReady() bool {
	c.mu.RLock()
	rd := c.ready
	c.mu.RUnlock()
	return rd && c.isConnected()
}

// ServerVersion returns the IBKR server protocol version reported by the
// gateway during the handshake. Returns 0 when no connection is established.
func (c *Connector) ServerVersion() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.conn == nil {
		return 0
	}
	return c.conn.ServerVersion()
}

// SubscribeOption issues a streaming market-data subscription for a fully
// specified option contract (symbol + YYYYMMDD expiry + strike + C/P right).
// The result is keyed by an OPRA-style identifier so chain consumers can
// look up the cached quote in GetMarketData. ctx cancellation aborts the
// underlying contract-resolution round trip; callers that already have a
// per-request deadline should pass that ctx through.
func (c *Connector) SubscribeOption(ctx context.Context, underlying, expiryYMD string, strike float64, right string) (string, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.isConnected() {
		return "", 0, ErrIBKRUnavailable
	}
	key := fmt.Sprintf("%s_%s%s%.0f", strings.ToUpper(underlying), expiryYMD[2:], strings.ToUpper(right), strike)

	c.subMu.RLock()
	if existing, ok := c.subscriptions[key]; ok {
		c.subMu.RUnlock()
		return key, existing.ReqID, nil
	}
	c.subMu.RUnlock()

	contract := Contract{
		Symbol:     strings.ToUpper(underlying),
		SecType:    "OPT",
		Exchange:   "SMART",
		Currency:   "USD",
		Expiry:     expiryYMD,
		Strike:     strike,
		Right:      strings.ToUpper(right),
		Multiplier: 100,
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return "", 0, ErrIBKRUnavailable
	}

	// IBKR rejects reqMktData with code 200 ("No security definition has
	// been found for the request") when an option Contract is sent without
	// a resolved ConID. resolveOptionContract issues reqContractDetails,
	// waits for contractDataEnd, picks the best exchange match, and
	// populates ConID + TradingClass. Subsequent calls hit the option-
	// contract cache and skip the network round-trip.
	if err := conn.resolveOptionContract(ctx, &contract, 5*time.Second); err != nil {
		return "", 0, fmt.Errorf("resolve option %s %s %.2f%s: %w",
			contract.Symbol, contract.Expiry, contract.Strike, contract.Right, err)
	}

	// Generic ticks mirror RequestOptionsMarketData: 100=opt volume,
	// 101=opt open interest, 104=hist vol, 106=implied vol. Without 106
	// the chain strikes view shows blank IV columns whenever IBKR doesn't
	// happen to dispatch a model-computation tick (msg 21) — common after
	// hours and pre-market. Explicit subscription closes that gap.
	reqID, err := conn.RequestMarketDataWithContract(contract, "100,101,104,106", false, false)
	if err != nil {
		return "", 0, err
	}

	c.subMu.Lock()
	c.reqIDMap[reqID] = key
	c.subscriptions[key] = &Subscription{
		Symbol:   key,
		ReqID:    reqID,
		LastTime: time.Now(),
	}
	c.subMu.Unlock()
	// Route option-computation ticks (msg 21, tick types 10/11/13) for this
	// reqID into optIV / optQuoteMid keyed by the OPRA chain key. This is the
	// same handler path SubscribeOptionIV uses for ATM IV; per-strike chain
	// renders just need a different key so multiple strikes coexist.
	c.optMu.Lock()
	c.optReqIDs[reqID] = key
	c.optMu.Unlock()
	return key, reqID, nil
}

// RequestAccountUpdates subscribes to streaming account + portfolio updates.
// The gateway pushes mark prices, market values, and unrealized P&L for each
// open position via msgPortfolioValue, populating the same internal map that
// GetPositions reads. Pass an empty account to use the connector's bound one.
func (c *Connector) RequestAccountUpdates(account string) error {
	if !c.isConnected() {
		return ErrIBKRUnavailable
	}
	if account == "" {
		account = c.conn.GetAccountCode()
	}
	return c.conn.RequestAccountUpdates(account)
}

// GetCachedPositions returns whatever positions are currently in the
// connection's local cache without issuing a fresh reqPositions (which would
// clear the map and lose mark/value/P&L populated by the streaming
// portfolioValue subscription started by RequestAccountUpdates).
//
// Intended for the daemon: call RequestAccountUpdates at startup and let the
// gateway keep the cache fresh; clients then call GetCachedPositions for a
// non-destructive read.
func (c *Connector) GetCachedPositions() ([]*Position, error) {
	if !c.isConnected() {
		return []*Position{}, nil
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return []*Position{}, nil
	}
	ibkrPositions := conn.GetPositions()
	c.seedContractCacheFromPositions(ibkrPositions)
	return c.convertIBKRPositions(ibkrPositions), nil
}

// convertIBKRPositions adapts wire-level RawPosition rows into the domain
// Position shape, sharing the conversion logic used by GetPositions.
func (c *Connector) convertIBKRPositions(ibkrPositions map[string]*RawPosition) []*Position {
	result := make([]*Position, 0, len(ibkrPositions))
	for key, pos := range ibkrPositions {
		if pos == nil {
			continue
		}
		if pos.Contract.SecType == "STK" && pos.Contract.ConID == 0 {
			continue
		}
		assetType := AssetTypeStock
		if pos.Contract.SecType == "OPT" {
			assetType = AssetTypeOption
		}
		if pos.Contract.SecType == "STK" && c.IsSymbolInactive(pos.Contract.Symbol) {
			continue
		}
		symbol := pos.Contract.Symbol
		if pos.Contract.SecType == "OPT" {
			symbol = fmt.Sprintf("%s_%s_%s%.0f",
				pos.Contract.Symbol, pos.Contract.Expiry, pos.Contract.Right, pos.Contract.Strike)
		}
		assetMultiplier := pos.Contract.Multiplier
		if assetType == AssetTypeStock && assetMultiplier == 100 {
			assetMultiplier = 1
		}
		result = append(result, &Position{
			ID: fmt.Sprintf("pos-%s-%d", key, time.Now().Unix()),
			Asset: Asset{
				Symbol:     symbol,
				Exchange:   pos.Contract.Exchange,
				AssetType:  assetType,
				Multiplier: assetMultiplier,
				Currency:   pos.Contract.Currency,
			},
			Quantity:      pos.Position,
			EntryPrice:    pos.AverageCost,
			CurrentPrice:  pos.MarketPrice,
			UnrealizedPnL: pos.UnrealizedPNL,
			RealizedPnL:   pos.RealizedPNL,
			OpenedAt:      time.Now(),
			UpdatedAt:     time.Now(),
		})
	}
	return result
}

// RegisterExecutionListener attaches a callback invoked for each execution report received.
// Multiple listeners are supported; listeners added before the connection is ready are
// automatically wired in when handlers register.
func (c *Connector) RegisterExecutionListener(listener func(*ExecutionReport)) {
	if listener == nil {
		return
	}

	c.execMu.Lock()
	c.execListeners = append(c.execListeners, listener)
	c.execMu.Unlock()

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn != nil {
		c.installExecutionHandler(conn)
	}
}

// RegisterCommissionListener attaches a callback invoked for each commission report received.
func (c *Connector) RegisterCommissionListener(listener func(*CommissionReport)) {
	if listener == nil {
		return
	}

	c.commMu.Lock()
	c.commListeners = append(c.commListeners, listener)
	c.commMu.Unlock()

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn != nil {
		c.installCommissionHandler(conn)
	}
}

// RequestExecutions forwards a reqExecutions call to the active IBKR connection.
func (c *Connector) RequestExecutions(filter ExecutionFilter) (int, error) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return 0, fmt.Errorf("IBKR connection not available")
	}
	return conn.RequestExecutions(filter)
}

// validateOrder performs basic order validation
func (c *Connector) validateOrder(order *Order) error {
	if order == nil {
		return fmt.Errorf("order is nil")
	}

	if order.Symbol == "" {
		return fmt.Errorf("symbol is required")
	}

	if order.Quantity <= 0 {
		return fmt.Errorf("quantity must be positive")
	}

	if order.Side != OrderSideBuy && order.Side != OrderSideSell {
		return fmt.Errorf("invalid order side: %s", order.Side)
	}

	if order.OrderType == OrderTypeLimit && order.LimitPrice <= 0 {
		return fmt.Errorf("limit price required for limit orders")
	}

	return nil
}

// registerHandlers sets up message handlers for IBKR responses.
// We keep handlers simple and thread-safe, and register them before any
// subscriptions so early messages (e.g., farm notices, nextValidId) are handled.
func (c *Connector) registerHandlers(conn *Connection) {
	if conn == nil {
		return
	}

	c.logInfo("Registering message handlers")

	// Register tick price handler (msgID 1)
	conn.RegisterHandler(1, func(fields []string) {
		c.handleTickPrice(fields)
	})

	// Register tick size handler (msgID 2)
	conn.RegisterHandler(2, func(fields []string) {
		c.handleTickSize(fields)
	})

	// Register generic tick handler (msgID 45) for items like option IV (106)
	conn.RegisterHandler(45, func(fields []string) {
		c.handleTickGeneric(fields)
	})

	// Register option computation handler (msgID 21) for greeks and model IV
	conn.RegisterHandler(msgTickOptionComputation, func(fields []string) {
		c.handleOptionComputation(fields)
	})

	// Register historical data handler (msgID 17) for HMDS backfill
	conn.RegisterHandler(msgHistoricalData, func(fields []string) {
		c.handleHistoricalData(fields)
	})

	// Register historical data end handler (msgID 108) to finalize empty results
	conn.RegisterHandler(msgHistoricalDataEnd, func(fields []string) {
		c.handleHistoricalDataEnd(fields)
	})

	// Register error handler (msgID 4) to proactively refresh subscriptions on errors
	conn.RegisterHandler(4, func(fields []string) {
		c.handleIBKRError(fields)
	})

	// Register position handler (msgID 61)
	conn.RegisterHandler(61, func(fields []string) {
		c.handlePosition(fields)
	})

	// Register portfolio value handler (msgID 7)
	conn.RegisterHandler(7, func(fields []string) {
		c.handlePortfolioValue(fields)
	})

	// Register order status handler (msgID 3)
	conn.RegisterHandler(3, func(fields []string) {
		c.handleOrderStatus(fields)
	})

	// Register system notification handler (msgID 204) for farm status changes
	conn.RegisterHandler(204, func(fields []string) {})
	conn.SetSystemNoticeHandler(func(note *systemNotification, alias reqAliasEntry) {
		c.processSystemNotice(alias, note)
	})

	// Register execution and commission handlers if listeners are present.
	c.execMu.RLock()
	hasExecListeners := len(c.execListeners) > 0
	c.execMu.RUnlock()
	if hasExecListeners {
		c.installExecutionHandler(conn)
	}

	c.commMu.RLock()
	hasCommissionListeners := len(c.commListeners) > 0
	c.commMu.RUnlock()
	if hasCommissionListeners {
		c.installCommissionHandler(conn)
	}
}

// handleTickPrice processes tick price updates.
// Accepts [msgID, version, reqID, tickType, price, ...] and updates the
// associated subscription (bid/ask/last) and freshness timestamp.
func (c *Connector) handleTickPrice(fields []string) {
	if len(fields) < 4 {
		return
	}

	// Format: [msgID, version, reqID, tickType, price, ...]
	if len(fields) < 5 {
		return
	}
	reqIDStr := fields[2]
	tickTypeStr := fields[3]
	priceStr := strings.TrimSpace(fields[4])
	// Parse reqID with validation
	reqID := 0
	if n, err := fmt.Sscanf(reqIDStr, "%d", &reqID); err != nil || n != 1 || reqID == 0 {
		marketDataLogger.Warnf("Invalid reqID in tick price: %q (error: %v)", reqIDStr, err)
		return
	}

	// Find the symbol for this request ID
	c.subMu.RLock()
	symbol, exists := c.reqIDMap[reqID]
	c.subMu.RUnlock()

	// Parse tickType with validation
	tickType := 0
	if n, err := fmt.Sscanf(tickTypeStr, "%d", &tickType); err != nil || n != 1 {
		if exists {
			marketDataLogger.Warnf("Invalid tickType in tick price for reqID %d: %q (error: %v)", reqID, tickTypeStr, err)
		}
		return
	}

	// Handle empty price payload (IBKR sends blank string for stale ticks)
	if priceStr == "" {
		if exists {
			c.subMu.Lock()
			if sub, ok := c.subscriptions[symbol]; ok {
				sub.LastTime = time.Now()
			}
			c.subMu.Unlock()
		}
		return
	}

	// Parse price with validation
	price := 0.0
	if n, err := fmt.Sscanf(priceStr, "%f", &price); err != nil || n != 1 {
		if exists {
			marketDataLogger.Warnf("Invalid price in tick price for reqID %d: %q (error: %v)", reqID, priceStr, err)
		}
		return
	}

	if !exists {
		// Unknown reqID - might be from previous session or automatic subscription
		// ReqID 6 appears to be an automatic subscription from IBKR for account positions
		if reqID != 6 {
			marketDataLogger.Debugf("Received tick for unknown reqID %d", reqID)
		}
		return
	}

	// Log all market data for debugging with comprehensive tick type mapping
	tickTypeName := "unknown"
	isImportantTick := false
	switch tickType {
	case 1:
		tickTypeName = "bid"
		isImportantTick = true
	case 2:
		tickTypeName = "ask"
		isImportantTick = true
	case 4:
		tickTypeName = "last"
		isImportantTick = true
	case 6:
		tickTypeName = "high"
	case 7:
		tickTypeName = "low"
	case 9:
		tickTypeName = "close"
	case 14:
		tickTypeName = "open"
	case 15:
		tickTypeName = "low_13_weeks"
	case 16:
		tickTypeName = "high_13_weeks"
	case 17:
		tickTypeName = "low_26_weeks"
	case 18:
		tickTypeName = "high_26_weeks"
	case 19:
		tickTypeName = "low_52_weeks"
	case 20:
		tickTypeName = "high_52_weeks"
	case 37:
		tickTypeName = "mark_price"
		isImportantTick = true
	case 221:
		tickTypeName = "mark_price_slow"
	case 225:
		tickTypeName = "auction_data"
		marketDataLogger.Infof("%s AUCTION DATA received (tick 225): %.2f", symbol, price)
	case 232:
		tickTypeName = "last_yield"
	case 233:
		tickTypeName = "rt_volume"
	default:
		tickTypeName = fmt.Sprintf("tick_%d", tickType)
	}

	// Handle option implied volatility specially
	if tickType == 106 { // Option Implied Volatility
		c.subMu.RLock()
		symbol, exists := c.reqIDMap[reqID]
		c.subMu.RUnlock()
		if exists && price > 0 {
			// Normalize: if coming in percent (e.g., 35.0), convert to fraction
			iv := price
			if iv > 1.5 { // heuristic threshold
				iv = iv / 100.0
			}
			c.optMu.Lock()
			c.optIV[symbol] = iv
			c.optMu.Unlock()
		}
		// Do not treat 106 as a price for subscription
		return
	}

	// If this tick belongs to an option reqID, capture option quote mid and do not update underlying subscription
	c.optMu.RLock()
	optSym, isOptionReq := c.optReqIDs[reqID]
	c.optMu.RUnlock()
	if isOptionReq {
		// Validate option prices same as regular prices
		if price > 0 {
			c.optMu.Lock()
			switch tickType {
			case 1:
				c.optQuoteBid[optSym] = price
			case 2:
				c.optQuoteAsk[optSym] = price
			case 37:
				c.optQuoteMid[optSym] = price
			}
			// Derive mid if we have bid/ask
			if b, okb := c.optQuoteBid[optSym]; okb {
				if a, oka := c.optQuoteAsk[optSym]; oka && b > 0 && a > 0 {
					c.optQuoteMid[optSym] = (b + a) / 2
				}
			}
			c.optMu.Unlock()
		}
		return
	}

	// Log important ticks only if price > 0
	if (isImportantTick || tickType == 225) && price > 0 {
		marketDataLogger.Infof("%s %s: %.2f", symbol, tickTypeName, price)
	}

	// Update subscription data based on tick type
	c.subMu.Lock()
	defer c.subMu.Unlock()

	sub, exists := c.subscriptions[symbol]
	if !exists {
		return
	}

	// Validate price before updating: reject zero and negative prices to prevent
	// overwriting valid prices with "no quote available" indicators from IBKR.
	// Exception: Very small but positive prices (e.g., 0.0001 for penny stocks) are allowed.
	if price <= 0 {
		// Update LastTime to show we received a tick, but don't update the price
		sub.LastTime = time.Now()
		return
	}

	// Mark subscription observed once we accept a valid price
	sub.Observed = true

	// Tick types: 1=bid, 2=ask, 4=last, 6=high, 7=low, 9=close, 14=open.
	// Close (9) is yesterday's regular-session close — the anchor for
	// change-vs-prev-close. IBKR sends it automatically once per reqMktData,
	// regardless of generic-tick flags.
	//
	// Tick types 15-20 (week-range highs/lows) only arrive when the
	// streaming subscribe asked for generic tick 165 (Misc Stats). They are
	// load-bearing for the scan-row 52w column; without them the column
	// would be silently blank for symbols that didn't have an explicit
	// historical-bars fetch.
	switch tickType {
	case 1:
		sub.Bid = price
	case 2:
		sub.Ask = price
	case 4:
		sub.LastPrice = price
	case 6:
		sub.High = price
	case 7:
		sub.Low = price
	case 9:
		sub.PrevClose = price
	case 14:
		sub.Open = price
	case 15:
		sub.Week13Low = price
	case 16:
		sub.Week13High = price
	case 17:
		sub.Week26Low = price
	case 18:
		sub.Week26High = price
	case 19:
		sub.Week52Low = price
	case 20:
		sub.Week52High = price
	}
	sub.LastTime = time.Now()
}

// handleTickGeneric processes generic tick updates (e.g., 106 = option implied vol).
func (c *Connector) handleTickGeneric(fields []string) {
	// Expected format: [msgID, version, reqID, tickType, value]
	if len(fields) < 5 {
		return
	}
	reqIDStr := fields[2]
	tickTypeStr := fields[3]
	valueStr := fields[4]

	reqID := 0
	fmt.Sscanf(reqIDStr, "%d", &reqID)
	tickType := 0
	fmt.Sscanf(tickTypeStr, "%d", &tickType)
	val := 0.0
	fmt.Sscanf(valueStr, "%f", &val)

	// Map reqID to underlying symbol
	c.subMu.RLock()
	symbol, exists := c.reqIDMap[reqID]
	c.subMu.RUnlock()
	if !exists {
		return
	}

	// 106 = Option Implied Volatility (averaged across the chain — the
	// "IV of the underlying" that retail platforms display).
	if tickType == 106 && val > 0 {
		iv := val
		if iv > 1.5 { // normalize percent inputs
			iv = iv / 100.0
		}
		c.optMu.Lock()
		c.optIV[symbol] = iv
		c.optMu.Unlock()
		// Also write to the per-symbol subscription so GetMarketData sees
		// it without having to consult the option-IV cache separately —
		// scan-row enrichment and `quote --json` both read from there.
		c.subMu.Lock()
		if sub, ok := c.subscriptions[symbol]; ok {
			sub.IV = iv
			sub.LastTime = time.Now()
		}
		c.subMu.Unlock()
	}
}

// handleOptionComputation processes IBKR option computation ticks (msgID 21),
// which provide implied volatility, greeks, and theoretical prices for option contracts.
func (c *Connector) handleOptionComputation(fields []string) {
	// Expected IB payload:
	// [msgID, version, reqID, tickType, impliedVol, delta, optPrice, pvDividend, gamma, vega, theta, underlyingPrice]
	if len(fields) < 12 {
		return
	}

	reqID, err := strconv.Atoi(fields[2])
	if err != nil {
		return
	}
	tickType, err := strconv.Atoi(fields[3])
	if err != nil {
		return
	}

	parseFloat := func(s string) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return math.NaN()
		}
		return v
	}

	impliedVol := parseFloat(fields[4])
	delta := parseFloat(fields[5])
	optionPrice := parseFloat(fields[6])
	gamma := parseFloat(fields[8])
	vega := parseFloat(fields[9])
	theta := parseFloat(fields[10])
	underlyingPrice := parseFloat(fields[11])

	c.optMu.Lock()
	symbol, exists := c.optReqIDs[reqID]
	if !exists {
		c.optMu.Unlock()
		return
	}

	switch tickType {
	case 10: // bid computation
		if optionPrice > 0 {
			c.optQuoteBid[symbol] = optionPrice
		}
	case 11: // ask computation
		if optionPrice > 0 {
			c.optQuoteAsk[symbol] = optionPrice
		}
	case 12: // last traded
		if optionPrice > 0 {
			c.optQuoteMid[symbol] = optionPrice
		}
	case 13: // model computation — canonical source for greeks
		if optionPrice > 0 {
			c.optQuoteMid[symbol] = optionPrice
		}
		if impliedVol > 0 {
			if impliedVol > 1.5 {
				impliedVol /= 100.0
			}
			c.optIV[symbol] = impliedVol
		}
		// IBKR sends a NaN/sentinel-tagged Greeks row when the model hasn't
		// priced the contract yet (typical for far OTM / illiquid OOH). We
		// only commit Greeks once at least one component is sane — never
		// fabricate zeros where the model abstained.
		g, ok := c.optGreeks[symbol]
		if !ok {
			g = Greeks{}
		}
		if saneGreek(delta, 1.05) { // delta bounded by 1; tiny slack for binomial drift
			g.Delta = delta
		}
		if saneGreek(gamma, 10) {
			g.Gamma = gamma
		}
		if saneGreek(theta, 1e6) {
			g.Theta = theta
		}
		if saneGreek(vega, 1e6) {
			g.Vega = vega
		}
		if g != (Greeks{}) {
			c.optGreeks[symbol] = g
		}
		if !math.IsNaN(underlyingPrice) && underlyingPrice > 0 && underlyingPrice < 1e9 {
			c.optUnderlyingPx[symbol] = underlyingPrice
		}
	}

	// Derive mid when both bid/ask present
	if b, ok := c.optQuoteBid[symbol]; ok && b > 0 {
		if a, ok := c.optQuoteAsk[symbol]; ok && a > 0 {
			c.optQuoteMid[symbol] = (b + a) / 2
		}
	}

	c.optMu.Unlock()
}

// saneGreek rejects NaN and IBKR's MaxFloat-style sentinel values that fire
// when the gateway emits a Greeks row before the model has priced the
// contract. bound is an asset-class-aware upper magnitude.
func saneGreek(v, bound float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	if math.Abs(v) > bound {
		return false
	}
	return true
}

// handleIBKRError receives raw IBKR error messages for proactive recovery.
// fields: [msgID=4, version, reqID, errorCode, errorMsg]
func (c *Connector) handleIBKRError(fields []string) {
	if len(fields) < 4 {
		return
	}
	// Parse reqID and code
	reqID := 0
	if len(fields) > 2 {
		if v, err := strconv.Atoi(fields[2]); err == nil {
			reqID = v
		}
	}
	code := 0
	if len(fields) > 3 {
		if v, err := strconv.Atoi(fields[3]); err == nil {
			code = v
		}
	}

	// Map to symbol if available (subscriptions or historical request)
	symbol := ""
	histPending := false
	if reqID > 0 {
		c.subMu.RLock()
		symbol = c.reqIDMap[reqID]
		c.subMu.RUnlock()
		if symbol == "" {
			c.historicalMu.Lock()
			if hr, ok := c.historicalReqs[reqID]; ok {
				symbol = hr.symbol
				histPending = true
			}
			c.historicalMu.Unlock()
		}
		if symbol == "" {
			if alias, ok := c.conn.lookupReqAlias(reqID); ok && alias.symbol != "" {
				symbol = alias.symbol
			}
		}
	}

	// Record for stability reporting
	if code != 0 {
		c.recordError(code)
	}

	rawMsg := ""
	if len(fields) > 4 {
		rawMsg = fields[4]
	}
	upperMsg := strings.ToUpper(rawMsg)
	upperSymbol := strings.ToUpper(symbol)
	if symbol != "" && symbol != upperSymbol {
		symbol = upperSymbol
	}
	parserMisalign := strings.Contains(upperMsg, "MART") ||
		strings.Contains(upperMsg, "'BOE") || strings.Contains(upperMsg, "\"BOE") || strings.Contains(upperMsg, " BOE")
	if parserMisalign {
		context := c.parserContext(symbol)
		if context != "" {
			ibkrLogger.Errorf("[IBKR] Parser misalignment detected (code=%d reqID=%d symbol=%s): %s | frame=%s", code, reqID, symbol, rawMsg, context)
		} else {
			ibkrLogger.Errorf("[IBKR] Parser misalignment detected (code=%d reqID=%d symbol=%s): %s", code, reqID, symbol, rawMsg)
		}
	}

	// If this error targets an outstanding historical request, fail it immediately
	if histPending {
		if code == 0 || code == -1 || (code >= 2100 && code < 2200) {
			return // informational notices
		}
		msg := rawMsg
		hErr := &HistoricalRequestError{Code: code, Message: msg}
		switch code {
		case 162:
			if hErr.Message == "" {
				hErr.Message = "historical data pacing violation"
			}
			hErr.RetryAfter = c.nextHistoricalBackoff(symbol)
		case 321:
			if hErr.Message == "" {
				hErr.Message = "historical data request failed validation"
			}
			c.resetHistoricalBackoff(symbol)
		default:
			c.resetHistoricalBackoff(symbol)
		}
		c.failHistoricalRequest(reqID, hErr)
		if symbol != "" {
			if code == 366 || (code == 162 && strings.Contains(upperMsg, "NO DATA")) {
				c.registerInactiveCandidate(symbol, rawMsg)
			}
		}
		return
	}

	if code == 200 && symbol != "" {
		if strings.Contains(upperMsg, "NO SECURITY DEFINITION HAS BEEN FOUND") {
			if c.registerInactiveCandidate(symbol, rawMsg) {
				return
			}
		}
	}

	switch code {
	case 2108: // Market data farm disconnected
		// Mark subs unobserved to force refresh path
		c.subMu.Lock()
		for _, sub := range c.subscriptions {
			sub.Observed = false
		}
		c.subMu.Unlock()
	case 2119, 2104: // Farm OK/connected
		// Trigger gentle refresh for all current subs
		go func() {
			time.Sleep(500 * time.Millisecond)
			c.subMu.RLock()
			syms := make([]string, 0, len(c.subscriptions))
			for s := range c.subscriptions {
				syms = append(syms, s)
			}
			c.subMu.RUnlock()
			for _, s := range syms {
				_, _ = c.EnsureMarketDataSubscription(s, []string{"BID", "ASK", "LAST", "VOLUME"}, 0)
			}
		}()
	case 200, 320, 321, 354:
		// Destination/parse/validation errors: refresh this specific symbol immediately with alternate routing
		if symbol != "" {
			go c.refreshSubscription(symbol)
		}
	}
}

// refreshSubscription cancels and re-requests a subscription with alternate routing hints.
func (c *Connector) refreshSubscription(symbol string) {
	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		return
	}
	c.subMu.Lock()
	sub, ok := c.subscriptions[symbol]
	if !ok {
		c.subMu.Unlock()
		return
	}
	// Best-effort cancel if active
	if c.conn != nil && c.conn.IsConnected() && sub.ReqID != 0 {
		_ = c.conn.CancelMarketData(sub.ReqID)
	}
	// Select routing: toggle primary hint if available
	primary := ""
	c.contractMu.RLock()
	if d, ok := c.contractCache[symbol]; ok {
		primary = d.PrimaryExch
	}
	c.contractMu.RUnlock()
	// Clear observed and reqID; we will replace it
	sub.Observed = false
	sub.ReqID = 0
	c.subMu.Unlock()

	// Re-request
	if !c.IsReady() {
		return
	}
	var (
		reqID int
		err   error
	)
	if primary != "" {
		// First try without primary to avoid repeating the same rejection
		reqID, err = c.conn.RequestMarketData(symbol)
		if err != nil {
			// Fall back to primary if no-primary fails
			reqID, err = c.conn.RequestMarketDataWithPrimary(symbol, primary)
		}
	} else {
		// Try with classified primary if known; otherwise plain SMART
		if _, exch, _, prim := classifySymbol(symbol); exch == "SMART" && prim != "" {
			reqID, err = c.conn.RequestMarketDataWithPrimary(symbol, prim)
		} else {
			reqID, err = c.conn.RequestMarketData(symbol)
		}
	}
	if err != nil {
		return
	}
	c.subMu.Lock()
	c.reqIDMap[reqID] = symbol
	if sub2, ok := c.subscriptions[symbol]; ok {
		sub2.ReqID = reqID
	}
	c.subMu.Unlock()
}

func (c *Connector) parserContext(symbol string) string {
	conn := c.conn
	if conn == nil {
		return ""
	}
	return conn.parserContext(symbol)
}

func (c *Connector) handleHistoricalData(fields []string) {
	if len(fields) < 2 {
		return
	}

	// For serverVersion >= 124, no version field in historical data messages
	// (We require minimum serverVersion 124, so version field never present)
	idx := 1

	if idx >= len(fields) {
		return
	}

	reqID, err := strconv.Atoi(fields[idx])
	if err != nil {
		return
	}
	idx++

	req := c.getHistoricalRequest(reqID)
	if req == nil {
		return
	}

	serverVersion := 0
	if c.conn != nil {
		serverVersion = c.conn.ServerVersion()
	}
	legacyFormat := false
	if serverVersion > 0 {
		legacyFormat = serverVersion < minServerVerHistoricalDataEnd
	} else if idx < len(fields) {
		// Auto-detect: if next field is non-numeric treat as legacy header
		if _, err := strconv.Atoi(fields[idx]); err != nil {
			legacyFormat = true
		}
	}

	start := ""
	end := ""
	if legacyFormat {
		start = safeField(fields, &idx)
		end = safeField(fields, &idx)
	}

	count := 0
	if idx < len(fields) {
		if v, err := strconv.Atoi(fields[idx]); err == nil {
			count = v
		}
		idx++
	}

	bars := make([]HistoricalBar, 0, count)
	for i := 0; i < count; i++ {
		if idx >= len(fields) {
			break
		}

		dateStr := fields[idx]
		idx++

		// Require six scalar fields (open, high, low, close, volume, average) plus barCount
		if idx+6 > len(fields) {
			break
		}

		openVal := parseFloat(fields[idx])
		idx++
		highVal := parseFloat(fields[idx])
		idx++
		lowVal := parseFloat(fields[idx])
		idx++
		closeVal := parseFloat(fields[idx])
		idx++
		volumeVal := parseFloat(fields[idx])
		idx++
		avgVal := parseFloat(fields[idx])
		idx++

		barCount := 0
		if idx < len(fields) {
			if v, err := strconv.Atoi(fields[idx]); err == nil {
				barCount = v
			}
			idx++
		}

		barTime, _ := parseHistoricalTimestamp(dateStr)
		bars = append(bars, HistoricalBar{
			Time:     barTime,
			Date:     dateStr,
			Open:     openVal,
			High:     highVal,
			Low:      lowVal,
			Close:    closeVal,
			Volume:   int64(volumeVal),
			Average:  avgVal,
			BarCount: barCount,
		})
	}

	c.completeHistoricalRequest(reqID, historicalResult{
		start: start,
		end:   end,
		bars:  bars,
	})
}

func (c *Connector) handleHistoricalDataEnd(fields []string) {
	if len(fields) < 3 {
		return
	}

	idx := 1
	if len(fields) > idx {
		if _, err := strconv.Atoi(fields[idx]); err == nil {
			idx++
		}
	}
	if idx >= len(fields) {
		return
	}

	reqID, err := strconv.Atoi(fields[idx])
	if err != nil {
		return
	}
	idx++

	start := ""
	if idx < len(fields) {
		start = fields[idx]
		idx++
	}
	end := ""
	if idx < len(fields) {
		end = fields[idx]
	}

	c.completeHistoricalRequest(reqID, historicalResult{start: start, end: end})
}

func safeField(fields []string, idx *int) string {
	if *idx >= len(fields) {
		return ""
	}
	val := fields[*idx]
	*idx = *idx + 1
	return val
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func parseHistoricalTimestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty historical timestamp")
	}

	normalized := strings.ReplaceAll(raw, "  ", " ")

	layouts := []string{
		"20060102 15:04:05",
		"20060102",
	}

	for _, layout := range layouts {
		if ts, err := time.ParseInLocation(layout, normalized, time.UTC); err == nil {
			return ts, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse historical timestamp: %s", raw)
}

func (c *Connector) getHistoricalRequest(reqID int) *historicalRequest {
	c.historicalMu.Lock()
	defer c.historicalMu.Unlock()
	return c.historicalReqs[reqID]
}

func (c *Connector) createHistoricalRequest(reqID int, symbol string) *historicalRequest {
	req := &historicalRequest{
		symbol: symbol,
		result: make(chan historicalResult, 1),
	}
	c.historicalMu.Lock()
	c.historicalReqs[reqID] = req
	c.historicalMu.Unlock()
	return req
}

func (c *Connector) completeHistoricalRequest(reqID int, res historicalResult) {
	c.historicalMu.Lock()
	req, ok := c.historicalReqs[reqID]
	if ok {
		delete(c.historicalReqs, reqID)
	}
	c.historicalMu.Unlock()
	if !ok {
		return
	}
	req.result <- res
	close(req.result)
	if len(res.bars) > 0 {
		c.resetHistoricalBackoff(req.symbol)
	}
}

func (c *Connector) failHistoricalRequest(reqID int, err error) {
	c.completeHistoricalRequest(reqID, historicalResult{err: err})
}

func (c *Connector) nextHistoricalBackoff(symbol string) time.Duration {
	const base = 30 * time.Second
	const maxDelay = 5 * time.Minute

	c.historicalMu.Lock()
	defer c.historicalMu.Unlock()

	count := c.historicalBackoff[symbol] + 1
	if count > 10 {
		count = 10
	}
	c.historicalBackoff[symbol] = count

	delay := base * time.Duration(1<<(count-1))
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

func (c *Connector) resetHistoricalBackoff(symbol string) {
	c.historicalMu.Lock()
	delete(c.historicalBackoff, symbol)
	c.historicalMu.Unlock()
}

func formatHistoricalDuration(lookbackDays int) string {
	if lookbackDays <= 0 {
		return "1 D"
	}
	if lookbackDays <= 365 {
		return fmt.Sprintf("%d D", lookbackDays)
	}
	years := lookbackDays / 365
	if years <= 0 {
		years = 1
	}
	if years == 1 {
		return "1 Y"
	}
	return fmt.Sprintf("%d Y", years)
}

// FetchHistoricalDailyBars requests HMDS daily bars for the provided symbol.
// It returns an error if the connector is not ready or the request times out.
func (c *Connector) FetchHistoricalDailyBars(symbol string, lookbackDays int, timeout time.Duration) ([]HistoricalBar, error) {
	if !c.IsReady() {
		return nil, fmt.Errorf("IBKR connection not ready")
	}

	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		return nil, ErrSymbolInactive
	}

	if lookbackDays <= 0 {
		lookbackDays = 400
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}

	secType, exchange, currency, primary := classifySymbol(symbol)
	baseContract := Contract{
		Symbol:      symbol,
		SecType:     secType,
		Exchange:    exchange,
		PrimaryExch: primary,
		Currency:    currency,
	}

	graceWindow := contractDetailsLateGrace
	if timeout > 0 {
		if half := timeout / 2; half > 0 && half < graceWindow {
			graceWindow = half
		}
	}

	var fetchErr error
	if detail, err := c.ensureContractDetails(symbol, 5*time.Second); err == nil && detail != nil {
		c.applyContractDetail(*detail, &baseContract)
	} else {
		fetchErr = err
		late := c.awaitContractDetail(symbol, graceWindow)
		if late != nil && c.applyContractDetail(*late, &baseContract) {
			c.logInfo("Contract details for %s arrived during grace window (conID=%d)", symbol, late.ConID)
		} else if fetchErr != nil {
			c.logWarn("Contract details for %s unavailable (%v); using static classification hints only", symbol, fetchErr)
		}
	}

	if baseContract.ConID == 0 {
		c.logWarn("Historical data request aborted for %s: contract ID unresolved (exchange=%s primary=%s)", symbol, baseContract.Exchange, baseContract.PrimaryExch)
		return nil, fmt.Errorf("contract details unresolved for %s (exchange=%s primary=%s)", symbol, baseContract.Exchange, baseContract.PrimaryExch)
	}

	baseWhat := defaultHistoricalWhat(baseContract.SecType)
	altWhat := alternateHistoricalWhat(baseWhat)

	type attempt struct {
		contract   Contract
		whatToShow string
		label      string
	}

	seq := historicalWhatSequence(symbol, baseContract.SecType, baseWhat, altWhat)
	attempts := make([]attempt, 0, len(seq)*2)
	seen := make(map[string]struct{})
	appendAttempt := func(contract Contract, what string) {
		if what == "" {
			return
		}
		key := strings.ToUpper(contract.Exchange) + "|" + strings.ToUpper(what)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		attempts = append(attempts, attempt{
			contract:   contract,
			whatToShow: what,
			label:      fmt.Sprintf("%s/%s", contract.Exchange, what),
		})
	}

	for _, what := range seq {
		appendAttempt(baseContract, what)
	}

	if primary != "" && strings.EqualFold(baseContract.Exchange, "SMART") {
		altContract := baseContract
		altContract.Exchange = primary
		altContract.PrimaryExch = ""
		for _, what := range seq {
			appendAttempt(altContract, what)
		}
	}

	var lastBars []HistoricalBar
	var lastErr error
	for idx, att := range attempts {
		bars, err := c.fetchHistoricalWithContract(symbol, att.contract, lookbackDays, timeout, att.whatToShow)
		if err != nil {
			if shouldRetryHistorical(err) && idx < len(attempts)-1 {
				c.logWarn("Historical data attempt %s for %s failed (%v); retrying with alternate route", att.label, symbol, err)
				lastErr = err
				continue
			}
			return nil, err
		}
		if len(bars) > 0 {
			if idx > 0 {
				c.logInfo("Historical data for %s recovered via %s", symbol, att.label)
			}
			return bars, nil
		}
		c.logWarn("Historical data for %s returned no rows via %s", symbol, att.label)
		lastBars = bars
	}

	if len(lastBars) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("historical data unavailable for %s", symbol)
	}
	return lastBars, nil
}

func defaultHistoricalWhat(secType string) string {
	switch strings.ToUpper(secType) {
	case "IND", "CMDTY":
		return "MIDPOINT"
	default:
		return "TRADES"
	}
}

func alternateHistoricalWhat(current string) string {
	if strings.EqualFold(current, "TRADES") {
		return "MIDPOINT"
	}
	if strings.EqualFold(current, "MIDPOINT") {
		return "TRADES"
	}
	return current
}

func historicalWhatSequence(symbol, secType, baseWhat, altWhat string) []string {
	seq := make([]string, 0, 5)
	appendWhat := func(value string) {
		if value == "" {
			return
		}
		for _, existing := range seq {
			if strings.EqualFold(existing, value) {
				return
			}
		}
		seq = append(seq, value)
	}

	switch strings.ToUpper(strings.TrimSpace(symbol)) {
	case "VIX":
		appendWhat("TRADES")
		appendWhat("MIDPOINT")
	default:
		appendWhat(baseWhat)
		if strings.EqualFold(strings.TrimSpace(secType), "STK") {
			appendWhat("ADJUSTED_LAST")
		}
		if !strings.EqualFold(baseWhat, altWhat) {
			appendWhat(altWhat)
		}
	}

	return seq
}

func shouldRetryHistorical(err error) bool {
	var hErr *HistoricalRequestError
	if errors.As(err, &hErr) {
		switch hErr.Code {
		case 162:
			return true
		}
	}
	return false
}

func (c *Connector) fetchHistoricalWithContract(symbol string, contract Contract, lookbackDays int, timeout time.Duration, whatToShow string) ([]HistoricalBar, error) {
	if contract.ConID == 0 {
		c.logWarn("Skipping historical data request for %s: unresolved contract ID (exchange=%s primary=%s)", symbol, contract.Exchange, contract.PrimaryExch)
		return nil, fmt.Errorf("contract ID unresolved for %s", symbol)
	}
	var req *historicalRequest
	duration := formatHistoricalDuration(lookbackDays)
	reqID, err := c.conn.RequestHistoricalData(contract, "", duration, "1 day", whatToShow, true, false, 1, false, func(id int) {
		req = c.createHistoricalRequest(id, symbol)
	})
	if err != nil {
		return nil, err
	}
	if req == nil {
		req = c.createHistoricalRequest(reqID, symbol)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-req.result:
		if res.err != nil {
			return nil, res.err
		}
		return res.bars, nil
	case <-timer.C:
		_ = c.conn.CancelHistoricalData(reqID)
		c.failHistoricalRequest(reqID, fmt.Errorf("historical data timeout after %s", timeout))
		return nil, fmt.Errorf("historical data timeout for %s", symbol)
	}
}

// GetOptionIV returns last observed implied volatility for an underlying
func (c *Connector) GetOptionIV(symbol string) (float64, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	v, ok := c.optIV[symbol]
	return v, ok
}

// GetOptionGreeks returns the last observed model-computation Greeks for an
// option key (the OPRA-style key produced by SubscribeOption). The boolean
// is true only when at least one field has been populated from a valid
// model-computation tick — callers must not infer zero from "absent".
func (c *Connector) GetOptionGreeks(symbol string) (Greeks, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	g, ok := c.optGreeks[symbol]
	return g, ok
}

// GetOptionUnderlyingPrice returns the underlying spot embedded in the
// most recent model-computation tick for an option key. This is the
// price IBKR used to price the Greeks, so callers computing dollar-
// delta should prefer it over an independently-fetched spot to keep
// the two values consistent.
func (c *Connector) GetOptionUnderlyingPrice(symbol string) (float64, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	v, ok := c.optUnderlyingPx[symbol]
	return v, ok
}

// GetOptionQuoteMid returns last observed option mid for the ATM-ish subscription
func (c *Connector) GetOptionQuoteMid(symbol string) (float64, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	v, ok := c.optQuoteMid[symbol]
	return v, ok
}

// GetOptionParams returns the option params used for subscription for a symbol
func (c *Connector) GetOptionParams(symbol string) (float64, time.Time, string, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	p, ok := c.optParams[symbol]
	if !ok {
		return 0, time.Time{}, "", false
	}
	return p.strike, p.expiry, p.right, true
}

// SetDerivedOptionIV stores a derived IV value for a symbol
func (c *Connector) SetDerivedOptionIV(symbol string, iv float64) {
	symbol = strings.ToUpper(symbol)
	c.optMu.Lock()
	defer c.optMu.Unlock()
	c.optIV[symbol] = iv
}

// SubscribeOptionIV subscribes to an ATM-ish option contract to receive implied volatility ticks (106).
// expiry should be in UTC; right is "C" or "P". ctx cancellation aborts
// the underlying contract-resolution round trip.
func (c *Connector) SubscribeOptionIV(ctx context.Context, symbol string, expiry time.Time, strike float64, right string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		c.logDebug("Skipping option IV subscription for inactive symbol %s", symbol)
		return 0, ErrSymbolInactive
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return 0, fmt.Errorf("IBKR connection not available")
	}

	// Format expiry as YYYYMMDD
	expStr := expiry.UTC().Format("20060102")
	reqID, err := conn.RequestOptionsMarketData(ctx, symbol, expStr, strike, strings.ToUpper(right))
	if err != nil {
		return 0, err
	}

	// Map reqID to underlying symbol so we can attribute IV updates
	c.subMu.Lock()
	c.reqIDMap[reqID] = symbol
	c.subMu.Unlock()
	c.optMu.Lock()
	c.optReqIDs[reqID] = symbol
	c.optParams[symbol] = struct {
		strike float64
		expiry time.Time
		right  string
	}{strike: strike, expiry: expiry, right: right}
	c.optMu.Unlock()

	c.logInfo("Subscribed option IV for %s %s %.2f %s (ReqID: %d)", symbol, expStr, strike, right, reqID)
	return reqID, nil
}

// handleTickSize processes tick size updates
func (c *Connector) handleTickSize(fields []string) {
	if len(fields) < 4 {
		return
	}

	// Format: [msgID, version, reqID, tickType, size]
	if len(fields) < 5 {
		return
	}
	reqIDStr := fields[2]
	tickTypeStr := fields[3]
	sizeStr := fields[4]

	reqID := 0
	fmt.Sscanf(reqIDStr, "%d", &reqID)
	tickType := 0
	fmt.Sscanf(tickTypeStr, "%d", &tickType)
	size := int64(0)
	fmt.Sscanf(sizeStr, "%d", &size)

	// Find the symbol for this request ID
	c.subMu.RLock()
	symbol, exists := c.reqIDMap[reqID]
	c.subMu.RUnlock()

	if !exists {
		return
	}

	// Update subscription data based on tick type
	c.subMu.Lock()
	defer c.subMu.Unlock()

	sub, exists := c.subscriptions[symbol]
	if !exists {
		return
	}

	// Mark observed on any size tick
	sub.Observed = true

	// IBKR tick types: 0=BID_SIZE, 3=ASK_SIZE, 8=VOLUME (cumulative day total).
	// 5=LAST_SIZE is intentionally dropped — too noisy and not surfaced.
	switch tickType {
	case 0:
		sub.BidSize = size
	case 3:
		sub.AskSize = size
	case 8:
		sub.Volume = size
	}
	sub.LastTime = time.Now()
}

// handlePosition processes position updates
func (c *Connector) handlePosition(fields []string) {
	if len(fields) < 8 {
		return
	}

	// Fields: version, account, symbol, secType, expiry, strike, right, multiplier, exchange, currency, localSymbol, tradingClass, position, avgCost
	symbol := fields[2]
	positionStr := fields[12]
	avgCostStr := fields[13]

	c.logInfo("Position update - Symbol: %s, Position: %s, AvgCost: %s",
		symbol, positionStr, avgCostStr)
}

// handlePortfolioValue processes portfolio updates
func (c *Connector) handlePortfolioValue(fields []string) {
	if len(fields) < 18 {
		return
	}

	// Extract relevant fields
	symbol := fields[1]
	position := fields[6]
	marketPrice := fields[7]
	marketValue := fields[8]
	avgCost := fields[9]
	unrealizedPnL := fields[10]
	realizedPnL := fields[11]

	c.logInfo("Portfolio update - Symbol: %s, Position: %s, Price: %s, Value: %s, UnrealizedPnL: %s, RealizedPnL: %s, AvgCost: %s",
		symbol, position, marketPrice, marketValue, unrealizedPnL, realizedPnL, avgCost)
}

// handleOrderStatus processes order status updates
func (c *Connector) handleOrderStatus(fields []string) {
	if len(fields) < 3 {
		return
	}

	start := 1
	if len(fields) > 3 && isNumeric(fields[1]) && isNumeric(fields[2]) {
		start = 2
	}
	if len(fields) <= start+3 {
		return
	}

	orderID := fields[start]
	status := fields[start+1]
	filled := fields[start+2]
	remaining := fields[start+3]
	avgFillPrice := "0"
	if len(fields) > start+4 {
		avgFillPrice = fields[start+4]
	}
	lastFillPrice := "0"
	if len(fields) > start+6 {
		lastFillPrice = fields[start+6]
	}
	whyHeld := ""
	if len(fields) > start+9 {
		whyHeld = fields[start+9]
	}

	filledQty, _ := strconv.ParseFloat(filled, 64)
	remainingQty, _ := strconv.ParseFloat(remaining, 64)
	avgPx, _ := strconv.ParseFloat(avgFillPrice, 64)
	lastPx, _ := strconv.ParseFloat(lastFillPrice, 64)

	c.logInfo("Order status - ID: %s, Status: %s, Filled: %.4f, Remaining: %.4f, AvgPrice: %.4f",
		orderID, status, filledQty, remainingQty, avgPx)

	c.orderMu.Lock()
	internalID, ok := c.brokerOrderIndex[orderID]
	if !ok {
		// Fallback: try direct lookup using broker ID as key (some tests store that way)
		internalID = orderID
	}
	order, exists := c.openOrders[internalID]
	if !exists {
		c.orderMu.Unlock()
		return
	}

	order.BrokerID = orderID
	order.FilledQty = filledQty
	if avgPx > 0 {
		order.FilledPrice = avgPx
	} else if lastPx > 0 {
		order.FilledPrice = lastPx
	}
	order.UpdatedAt = time.Now()
	order.Status = mapIBOrderStatus(status, filledQty, remainingQty)
	if whyHeld != "" {
		order.Reason = whyHeld
	}

	if order.Status == OrderStatusFilled && order.FilledAt == nil {
		now := time.Now()
		order.FilledAt = &now
	}
	if order.Status == OrderStatusCancelled && order.CancelledAt == nil {
		now := time.Now()
		order.CancelledAt = &now
	}

	// enqueue update snapshot for consumers
	updateCopy := *order
	c.orderUpdates = append(c.orderUpdates, updateCopy)

	// Remove from open orders once terminal
	if isTerminalOrderStatus(order.Status) {
		delete(c.openOrders, internalID)
		delete(c.brokerOrderIndex, orderID)
	}

	c.orderMu.Unlock()
}

// GetMarketData retrieves current market data for subscribed symbols
func (c *Connector) GetMarketData() map[string]*MarketData {
	c.subMu.RLock()
	defer c.subMu.RUnlock()

	data := make(map[string]*MarketData)

	for symbol, sub := range c.subscriptions {
		data[symbol] = &MarketData{
			Symbol:     symbol,
			Bid:        sub.Bid,
			Ask:        sub.Ask,
			Last:       sub.LastPrice,
			BidSize:    int(sub.BidSize),
			AskSize:    int(sub.AskSize),
			Volume:     sub.Volume,
			Close:      sub.PrevClose,
			Open:       sub.Open,
			High:       sub.High,
			Low:        sub.Low,
			Week13Low:  sub.Week13Low,
			Week13High: sub.Week13High,
			Week26Low:  sub.Week26Low,
			Week26High: sub.Week26High,
			Week52Low:  sub.Week52Low,
			Week52High: sub.Week52High,
			IV:         sub.IV,
			Timestamp:  sub.LastTime,
		}
	}

	return data
}

// DrainOrderUpdates returns the accumulated order updates since the last call.
// Callers receive copies of the tracked orders reflecting the most recent
// statuses observed from IBKR. The internal buffer is cleared atomically.
func (c *Connector) DrainOrderUpdates() []*Order {
	c.orderMu.Lock()
	defer c.orderMu.Unlock()

	if len(c.orderUpdates) == 0 {
		return nil
	}

	updates := make([]*Order, len(c.orderUpdates))
	for i := range c.orderUpdates {
		copy := c.orderUpdates[i]
		updates[i] = &copy
	}

	// reset buffer while retaining capacity
	clearLen := len(c.orderUpdates)
	for i := 0; i < clearLen; i++ {
		c.orderUpdates[i] = Order{}
	}
	c.orderUpdates = c.orderUpdates[:0]

	return updates
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return true
	}
	return false
}

func mapIBOrderType(orderType string) OrderType {
	switch strings.ToUpper(orderType) {
	case "MKT":
		return OrderTypeMarket
	case "LMT":
		return OrderTypeLimit
	case "STP":
		return OrderTypeStop
	case "STP LMT", "STPLMT":
		return OrderTypeStopLimit
	case "MOC":
		return OrderTypeMOC
	case "LOC":
		return OrderTypeLOC
	case "PEG MID", "PEGMID", "PEGMIDPT":
		return OrderTypePegMid
	default:
		return OrderType(strings.ToUpper(orderType))
	}
}

func mapIBTimeInForce(tif string) TimeInForce {
	switch strings.ToUpper(tif) {
	case "DAY":
		return TimeInForceDay
	case "GTC":
		return TimeInForceGTC
	case "IOC":
		return TimeInForceIOC
	case "FOK":
		return TimeInForceFOK
	case "GTD":
		return TimeInForceGTD
	case "OPG":
		return TimeInForceOPG
	default:
		return TimeInForce(strings.ToUpper(tif))
	}
}

func mapIBOrderStatus(status string, filled, remaining float64) OrderStatus {
	s := strings.ToLower(status)
	switch s {
	case "pendingsubmit", "apipending":
		return OrderStatusPending
	case "presubmitted":
		if filled > 0 && remaining > 0 {
			return OrderStatusPartial
		}
		return OrderStatusSubmitted
	case "submitted", "pendingcancel":
		if filled > 0 && remaining > 0 {
			return OrderStatusPartial
		}
		if remaining == 0 && filled > 0 {
			return OrderStatusFilled
		}
		return OrderStatusSubmitted
	case "partiallyfilled":
		return OrderStatusPartial
	case "filled":
		return OrderStatusFilled
	case "cancelled", "apicancelled":
		return OrderStatusCancelled
	case "inactive", "rejected", "error":
		return OrderStatusRejected
	case "expired":
		return OrderStatusExpired
	case "completed":
		return OrderStatusFilled
	default:
		if remaining == 0 && filled > 0 {
			return OrderStatusFilled
		}
		if filled > 0 && remaining > 0 {
			return OrderStatusPartial
		}
		return OrderStatus(strings.ToUpper(status))
	}
}

func isTerminalOrderStatus(status OrderStatus) bool {
	switch status {
	case OrderStatusFilled, OrderStatusCancelled, OrderStatusRejected, OrderStatusExpired:
		return true
	default:
		return false
	}
}

func (c *Connector) installExecutionHandler(conn *Connection) {
	if conn == nil {
		return
	}

	conn.RegisterHandler(msgExecutionData, func(fields []string) {
		report, err := parseExecutionReport(fields, conn.ServerVersion())
		if err != nil {
			c.logWarn("Failed to parse execution report: %v", err)
			return
		}
		c.dispatchExecutionReport(report)
	})
}

func (c *Connector) installCommissionHandler(conn *Connection) {
	if conn == nil {
		return
	}

	conn.RegisterHandler(msgCommissionReport, func(fields []string) {
		report, err := parseCommissionAndFees(fields)
		if err != nil {
			c.logWarn("Failed to parse commission report: %v", err)
			return
		}
		c.dispatchCommissionReport(report)
	})
}

func (c *Connector) dispatchExecutionReport(report *ExecutionReport) {
	if report == nil {
		return
	}
	listeners := c.snapshotExecutionListeners()
	for _, listener := range listeners {
		if listener == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.logError("Execution listener panic (execID=%s): %v", report.ExecID, r)
				}
			}()
			listener(report)
		}()
	}
}

func (c *Connector) dispatchCommissionReport(report *CommissionReport) {
	if report == nil {
		return
	}
	listeners := c.snapshotCommissionListeners()
	for _, listener := range listeners {
		if listener == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.logError("Commission listener panic (execID=%s): %v", report.ExecID, r)
				}
			}()
			listener(report)
		}()
	}
}

func (c *Connector) snapshotExecutionListeners() []func(*ExecutionReport) {
	c.execMu.RLock()
	defer c.execMu.RUnlock()
	if len(c.execListeners) == 0 {
		return nil
	}
	listeners := make([]func(*ExecutionReport), len(c.execListeners))
	copy(listeners, c.execListeners)
	return listeners
}

func (c *Connector) snapshotCommissionListeners() []func(*CommissionReport) {
	c.commMu.RLock()
	defer c.commMu.RUnlock()
	if len(c.commListeners) == 0 {
		return nil
	}
	listeners := make([]func(*CommissionReport), len(c.commListeners))
	copy(listeners, c.commListeners)
	return listeners
}
