// Package daemon implements the ibkrd background process: a single owner of
// the IB Gateway connection that fans out account/quote/chain/scan reads and
// streaming subscriptions to short-lived CLI clients over a Unix socket.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/discover"
	"github.com/osauer/ibkr/internal/rpc"
)

// maxFrameBytes caps each newline-delimited JSON-RPC request the daemon will
// read from a single Unix-socket peer. Bound is generous (1 MiB) — every
// real CLI/MCP request is well under 10 KiB; the cap exists to prevent a
// hostile or buggy client from OOM'ing the daemon by sending a long
// newline-free byte stream that bufio.ReadBytes would otherwise grow into.
const maxFrameBytes = 1 << 20

// errFrameTooLarge is the sentinel returned by readBoundedLine when a peer
// sends more than maxFrameBytes without a terminating newline. The serve
// loop converts it to a CodeBadRequest response and closes the connection
// (we may be mid-frame and can't resync safely).
var errFrameTooLarge = fmt.Errorf("request frame exceeds %d bytes", maxFrameBytes)

// handshakeWatchdogDelay bounds how long the daemon waits for the IBKR
// handshake to complete before publishing a degraded-state hint to
// lastConnectError. pkg/ibkr's per-attempt budget is 10s; 12s is just past
// that so a healthy attempt always lands first, and a wedged gateway
// surfaces a verdict to `ibkr status` well inside its 25s budget.
const handshakeWatchdogDelay = 12 * time.Second

// perCandidateConnectBudget is the hard cap on one candidate's connect+
// handshake before the failover loop moves on. pkg/ibkr's plain-handshake
// path is bounded internally (~20s for 2 attempts), but its TLS-fallback
// retry uses tls.Conn.HandshakeContext — which only ends when ctx is
// cancelled. Against a host that accepts TCP but never replies to
// ClientHello (e.g. an IB Gateway that's stuck mid-startup, or any
// non-TLS listener), the retry hangs indefinitely and failover never
// advances. This deadline guarantees the loop reaches the alternate
// within a bounded time even in the worst case. 25s matches the
// `ibkr status` budget so a fresh status invocation triggers one full
// candidate attempt; the next status invocation sees the failover result.
//
// `var` (not const) so tests can drop it to milliseconds to exercise
// the deadline branch without sleeping for real.
var perCandidateConnectBudget = 25 * time.Second

// Server is the daemon process state.
type Server struct {
	cfg        *config.Resolved
	socketPath string
	startedAt  time.Time
	version    string

	listener net.Listener

	mu               sync.Mutex
	endpoint         discover.Endpoint // post-discovery, fully concrete; mutated on reconnect (issue: AUTO rediscover)
	connector        *ibkrlib.Connector
	streams          map[string]context.CancelFunc
	lastConnectError string
	// lastDiscoveryWarn remembers the most recent reconnect-discovery
	// WARN line so the reconnect loop logs each unique verdict once
	// instead of repeating the same line every poll cycle (~500ms while
	// `ibkr status` waits for handshake).
	lastDiscoveryWarn string

	// connectInFlight is true while a connect attempt (initial or reconnect)
	// is running its handshake. triggerReconnect refuses to fire while this
	// is set so a stream of `ibkr status` calls during a wedged-gateway
	// recovery doesn't pile up parallel connect goroutines.
	connectInFlight bool
	// serverCtx is captured at Start time so handlers can launch
	// reconnect goroutines whose lifetime tracks the daemon, not the
	// short-lived request ctx that triggered the rediscover.
	serverCtx context.Context

	idleTimer   *time.Timer
	idleStop    chan struct{}
	activeConns int

	// subs owns refcounted market-data subscriptions shared between the
	// streaming quote handler, the snapshot quote handler, and any MCP
	// resource subscribers. Initialized in New (depends only on Server's
	// own pointer for the connector lookup).
	subs *subManager

	// expiryIVCache memoises per-(symbol, expiry) ATM IV so a fresh
	// `ibkr chain SYM` invocation reuses results from earlier calls
	// within the TTL window. The first call pays the per-expiry
	// subscribe cost; subsequent calls are near-instant.
	expiryIVs *expiryIVCache

	// prevCloses memoises per-symbol previous-session close (tick 9)
	// so the positions handler can render daily-change deltas without
	// re-subscribing on every invocation. The first `ibkr positions`
	// call after daemon startup pre-warms; later calls are instant.
	prevCloses *prevCloseCache

	// greeks memoises per-option model-computation Greeks so the
	// positions handler doesn't re-subscribe to each option leg on
	// every invocation. Short TTL (60 s) because Greeks shift with
	// spot, but long enough to make back-to-back calls free.
	greeks *greeksCache

	lock *instanceLock

	logger *Logger

	// attempterFactory builds a connectAttempter for a candidate endpoint
	// during the failover loop. Production uses buildAttempter (which
	// wraps newConnector). Tests override this to inject a fake that
	// decides per-port whether the handshake "succeeds." Set by New;
	// callers that construct *Server directly (legacy tests) get a nil
	// factory and must either assign it themselves or only exercise
	// code paths that don't connect.
	attempterFactory func(discover.Endpoint) connectAttempter
}

