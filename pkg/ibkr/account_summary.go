package ibkr

import (
	"context"
	"errors"
	"fmt"
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
	AsOf              time.Time
	// Raw is the unparsed map from IBKR keyed exactly as the gateway returned it
	// (`<tag>` for BASE currency, `<tag>_<currency>` otherwise). Provided for
	// diagnostic and forward-compatibility purposes.
	Raw map[string]string
}

const (
	defaultAccountSummaryTimeout = 5 * time.Second
	accountSummaryTags           = "NetLiquidation,BuyingPower,AvailableFunds,ExcessLiquidity,TotalCashValue,MaintenanceMarginReq,InitMarginReq"
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
func parseAccountSummary(raw map[string]string, accountID string) *RawAccountSummary {
	summary := &RawAccountSummary{
		AccountID: accountID,
		AsOf:      time.Now().UTC(),
		Raw:       make(map[string]string, len(raw)),
	}
	for k, v := range raw {
		summary.Raw[k] = v
	}

	tagBindings := []struct {
		tag   string
		field **float64
	}{
		{"NetLiquidation", &summary.NetLiquidation},
		{"BuyingPower", &summary.BuyingPower},
		{"AvailableFunds", &summary.AvailableFunds},
		{"ExcessLiquidity", &summary.ExcessLiquidity},
		{"TotalCashValue", &summary.TotalCashValue},
		{"MaintenanceMarginReq", &summary.MaintenanceMargin},
		{"InitMarginReq", &summary.InitMarginReq},
	}

	for _, b := range tagBindings {
		val, currency, ok := lookupAccountValue(raw, b.tag)
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
	}

	return summary
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
