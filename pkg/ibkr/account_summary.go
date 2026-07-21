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

// RawAccountSummary is a point-in-time view of the account values returned by
// IBKR. Currency-denominated top-level fields use the account's base-currency
// row when IBKR supplied one. If a base row is absent, the parser selects a
// currency-specific row deterministically. Currency records the first such
// fallback; Raw preserves the currency suffix for every field.
//
// Fields are pointers when their absence is meaningful (IBKR may omit tags
// the user does not have permission for, e.g., margin fields on a cash
// account, or LookAhead* on cash). Callers must check for nil before
// dereferencing.
type RawAccountSummary struct {
	AccountID            string
	AccountType          string
	NetLiquidation       *float64
	BuyingPower          *float64
	AvailableFunds       *float64
	ExcessLiquidity      *float64
	TotalCashValue       *float64
	MaintenanceMargin    *float64
	InitMarginReq        *float64
	GrossPositionValue   *float64
	UnrealizedPnL        *float64
	RealizedPnL          *float64
	Cushion              *float64
	LookAheadInitMargin  *float64
	LookAheadMaintMargin *float64
	LookAheadAvailable   *float64
	LookAheadExcess      *float64
	Currency             string
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

// CurrencyLedger is one non-base-currency IBKR $LEDGER row. Monetary values
// are denominated in that row's currency, not converted to the account base
// currency. ExchangeRate is base-currency units per ledger-currency unit, so
// multiplying a monetary field by ExchangeRate yields its base-currency
// contribution. A zero field may be either an observed zero or an omitted
// value; the wire format does not preserve that distinction here.
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
	// MaintMarginReq is the canonical tag. The parser also accepts the longer
	// MaintenanceMarginReq alias emitted by some gateway/account combinations.
	accountSummaryTags = "NetLiquidation,BuyingPower,AvailableFunds,ExcessLiquidity,TotalCashValue,MaintMarginReq,InitMarginReq,GrossPositionValue,UnrealizedPnL,RealizedPnL,Cushion,LookAheadInitMarginReq,LookAheadMaintMarginReq,LookAheadAvailableFunds,LookAheadExcessLiquidity,AccountType,$LEDGER:ALL"
)

// RequestAccountSummary issues a synchronous reqAccountSummary request and
// returns a caller-owned parsed snapshot. ctx must be non-nil. The call blocks
// until the gateway emits accountSummaryEnd, ctx is cancelled, or timeout
// elapses.
//
// Behavior:
//   - Returns ErrIBKRUnavailable immediately if the connector is not
//     connected; no network traffic is generated.
//   - On timeout the request is cancelled (cancelAccountSummary sent) so the
//     gateway does not continue streaming updates against the consumed reqID.
//   - timeout <= 0 falls back to defaultAccountSummaryTimeout (5s).
//
// The method is safe to call concurrently; each invocation uses a fresh
// request ID and normally reads only that request's rows. If the gateway emits
// an end marker without rows, it falls back to a defensive copy of the
// streaming account-updates cache.
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

	type snapshotResult struct {
		rows map[string]string
		err  error
	}
	resCh := make(chan snapshotResult, 1)
	go func() {
		rows, err := c.conn.awaitAccountSummarySnapshot(reqID, timeout)
		resCh <- snapshotResult{rows: rows, err: err}
	}()

	var raw map[string]string
	select {
	case res := <-resCh:
		if res.err != nil {
			return nil, fmt.Errorf("await account summary end: %w", res.err)
		}
		raw = res.rows
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Keep normal reads isolated from concurrent streaming account updates. An
	// end marker without rows falls back to the streaming cache so callers can
	// still consume a previously observed snapshot.
	if len(raw) == 0 {
		raw = c.conn.GetAccountSummary()
	}
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
		{[]string{"GrossPositionValue"}, &summary.GrossPositionValue},
		{[]string{"UnrealizedPnL"}, &summary.UnrealizedPnL},
		{[]string{"RealizedPnL"}, &summary.RealizedPnL},
		{[]string{"Cushion"}, &summary.Cushion},
		{[]string{"LookAheadInitMarginReq"}, &summary.LookAheadInitMargin},
		{[]string{"LookAheadMaintMarginReq"}, &summary.LookAheadMaintMargin},
		{[]string{"LookAheadAvailableFunds"}, &summary.LookAheadAvailable},
		{[]string{"LookAheadExcessLiquidity"}, &summary.LookAheadExcess},
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

	// AccountType is a string tag (e.g. "INDIVIDUAL", "IB-MARGIN") rather
	// than a numeric value, so it does not pass through the float-bindings
	// loop. The gateway emits it with an empty currency field.
	if v, _, ok := lookupAccountValue(raw, "AccountType"); ok {
		summary.AccountType = strings.TrimSpace(v)
	}

	summary.CurrencyLedger = extractCurrencyLedger(raw)

	return summary
}

// CurrencyLedgerSnapshot returns a caller-owned map derived from the
// connector's streaming account-summary cache. It neither blocks nor issues
// gateway traffic. An empty map means either no non-base exposure was observed
// or the cache is not populated yet; use connection state to distinguish them.
// The method is safe to call concurrently with streaming cache updates.
func (c *Connector) CurrencyLedgerSnapshot() map[string]CurrencyLedger {
	raw := c.AccountSummaryRaw()
	return extractCurrencyLedger(raw)
}

// AccountSummaryRaw returns a defensive copy of the connector's current raw
// account-summary cache. The map uses IBKR keys: bare tags for base-currency
// values and `<tag>_<currency>` for currency-specific values. It is empty when
// no connection or observations are available; emptiness alone does not
// describe connection state. The method is safe to call concurrently with
// streaming cache updates.
func (c *Connector) AccountSummaryRaw() map[string]string {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return map[string]string{}
	}
	return conn.GetAccountSummary()
}

// CachedAccountSummary returns a caller-owned typed snapshot of the connector's
// streaming account-summary cache. It does not issue gateway traffic and
// returns nil until at least one core account value has been observed. The
// method is safe to call concurrently with streaming cache updates.
func (c *Connector) CachedAccountSummary() *RawAccountSummary {
	raw := c.AccountSummaryRaw()
	if len(raw) == 0 {
		return nil
	}
	summary := parseAccountSummary(raw, c.AccountID())
	if summary.NetLiquidation == nil && summary.BuyingPower == nil &&
		summary.AvailableFunds == nil && summary.TotalCashValue == nil {
		return nil
	}
	return summary
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