// Options configures a Server.
type Options struct {
	Config     *config.Resolved
	SocketPath string
	Version    string
	Logger     *Logger
}

// New constructs a Server with the supplied options.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = NewLogger(os.Stderr, opts.Config.Daemon.LogLevel)
	}
	s := &Server{
		cfg:        opts.Config,
		socketPath: opts.SocketPath,
		version:    opts.Version,
		streams:    map[string]context.CancelFunc{},
		idleStop:   make(chan struct{}),
		logger:     opts.Logger,
		expiryIVs:  newExpiryIVCache(),
		prevCloses: newPrevCloseCache(),
		greeks:     newGreeksCache(),
	}
	s.attempterFactory = s.buildAttempter
	s.installSubs()
	return s
}

// installSubs wires the per-symbol subscription manager onto s. Called by
// New (production path) and by test helpers that construct *Server directly
// without going through New. The connector closure re-fetches via
// gatewayConnector on each call so a daemon-side reconnect is observed
// without re-registering the manager.
func (s *Server) installSubs() {
	s.subs = newSubManager(func() ibkrMarketConnector {
		c := s.gatewayConnector()
		if c == nil {
			return nil
		}
		return c
	})
}

// Start runs discovery against the configured (possibly partial) gateway,
// opens the IB Gateway connection in the background, listens on the Unix
// socket, and blocks until ctx is cancelled or Stop is called. Returns
// the first fatal error encountered. Returns ErrAlreadyRunning (without
// touching the gateway) if another ibkrd holds the instance lock.
func (s *Server) Start(ctx context.Context) error {
	lock, err := acquireInstanceLock(s.socketPath)
	if err != nil {
		return err
	}
	s.lock = lock

	// Discovery is fast: no probe at all when port+tls are pinned, and
	// 200ms-per-port TCP probes when not. We do this synchronously before
	// the socket comes up so handleStatusHealth can report the endpoint
	// the daemon will be talking to. Failure here only happens when the
	// user left port unpinned AND no IBKR ports respond — we still bring
	// the socket up so `ibkr status` can render the verdict.
	// Derive a cancellable child of the caller's ctx for background
	// goroutines (reconnect, watchdog). The deferred cancel below ensures
	// any reconnect goroutine launched late by a handler unwinds promptly
	// once Start returns — including the idle-shutdown path where the
	// caller ctx itself isn't cancelled.
	serverCtx, serverCancel := context.WithCancel(ctx)
	defer serverCancel()

	ep, derr := discover.Resolve(serverCtx, partialFromConfig(s.cfg.Gateway))
	s.mu.Lock()
	s.endpoint = ep
	s.serverCtx = serverCtx
	if derr != nil {
		s.lastConnectError = derr.Error()
	}
	s.mu.Unlock()
	if derr != nil {
		s.logger.Warnf("Endpoint discovery: %v (daemon will start anyway)", derr)
	} else {
		s.logger.Infof("Endpoint resolved: %s:%d (port=%s, tls=%v %s, alternates=%v)",
			ep.Host, ep.Port, ep.PortOrigin, ep.TLS, ep.TLSOrigin, ep.Alternates)
	}

	// With AUTO discovery, a startup with no IBKR running ends up with
	// derr != nil; we skip the connect attempt entirely. Triggering
	// reconnect later (via triggerReconnect) will run a fresh discovery
	// once the user starts IBKR. When discovery succeeded, the failover
	// loop below builds and publishes the connector itself — each
	// candidate endpoint gets its own attempter, and s.connector is set
	// to whichever one lands Connected first.
	defer s.stopConnector()

	if err := s.openSocket(); err != nil {
		s.lock.Release()
		s.lock = nil
		return err
	}
	// closeListener is the load-bearing handoff for both clean and panic
	// shutdown paths: idempotent, mu-guarded, unlinks the socket file via
	// UnixListener.Close. The defer here is the safety net for a panic in
	// acceptLoop's spawn or the idle watcher itself.
	defer s.closeListener()

	s.startedAt = time.Now()
	s.logger.Infof("ibkr daemon %s listening on %s (gateway=%s:%d, clientID=%d)",
		s.version, s.socketPath, ep.Host, ep.Port, ep.ClientID)

	// Skip the connect goroutine when discovery already failed — there's
	// nothing to connect to. The socket is still up so `ibkr status`
	// renders the discovery error, and the next request will trigger a
	// rediscover.
	go s.acceptLoop(ctx, s.listener)
	if derr == nil {
		s.mu.Lock()
		s.connectInFlight = true
		s.mu.Unlock()
		go s.runConnectAttempt(serverCtx, ep)
	}
	s.runIdleWatcher(ctx)

	s.closeListener()
	return nil
}

// partialFromConfig translates a config.Gateway (pointer-fielded user
// input) into the minimal struct discover.Resolve consumes.
func partialFromConfig(g config.Gateway) discover.PartialGateway {
	return discover.PartialGateway{
		Host:     g.HostOrDefault(),
		Port:     g.Port,
		ClientID: g.ClientID,
		Account:  g.Account,
		TLS:      g.TLS,
	}
}

