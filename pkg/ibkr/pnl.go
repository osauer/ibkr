package ibkr

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// AccountDailyPnL captures the most recent account-level Daily P&L frame
// IBKR has emitted on the reqPnL subscription.
//
// Fields:
//   - DailyPnL: start-of-trading-day to now total P&L (this is the
//     value TWS shows in the portfolio header).
//   - UnrealizedDailyPnL, RealizedDailyPnL: the day's swing decomposed.
//     Distinct from the session-running unrealized/realized totals on
//     reqAccountUpdates / reqAccountSummary — the latter are inception
//     to today, not start-of-day to today.
//
// Pointers (not bare floats) so a value of zero is distinguishable from
// "no frame yet" / "DBL_MAX sentinel". Older gateway versions only emit
// the dailyPnL field; the two unrealized/realized pointers stay nil in
// that case.
type AccountDailyPnL struct {
	DailyPnL           *float64
	UnrealizedDailyPnL *float64
	RealizedDailyPnL   *float64
	AsOf               time.Time
}

// PositionDailyPnL captures the most recent per-conId Daily P&L frame
// IBKR has emitted on the reqPnLSingle subscription. Same pointer
// semantics as AccountDailyPnL — nil means "no value yet / sentinel /
// no entitlement".
type PositionDailyPnL struct {
	DailyPnL           *float64
	UnrealizedDailyPnL *float64
	RealizedDailyPnL   *float64
	AsOf               time.Time
}

// pnlCache holds the connector-side state for both account and per-
// position PnL subscriptions. It lives on the Connector (not the
// Connection) so a Connection-rebuild resets it naturally — handlers
// re-register against the new conn and the daemon re-subscribes via
// post-connect setup.
type pnlCache struct {
	mu sync.RWMutex

	accountReqID int // 0 means no active subscription
	accountAcct  string
	account      AccountDailyPnL

	// positionReqIDs maps conId -> reqID. positionByReqID is the
	// reverse map so the inbound handler (which sees reqID, not
	// conId) can find the cache entry to update.
	positionReqIDs   map[int]int
	positionByReqID  map[int]int
	positionSnapshot map[int]PositionDailyPnL
}

func newPnLCache() *pnlCache {
	return &pnlCache{
		positionReqIDs:   make(map[int]int),
		positionByReqID:  make(map[int]int),
		positionSnapshot: make(map[int]PositionDailyPnL),
	}
}

// dblMaxNotSent is IBKR's "not yet computed" sentinel for double
// fields. The wire emits the exact DBL_MAX value (1.7976931348623157e+308);
// we accept anything within an eyelash of that to absorb gateway-side
// float-printing variance.
const dblMaxNotSent = 1e300

// parsePnLFloat converts a wire field to a pointer-or-nil. Empty
// strings, unparseable values, and the DBL_MAX sentinel all become nil.
// We don't surface parse errors — IBKR's gateway is the source of truth;
// a malformed field is something to be silent about, not a hard fail.
func parsePnLFloat(s string) *float64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	if math.IsNaN(v) || math.Abs(v) >= dblMaxNotSent {
		return nil
	}
	return &v
}

// RequestPnL sends a reqPnL subscription request for the named account.
// modelCode is empty for non-FA accounts; FA users pass the model name.
//
// This is a library-callable wrapper around the wire opcode; production
// callers go through Connector.SubscribeAccountPnL which adds caching
// and handler registration.
func (c *Connection) RequestPnL(reqID int, account, modelCode string) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	if account == "" {
		return fmt.Errorf("account is required for reqPnL")
	}
	if err := ensureASCII("account", account); err != nil {
		return err
	}
	if err := ensureASCII("modelCode", modelCode); err != nil {
		return err
	}
	msg := c.encodeMsg(reqPnL, reqID, account, modelCode)
	return c.sendMessage(msg)
}

// CancelPnL tears down a reqPnL subscription. Best-effort — the gateway
// drops subscriptions on socket close anyway, so a connection-down state
// is not an error.
func (c *Connection) CancelPnL(reqID int) error {
	if !c.IsConnected() {
		return nil
	}
	msg := c.encodeMsg(cancelPnL, reqID)
	return c.sendMessage(msg)
}

