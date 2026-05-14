package ibkr

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// ErrIBKRUnavailable is returned by request methods when the connector cannot
// reach IBKR (gateway disconnected, connector not started). Callers serving
// trading-critical reads (account values, fresh quotes) should refuse rather
// than fall back to stale data.
var ErrIBKRUnavailable = errors.New("ibkr connection unavailable")

// RawAccountSummary captures the essential account values returned by IBKR
// reqAccountSummary. All currency-denominated fields are reported in the
// account base currency unless Currency identifies a non-BASE override.
//
// Fields are pointers when their absence is meaningful (IBKR may omit tags
// the user does not have permission for, e.g., margin fields on a cash
// account). Callers must check for nil before dereferencing.
type RawAccountSummary struct {
	AccountID         string
	NetLiquidation    *float64
	BuyingPower       *float64
	AvailableFunds    *float64
	ExcessLiquidity   *float64
	TotalCashValue    *float64
	MaintenanceMargin *float64
	InitMarginReq     *float64
	Currency          string
	// CurrencyLedger holds the per-currency rollup the gateway emitted
	// in response to the $LEDGER:ALL tag — one entry per non-BASE
	// currency present in the portfolio. Empty for same-currency
	// accounts. The "BASE" pseudo-currency entry IBKR emits is dropped
	// here because it duplicates the top-level totals already reported.
	CurrencyLedger map[string]CurrencyLedger
	AsOf           time.Time
	// Raw is the unparsed map from IBKR keyed exactly as the gateway returned it
	// (`<tag>` for BASE currency, `<tag>_<currency>` otherwise). Provided for
	// diagnostic and forward-compatibility purposes.
	Raw map[string]string
}

// CurrencyLedger is one IBKR $LEDGER row decomposed by currency. All
// values are reported in that currency (so `CashBalance` for a USD
// row is the USD figure, not a base-currency conversion). ExchangeRate
// is base/CCY (consistent with what the gateway emits) so multiplying
// any of the *Ccy values by ExchangeRate yields the base-currency
// contribution.
type CurrencyLedger struct {
	NetLiquidationByCurrency float64
	CashBalance              float64
	StockMarketValue         float64
	OptionMarketValue        float64
	UnrealizedPnL            float64
	RealizedPnL              float64
	ExchangeRate             float64
}

const (
	defaultAccountSummaryTimeout = 5 * time.Second
	// $LEDGER:ALL asks IBKR to emit per-currency rows (one block per
	// currency present in the portfolio) carrying NetLiquidation, MarketValue,
	// CashBalance, UnrealizedPnL, RealizedPnL, ExchangeRate, etc., each tagged
	// `<Field>_<CCY>`. This is the canonical mechanism for multi-currency
	// exposure surfacing — without it we'd have no FX rate at all.
	//
	// MaintMarginReq is the IBKR canonical tag name; the longer
	// "MaintenanceMarginReq" we used pre-v0.10.0 happened to work on some
	// account types but stopped returning a bare-form row once we added
	// $LEDGER:ALL. The parser accepts both forms so an account that does
	// still echo the long form is read correctly.
	accountSummaryTags = "NetLiquidation,BuyingPower,AvailableFunds,ExcessLiquidity,TotalCashValue,MaintMarginReq,InitMarginReq,$LEDGER:ALL"
)