// closeListener closes and forgets the listener under mu. Idempotent.
// On UnixListener, Close also unlinks the socket file.
func (s *Server) closeListener() {
	s.mu.Lock()
	l := s.listener
	s.listener = nil
	s.mu.Unlock()
	if l != nil {
		_ = l.Close()
	}
}

// Stop closes the listener and IBKR connection. Safe to call multiple times.
// A Server that never reached openSocket (e.g. lock contention exit) must
// not touch the socket file — that would unlink the active peer's socket
// and break the running daemon.
func (s *Server) Stop() {
	// Notify any live streaming subscribers BEFORE we tear the listener
	// down: emits a daemon_shutdown error frame, lets the consumer render
	// a clean message, and unsubscribes the IBKR market-data lines so the
	// gateway doesn't carry zombie subs across daemon restarts.
	if s.subs != nil {
		s.subs.Close()
	}
	s.mu.Lock()
	for _, c := range s.streams {
		c()
	}
	s.streams = map[string]context.CancelFunc{}
	l := s.listener
	s.listener = nil
	s.mu.Unlock()
	if l != nil {
		_ = l.Close()
		_ = os.Remove(s.socketPath)
	}
	s.stopConnector()
	if s.lock != nil {
		s.lock.Release()
		s.lock = nil
	}
}

// connectAttempter is the subset of *ibkrlib.Connector that the daemon's
// connect/handshake/failover path needs. *ibkrlib.Connector satisfies it
// structurally; defining the interface here lets the per-candidate
// handshake be unit-tested with a fake that decides per-port whether to
// return Connected, without needing a real TCP server.
type connectAttempter interface {
	Start(ctx context.Context) error
	Stop() error
	IsConnected() bool
	UsingTLS() bool
	SetMarketDataType(int) error
	RequestAccountUpdates(account string) error
}

// newConnector constructs (but does not start) the IBKR connector from
// the supplied endpoint. Returns immediately — no network I/O.
//
// Endpoint is passed in (not read from s.endpoint) because reconnect
// rebuilds the connector against a freshly-resolved endpoint that may not
// yet be the published one — the caller decides which endpoint applies.
//
// EnableTLSFallback comes from the discovery layer, not the raw config:
// pinned tls (true or false) → strict, no fallback (issue #3). Auto →
// fallback enabled so the SDK retries the alternate mode if the primary
// gets no handshake response.
func (s *Server) newConnector(ep discover.Endpoint) *ibkrlib.Connector {
	conn := ibkrlib.DefaultConfig()
	conn.Host = ep.Host
	conn.Port = ep.Port
	conn.ClientID = ep.ClientID
	conn.Account = ep.Account
	conn.UseTLS = ep.TLS
	conn.EnableTLSFallback = ep.EnableTLSFallback

	pool := ibkrlib.DefaultPoolConfig()
	pool.ClientIDs = []int{ep.ClientID}
	pool.BaseConfig = conn

	cc := &ibkrlib.ConnectorConfig{
		ServiceName:       "ibkrd",
		PreferredClientID: ep.ClientID,
		PoolConfig:        pool,
	}
	return ibkrlib.NewConnector(cc)
}

// buildAttempter is the production attempter factory. Tests replace
// s.attempterFactory to inject fakes without touching the live SDK.
func (s *Server) buildAttempter(ep discover.Endpoint) connectAttempter {
	return s.newConnector(ep)
}

// runConnectAttempt is the single entry point for "do one full connect
// flow (including handshake failover across alternates) and clear
// connectInFlight when done." Used by both Server.Start (initial attempt)
// and reconnectFlow (after a fresh discovery). Splitting this out is what
// makes triggerReconnect's in-flight gate authoritative — both code paths
// set/clear the same flag.
func (s *Server) runConnectAttempt(ctx context.Context, primary discover.Endpoint) {
	defer func() {
		s.mu.Lock()
		s.connectInFlight = false
		s.mu.Unlock()
	}()
	s.connectWithFailover(ctx, primary)
}

