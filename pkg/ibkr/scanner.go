package ibkr

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// ScannerSubscription is a minimal scanner subscription request. Only the
// fields commonly needed for the v1 presets are surfaced; less-used filters
// (rating, market cap, etc.) are sent as empty strings as IBKR expects.
type ScannerSubscription struct {
	Type       string // scanCode, e.g. TOP_PERC_GAIN
	Exchange   string // locationCode, e.g. STK.US.MAJOR
	Instrument string // e.g. STK; defaults to STK
	Limit      int    // numberOfRows; <=0 means default
}

// ScannerRow is one element of the scanner result set.
type ScannerRow struct {
	Rank        int
	Symbol      string
	SecType     string
	Exchange    string
	Currency    string
	LocalSymbol string
	Distance    string
	Benchmark   string
	Projection  string
	Comment     string
}

// scannerSession tracks pending scanner state for a single subscription.
type scannerSession struct {
	reqID int
	rows  []ScannerRow
	done  chan struct{}
	mu    sync.Mutex
	once  sync.Once
}

// RunScannerSubscription issues a one-shot scanner subscription, waits for
// the gateway's first scannerData payload, then cancels the subscription.
//
// IBKR streams the same scanner result repeatedly while the subscription is
// active; v1 only needs the first frame so the daemon does not pay for an
// open-ended stream of duplicates.
func (c *Connector) RunScannerSubscription(ctx context.Context, sub ScannerSubscription, timeout time.Duration) ([]ScannerRow, error) {
	if !c.IsReady() {
		return nil, ErrIBKRUnavailable
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	instrument := sub.Instrument
	if instrument == "" {
		instrument = "STK"
	}

	reqID := c.conn.GetNextRequestID()
	session := &scannerSession{reqID: reqID, done: make(chan struct{})}

	dataHandlerID := c.conn.RegisterHandler(msgScannerData, func(fields []string) {
		// Layout passed by the dispatcher: [msgID, version, reqID, count, rows...]
		if len(fields) < 4 {
			return
		}
		gotID, err := strconv.Atoi(fields[2])
		if err != nil || gotID != reqID {
			return
		}
		session.mu.Lock()
		session.rows = parseScannerData(fields)
		session.mu.Unlock()
		session.once.Do(func() { close(session.done) })
	})
	defer c.conn.UnregisterHandler(msgScannerData, dataHandlerID)

	if err := c.requestScannerSubscription(reqID, sub, instrument); err != nil {
		return nil, err
	}
	defer func() { _ = c.cancelScannerSubscription(reqID) }()

	select {
	case <-session.done:
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.rows, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("scanner timed out after %s", timeout)
	}
}

func (c *Connector) requestScannerSubscription(reqID int, sub ScannerSubscription, instrument string) error {
	limit := sub.Limit
	if limit <= 0 {
		limit = -1
	}

	// reqScannerSubscription field order (no version after serverVersion 143):
	//   reqId, numberOfRows, instrument, locationCode, scanCode,
	//   abovePrice, belowPrice, aboveVolume, marketCapAbove, marketCapBelow,
	//   moodyRatingAbove, moodyRatingBelow, spRatingAbove, spRatingBelow,
	//   maturityDateAbove, maturityDateBelow, couponRateAbove, couponRateBelow,
	//   excludeConvertible, averageOptionVolumeAbove, scannerSettingPairs,
	//   stockTypeFilter
	fields := []interface{}{
		reqScannerSubscription,
		reqID,
		strconv.Itoa(limit),
		instrument,
		sub.Exchange,
		sub.Type,
		"", "", "", "", "",
		"", "", "", "",
		"", "", "", "",
		"", "", "", "",
	}
	msg := c.conn.encodeMsg(fields...)
	return c.conn.sendMessage(msg)
}

func (c *Connector) cancelScannerSubscription(reqID int) error {
	msg := c.conn.encodeMsg(cancelScannerSubscription, "1", reqID)
	return c.conn.sendMessage(msg)
}

// parseScannerData decodes the scanner-data payload. The dispatcher hands us
// fields as [msgID, version, reqID, numberOfElements, per-row × 16 ...].
// Per-row layout:
//
//	rank, conId, symbol, secType, expiry, strike, right, exchange,
//	currency, localSymbol, marketName, tradingClass,
//	distance, benchmark, projection, legsStr
//
// Field count varies slightly by server version; we read defensively and
// stop at the first short element.
func parseScannerData(fields []string) []ScannerRow {
	if len(fields) < 4 {
		return nil
	}
	n, err := strconv.Atoi(fields[3])
	if err != nil {
		return nil
	}
	const fieldsPerRow = 16
	rows := make([]ScannerRow, 0, n)
	idx := 4
	for i := 0; i < n; i++ {
		if idx+fieldsPerRow > len(fields) {
			break
		}
		row := ScannerRow{}
		row.Rank, _ = strconv.Atoi(fields[idx+0])
		// fields[idx+1] is conId — not surfaced
		row.Symbol = fields[idx+2]
		row.SecType = fields[idx+3]
		// expiry, strike, right at idx+4..6 are option-only; skipped here
		row.Exchange = fields[idx+7]
		row.Currency = fields[idx+8]
		row.LocalSymbol = fields[idx+9]
		// marketName at idx+10
		// tradingClass at idx+11
		row.Distance = fields[idx+12]
		row.Benchmark = fields[idx+13]
		row.Projection = fields[idx+14]
		row.Comment = fields[idx+15]
		rows = append(rows, row)
		idx += fieldsPerRow
	}
	return rows
}