// RequestAccountSummary issues a synchronous reqAccountSummary against IBKR
// and returns the parsed snapshot. The call blocks until the gateway emits
// accountSummaryEnd, the supplied context is cancelled, or timeout elapses.
//
// Behavior:
//   - Returns ErrIBKRUnavailable immediately if the connector is not
//     connected; no network traffic is generated.
//   - On timeout the request is cancelled (cancelAccountSummary sent) so the
//     gateway does not continue streaming updates against the consumed reqID.
//   - timeout <= 0 falls back to defaultAccountSummaryTimeout (5s).
//
// The method is safe to call concurrently; each invocation uses a fresh
// reqID and reads the connection's accumulated map after end-of-stream.
func (c *Connector) RequestAccountSummary(ctx context.Context, timeout time.Duration) (*RawAccountSummary, error) {
	if !c.isConnected() {
		return nil, ErrIBKRUnavailable
	}
	if timeout <= 0 {
		timeout = defaultAccountSummaryTimeout
	}

	reqID := c.conn.GetNextRequestID()

	if err := c.conn.RequestAccountSummary(reqID, accountSummaryTags); err != nil {
		return nil, fmt.Errorf("request account summary: %w", err)
	}

	// Always cancel the subscription on the way out: end-of-stream means IBKR
	// has sent the snapshot, but the request remains active until cancelled.
	defer func() {
		if c.isConnected() {
			if cancelErr := c.conn.CancelAccountSummary(reqID); cancelErr != nil {
				connectorLogger.Debugf("CancelAccountSummary(reqID=%d) failed: %v", reqID, cancelErr)
			}
		}
	}()

	endCh := make(chan error, 1)
	go func() {
		endCh <- c.conn.WaitForAccountSummaryEnd(timeout)
	}()

	select {
	case err := <-endCh:
		if err != nil {
			return nil, fmt.Errorf("await account summary end: %w", err)
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	raw := c.conn.GetAccountSummary()
	summary := parseAccountSummary(raw, c.conn.GetAccountCode())
	return summary, nil
}

// parseAccountSummary converts the IBKR-format key/value map (as returned by
// Connection.GetAccountSummary) into a typed AccountSummary. The IBKR key
// format is `<tag>` for the account base currency and `<tag>_<currency>` for
// explicit currency overrides. We prefer the BASE-currency form; if absent
// for a tag we fall back to the first currency-specific entry encountered.
//
// The $LEDGER:ALL tag (in accountSummaryTags) instructs the gateway to also
// emit per-currency rows — those are aggregated into CurrencyLedger so
// callers can attribute currency exposure without re-fetching.
func parseAccountSummary(raw map[string]string, accountID string) *RawAccountSummary {
	summary := &RawAccountSummary{
		AccountID:      accountID,
		AsOf:           time.Now().UTC(),
		CurrencyLedger: make(map[string]CurrencyLedger),
		Raw:            make(map[string]string, len(raw)),
	}
	maps.Copy(summary.Raw, raw)

	// Each binding accepts one or more accepted tag names — the parser
	// tries each in order and uses the first that resolves. This makes the
	// canonical and legacy names interchangeable so a gateway that still
	// emits the long form (or a future protocol shift) doesn't lose the
	// value silently.
	tagBindings := []struct {
		tags  []string
		field **float64
	}{
		{[]string{"NetLiquidation"}, &summary.NetLiquidation},
		{[]string{"BuyingPower"}, &summary.BuyingPower},
		{[]string{"AvailableFunds"}, &summary.AvailableFunds},
		{[]string{"ExcessLiquidity"}, &summary.ExcessLiquidity},
		{[]string{"TotalCashValue"}, &summary.TotalCashValue},
		{[]string{"MaintMarginReq", "MaintenanceMarginReq"}, &summary.MaintenanceMargin},
		{[]string{"InitMarginReq"}, &summary.InitMarginReq},
	}

	for _, b := range tagBindings {
		for _, tag := range b.tags {
			val, currency, ok := lookupAccountValue(raw, tag)
			if !ok {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
			if err != nil {
				continue
			}
			*b.field = &parsed
			if summary.Currency == "" && currency != "" {
				summary.Currency = currency
			}
			break
		}
	}

	summary.CurrencyLedger = extractCurrencyLedger(raw)

	return summary
}

// CurrencyLedgerSnapshot returns the per-currency ledger derived from the
// connector's continuously-updated accountSummary state (kept fresh by the
// reqAccountUpdates subscription started at connect time). Reads do not
// issue a new gateway round trip and never block — callers that arrived
// pre-handshake will see an empty map, which they should treat the same
// as "no non-base exposure available yet".
func (c *Connector) CurrencyLedgerSnapshot() map[string]CurrencyLedger {
	raw := c.AccountSummaryRaw()
	return extractCurrencyLedger(raw)
}

// AccountSummaryRaw returns a defensive copy of the connector's current
// accountSummary map, populated by the streaming reqAccountUpdates
// subscription. Empty map when the connector isn't ready or no values
// have been received yet — callers must not infer connection state
// from emptiness alone.
func (c *Connector) AccountSummaryRaw() map[string]string {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return map[string]string{}
	}
	return conn.GetAccountSummary()
}

// extractCurrencyLedger walks the raw map for `<field>_<CCY>` entries
// matching the canonical IBKR $LEDGER fields and aggregates them by
// currency. The "BASE" pseudo-currency entry IBKR also emits is
// dropped — it duplicates the top-level totals.
//
// Currencies appearing only in margin-related fields (with no
// NetLiquidationByCurrency or CashBalance) are also dropped — they
// represent zero-balance currencies the gateway happened to include.
func extractCurrencyLedger(raw map[string]string) map[string]CurrencyLedger {
	ledger := map[string]*CurrencyLedger{}
	assign := func(field, ccy, val string) {
		if ccy == "" || ccy == "BASE" {
			return
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return
		}
		row, ok := ledger[ccy]
		if !ok {
			row = &CurrencyLedger{}
			ledger[ccy] = row
		}
		switch field {
		case "NetLiquidationByCurrency":
			row.NetLiquidationByCurrency = parsed
		case "CashBalance":
			row.CashBalance = parsed
		case "StockMarketValue":
			row.StockMarketValue = parsed
		case "OptionMarketValue":
			row.OptionMarketValue = parsed
		case "UnrealizedPnL":
			row.UnrealizedPnL = parsed
		case "RealizedPnL":
			row.RealizedPnL = parsed
		case "ExchangeRate":
			row.ExchangeRate = parsed
		}
	}
	for k, v := range raw {
		idx := strings.LastIndexByte(k, '_')
		if idx <= 0 || idx == len(k)-1 {
			continue
		}
		field := k[:idx]
		ccy := k[idx+1:]
		assign(field, ccy, v)
	}
	out := make(map[string]CurrencyLedger, len(ledger))
	for ccy, row := range ledger {
		if row == nil {
			continue
		}
		// Keep only currencies the user actually holds value in. Without
		// NetLiquidation OR a non-zero cash/market value, the row is
		// noise (zero-balance currency the gateway included).
		if row.NetLiquidationByCurrency == 0 && row.CashBalance == 0 &&
			row.StockMarketValue == 0 && row.OptionMarketValue == 0 {
			continue
		}
		out[ccy] = *row
	}
	return out
}

// lookupAccountValue returns the value, currency, and ok flag for a tag.
// IBKR encodes BASE-currency values under the bare tag and non-BASE values
// under `<tag>_<currency>`. We prefer the bare form; otherwise we accept the
// first currency-suffixed entry deterministically (sorted by suffix).
func lookupAccountValue(raw map[string]string, tag string) (string, string, bool) {
	if v, ok := raw[tag]; ok {
		return v, "", true
	}
	prefix := tag + "_"
	var bestKey string
	for k := range raw {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if bestKey == "" || k < bestKey {
			bestKey = k
		}
	}
	if bestKey == "" {
		return "", "", false
	}
	return raw[bestKey], strings.TrimPrefix(bestKey, prefix), true
}