// connectWithFailover walks the primary endpoint then each alternate in
// preference order, building a fresh attempter for each and running its
// handshake under the watchdog. The first attempter to land Connected
// wins; it's published as s.connector and post-connect setup runs.
//
// Failover exists because the TCP probe in discover/ is coarse: a port
// can accept connections without its IBKR backend being ready to talk
// (e.g. Gateway is up but not logged in; API checkbox off; mid-startup).
// When both Gateway and TWS are running locally, discovery's preference
// order picks Gateway-live (4001) first, but if its handshake never
// completes, the daemon used to stay degraded forever even though TWS
// on 7496 was sitting right there in Endpoint.Alternates. This loop is
// what finally uses them.
//
// On a healthy primary the loop completes in one iteration. On an
// unhealthy primary, each failed candidate adds roughly pkg/ibkr's
// per-attempt budget (~20s with TLS fallback) before moving on — worst
// case can exceed `ibkr status`'s 25s budget, in which case the next
// status call surfaces the verdict.
func (s *Server) connectWithFailover(ctx context.Context, primary discover.Endpoint) {
	factory := s.attempterFactory
	if factory == nil {
		// Direct &Server{} construction (legacy tests) leaves the field
		// unset. Default to the production wrapper so the loop is
		// always callable.
		factory = s.buildAttempter
	}

	candidates := []discover.Endpoint{primary}
	for _, port := range primary.Alternates {
		alt := primary
		alt.Port = port
		alt.Alternates = nil
		candidates = append(candidates, alt)
	}

	for i, cand := range candidates {
		if ctx.Err() != nil {
			return
		}
		if i > 0 {
			s.logger.Infof("Failover: %s:%d did not handshake; trying alternate %s:%d (%d/%d)",
				candidates[i-1].Host, candidates[i-1].Port, cand.Host, cand.Port, i+1, len(candidates))
		}

		a := factory(cand)
		// Publish the candidate so handlers / status see the port the
		// daemon is currently talking to. The type assertion is safe in
		// production (buildAttempter returns *ibkrlib.Connector); test
		// fakes skip the s.connector publish but the loop still drives
		// the per-candidate handshake.
		s.mu.Lock()
		s.endpoint = cand
		s.lastConnectError = ""
		if real, ok := a.(*ibkrlib.Connector); ok {
			s.connector = real
		}
		s.mu.Unlock()

		if s.tryOneHandshake(ctx, a, cand) {
			s.postConnectSetup(a, cand)
			return
		}

		// This candidate failed. Stop it so its background goroutines
		// don't leak into the next attempt. Clear s.connector iff it
		// still points at this attempter (a concurrent reconnect could
		// have moved on already, though connectInFlight gates that).
		if err := a.Stop(); err != nil {
			s.logger.Warnf("Failover: stop failed candidate %s:%d: %v", cand.Host, cand.Port, err)
		}
		if real, ok := a.(*ibkrlib.Connector); ok {
			s.mu.Lock()
			if s.connector == real {
				s.connector = nil
			}
			s.mu.Unlock()
		}
	}

	if ctx.Err() != nil {
		return
	}
	// All candidates exhausted. Publish a verdict that names what we
	// tried so `ibkr status` shows the user the full picture (not just
	// the original probe winner).
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, fmt.Sprintf("%s:%d", c.Host, c.Port))
	}
	hint := fmt.Sprintf(
		"none of %d discovered endpoint(s) completed TWS handshake (tried %s); confirm the IBKR app you intend to use has 'Enable ActiveX and Socket Clients' on and is logged in",
		len(candidates), strings.Join(names, ", "),
	)
	s.mu.Lock()
	s.lastConnectError = hint
	s.mu.Unlock()
	s.logger.Warnf("Daemon up but no endpoint usable: %s", hint)
}

// tryOneHandshake runs a single candidate's connect under the watchdog
// and returns true iff the attempter ended Connected. On failure it
// publishes a per-candidate hint to lastConnectError so a status poll
// mid-failover shows the truth.
//
// The candidate runs under a bounded ctx (perCandidateConnectBudget) so
// the failover loop can advance even when pkg/ibkr's TLS-handshake
// retry would otherwise hang against a black-hole peer.
func (s *Server) tryOneHandshake(ctx context.Context, a connectAttempter, ep discover.Endpoint) bool {
	candidateCtx, candidateCancel := context.WithTimeout(ctx, perCandidateConnectBudget)
	defer candidateCancel()

	watchdogCtx, watchdogCancel := context.WithCancel(candidateCtx)
	defer watchdogCancel()
	go s.handshakeWatchdog(watchdogCtx, a.IsConnected, handshakeWatchdogDelay, ep)

	err := a.Start(candidateCtx)
	// Outer (daemon) ctx cancelled → shutdown raced with us; exit silently
	// so the surrounding loop's ctx check stops the iteration too.
	if ctx.Err() != nil {
		return false
	}
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		// Per-candidate budget expired with the SDK still in handshake
		// retry. This is the black-hole-TLS path: TCP accepted, TLS
		// ClientHello sent, no ServerHello ever arrived. Publish a
		// candidate-specific timeout hint so the user sees which port
		// burned the budget.
		hint := fmt.Sprintf("gateway %s:%d did not handshake within %s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
			ep.Host, ep.Port, perCandidateConnectBudget)
		s.mu.Lock()
		s.lastConnectError = hint
		s.mu.Unlock()
		s.logger.Warnf("Candidate budget expired: %s", hint)
		return false
	case err != nil && candidateCtx.Err() != nil:
		// SDK returned a wrapped ctx error (e.g. "tls handshake failed:
		// context deadline exceeded"). Treat as candidate-budget expiry
		// for the same reason as above.
		hint := fmt.Sprintf("gateway %s:%d did not handshake within %s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
			ep.Host, ep.Port, perCandidateConnectBudget)
		s.mu.Lock()
		s.lastConnectError = hint
		s.mu.Unlock()
		s.logger.Warnf("Candidate budget expired: %s", hint)
		return false
	case err != nil:
		s.mu.Lock()
		s.lastConnectError = err.Error()
		s.mu.Unlock()
		s.logger.Errorf("connect to IB Gateway %s:%d: %v", ep.Host, ep.Port, err)
		return false
	}

	// pkg/ibkr's pool returns success even when the underlying TCP
	// handshake hasn't completed (e.g. gateway unreachable, API socket
	// disabled). Probe IsConnected so we log the truth.
	if !a.IsConnected() {
		hint := fmt.Sprintf("gateway %s:%d did not complete TWS handshake; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
			ep.Host, ep.Port)
		s.mu.Lock()
		s.lastConnectError = hint
		s.mu.Unlock()
		s.logger.Warnf("Daemon up but gateway not connected: %s", hint)
		return false
	}
	return true
}