// RequestPnLSingle sends a reqPnLSingle subscription request for one
// conId on the named account. Library-callable; production callers go
// through Connector.SubscribePositionDailyPnL.
func (c *Connection) RequestPnLSingle(reqID int, account, modelCode string, conID int) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	if account == "" {
		return fmt.Errorf("account is required for reqPnLSingle")
	}
	if conID <= 0 {
		return fmt.Errorf("conId is required for reqPnLSingle")
	}
	if err := ensureASCII("account", account); err != nil {
		return err
	}
	if err := ensureASCII("modelCode", modelCode); err != nil {
		return err
	}
	msg := c.encodeMsg(reqPnLSingle, reqID, account, modelCode, conID)
	return c.sendMessage(msg)
}

// CancelPnLSingle tears down a reqPnLSingle subscription.
func (c *Connection) CancelPnLSingle(reqID int) error {
	if !c.IsConnected() {
		return nil
	}
	msg := c.encodeMsg(cancelPnLSingle, reqID)
	return c.sendMessage(msg)
}

// parseAccountPnLFields parses an inbound msgPnL frame. The wire layout
// after the leading msgID is:
//
//	[reqId] [dailyPnL] [unrealizedPnL] [realizedPnL]
//
// Older gateways (server < 150) emit only the first two fields after
// reqId; we accept the short form and leave the remaining pointers nil.
// Returns reqID and a populated snapshot; ok=false on a malformed frame.
func parseAccountPnLFields(fields []string) (reqID int, snap AccountDailyPnL, ok bool) {
	// fields[0] = msgID; fields[1] = reqId; fields[2+] = payload.
	if len(fields) < 3 {
		return 0, AccountDailyPnL{}, false
	}
	rid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, AccountDailyPnL{}, false
	}
	snap.DailyPnL = parsePnLFloat(fields[2])
	if len(fields) > 3 {
		snap.UnrealizedDailyPnL = parsePnLFloat(fields[3])
	}
	if len(fields) > 4 {
		snap.RealizedDailyPnL = parsePnLFloat(fields[4])
	}
	snap.AsOf = time.Now().UTC()
	return rid, snap, true
}

// parsePositionPnLFields parses an inbound msgPnLSingle frame. Wire
// layout after the leading msgID:
//
//	[reqId] [pos] [dailyPnL] [unrealizedPnL] [realizedPnL] [value]
//
// pos and value are not surfaced — pos is just a sanity check on
// position size (we already have it from reqAccountUpdates), and value
// is a stale snapshot of MarketValue. Older gateways may omit the
// unrealizedPnL / realizedPnL fields; same nil-as-unknown semantics.
func parsePositionPnLFields(fields []string) (reqID int, snap PositionDailyPnL, ok bool) {
	// fields[0] = msgID; fields[1] = reqId; fields[2] = pos;
	// fields[3] = dailyPnL; fields[4] = unrealizedPnL; fields[5] = realizedPnL;
	// fields[6] = value.
	if len(fields) < 4 {
		return 0, PositionDailyPnL{}, false
	}
	rid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, PositionDailyPnL{}, false
	}
	snap.DailyPnL = parsePnLFloat(fields[3])
	if len(fields) > 4 {
		snap.UnrealizedDailyPnL = parsePnLFloat(fields[4])
	}
	if len(fields) > 5 {
		snap.RealizedDailyPnL = parsePnLFloat(fields[5])
	}
	snap.AsOf = time.Now().UTC()
	return rid, snap, true
}

// SubscribeAccountPnL starts (or re-uses) a streaming reqPnL subscription
// for the named account. Idempotent — repeated calls with the same
// account are noops on the wire; if the account changes, the existing
// subscription is cancelled and a fresh one issued.
//
// account must be non-empty. The caller's typical entry point is the
// daemon's post-connect setup, immediately after RequestAccountUpdates.
// Subscription state lives on the Connector; AccountDailyPnL() reads
// the cache without issuing wire traffic.
func (c *Connector) SubscribeAccountPnL(account string) error {
	if !c.isConnected() {
		return ErrIBKRUnavailable
	}
	if account == "" {
		return fmt.Errorf("account is required")
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return ErrIBKRUnavailable
	}

	c.pnl.mu.Lock()
	if c.pnl.accountReqID != 0 && c.pnl.accountAcct == account {
		c.pnl.mu.Unlock()
		return nil
	}
	// Account changed (rare): tear down the old subscription before
	// claiming the new reqID slot.
	oldReqID := c.pnl.accountReqID
	c.pnl.mu.Unlock()
	if oldReqID != 0 {
		if err := conn.CancelPnL(oldReqID); err != nil {
			connectorLogger.Debugf("CancelPnL(reqID=%d) failed during account change: %v", oldReqID, err)
		}
	}

	reqID := conn.GetNextRequestID()
	c.pnl.mu.Lock()
	if c.pnl.accountReqID != 0 && c.pnl.accountAcct == account {
		c.pnl.mu.Unlock()
		return nil
	}
	c.pnl.accountReqID = reqID
	c.pnl.accountAcct = account
	c.pnl.account = AccountDailyPnL{}
	c.pnl.mu.Unlock()

	if err := conn.RequestPnL(reqID, account, ""); err != nil {
		c.pnl.mu.Lock()
		if c.pnl.accountReqID == reqID {
			c.pnl.accountReqID = 0
			c.pnl.accountAcct = ""
			c.pnl.account = AccountDailyPnL{}
		}
		c.pnl.mu.Unlock()
		return fmt.Errorf("request PnL: %w", err)
	}
	return nil
}

