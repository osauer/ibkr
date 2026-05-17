package ibkr

import (
	"bufio"
	"bytes"
	"strconv"
	"testing"
	"time"
)

func TestSeedContractCacheFromPositions(t *testing.T) {
	conn := NewConnector(nil)

	positions := map[string]*RawPosition{
		"bb": {
			Contract: Contract{
				Symbol:       "bb",
				SecType:      "STK",
				ConID:        12345,
				Exchange:     "NYSE",
				PrimaryExch:  "NYSE",
				LocalSymbol:  "BB",
				TradingClass: "NYSE",
			},
		},
		"duplicate": {
			Contract: Contract{
				Symbol:  "BB",
				SecType: "STK",
				ConID:   12345,
			},
		},
	}

	conn.seedContractCacheFromPositions(positions)

	conn.contractMu.RLock()
	detail, ok := conn.contractCache["BB"]
	conn.contractMu.RUnlock()
	if !ok {
		t.Fatalf("expected contract cache entry for BB")
	}
	if detail.ConID != 12345 {
		t.Fatalf("expected conID 12345, got %d", detail.ConID)
	}
	if detail.Exchange != "NYSE" {
		t.Fatalf("expected exchange NYSE, got %s", detail.Exchange)
	}
	if detail.PrimaryExch != "NYSE" {
		t.Fatalf("expected primary exchange NYSE, got %s", detail.PrimaryExch)
	}
	if detail.LocalSymbol != "BB" {
		t.Fatalf("expected local symbol BB, got %s", detail.LocalSymbol)
	}
	if detail.TradingClass != "NYSE" {
		t.Fatalf("expected trading class NYSE, got %s", detail.TradingClass)
	}
}

// Held option positions must not seed the bare-symbol cache; doing so
// caused `quote SPY` to return the option's pricing (~$4.89) instead
// of the ETF's (~$700) because prepareContract picked up the option's
// ConID for what was supposed to be a stock subscribe.
func TestSeedContractCacheSkipsNonStockPositions(t *testing.T) {
	conn := NewConnector(nil)

	positions := map[string]*RawPosition{
		"spy-put": {
			Contract: Contract{
				Symbol:       "SPY",
				SecType:      "OPT",
				Right:        "P",
				Strike:       700,
				Expiry:       "20260618",
				ConID:        7777777,
				Exchange:     "SMART",
				PrimaryExch:  "AMEX",
				LocalSymbol:  "SPY   260618P00700000",
				TradingClass: "SPY",
			},
		},
		"vix-call": {
			Contract: Contract{
				Symbol:  "VIX",
				SecType: "OPT",
				ConID:   888888,
			},
		},
	}

	conn.seedContractCacheFromPositions(positions)

	conn.contractMu.RLock()
	_, spyCached := conn.contractCache["SPY"]
	_, vixCached := conn.contractCache["VIX"]
	conn.contractMu.RUnlock()

	if spyCached {
		t.Fatalf("option position must not seed bare-symbol cache for SPY")
	}
	if vixCached {
		t.Fatalf("option position must not seed bare-symbol cache for VIX")
	}
}

// When a stock and an option for the same underlying are both held,
// only the stock's ConID may seed the cache. The merge order does not
// matter; the option must always be filtered out before merge.
func TestSeedContractCachePrefersStockOverOption(t *testing.T) {
	conn := NewConnector(nil)

	positions := map[string]*RawPosition{
		"amd-stock": {
			Contract: Contract{
				Symbol:      "AMD",
				SecType:     "STK",
				ConID:       111,
				PrimaryExch: "NASDAQ",
			},
		},
		"amd-call": {
			Contract: Contract{
				Symbol:  "AMD",
				SecType: "OPT",
				ConID:   222,
			},
		},
	}

	conn.seedContractCacheFromPositions(positions)

	conn.contractMu.RLock()
	detail, ok := conn.contractCache["AMD"]
	conn.contractMu.RUnlock()
	if !ok {
		t.Fatalf("expected stock position to seed AMD cache")
	}
	if detail.ConID != 111 {
		t.Fatalf("expected stock conID 111, got option's %d", detail.ConID)
	}
}