// postConnectSetup runs the best-effort initialization that follows a
// successful handshake (market-data type + account-updates stream).
// Failures here are non-fatal: snapshot data still flows; only the
// streaming mark/value/P&L decoration on positions is degraded.
func (s *Server) postConnectSetup(a connectAttempter, ep discover.Endpoint) {
	s.mu.Lock()
	s.lastConnectError = ""
	s.mu.Unlock()
	s.logger.Infof("Connected to IB Gateway %s:%d (clientID=%d, tls=%v)",
		ep.Host, ep.Port, ep.ClientID, a.UsingTLS())

	// Default to type 2 (frozen-aware): IBKR returns live ticks for
	// entitled symbols during market hours and the last-known close
	// otherwise. Snapshot requests reliably terminate with
	// tickSnapshotEnd this way; pure live (type 1) can leave snapshots
	// hanging when the market is closed.
	if err := a.SetMarketDataType(2); err != nil {
		s.logger.Warnf("SetMarketDataType(frozen) failed: %v", err)
	}
	// Start the streaming account+portfolio subscription so position
	// rows carry live mark/value/P&L.
	if err := a.RequestAccountUpdates(ep.Account); err != nil {
		s.logger.Warnf("RequestAccountUpdates failed (positions will lack marks): %v", err)
	}
}

// handshakeWatchdog publishes a degraded-state hint to lastConnectError if
// the gateway hasn't connected by `delay`. Takes isConnected as a function
// pointer so tests can drive this directly without a real *Connector.
//
// The gate (s.lastConnectError == "") avoids clobbering a real error that
// the main path may have already set. The success branch in
// connectGatewayBackground clears the hint when the connect eventually
// lands (e.g. via the SDK's TLS fallback retry) — so the watchdog is
// informational only, not authoritative.
func (s *Server) handshakeWatchdog(ctx context.Context, isConnected func() bool, delay time.Duration, ep discover.Endpoint) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	if isConnected() {
		return
	}
	hint := fmt.Sprintf(
		"gateway %s:%d not responding to TWS handshake within %s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
		ep.Host, ep.Port, delay,
	)
	s.mu.Lock()
	if s.lastConnectError == "" {
		s.lastConnectError = hint
	}
	s.mu.Unlock()
	s.logger.Warnf("Handshake watchdog: %s", hint)
}

// triggerReconnect launches a rediscover+reconnect attempt in a
// background goroutine if (a) no connect attempt is already in flight and
// (b) the daemon is not already healthy. Returns true iff a fresh attempt
// was started.
//
// The motivation is the AUTO discovery flow: discovery only ran once at
// daemon startup, and after a failed handshake the endpoint stayed pinned
// to the original probe winner forever. If the user closes IB Gateway
// (4001) and starts TWS (7496) instead, the daemon would render
// "degraded — gateway not connected" against 4001 indefinitely. Calling
// this from `gatewayConnector()` and from `handleStatusHealth` means each
// new request kicks off a rediscover, picks up the new listener, and
// recovers without a daemon restart.
//
// We clear lastConnectError when claiming the in-flight slot so the
// existing CLI status loop (which polls until either Connected or
// LastError != "") treats the recovery attempt as "in flight" and waits
// for a verdict — exactly the same UX as the initial connect path.
func (s *Server) triggerReconnect() bool {
	s.mu.Lock()
	if s.connectInFlight {
		s.mu.Unlock()
		return false
	}
	// IsReady, not IsConnected: TCP-up is not enough. If the connector
	// landed in the {ready=false, conn=true} state (handlers cleared
	// during a transient gateway hiccup), we must re-establish — and the
	// only way out is to tear the old TCP socket down in reconnectFlow.
	if s.connector != nil && s.connector.IsReady() {
		s.mu.Unlock()
		return false
	}
	if s.serverCtx == nil || s.serverCtx.Err() != nil {
		// Server.Start hasn't run yet, the daemon has already begun
		// shutting down, or this Server was constructed outside the
		// normal lifecycle (e.g. in a test that doesn't go through
		// Start). Nothing to do.
		s.mu.Unlock()
		return false
	}
	s.connectInFlight = true
	s.lastConnectError = ""
	ctx := s.serverCtx
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.connectInFlight = false
			s.mu.Unlock()
		}()
		s.reconnectFlow(ctx)
	}()
	return true
}