// AccountDailyPnL returns the most recently received account Daily P&L
// snapshot. ok=false until the gateway emits the first frame after
// SubscribeAccountPnL — typically a few hundred milliseconds. Reads do
// not block and never issue wire traffic.
func (c *Connector) AccountDailyPnL() (AccountDailyPnL, bool) {
	c.pnl.mu.RLock()
	defer c.pnl.mu.RUnlock()
	if c.pnl.accountReqID == 0 || c.pnl.account.AsOf.IsZero() {
		return AccountDailyPnL{}, false
	}
	// Defensive copy; the snapshot holds pointers but they aren't
	// mutated in place — handlers always rebuild the struct.
	return c.pnl.account, true
}

// SubscribePositionDailyPnL starts a reqPnLSingle subscription for one
// contract. Idempotent — repeated calls for the same conId are noops.
// Returns nil on success (subscription kicked or already active);
// ErrIBKRUnavailable when disconnected.
//
// conId must be > 0; account must be non-empty. The caller (daemon)
// typically invokes this for each held position on first positions.list
// call.
func (c *Connector) SubscribePositionDailyPnL(account string, conID int) error {
	if !c.isConnected() {
		return ErrIBKRUnavailable
	}
	if account == "" {
		return fmt.Errorf("account is required")
	}
	if conID <= 0 {
		return fmt.Errorf("conId is required")
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return ErrIBKRUnavailable
	}

	reqID := conn.GetNextRequestID()
	c.pnl.mu.Lock()
	if _, ok := c.pnl.positionReqIDs[conID]; ok {
		c.pnl.mu.Unlock()
		return nil
	}
	c.pnl.positionReqIDs[conID] = reqID
	c.pnl.positionByReqID[reqID] = conID
	// Pre-populate an empty snapshot so AccountDailyPnL-style "exists
	// but unknown" reads can disambiguate from "never subscribed".
	c.pnl.positionSnapshot[conID] = PositionDailyPnL{}
	c.pnl.mu.Unlock()

	if err := conn.RequestPnLSingle(reqID, account, "", conID); err != nil {
		c.pnl.mu.Lock()
		if current, ok := c.pnl.positionReqIDs[conID]; ok && current == reqID {
			delete(c.pnl.positionReqIDs, conID)
			delete(c.pnl.positionByReqID, reqID)
			delete(c.pnl.positionSnapshot, conID)
		}
		c.pnl.mu.Unlock()
		return fmt.Errorf("request PnL single: %w", err)
	}
	return nil
}

// PositionDailyPnL returns the most recently received per-conId Daily
// P&L snapshot. ok=false when no subscription has been issued for the
// conId. The snapshot's pointers may still be nil even when ok=true —
// that's the "subscribed but no frame yet / sentinel" state.
func (c *Connector) PositionDailyPnL(conID int) (PositionDailyPnL, bool) {
	c.pnl.mu.RLock()
	defer c.pnl.mu.RUnlock()
	snap, ok := c.pnl.positionSnapshot[conID]
	return snap, ok
}

// ActiveDailyPnLSubscriptions reports how many per-conId reqPnLSingle
// streams the connector currently holds. Callers use this to enforce
// their own subscription caps without reaching into private state.
func (c *Connector) ActiveDailyPnLSubscriptions() int {
	c.pnl.mu.RLock()
	defer c.pnl.mu.RUnlock()
	return len(c.pnl.positionReqIDs)
}

