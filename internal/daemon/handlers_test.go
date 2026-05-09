package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

// newTestServer constructs a Server with no connector — a "daemon up but
// gateway not connected" simulation. gatewayReady() is the seam every
// read handler short-circuits on.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Resolved{
		ProfileName: "live",
		Profile:     config.Profile{Host: "127.0.0.1", Port: 4001, ClientID: 15},
	}
	return &Server{
		cfg:     cfg,
		version: "test",
		streams: map[string]context.CancelFunc{},
		logger:  NewLogger(&bytes.Buffer{}, "error"),
	}
}

// When the gateway isn't connected, every read handler must return
// ErrIBKRUnavailable so dispatch maps to gateway_unavailable instead of
// silently returning empty results (D1, D2, D3 in the review).
func TestReadHandlersReturnGatewayUnavailableWhenDisconnected(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ctx := context.Background()

	t.Run("account.summary", func(t *testing.T) {
		_, err := srv.handleAccountSummary(ctx)
		assertGatewayUnavailable(t, err)
	})

	t.Run("positions.list", func(t *testing.T) {
		req := &rpc.Request{ID: "t1", Method: rpc.MethodPositionsList, Params: json.RawMessage(`{}`)}
		_, err := srv.handlePositionsList(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("quote.snapshot", func(t *testing.T) {
		params, _ := json.Marshal(rpc.QuoteSnapshotParams{
			Contract:  rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
			TimeoutMs: 100,
		})
		req := &rpc.Request{ID: "t2", Method: rpc.MethodQuoteSnapshot, Params: params}
		_, err := srv.handleQuoteSnapshot(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("chain.fetch", func(t *testing.T) {
		params, _ := json.Marshal(rpc.ChainFetchParams{
			Symbol: "AAPL", Expiry: "2026-06-19", Width: 1, Side: "both",
		})
		req := &rpc.Request{ID: "t3", Method: rpc.MethodChainFetch, Params: params}
		_, err := srv.handleChainFetch(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("scan.run with valid preset", func(t *testing.T) {
		srv.cfg.Scans = map[string]config.Scan{
			"top-movers": {Type: "TOP_PERC_GAIN", Exchange: "STK.US.MAJOR", Limit: 20},
		}
		params, _ := json.Marshal(rpc.ScanRunParams{Preset: "top-movers"})
		req := &rpc.Request{ID: "t4", Method: rpc.MethodScanRun, Params: params}
		_, err := srv.handleScanRun(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("history.daily", func(t *testing.T) {
		params, _ := json.Marshal(rpc.HistoryDailyParams{Symbol: "AAPL", Days: 30})
		req := &rpc.Request{ID: "t6", Method: rpc.MethodHistoryDaily, Params: params}
		_, err := srv.handleHistoryDaily(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("chain.expiries", func(t *testing.T) {
		params, _ := json.Marshal(rpc.ChainExpiriesParams{Symbol: "AAPL"})
		req := &rpc.Request{ID: "t7", Method: rpc.MethodChainExpiries, Params: params}
		_, err := srv.handleChainExpiries(ctx, req)
		assertGatewayUnavailable(t, err)
	})
}

// chain.expiries with an empty symbol must surface as bad_request, not
// internal — the CLI relies on this to render a usage hint.
func TestChainExpiriesEmptySymbolIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	params, _ := json.Marshal(rpc.ChainExpiriesParams{Symbol: " "})
	req := &rpc.Request{ID: "tx", Method: rpc.MethodChainExpiries, Params: params}
	_, err := srv.handleChainExpiries(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty symbol")
	}
	code, _ := classifyError(err)
	if code != rpc.CodeBadRequest {
		t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeBadRequest)
	}
}

// closestStrike picks the strike closest to spot. Verifies the tie-break
// rule (lower wins) and the boundary cases at both ends of the array.
func TestClosestStrike(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		strikes []float64
		spot    float64
		want    float64
	}{
		{"exact match", []float64{200, 210, 220}, 210, 210},
		{"middle picks closer side", []float64{200, 210, 220}, 213, 210},
		{"tie picks lower", []float64{200, 210}, 205, 200},
		{"below range", []float64{200, 210, 220}, 100, 200},
		{"above range", []float64{200, 210, 220}, 500, 220},
		{"single strike", []float64{215}, 100, 215},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := closestStrike(tc.strikes, tc.spot); got != tc.want {
				t.Fatalf("closestStrike(%v,%v) = %v, want %v", tc.strikes, tc.spot, got, tc.want)
			}
		})
	}
}

// groupByUnderlying nests stock + option legs per underlying and sums
// market value / unrealized P&L. A pure-options group has Stock=nil; a
// stock-only group has empty Options.
func TestGroupByUnderlying(t *testing.T) {
	t.Parallel()
	stocks := []rpc.PositionView{
		{Symbol: "AAPL", Quantity: 100, MarketValue: 20000, UnrealizedPnL: 1500},
		{Symbol: "MSFT", Quantity: 50, MarketValue: 25000, UnrealizedPnL: -200},
	}
	options := []rpc.PositionView{
		{Symbol: "AAPL", Right: "C", Strike: 215, Expiry: "20260619", Quantity: 5, MarketValue: 4700, UnrealizedPnL: 1290},
		{Symbol: "TSLA", Right: "P", Strike: 200, Expiry: "20260516", Quantity: 2, MarketValue: 800, UnrealizedPnL: -90},
	}
	groups := groupByUnderlying(stocks, options)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
	byName := map[string]rpc.PositionGroup{}
	for _, g := range groups {
		byName[g.Underlying] = g
	}
	aapl := byName["AAPL"]
	if aapl.Stock == nil || aapl.Stock.Quantity != 100 {
		t.Errorf("AAPL stock leg missing or wrong qty: %+v", aapl.Stock)
	}
	if len(aapl.Options) != 1 {
		t.Errorf("AAPL options: want 1, got %d", len(aapl.Options))
	}
	if aapl.GroupMarketValue != 24700 {
		t.Errorf("AAPL group MV: want 24700, got %g", aapl.GroupMarketValue)
	}
	if aapl.GroupUnrealizedPnL != 2790 {
		t.Errorf("AAPL group PnL: want 2790, got %g", aapl.GroupUnrealizedPnL)
	}
	tsla := byName["TSLA"]
	if tsla.Stock != nil {
		t.Errorf("TSLA expected pure-options group, got stock leg %+v", tsla.Stock)
	}
	if len(tsla.Options) != 1 {
		t.Errorf("TSLA options: want 1, got %d", len(tsla.Options))
	}
	msft := byName["MSFT"]
	if msft.Stock == nil || len(msft.Options) != 0 {
		t.Errorf("MSFT expected stock-only group, got %+v", msft)
	}
}

// history.daily with an empty symbol must surface as bad_request, not
// internal — the CLI relies on this to render a usage hint.
func TestHistoryDailyEmptySymbolIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	params, _ := json.Marshal(rpc.HistoryDailyParams{Symbol: "  ", Days: 30})
	req := &rpc.Request{ID: "t7", Method: rpc.MethodHistoryDaily, Params: params}
	_, err := srv.handleHistoryDaily(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty symbol")
	}
	code, _ := classifyError(err)
	if code != rpc.CodeBadRequest {
		t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeBadRequest)
	}
}

// scan.run with an unknown preset is a client error, not internal — and
// classifyError must map it to bad_request, not internal (D6 in the review).
func TestScanRunUnknownPresetIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.cfg.Scans = map[string]config.Scan{}

	params, _ := json.Marshal(rpc.ScanRunParams{Preset: "nope"})
	req := &rpc.Request{ID: "t5", Method: rpc.MethodScanRun, Params: params}
	_, err := srv.handleScanRun(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for unknown preset")
	}
	code, _ := classifyError(err)
	if code != rpc.CodeBadRequest {
		t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeBadRequest)
	}
}

// status.health is the only read endpoint that must succeed when the
// gateway is down — that is its entire purpose.
func TestStatusHealthReportsDisconnected(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.lastConnectError = "test: handshake never completed"

	res := srv.handleStatusHealth()
	if res.Connected {
		t.Fatal("expected Connected=false when connector is nil")
	}
	if res.LastError != "test: handshake never completed" {
		t.Fatalf("LastError = %q, want test message", res.LastError)
	}
	if res.DataType != "" {
		t.Fatalf("DataType = %q, want empty when disconnected", res.DataType)
	}
	if res.Profile != "live" {
		t.Fatalf("Profile = %q, want live", res.Profile)
	}
}

// classifyError is the seam between handler errors and CLI-visible codes.
func TestClassifyError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"bad request", errBadRequest("missing symbol"), rpc.CodeBadRequest},
		{"gateway unavailable", ibkrlib.ErrIBKRUnavailable, rpc.CodeGatewayUnavailable},
		{"symbol inactive", ibkrlib.ErrSymbolInactive, rpc.CodeSymbolInactive},
		{"deadline exceeded", context.DeadlineExceeded, rpc.CodeTimeout},
		{"unclassified", errors.New("boom"), rpc.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := classifyError(tc.err)
			if code != tc.want {
				t.Fatalf("code = %q, want %q", code, tc.want)
			}
			if !strings.Contains(msg, tc.err.Error()) {
				t.Fatalf("message %q does not contain underlying error %q", msg, tc.err.Error())
			}
		})
	}
}

func assertGatewayUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected gateway_unavailable error, got nil")
	}
	if !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
		t.Fatalf("expected ErrIBKRUnavailable, got %v", err)
	}
	code, _ := classifyError(err)
	if code != rpc.CodeGatewayUnavailable {
		t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeGatewayUnavailable)
	}
}