// reconnectFlow tears down any existing connector, re-runs port
// discovery, builds a fresh connector against whichever endpoint the
// probe winner produces, and runs the handshake. Caller must have
// already claimed connectInFlight; this function does not manage the
// flag itself (the goroutine wrapper in triggerReconnect does).
//
// Tearing down the old connector is required: pkg/ibkr's pool can't be
// "restarted" against a different host/port, and we may genuinely need
// to switch (e.g. GW on 4001 → TWS on 7496). Stop is idempotent and
// safe even when the old connector never finished its first handshake.
func (s *Server) reconnectFlow(ctx context.Context) {
	s.mu.Lock()
	old := s.connector
	s.connector = nil
	s.mu.Unlock()
	if old != nil {
		if err := old.Stop(); err != nil {
			s.logger.Warnf("Reconnect: stop old connector: %v", err)
		}
	}

	ep, derr := discover.Resolve(ctx, partialFromConfig(s.cfg.Gateway))
	s.mu.Lock()
	s.endpoint = ep
	if derr != nil {
		s.lastConnectError = derr.Error()
	}
	prevWarn := s.lastDiscoveryWarn
	switch {
	case derr != nil:
		s.lastDiscoveryWarn = derr.Error()
	default:
		s.lastDiscoveryWarn = ""
	}
	curWarn := s.lastDiscoveryWarn
	s.mu.Unlock()
	if derr != nil {
		// Same verdict as the previous attempt → already logged; stay quiet.
		// A changed verdict (or first failure) logs once. This keeps the
		// reconnect-during-status-poll loop from emitting the same WARN line
		// every 500ms while the user is waiting for the handshake.
		if curWarn != prevWarn {
			s.logger.Warnf("Reconnect: discovery: %v", derr)
		}
		return
	}
	s.logger.Infof("Reconnect: endpoint resolved: %s:%d (port=%s, tls=%v %s, alternates=%v)",
		ep.Host, ep.Port, ep.PortOrigin, ep.TLS, ep.TLSOrigin, ep.Alternates)

	s.connectWithFailover(ctx, ep)
}

// stopConnector tears down the IBKR connector and forgets it. Idempotent.
// Holds s.mu across the entire lifecycle transition so handlers reading
// s.connector via gatewayConnector() never observe a value mid-Stop.
func (s *Server) stopConnector() {
	s.mu.Lock()
	c := s.connector
	s.connector = nil
	s.mu.Unlock()
	if c == nil {
		return
	}
	if err := c.Stop(); err != nil {
		s.logger.Warnf("Connector.Stop: %v", err)
	}
}

// gatewayConnector returns the live IBKR connector if one is constructed
// AND fully connected, or nil otherwise. All read handlers go through
// this — taking the snapshot under mu prevents the race where a handler
// reads s.connector as non-nil, stopConnector nils it, then the handler
// dereferences it for a method call.
//
// When the daemon is degraded (no connector, or connector not connected),
// this kicks off a background rediscover+reconnect via triggerReconnect.
// The current handler still returns ErrIBKRUnavailable to its caller —
// the next call to the daemon will see the result of the reconnect. This
// is what closes the AUTO-discovery hole where the daemon would stay
// pinned to a stale port after the user moved between Gateway and TWS.
func (s *Server) gatewayConnector() *ibkrlib.Connector {
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	// IsReady, not IsConnected: handlers also need to be armed. A connector
	// in {ready=false, conn=true} can't serve data — return nil so the
	// caller surfaces ErrIBKRUnavailable, while triggerReconnect rebuilds.
	if c == nil || !c.IsReady() {
		s.triggerReconnect()
		return nil
	}
	return c
}

func (s *Server) openSocket() error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o700); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// We hold the instance flock; any peer holding the socket is by
	// definition stale (its lock would be released). Dial-probe first to
	// distinguish "stale file from a crashed predecessor" (safe to remove)
	// from "live peer that beat us to flock acquisition" (impossible, but
	// surface clearly if it ever happens).
	if fi, err := os.Stat(s.socketPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if c, err := net.DialTimeout("unix", s.socketPath, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return fmt.Errorf("socket %s already serving despite holding lock; refusing to evict", s.socketPath)
		}
		if err := os.Remove(s.socketPath); err != nil {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen unix: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.listener = l
	return nil
}

// acceptLoop runs against a stable listener reference captured at Start
// time. Mutating s.listener (closeListener / Stop) only affects future
// Start calls; the live loop sees Accept return net.ErrClosed once that
// listener is closed and exits.
func (s *Server) acceptLoop(ctx context.Context, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Warnf("accept: %v", err)
			continue
		}
		s.bumpActive(+1)
		go func() {
			defer s.bumpActive(-1)
			s.serveConn(ctx, conn)
		}()
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	r := bufio.NewReaderSize(conn, 64<<10)
	enc := json.NewEncoder(conn)
	for {
		line, err := readBoundedLine(r, maxFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
				return
			}
			if errors.Is(err, errFrameTooLarge) {
				// Frame too big to dispatch and we may be mid-frame
				// (no newline seen) — drop the connection after a
				// classified error so a buggy client gets a clear
				// signal instead of a silent close.
				_ = enc.Encode(rpc.Response{ID: "", Ok: false, Error: &rpc.Error{Code: rpc.CodeBadRequest, Message: err.Error()}})
				return
			}
			s.logger.Debugf("conn read: %v", err)
			return
		}
		var req rpc.Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpc.Response{ID: "", Ok: false, Error: &rpc.Error{Code: rpc.CodeBadRequest, Message: err.Error()}})
			continue
		}
		if terminal := s.dispatch(connCtx, &req, enc, r); terminal {
			return
		}
	}
}

