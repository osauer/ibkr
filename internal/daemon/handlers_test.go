package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/discover"
	"github.com/osauer/ibkr/internal/rpc"
)

// newTestServer constructs a Server with no connector — a "daemon up but
// gateway not connected" simulation. gatewayReady() is the seam every
// read handler short-circuits on.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4001), ClientID: new(15)},
	}
	s := &Server{
		cfg:        cfg,
		endpoint:   discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15, PortOrigin: discover.OriginPinned},
		version:    "test",
		streams:    map[string]context.CancelFunc{},
		logger:     NewLogger(&bytes.Buffer{}, "error"),
		expiryIVs:  newExpiryIVCache(),
		prevCloses: newPrevCloseCache(),
		zeroGamma:  newGammaZeroCache(),
	}
	s.installSubs()
	return s
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

	// FX-pair quote (USD.JPY) routes through the same handler as STK but
	// flows through pkg/ibkr.classifySymbol → CASH/IDEALPRO inside the
	// connector. With the gateway down, the handler must short-circuit
	// before it tries to subscribe, exactly like the STK path above.
	t.Run("quote.snapshot/fx-pair", func(t *testing.T) {
		params, _ := json.Marshal(rpc.QuoteSnapshotParams{
			Contract:  rpc.ContractParams{Symbol: "USD.JPY"},
			TimeoutMs: 100,
		})
		req := &rpc.Request{ID: "t2fx", Method: rpc.MethodQuoteSnapshot, Params: params}
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

	t.Run("breadth.spx", func(t *testing.T) {
		req := &rpc.Request{ID: "t6b", Method: rpc.MethodBreadthSPX, Params: json.RawMessage(`{}`)}
		_, err := srv.handleBreadthSPX(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("gamma.zero_spx", func(t *testing.T) {
		req := &rpc.Request{ID: "t6c", Method: rpc.MethodGammaZeroSPX, Params: json.RawMessage(`{}`)}
		_, err := srv.handleGammaZeroSPX(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("regime.snapshot", func(t *testing.T) {
		req := &rpc.Request{ID: "t6d", Method: rpc.MethodRegimeSnapshot, Params: json.RawMessage(`{}`)}
		_, err := srv.handleRegimeSnapshot(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("chain.expiries", func(t *testing.T) {
		params, _ := json.Marshal(rpc.ChainExpiriesParams{Symbol: "AAPL"})
		req := &rpc.Request{ID: "t7", Method: rpc.MethodChainExpiries, Params: params}
		_, err := srv.handleChainExpiries(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("quote.subscribe", func(t *testing.T) {
		params, _ := json.Marshal(rpc.QuoteSubscribeParams{
			Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		})
		req := &rpc.Request{ID: "t8", Method: rpc.MethodQuoteSubscribe, Params: params}
		var buf bytes.Buffer
		srv.handleQuoteSubscribe(ctx, req, json.NewEncoder(&buf), bufio.NewReader(bytes.NewReader(nil)))
		var resp rpc.Response
		if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Ok {
			t.Fatalf("expected !ok envelope, got %+v", resp)
		}
		if resp.Error == nil || resp.Error.Code != rpc.CodeGatewayUnavailable {
			t.Fatalf("got error %+v, want code %s", resp.Error, rpc.CodeGatewayUnavailable)
		}
	})
}

// computeQuoteChange returns nil for either output unless both Last and
// PrevClose are present and PrevClose is strictly positive. No
// fabrication — pre-market with no Last must show em-dash, not zero.
func TestComputeQuoteChange(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }

	cases := []struct {
		name      string
		last      *float64
		prev      *float64
		wantChg   *float64
		wantPct   *float64
		precision float64
	}{
		{"both nil", nil, nil, nil, nil, 0},
		{"last nil", nil, f(100), nil, nil, 0},
		{"prev nil", f(105), nil, nil, nil, 0},
		{"prev zero", f(105), f(0), nil, nil, 0},
		{"prev negative", f(105), f(-1), nil, nil, 0},
		{"positive change", f(101.50), f(100), f(1.50), f(1.50), 0.0001},
		{"negative change", f(95), f(100), f(-5), f(-5), 0.0001},
		{"zero change", f(100), f(100), f(0), f(0), 0.0001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chg, pct := computeQuoteChange(tc.last, tc.prev)
			if (chg == nil) != (tc.wantChg == nil) {
				t.Fatalf("chg nil mismatch: got=%v want=%v", chg, tc.wantChg)
			}
			if (pct == nil) != (tc.wantPct == nil) {
				t.Fatalf("pct nil mismatch: got=%v want=%v", pct, tc.wantPct)
			}
			if chg != nil && math.Abs(*chg-*tc.wantChg) > tc.precision {
				t.Errorf("chg got %f want %f", *chg, *tc.wantChg)
			}
			if pct != nil && math.Abs(*pct-*tc.wantPct) > tc.precision {
				t.Errorf("pct got %f want %f", *pct, *tc.wantPct)
			}
		})
	}
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

// mergeStrikeSide is the publish step in handleChainFetch's parallel
// fan-out: each worker fills a private rpc.ChainStrike and merges its
// side under a shared mutex. Verifies the side-disjoint copy: a "C"
// merge must touch only Call* fields and leave Put* untouched, and
// vice versa — otherwise concurrent workers would clobber each
// other's results.
func TestMergeStrikeSide(t *testing.T) {
	t.Parallel()
	cb, ca, cl, civ := 1.0, 2.0, 3.0, 0.4
	pb, pa, pl, piv := 4.0, 5.0, 6.0, 0.5

	t.Run("call merge leaves put fields untouched", func(t *testing.T) {
		dst := rpc.ChainStrike{Strike: 100, IsATM: true,
			PutBid: &pb, PutAsk: &pa, PutLast: &pl, PutIV: &piv,
		}
		src := rpc.ChainStrike{
			CallBid: &cb, CallAsk: &ca, CallLast: &cl, CallIV: &civ,
		}
		mergeStrikeSide(&dst, &src, "C")
		if dst.CallBid == nil || *dst.CallBid != cb {
			t.Errorf("CallBid not copied")
		}
		if dst.PutBid == nil || *dst.PutBid != pb {
			t.Errorf("PutBid was clobbered: %+v", dst.PutBid)
		}
		if !dst.IsATM || dst.Strike != 100 {
			t.Errorf("metadata fields lost: strike=%v atm=%v", dst.Strike, dst.IsATM)
		}
	})

	t.Run("put merge leaves call fields untouched", func(t *testing.T) {
		dst := rpc.ChainStrike{Strike: 100,
			CallBid: &cb, CallAsk: &ca, CallLast: &cl, CallIV: &civ,
		}
		src := rpc.ChainStrike{
			PutBid: &pb, PutAsk: &pa, PutLast: &pl, PutIV: &piv,
		}
		mergeStrikeSide(&dst, &src, "P")
		if dst.PutBid == nil || *dst.PutBid != pb {
			t.Errorf("PutBid not copied")
		}
		if dst.CallBid == nil || *dst.CallBid != cb {
			t.Errorf("CallBid was clobbered")
		}
	})
}

// marketDataTypeName maps the gateway's per-reqID notice to the
// stable wire string. Locks the mapping so a future change to the
// IBKR enum surfaces here, not in the CLI's badge-rendering switch.
func TestMarketDataTypeName(t *testing.T) {
	t.Parallel()
	cases := map[int]string{
		0: "",
		1: "live",
		2: "frozen",
		3: "delayed",
		4: "delayed-frozen",
		5: "",
	}
	for in, want := range cases {
		if got := marketDataTypeName(in); got != want {
			t.Errorf("marketDataTypeName(%d) = %q, want %q", in, got, want)
		}
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

// Greeks zero-substitution regression: a genuinely-zero Greek from the
// model (deep-ITM theta ≈ 0, ATM-straddle delta ≈ 0) must surface as a
// non-nil pointer. The previous per-field `!= 0` filter silently dropped
// real zeros and made consumers branching on `nil-as-unavailable` lie.
// Wire contract is documented at rpc.PositionView.Delta etc. ("never
// zero-substituted").
func TestFillOptionGreeksPreservesGenuineZero(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.greeks = newGreeksCache()

	opt := rpc.PositionView{
		Symbol:  "AAPL",
		SecType: rpc.SecTypeOption,
		Expiry:  "20260619",
		Strike:  195,
		Right:   "C",
	}
	key := optionGreeksKey(opt)
	if key == "" {
		t.Fatalf("optionGreeksKey returned empty for %+v", opt)
	}
	// Deep-ITM near expiry: delta ≈ 1.0, gamma ≈ 0, theta ≈ 0, vega > 0.
	srv.greeks.put(key, greeksEntry{
		value: ibkrlib.Greeks{Delta: 1.0, Gamma: 0, Theta: 0, Vega: 0.5},
		ok:    true,
	}, time.Now())

	options := []rpc.PositionView{opt}
	srv.fillOptionGreeks(nil, options)
	p := options[0]

	if p.Delta == nil || *p.Delta != 1.0 {
		t.Errorf("Delta = %v, want 1.0", ptrStr(p.Delta))
	}
	if p.Gamma == nil || *p.Gamma != 0 {
		t.Errorf("Gamma = %v, want non-nil 0", ptrStr(p.Gamma))
	}
	if p.Theta == nil || *p.Theta != 0 {
		t.Errorf("Theta = %v, want non-nil 0", ptrStr(p.Theta))
	}
	if p.Vega == nil || *p.Vega != 0.5 {
		t.Errorf("Vega = %v, want 0.5", ptrStr(p.Vega))
	}
}

func ptrStr(p *float64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
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
	if res.GatewayPort != 4001 {
		t.Fatalf("GatewayPort = %d, want 4001", res.GatewayPort)
	}
	if res.PortOrigin != string(discover.OriginPinned) {
		t.Fatalf("PortOrigin = %q, want pinned", res.PortOrigin)
	}
}

// Malformed params on any unary handler must classify as bad_request, not
// internal. Before the fix every handler returned fmt.Errorf("decode params:")
// which fell through to CodeInternal — the CLI couldn't distinguish a
// client-side mistake from a daemon bug.
func TestDecodeParamsMalformedIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	malformed := json.RawMessage(`{"symbol":`)

	cases := []struct {
		name string
		call func() error
	}{
		{"positions.list", func() error {
			_, err := srv.handlePositionsList(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"chain.expiries", func() error {
			_, err := srv.handleChainExpiries(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"chain.fetch", func() error {
			_, err := srv.handleChainFetch(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"scan.run", func() error {
			_, err := srv.handleScanRun(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"history.daily", func() error {
			_, err := srv.handleHistoryDaily(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"quote.snapshot", func() error {
			_, err := srv.handleQuoteSnapshot(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"cancel", func() error {
			_, err := srv.handleCancel(&rpc.Request{ID: "t", Params: malformed})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected error for malformed params")
			}
			code, _ := classifyError(err)
			if code != rpc.CodeBadRequest {
				t.Fatalf("%s: code = %q, want %q (err=%v)", tc.name, code, rpc.CodeBadRequest, err)
			}
		})
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
		{"contract details timeout (raw)", ibkrlib.ErrContractDetailsTimeout, rpc.CodeTimeout},
		{"chain contract timeout (wrapped)", wrapChainExpiriesErr("AAPL", ibkrlib.ErrContractDetailsTimeout), rpc.CodeTimeout},
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

// TestClassifyBreadthState pins the breadth-handler state-classification
// contract end-to-end. Three of the four wire states are produced from
// the (snapshot-exists, refreshing) pair; "degraded" is reserved on the
// enum but the v0.27.3 engine doesn't emit it (it refuses to persist
// below the coverage threshold instead), so the table deliberately
// does not exercise that case.
//
// The classification was a v0.27.3 fix: prior versions side-channelled
// "refreshing" via fetchRegimeBreadth, which was prone to drift between
// the breadth handler and the regime fetcher. This test pins the single
// source of truth so any future surface added to the daemon must call
// the same helper.
func TestClassifyBreadthState(t *testing.T) {
	cases := []struct {
		name       string
		snap       bool
		refreshing bool
		want       rpc.BreadthState
	}{
		{"snapshot exists, no refresh in flight -> ready", true, false, rpc.BreadthStateReady},
		{"snapshot exists, refresh in flight     -> computing", true, true, rpc.BreadthStateComputing},
		{"no snapshot, refresh in flight         -> computing", false, true, rpc.BreadthStateComputing},
		{"no snapshot, no refresh                -> cold", false, false, rpc.BreadthStateCold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyBreadthState(tc.snap, tc.refreshing)
			if got != tc.want {
				t.Errorf("classifyBreadthState(snap=%v, refreshing=%v) = %q, want %q",
					tc.snap, tc.refreshing, got, tc.want)
			}
		})
	}
}