func TestFetchContractDetailsUsesCache(t *testing.T) {
	conn := NewConnector(nil)
	conn.contractMu.Lock()
	conn.contractCache["BB"] = ContractDetailsLite{
		Symbol:      "BB",
		ConID:       998877,
		Exchange:    "NYSE",
		PrimaryExch: "NYSE",
		LocalSymbol: "BB",
	}
	conn.contractMu.Unlock()

	details, err := conn.FetchContractDetails("bb", 0)
	if err != nil {
		t.Fatalf("FetchContractDetails returned error: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("expected 1 contract detail, got %d", len(details))
	}
	if details[0].ConID != 998877 {
		t.Fatalf("expected conID 998877, got %d", details[0].ConID)
	}
}

// TestFetchContractDetailsPopulatesCache verifies that a successful
// FetchContractDetails call writes the resolved contract into
// contractCache. The daemon's prewarmRegimeSymbols goroutine calls
// FetchContractDetails directly and discards the returned slice,
// expecting the cache to be primed for the next regime call. Without
// the cache write here, the prewarm is a no-op for cold-cache symbols
// like HYG whose classifySymbol entry has no PrimaryExch and whose
// gateway resolution can drift past prepareContract's 2 s budget.
func TestFetchContractDetailsPopulatesCache(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	var out bytes.Buffer
	conn.writer = bufio.NewWriter(&out)
	c.conn = conn
	c.running = true
	c.ready = true

	done := make(chan struct{})
	go func() {
		defer close(done)
		var reqID int
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			conn.handlersMu.RLock()
			registered := len(conn.msgHandlers[msgContractData]) > 0
			conn.handlersMu.RUnlock()
			if registered {
				conn.reqIDMu.Lock()
				reqID = conn.reqIDSeq - 1
				conn.reqIDMu.Unlock()
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if reqID == 0 {
			t.Errorf("contract data handlers never registered")
			return
		}

		// Modelled on the IBKR gateway's wire frame for msgContractData
		// at serverVersion >= 182 (minServerVerLastTradeDate). The
		// parser walks positional indexes; intermediate fields it
		// discards still need slot reservations so primaryExch lands
		// at the expected idx.
		frame := make([]string, 29)
		frame[0] = strconv.Itoa(msgContractData)
		frame[1] = strconv.Itoa(reqID)
		frame[2] = "HYG"
		frame[3] = "STK"
		frame[8] = "ARCA"
		frame[9] = "USD"
		frame[10] = "HYG"
		frame[12] = "HYG"
		frame[13] = "756733"
		frame[21] = "ARCA"

		endFrame := []string{
			strconv.Itoa(msgContractDataEnd),
			"1",
			strconv.Itoa(reqID),
		}

		for _, h := range conn.snapshotHandlers(msgContractData) {
			h(frame)
		}
		// Give the wait loop a tick to drain detailsCh and re-enter the
		// select. The end-marker handler uses a non-blocking send on
		// doneCh, so racing the receiver loses the signal.
		time.Sleep(20 * time.Millisecond)
		for _, h := range conn.snapshotHandlers(msgContractDataEnd) {
			h(endFrame)
		}
	}()

	details, err := c.FetchContractDetails("HYG", 2*time.Second)
	if err != nil {
		t.Fatalf("FetchContractDetails: %v", err)
	}
	<-done

	if len(details) != 1 {
		t.Fatalf("expected 1 detail, got %d", len(details))
	}
	if details[0].ConID != 756733 {
		t.Fatalf("returned ConID mismatch: got %d, want 756733", details[0].ConID)
	}

	cached := c.cachedContractDetail("HYG")
	if cached == nil {
		t.Fatal("expected HYG in contract cache after successful FetchContractDetails")
	}
	if cached.ConID != 756733 {
		t.Fatalf("cached ConID mismatch: got %d, want 756733", cached.ConID)
	}
	if cached.PrimaryExch != "ARCA" {
		t.Fatalf("cached PrimaryExch mismatch: got %q, want ARCA", cached.PrimaryExch)
	}
}