// readBoundedLine reads from r up to and including the next '\n', returning
// the line bytes including the trailing newline. Returns errFrameTooLarge
// if no newline appears within maxBytes — the caller should treat this as
// terminal and close the connection (mid-frame state is unrecoverable
// without an out-of-band resync token, which the protocol does not have).
//
// Uses bufio.ReadSlice in a loop so the per-iteration allocation stays
// bounded by bufio's internal buffer; the accumulated len(buf) check
// short-circuits before append can grow without bound.
func readBoundedLine(r *bufio.Reader, maxBytes int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxBytes {
			return nil, errFrameTooLarge
		}
		buf = append(buf, chunk...)
		if err == nil {
			return buf, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// Got a partial chunk that filled bufio's internal buffer
			// without finding '\n'; keep reading.
			continue
		}
		return buf, err
	}
}

// recoverHandler is the defer target the dispatcher uses to convert a
// handler panic into an internal_error response on the *same* JSON-RPC id
// instead of letting it unwind through serveConn and kill the listener
// goroutine. The stack trace lands in the daemon log so the panic is
// debuggable; the misbehaving client gets a classified error and the
// other connected clients keep working.
func recoverHandler(logger *Logger, enc *json.Encoder, req *rpc.Request) {
	rec := recover()
	if rec == nil {
		return
	}
	method := ""
	id := ""
	if req != nil {
		method = req.Method
		id = req.ID
	}
	logger.Errorf("panic in handler method=%s id=%s: %v\n%s", method, id, rec, debug.Stack())
	writeError(enc, id, rpc.CodeInternal, fmt.Sprintf("internal panic: %v", rec))
}

func (s *Server) dispatch(ctx context.Context, req *rpc.Request, enc *json.Encoder, r *bufio.Reader) (terminal bool) {
	// A handler panic must not unwind through serveConn — that would kill
	// the per-connection goroutine and disconnect *other* clients sharing
	// the listener. Convert to a classified error on the request's id so
	// the panic is debuggable from the log and the misbehaving caller
	// gets a clean error.
	defer recoverHandler(s.logger, enc, req)
	// Per-request deadline. Streaming methods get no deadline (the stream
	// itself owns its lifetime). Unary deadlines are bounded below the
	// CLI's 60s per-invocation budget so the daemon errors first with a
	// classified message instead of the CLI surfacing a generic socket
	// timeout. Without this, a slow handler (e.g. chain.fetch's
	// per-strike contract resolution) could outlive the CLI cancellation
	// and leak gateway market-data slots.
	if d := unaryDeadline(req.Method); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	switch req.Method {
	case rpc.MethodAccountSummary:
		s.unary(req, enc, func() (any, error) { return s.handleAccountSummary(ctx) })
	case rpc.MethodPositionsList:
		s.unary(req, enc, func() (any, error) { return s.handlePositionsList(ctx, req) })
	case rpc.MethodQuoteSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleQuoteSnapshot(ctx, req) })
	case rpc.MethodChainFetch:
		s.unary(req, enc, func() (any, error) { return s.handleChainFetch(ctx, req) })
	case rpc.MethodChainExpiries:
		s.unary(req, enc, func() (any, error) { return s.handleChainExpiries(ctx, req) })
	case rpc.MethodScanRun:
		s.unary(req, enc, func() (any, error) { return s.handleScanRun(ctx, req) })
	case rpc.MethodScanList:
		s.unary(req, enc, func() (any, error) { return s.handleScanList(), nil })
	case rpc.MethodScanParams:
		s.unary(req, enc, func() (any, error) { return s.handleScanParams(ctx, req) })
	case rpc.MethodHistoryDaily:
		s.unary(req, enc, func() (any, error) { return s.handleHistoryDaily(ctx, req) })
	case rpc.MethodStatusHealth:
		s.unary(req, enc, func() (any, error) { return s.handleStatusHealth(), nil })
	case rpc.MethodQuoteSubscribe:
		s.handleQuoteSubscribe(ctx, req, enc, r)
		return true
	case rpc.MethodCancel:
		s.unary(req, enc, func() (any, error) { return s.handleCancel(req) })
	case rpc.MethodOrderPlace:
		_, err := handleOrderPlace(ctx, req)
		writeError(enc, req.ID, rpc.CodeTradingDisabled, err.Error())
	case rpc.MethodOrderCancel:
		_, err := handleOrderCancel(ctx, req)
		writeError(enc, req.ID, rpc.CodeTradingDisabled, err.Error())
	default:
		writeError(enc, req.ID, rpc.CodeUnknownMethod, "unknown method: "+req.Method)
	}
	return false
}