// AccountID returns the account code IBKR sent on the managedAccounts
// frame at handshake. Empty string when the connector hasn't completed
// its handshake yet. Useful for callers that need to issue per-account
// subscriptions (reqPnL, reqPnLSingle) without re-parsing accountSummary.
func (c *Connector) AccountID() string {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return ""
	}
	return conn.GetAccountCode()
}

// SeedPositionDailyPnLForTest installs a cache entry for the named
// conId without going through the wire. Test-only — production code
// must go through SubscribePositionDailyPnL so the gateway emits real
// frames. Exposed (rather than test-only via an _test.go file) because
// the daemon's own daily-pnl tests live in package daemon and can't
// reach private fields here.
func (c *Connector) SeedPositionDailyPnLForTest(conID int, snap PositionDailyPnL) {
	c.pnl.mu.Lock()
	defer c.pnl.mu.Unlock()
	c.pnl.positionSnapshot[conID] = snap
	if _, ok := c.pnl.positionReqIDs[conID]; !ok {
		// Synthesize a reqID slot so ActiveDailyPnLSubscriptions
		// counts the seeded entries — matches the production
		// invariant "snapshot exists ↔ subscription exists".
		c.pnl.positionReqIDs[conID] = -1 * conID
	}
}

// SeedAccountDailyPnLForTest installs an account-level snapshot for
// AccountDailyPnL to read back. Test-only.
func (c *Connector) SeedAccountDailyPnLForTest(account string, snap AccountDailyPnL) {
	c.pnl.mu.Lock()
	defer c.pnl.mu.Unlock()
	c.pnl.accountReqID = -1
	c.pnl.accountAcct = account
	c.pnl.account = snap
}

// cancelAllPnL is called from Connector.Stop to tear down every
// outstanding PnL subscription. Best-effort: the gateway drops
// subscriptions on socket close anyway, so cancel-time errors are
// logged at debug and otherwise ignored.
func (c *Connector) cancelAllPnL() {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return
	}
	c.pnl.mu.Lock()
	acctReq := c.pnl.accountReqID
	posReqs := make([]int, 0, len(c.pnl.positionReqIDs))
	for _, r := range c.pnl.positionReqIDs {
		posReqs = append(posReqs, r)
	}
	c.pnl.accountReqID = 0
	c.pnl.accountAcct = ""
	c.pnl.positionReqIDs = make(map[int]int)
	c.pnl.positionByReqID = make(map[int]int)
	c.pnl.positionSnapshot = make(map[int]PositionDailyPnL)
	c.pnl.mu.Unlock()

	if acctReq != 0 {
		if err := conn.CancelPnL(acctReq); err != nil {
			connectorLogger.Debugf("CancelPnL(reqID=%d) on shutdown: %v", acctReq, err)
		}
	}
	for _, r := range posReqs {
		if err := conn.CancelPnLSingle(r); err != nil {
			connectorLogger.Debugf("CancelPnLSingle(reqID=%d) on shutdown: %v", r, err)
		}
	}
}

// handlePnL is the connector-side msgPnL handler. Decodes the frame
// and updates the cache so AccountDailyPnL reads see fresh values.
// Registered via registerHandlers.
func (c *Connector) handlePnL(fields []string) {
	reqID, snap, ok := parseAccountPnLFields(fields)
	if !ok {
		return
	}
	c.pnl.mu.Lock()
	if c.pnl.accountReqID == 0 || c.pnl.accountReqID != reqID {
		// Stale frame from a previous subscription (or we never
		// subscribed). Drop silently — re-subscribing after a
		// reconnect can race with the last few frames of the prior
		// connector's subscription if the gateway is slow to honor
		// the cancel.
		c.pnl.mu.Unlock()
		return
	}
	c.pnl.account = snap
	c.pnl.mu.Unlock()
}

// handlePnLSingle is the connector-side msgPnLSingle handler.
func (c *Connector) handlePnLSingle(fields []string) {
	reqID, snap, ok := parsePositionPnLFields(fields)
	if !ok {
		return
	}
	c.pnl.mu.Lock()
	conID, known := c.pnl.positionByReqID[reqID]
	if !known {
		c.pnl.mu.Unlock()
		return
	}
	c.pnl.positionSnapshot[conID] = snap
	c.pnl.mu.Unlock()
}
