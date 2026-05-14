//go:build !windows

// Package integration runs end-to-end tests of the full ibkrd + ibkr CLI
// stack against a live IB Gateway. The tests deliberately do not mock or
// stub IBKR — they exist to prove the actual binaries connect and talk to
// the real gateway.
//
// Tests skip if the IB Gateway is not reachable on the configured port; this
// matches the project's "no mock" stance: when the live gateway is down we
// don't paper over the gap, we surface it.
//
// Unix-only: launchSharedDaemon uses Setpgid + kill(-pgid) to ensure the
// spawned daemon never orphans if go test is interrupted. Windows has no
// equivalent in syscall.SysProcAttr; the package was already Unix-only in
// practice (TestMain skips with "Unix-only daemon" on Windows) so the
// build tag just makes that explicit.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/rpc"
)

var (
	sharedSocket  string
	sharedCLI     string
	sharedStop    func()
	sharedSkipped bool
)

// TestMain probes the IB Gateway, builds the single ibkr binary, and
// launches one daemon (`ibkr daemon --foreground`) shared by every test
// in this package. Per-test daemons are too slow (each handshake is
// multi-second) and risk overwhelming the gateway with rapid-fire
// client-ID changes.
func TestMain(m *testing.M) {
	// Always build the binary first — lifecycle tests (kill/respawn,
	// non-responsive daemon) exercise the CLI's daemon-management path
	// which doesn't need a live gateway.
	cli, err := buildBin()
	if err != nil {
		_, _ = os.Stderr.WriteString("integration: build failed: " + err.Error() + "\n")
		os.Exit(2)
	}
	sharedCLI = cli

	if !probeGatewayReachable() {
		sharedSkipped = true
		os.Exit(m.Run())
	}
	socketPath, stop, err := launchSharedDaemon(cli)
	if err != nil {
		_, _ = os.Stderr.WriteString("integration: launch failed (gateway may be in degraded API-mute state — restart it and re-run): " + err.Error() + "\n")
		sharedSkipped = true
		if stop != nil {
			stop()
		}
		os.Exit(m.Run())
	}
	sharedSocket = socketPath
	sharedStop = stop

	// The daemon socket appears even when ibkrd's connector ran into the
	// "degraded mode" branch (handshake failed → daemon stays up but
	// disconnected). Ask the daemon whether it actually reached the gateway
	// before declaring the suite live; on a degraded gateway, every test
	// skips cleanly instead of failing with cascading IBKR-unavailable errors.
	if !daemonReachedGateway(socketPath) {
		_, _ = os.Stderr.WriteString("integration: daemon started but failed to handshake with IB Gateway (likely in degraded API-mute state — restart it and re-run); skipping live tests.\n")
		sharedSkipped = true
	}

	// Route SIGINT/SIGTERM through stop() so a Ctrl-C on `go test` (or any
	// signal short of SIGKILL) tears the spawned daemon down rather than
	// orphaning it. SIGKILL is unrecoverable — nothing we can do there. The
	// goroutine exits when stop() runs to completion below or when the
	// process dies.
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigC
		if sharedStop != nil {
			sharedStop()
		}
		os.Exit(130)
	}()

	code := m.Run()
	stop()
	os.Exit(code)
}

// daemonReachedGateway calls status.health on the freshly launched daemon
// and returns true only when the daemon reports connected=true and a
// non-zero server version. The daemon stays up in degraded mode when its
// connector can't handshake; we use this signal to mark the suite as
// "no live gateway" rather than letting every test fail with internal
// errors.
func daemonReachedGateway(socketPath string) bool {
	conn, err := dial.Connect(socketPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var res rpc.HealthResult
	if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
		return false
	}
	return res.Connected && res.ServerVersion > 0
}