// unaryDeadline returns the per-request deadline for a method, or 0 for
// methods that own their own lifetime (streaming). Values are picked to
// stay below the CLI's 60s per-invocation budget in cmd/ibkr/main.go so
// the daemon's classified error reaches the user instead of a raw socket
// timeout, while still being generous enough for the slowest legitimate
// path: chain.fetch's sequential per-strike contract resolution.
//
// MethodPositionsList gets a long budget because the first call after
// daemon start has to resolve every held option's contract details
// (cold cache, 4-way parallel × up to 5 s per leg) before the Greeks
// model-computation tick can be polled. After the option-contract
// cache is warm, subsequent positions calls return in under 5 s; the
// 30 s ceiling is for the first-call worst case.
func unaryDeadline(method string) time.Duration {
	switch method {
	case rpc.MethodChainFetch, rpc.MethodChainExpiries:
		return 25 * time.Second
	case rpc.MethodHistoryDaily, rpc.MethodPositionsList:
		return 30 * time.Second
	case rpc.MethodScanRun:
		// Up to 35 s scanner-subscription budget (off-hours cold-start
		// for HIGH_OPEN_GAP / TOP_PERC_GAIN / HIGH_OPT_IMP_VOLAT_OVER_HIST
		// can take 25-45 s) + per-row enrichment in waves of
		// scanEnrichConcurrency × scanEnrichWindow each. Typical RTH scan:
		// ~15 s. Worst case off-hours: 35 s scan + 20 s enrichment + slack.
		// The CLI per-invocation deadline at cmd/ibkr/main.go is bumped to
		// 90 s in lockstep so the daemon's classified error reaches the
		// user instead of a raw socket timeout.
		return 75 * time.Second
	case rpc.MethodScanParams:
		// ~200 KB XML payload, single round trip — usually <2 s. Generous
		// budget to absorb a degraded gateway without timing out before
		// the connection error surfaces.
		return 15 * time.Second
	case rpc.MethodAccountSummary, rpc.MethodQuoteSnapshot:
		return 10 * time.Second
	case rpc.MethodStatusHealth, rpc.MethodScanList:
		return 5 * time.Second
	case rpc.MethodQuoteSubscribe:
		return 0
	default:
		return 15 * time.Second
	}
}

// unary wraps a handler so result/error envelopes are uniform.
func (s *Server) unary(req *rpc.Request, enc *json.Encoder, fn func() (any, error)) {
	res, err := fn()
	if err != nil {
		code, msg := classifyError(err)
		writeError(enc, req.ID, code, msg)
		return
	}
	buf, err := json.Marshal(res)
	if err != nil {
		writeError(enc, req.ID, rpc.CodeInternal, "marshal result: "+err.Error())
		return
	}
	_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Result: buf})
}

func writeError(enc *json.Encoder, id, code, message string) {
	_ = enc.Encode(rpc.Response{ID: id, Ok: false, Error: &rpc.Error{Code: code, Message: message}})
}

func classifyError(err error) (string, string) {
	var bad *badRequestError
	var contractTimeout *chainContractTimeoutError
	switch {
	case errors.As(err, &bad):
		return rpc.CodeBadRequest, err.Error()
	case errors.As(err, &contractTimeout):
		return rpc.CodeTimeout, err.Error()
	case errors.Is(err, ibkrlib.ErrSymbolInactive):
		return rpc.CodeSymbolInactive, err.Error()
	case errors.Is(err, ibkrlib.ErrIBKRUnavailable):
		return rpc.CodeGatewayUnavailable, err.Error()
	case errors.Is(err, ibkrlib.ErrContractDetailsTimeout):
		return rpc.CodeTimeout, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return rpc.CodeTimeout, err.Error()
	default:
		return rpc.CodeInternal, err.Error()
	}
}

func (s *Server) bumpActive(delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeConns += delta
	s.resetIdleLocked()
}

func (s *Server) resetIdleLocked() {
	if s.idleTimer == nil {
		return
	}
	if s.activeConns > 0 {
		s.idleTimer.Stop()
		return
	}
	s.idleTimer.Reset(s.cfg.Daemon.IdleTimeout.Std())
}

func (s *Server) runIdleWatcher(ctx context.Context) {
	timeout := s.cfg.Daemon.IdleTimeout.Std()
	if timeout <= 0 {
		<-ctx.Done()
		return
	}
	s.mu.Lock()
	s.idleTimer = time.NewTimer(timeout)
	s.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			s.idleTimer.Stop()
			return
		case <-s.idleStop:
			s.idleTimer.Stop()
			return
		case <-s.idleTimer.C:
			s.mu.Lock()
			active := s.activeConns
			s.mu.Unlock()
			if active == 0 {
				s.logger.Infof("Idle timeout reached (%s); shutting down", timeout)
				return
			}
			s.mu.Lock()
			s.idleTimer.Reset(timeout)
			s.mu.Unlock()
		}
	}
}