func probeGatewayReachable() bool {
	host := "127.0.0.1"
	port := 4001
	if v := os.Getenv("IBKR_TEST_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func skipIfNoGateway(t *testing.T) {
	t.Helper()
	if sharedSkipped {
		t.Skip("IB Gateway not reachable; skipping live integration test")
	}
}

func buildBin() (string, error) {
	dir, err := os.MkdirTemp("", "ibkr-integration-")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "ibkr")
	cmd := exec.Command("go", "build", "-o", out, "../../cmd/ibkr")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out, nil
}

func launchSharedDaemon(cliBin string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "ibkr-integration-run-")
	if err != nil {
		return "", nil, err
	}
	socketPath := filepath.Join(dir, "ibkr.sock")
	logPath := filepath.Join(dir, "ibkr-daemon.log")
	cfgPath := filepath.Join(dir, "config.toml")
	cid := nextClientID()
	port := 4001
	if v := os.Getenv("IBKR_TEST_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	// Pin every dimension explicitly so the test harness doesn't depend
	// on AUTO discovery — a deterministic config matches the historical
	// behavior the integration tests were written for, and doesn't probe
	// extra ports on the live gateway during a test run.
	cfg := "[gateway]\nhost = \"127.0.0.1\"\nport = " +
		strconv.Itoa(port) + "\nclient_id = " + strconv.Itoa(cid) + "\ntls = false\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return "", nil, err
	}
	cmd := exec.Command(cliBin, "daemon",
		"--config", cfgPath,
		"--socket", socketPath,
		"--foreground",
		"--log", logPath,
	)
	// Place the daemon in its own process group so stop() can signal the
	// whole group via kill(-pid). Without this, a daemon that ever spawned
	// helpers (or any future grandchild) would survive a test panic that
	// skipped stop(). macOS does not propagate parent death to children, so
	// the group-signal is the only reliable cleanup path.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	pgid := cmd.Process.Pid
	stop := func() {
		_ = syscall.Kill(-pgid, syscall.SIGINT)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = os.RemoveAll(dir)
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return socketPath, stop, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	stop()
	return "", nil, fmt.Errorf("daemon socket did not appear within 25s; see %s", logPath)
}

// nextClientID generates a unique client ID per daemon process so the IBKR
// gateway doesn't reject overlapping handshakes (one connection per ID).
// Range chosen well clear of regime's 100-104 reservation and the default
// daemon client ID 15.
var clientIDCounter int32 = 19

func nextClientID() int { return int(atomic.AddInt32(&clientIDCounter, 1)) }

func client(t *testing.T) *dial.Conn {
	t.Helper()
	conn, err := dial.Connect(sharedSocket)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestStatusReportsConnected(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var res rpc.HealthResult
	if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
		t.Fatalf("status.health: %v", err)
	}
	if !res.Connected {
		t.Fatalf("expected daemon to report connected, got %+v", res)
	}
	if res.ServerVersion == 0 {
		t.Errorf("expected non-zero server version, got %d", res.ServerVersion)
	}
}

func TestAccountSummaryReturnsLiveData(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	var res rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &res); err != nil {
		t.Fatalf("account.summary: %v", err)
	}
	if res.AccountID == "" {
		t.Fatalf("account_id missing from response: %+v", res)
	}
	if res.NetLiquidation == 0 {
		t.Errorf("net_liquidation reported as zero (suspicious): %+v", res)
	}
}

func TestPositionsReturnLiveMarks(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	// First call may race the streaming portfolio update; retry briefly until
	// at least one position has a non-zero mark.
	var res rpc.PositionsResult
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.Call(ctx, rpc.MethodPositionsList, nil, &res); err != nil {
			t.Fatalf("positions.list: %v", err)
		}
		if positionsHaveMarks(res.Stocks) || positionsHaveMarks(res.Options) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(res.Stocks)+len(res.Options) == 0 {
		t.Skip("paper account has no open positions to verify marks against")
	}
	t.Errorf("no position carried a non-zero mark within 10s: %+v", res)
}

func positionsHaveMarks(rows []rpc.PositionView) bool {
	for _, p := range rows {
		if p.Mark != 0 {
			return true
		}
	}
	return false
}

func TestQuoteSnapshotReturnsPrice(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var q rpc.Quote
	params := rpc.QuoteSnapshotParams{
		Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Currency: "USD"},
	}
	if err := conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
		t.Fatalf("quote.snapshot AAPL: %v", err)
	}
	if q.Symbol != "AAPL" {
		t.Errorf("symbol echoed wrong: %q", q.Symbol)
	}
	if q.DataType == "" {
		t.Errorf("data_type required on every quote response")
	}
	if q.Bid == nil && q.Ask == nil && q.Last == nil {
		t.Errorf("AAPL snapshot delivered no bid/ask/last; suspect timeout or entitlement issue: %+v", q)
	}
}

func TestUnknownMethodReturnsStructuredError(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := conn.Call(ctx, "no.such.method", nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
	rpcErr, ok := err.(*rpc.Error)
	if !ok {
		t.Fatalf("expected *rpc.Error, got %T: %v", err, err)
	}
	if rpcErr.Code != rpc.CodeUnknownMethod {
		t.Errorf("expected code %q, got %q", rpc.CodeUnknownMethod, rpcErr.Code)
	}
}

func TestTradingVerbsRefused(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, m := range []string{rpc.MethodOrderPlace, rpc.MethodOrderCancel} {
		err := conn.Call(ctx, m, json.RawMessage(`{}`), nil)
		if err == nil {
			t.Errorf("%s: expected refusal in v1, got success", m)
			continue
		}
		rpcErr, ok := err.(*rpc.Error)
		if !ok {
			t.Errorf("%s: expected *rpc.Error, got %T (%v)", m, err, err)
			continue
		}
		if rpcErr.Code != rpc.CodeTradingDisabled {
			t.Errorf("%s: expected code %q, got %q", m, rpc.CodeTradingDisabled, rpcErr.Code)
		}
	}
}

func TestCLIBinaryAccountText(t *testing.T) {
	skipIfNoGateway(t)

	cmd := exec.Command(sharedCLI, "account")
	cmd.Env = append(os.Environ(), "IBKR_SOCKET="+sharedSocket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ibkr account: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "Account") || !strings.Contains(s, "Net liquidation") {
		t.Errorf("unexpected ibkr account text output:\n%s", s)
	}
}

// TestScanTopMoversReturnsRows guards against the scanner field-offset
// regression that silently dropped every msgScannerData frame. If the
// dispatcher contract or the parser drifts again, this test catches it
// against the real gateway. Skips when the gateway returns 0 rows (e.g. on
// weekends/holidays the scanner can come back empty for some presets).
func TestScanTopMoversReturnsRows(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var res rpc.ScanResult
	params := rpc.ScanRunParams{Preset: "top-movers", Limit: 10}
	if err := conn.Call(ctx, rpc.MethodScanRun, params, &res); err != nil {
		// Off-hours scanner subscriptions sometimes hang for the full
		// timeout instead of returning empty rows — TWS behavior, not a
		// regression on our side. Skip rather than fail the suite.
		if isScannerTimeout(err) {
			t.Skipf("scanner timed out (off-hours flakiness): %v", err)
		}
		t.Fatalf("scan.run top-movers: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Skip("scanner returned 0 rows (gateway/preset may have no candidates outside market hours)")
	}
	first := res.Rows[0]
	if first.Symbol == "" {
		t.Errorf("first scanner row has empty symbol: %+v", res)
	}
	if res.Type == "" {
		t.Errorf("scan result missing scan type: %+v", res)
	}
}

// isScannerTimeout matches only the scanner-subscription-specific
// timeout shapes that surface from a cold or off-hours scanner farm.
// Generic "context deadline exceeded" / "i/o timeout" used to be in
// this list too — they were dropped because they also fire on a
// deadlocked handler, a dropped socket, or any other regression a
// scanner test should catch, not skip.
func isScannerTimeout(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "scanner subsystem did not respond") ||
		strings.Contains(s, "scanner timed out") ||
		strings.Contains(s, "scanner parameters timed out")
}

// TestScanParamsReturnsCatalog exercises the reqScannerParameters round-trip
// against the live gateway. The XML payload is large (typically 1-2 MB on a
// Pro account); this test pins (a) that the wire-level reader handles a
// multi-megabyte frame without the 1 MB cap that originally truncated it
// and desynced the stream, (b) that the XML parser extracts the three
// catalog lists, and (c) that --instrument STK is a strict subset of the
// unfiltered catalog. If any of those regress, this test fails before
// users see "scanner timed out".
func TestScanParamsReturnsCatalog(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var full rpc.ScanParamsResult
	if err := conn.Call(ctx, rpc.MethodScanParams, rpc.ScanParamsParams{}, &full); err != nil {
		// The catalog request is gateway-stored data, not a live scanner
		// subscription — no off-hours skip applies here. This test is the
		// time-of-day-independent regression guard for the wire/parser
		// path, so an error must fail, not be silently swallowed.
		t.Fatalf("scan.params (unfiltered): %v", err)
	}
	if len(full.Instruments) == 0 || len(full.Locations) == 0 || len(full.ScanTypes) == 0 {
		t.Fatalf("scan.params returned empty catalog: %+v", full)
	}
	// Sanity-check well-known scanCodes the daemon hardcodes as defaults.
	want := map[string]bool{
		"TOP_PERC_GAIN":                false,
		"TOP_PERC_LOSE":                false,
		"MOST_ACTIVE":                  false,
		"HOT_BY_VOLUME":                false,
		"HIGH_OPEN_GAP":                false,
		"HIGH_OPT_IMP_VOLAT_OVER_HIST": false,
		"HOT_BY_OPT_VOLUME":            false,
	}
	for _, st := range full.ScanTypes {
		if _, ok := want[st.Code]; ok {
			want[st.Code] = true
		}
	}
	for code, found := range want {
		if !found {
			t.Errorf("scan.params missing default-preset scanCode %q from the live catalog (the v0.11 defaults were validated against IB Gateway server-version 203 — if your gateway drops one of these, the default preset fails)", code)
		}
	}

	var stk rpc.ScanParamsResult
	if err := conn.Call(ctx, rpc.MethodScanParams, rpc.ScanParamsParams{Instrument: "STK"}, &stk); err != nil {
		t.Fatalf("scan.params (instrument=STK): %v", err)
	}
	if len(stk.ScanTypes) == 0 {
		t.Fatalf("scan.params with --instrument STK returned 0 scan types")
	}
	if len(stk.ScanTypes) > len(full.ScanTypes) {
		t.Errorf("filtered scan_types (%d) > unfiltered (%d) — filter must be a subset", len(stk.ScanTypes), len(full.ScanTypes))
	}
}

// TestScanAdHocAgainstDefaultLocation runs an ad-hoc scan (Type+Exchange
// only, no preset). Sister to TestScanTopMoversReturnsRows but exercises
// the new ad-hoc dispatch path in handleScanRun. Skips on 0 rows because
// scanner output can be empty outside market hours.
func TestScanAdHocAgainstDefaultLocation(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	params := rpc.ScanRunParams{Type: "TOP_PERC_GAIN", Exchange: "STK.US.MAJOR", Limit: 5}
	var res rpc.ScanResult
	if err := conn.Call(ctx, rpc.MethodScanRun, params, &res); err != nil {
		// Off-hours scanner subscriptions can hang for the full timeout
		// rather than returning empty rows. That's a property of TWS, not
		// a bug in our code — skip the same way TestScanTopMoversReturnsRows
		// would have if rows came back empty. The gateway's catalog test
		// (TestScanParamsReturnsCatalog above) is the time-of-day-independent
		// regression guard for the wire/parser path.
		if isScannerTimeout(err) {
			t.Skipf("scanner timed out (off-hours flakiness): %v", err)
		}
		t.Fatalf("ad-hoc scan: %v", err)
	}
	if res.Preset != "" {
		t.Errorf("ad-hoc result.Preset = %q, want empty (preset is only set for named runs)", res.Preset)
	}
	if res.Type != "TOP_PERC_GAIN" {
		t.Errorf("ad-hoc result.Type = %q, want TOP_PERC_GAIN (must echo the scanCode the caller passed)", res.Type)
	}
	if len(res.Rows) == 0 {
		t.Skip("ad-hoc scanner returned 0 rows (off-hours/quiet market)")
	}
}

// TestScanAdHocMissingExchangeIsBadRequest covers the validation path:
// passing --type without --exchange is a user error (or a confused
// agent), and must surface as bad_request rather than wedging the daemon
// or sending an under-specified subscription to the gateway.
func TestScanAdHocMissingExchangeIsBadRequest(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var res rpc.ScanResult
	err := conn.Call(ctx, rpc.MethodScanRun, rpc.ScanRunParams{Type: "TOP_PERC_GAIN"}, &res)
	if err == nil {
		t.Fatalf("expected bad_request error for ad-hoc scan missing exchange, got success: %+v", res)
	}
	if !strings.Contains(err.Error(), "bad_request") && !strings.Contains(err.Error(), "exchange") {
		t.Errorf("expected error to mention bad_request or exchange, got: %v", err)
	}
}

// TestChainAAPLLegsPopulated guards against the option-chain ConID resolution
// regression. Before the fix, every leg subscribed without a resolved ConID
// and IBKR returned code 200 ("No security definition has been found"); the
// CLI rendered an all-blank table. The fix calls reqContractDetails first
// and feeds the resolved ConID into reqMktData.
//
// The "no fabrication" invariant means cells legitimately stay nil when the
// gateway doesn't deliver a price (e.g. illiquid strikes far from ATM). We
// only assert that AT LEAST one strike has at least one populated leg field —
// any more and the test would be brittle against weekend frozen-data quirks.
func TestChainAAPLLegsPopulated(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	expiry := nextThirdFriday(time.Now().UTC())
	params := rpc.ChainFetchParams{
		Symbol: "AAPL",
		Expiry: expiry.Format("2006-01-02"),
		Width:  3, // 7 strikes — keeps the per-leg round-trip count modest
		Side:   "both",
	}
	var res rpc.ChainResult
	if err := conn.Call(ctx, rpc.MethodChainFetch, params, &res); err != nil {
		t.Fatalf("chain.fetch AAPL %s: %v", params.Expiry, err)
	}
	wantStrikes := 2*params.Width + 1
	if got := len(res.Strikes); got != wantStrikes {
		t.Fatalf("expected %d strikes (ATM ± %d), got %d", wantStrikes, params.Width, got)
	}
	if res.Spot <= 0 {
		t.Errorf("chain spot price not populated: %+v", res)
	}
	if res.DTE <= 0 {
		t.Errorf("chain DTE should be positive, got %d", res.DTE)
	}
	populated := 0
	for _, s := range res.Strikes {
		if s.CallBid != nil || s.CallAsk != nil || s.CallLast != nil ||
			s.PutBid != nil || s.PutAsk != nil || s.PutLast != nil {
			populated++
		}
	}
	if populated == 0 {
		t.Errorf("no strike had any leg field populated; ConID resolution likely broken again. result=%+v", res)
	}
}

// nextThirdFriday returns the third Friday of the month at least 7 days from
// now. AAPL has weekly options too but third-Friday monthlies are universally
// liquid — picking them keeps the test stable across weeks. The resulting
// date is also always > current time, so DTE > 0 is guaranteed.
func nextThirdFriday(now time.Time) time.Time {
	month := now.Month()
	year := now.Year()
	for {
		first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
		// Friday is weekday 5 (Sunday=0). Third Friday = first Friday + 14 days.
		offset := (int(time.Friday) - int(first.Weekday()) + 7) % 7
		third := first.AddDate(0, 0, offset+14)
		if third.Sub(now) >= 7*24*time.Hour {
			return third
		}
		month++
		if month > 12 {
			month = 1
			year++
		}
	}
}
