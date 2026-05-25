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
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/breadth/spx"
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

	// zeroGamma holds the current/most-recent dealer zero-gamma
	// compute for SPX, keyed on NY trading-session date. The compute
	// is a multi-minute fan-out across hundreds of option legs; this
	// cache singleflights concurrent callers and outlives any single
	// RPC ctx so a client disconnect mid-compute doesn't kill the
	// run that other pollers are waiting on.
	zeroGamma *gammaZeroCache
	// gammaStarted guards the boot-time prewarm so it only fires once
	// per Server lifetime. postConnectSetup runs once per successful
	// candidate (including post-reconnect), so without this Once a
	// reconnect-driven second postConnectSetup would re-kick a fresh
	// gamma compute on top of the cached one, doubling gateway slot
	// pressure for no benefit (the soft-TTL refresh in kickOrJoin
	// already keeps the cache from going stale).
	gammaStarted sync.Once

	// breadth runs the SPX 50-DMA breadth compute. The engine owns
	// a persisted constituent-close cache, a once-daily scheduler
	// goroutine launched from postConnectSetup, and a rolling-history
	// file the handler reads for sparkline rendering. nil before
	// installBreadthEngine.
	breadth *spx.Engine
	// breadthStarted guards breadth.Run() so the scheduler goroutine
	// only spins up after the gateway completes its first handshake.
	// postConnectSetup runs once per successful candidate, including
	// post-reconnect — this Once ensures Run() launches exactly once
	// per Server lifetime so we don't end up with parallel daily-tick
	// loops competing on refreshMu.
	breadthStarted sync.Once
	// breadthConnector is a dedicated IBKR client connection (separate
	// clientID, separate handshake, separate rate-limit budget) that
	// backs the breadth refresh's historical-bar fan-out. Carved off
	// the primary connector so breadth's ~503 historical requests don't
	// compete with interactive RPCs and the gamma option-leg fan-out
	// for the primary's 40-msg/sec and 60-historical/10-min budgets.
	// nil before postConnectSetup completes the bulk handshake (or if
	// the bulk handshake fails — breadth gracefully reports
	// "no gateway" for that refresh and retries next tick).
	breadthConnector *ibkrlib.Connector
	// breadthClientStarted guards bulk-connector startup the same way
	// breadthStarted guards Run() — exactly once per Server lifetime
	// across the postConnectSetup re-runs that follow each reconnect.
	breadthClientStarted sync.Once

	// membersRefresher runs the daemon-internal SPX-constituent
	// refresh: daily 02:30 ET fetch from Wikipedia, startup catchup,
	// opportunistic post-rollover trigger from the breadth handler.
	// All three triggers converge on one singleflighted fetch
	// goroutine; the result is written to
	// `~/.cache/ibkr/spx-members/sp500-members.json` and pushed into
	// the breadth engine. nil when disabled by config / env, or when
	// the cache-path resolution failed at New (no $HOME / XDG).
	membersRefresher *spx.Refresher
	// membersRefresherStarted guards Run() launch so a reconnect-
	// driven second postConnectSetup doesn't spawn a duplicate
	// scheduler loop. Mirrors breadthStarted / breadthClientStarted.
	membersRefresherStarted sync.Once
	// membersCachePath is captured at install so handlers reading the
	// loaded file (status renderer) don't have to re-resolve the XDG
	// path on every call. Empty when membersRefresher is nil.
	membersCachePath string

	// contractStore persists symbol→conID resolutions across daemon
	// restarts. IBKR caps reqContractDetails at ~50 per 10 minutes
	// per ACCOUNT (the per-clientID isolation breadthConnector enables
	// doesn't help here — the cap is upstream); every restart that
	// re-resolves the 503 SPX members from scratch pays that bucket
	// over and over. The store loads at postConnectSetup and seeds
	// both connectors, saves every minute + on Stop. Reconstitution
	// is handled via a members-hash field on the file: stale members
	// get pruned at load when the current member list differs from
	// the cached one. nil only on the rare path where
	// DefaultContractStoreDir returns an error (no HOME / XDG_CACHE
	// set) — the daemon continues without persistence in that case.
	contractStore *ibkrlib.ContractStore
	// contractCacheLoaded records what Load() returned so both
	// connectors can be seeded from the same set without double disk
	// reads. Written once in postConnectSetup before the primary
	// connector ready signal, read by startBreadthConnector when the
	// bulk lane comes up. nil before load, possibly empty map after.
	contractCacheLoaded map[string]ibkrlib.ContractDetailsLite
	// contractCacheSaveStarted gates the background save loop to
	// exactly once per Server lifetime, mirroring breadthStarted /
	// breadthClientStarted. postConnectSetup re-runs on each
	// reconnect; without the Once a second postConnectSetup would
	// spawn a duplicate save goroutine racing the first.
	contractCacheSaveStarted sync.Once

	// streaks persists each regime indicator's consecutive-sessions-in-
	// band tally across daemon restarts. Loaded lazily on first Tick;
	// written atomically (temp+rename) on every band change. Same shape
	// as contractStore — own persistence file at
	// $XDG_CACHE_HOME/ibkr/regime-streaks.json, own version field, own
	// invalidation rules (entries persist across days; counters change
	// only on band transitions). Nil only on the rare XDG/HOME-unset
	// path; the daemon then runs without streak persistence (every
	// restart resets the counters).
	streaks *StreakStore

	// regimePrewarming is set while prewarmRegimeSymbols' fan-out is in
	// flight. Surfaces via backgroundTasks() so the idle watcher defers
	// shutdown and `ibkr status` reflects the work — same coherence
	// guarantee breadth-spx and gamma-zero ride. Up to ~30 s of
	// gateway-slot pressure during postConnectSetup; if the user
	// autospawns the daemon and walks away, the idle watcher could
	// previously fire mid-prewarm.
	regimePrewarming atomic.Bool
	// postConnectSetupDone latches true at the end of the first
	// successful postConnectSetup. Gates the Connected field of
	// handleStatusHealth so an `ibkr status` that lands between
	// `c.ready=true` (set asynchronously by the connection read loop
	// in pkg/ibkr) and postConnectSetup finishing its synchronous
	// prewarm sentinel-setting reports Connected=false (the CLI keeps
	// polling) rather than Connected=true with empty BackgroundTasks.
	// Without this latch, the FIRST status after restart could miss
	// the imminent gamma-zero / breadth-spx / regime-prewarm work —
	// the daemon is technically handshaken but not yet ready to
	// publish its full background-task surface.
	//
	// Latched true once and not reset on reconnect: subsequent
	// reconnects reuse the same prewarm machinery (Once-guarded), so
	// the daemon never re-enters a "starting up" state from the
	// user's point of view. A daemon panic during postConnectSetup
	// crashes the process (no recover at that level), so the flag
	// never stays false forever in practice.
	postConnectSetupDone atomic.Bool

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
		zeroGamma:  newGammaZeroCache(),
	}
	s.attempterFactory = s.buildAttempter
	s.installSubs()
	s.installBreadthEngine()
	s.installMembersRefresher()
	s.installContractStore()
	s.installStreakStore()
	s.installGammaZeroCache()
	return s
}

// installGammaZeroCache replaces the bootstrap in-memory gamma cache
// with a store-backed one. Best-effort: a missing XDG_CACHE_HOME /
// HOME pair leaves the in-memory cache in place — every restart pays
// the full ~5 min compute, but the daemon itself starts fine.
//
// When the store is attached, any persisted result from today's NY
// session is loaded and installed as `current`, so the first caller
// after restart skips the compute. See
// docs/design/gamma-zero-cache-persistence.md for the cost/benefit
// rationale (cold combined runs can cross the 5-min threshold).
func (s *Server) installGammaZeroCache() {
	dir, err := gammaZeroStoreDefaultDir()
	if err != nil {
		s.logger.Warnf("gamma cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.zeroGamma = newGammaZeroCacheWithStore(newGammaZeroStore(dir), time.Now(), s.logger)
}

// installStreakStore constructs the regime-streak persistence layer.
// Best-effort: a missing XDG_CACHE_HOME / HOME pair leaves streaks
// nil and the daemon runs without persistence (every restart resets
// the counters). The regime-snapshot path nil-guards before calling
// Tick so the rest of the row population continues unaffected.
func (s *Server) installStreakStore() {
	dir, err := DefaultStreakStoreDir()
	if err != nil {
		s.logger.Warnf("regime streaks: resolve cache dir: %v (counters disabled)", err)
		return
	}
	s.streaks = NewStreakStore(dir)
}

// infof / warnf are nil-safe wrappers around s.logger. The tests that
// construct a zero-value Server directly (breadth_connector_test.go)
// reach installBreadthEngine / installMembersRefresher with logger=nil;
// these wrappers let those tests keep working without seeding a logger
// the test doesn't need.
func (s *Server) infof(format string, args ...any) {
	if s.logger != nil {
		s.logger.Infof(format, args...)
	}
}

func (s *Server) warnf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warnf(format, args...)
	}
}

// installContractStore constructs the on-disk contract-cache store and
// attaches it to s. Best-effort: if XDG_CACHE_HOME and HOME are both
// unset (effectively never on a real OS user account), the field stays
// nil and the daemon runs without contract-cache persistence — every
// restart pays the full IBKR rate-limit tax to re-resolve, but the
// daemon itself starts fine.
func (s *Server) installContractStore() {
	dir, err := ibkrlib.DefaultContractStoreDir()
	if err != nil {
		s.logger.Warnf("contract cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.contractStore = ibkrlib.NewContractStore(dir)
}

// installBreadthEngine builds the SPX 50-DMA breadth engine and
// attaches it to s. Construction is best-effort: a failure to resolve
// the on-disk cache dir is logged but does not block daemon startup —
// the engine field stays nil and handleBreadthSPX surfaces
// status="unavailable" with a notes pointer.
//
// The fetcher closes over s.breadthGatewayConnector — the dedicated
// bulk-historical client — so breadth's ~500-name fan-out runs against
// its own IBKR clientID, separate from the primary connector that
// serves interactive RPCs and the gamma compute. Reading the live
// pointer on every call lets the engine survive a bulk-connector
// reconnect without re-instantiation, mirroring the primary-thunk
// pattern.
//
// Members seeding: prefers the runtime-refreshed cache file
// (`~/.cache/ibkr/spx-members/sp500-members.json`) over the embedded
// list, so a daemon installed from a months-old release that has
// since cached a fresher list serves current membership immediately.
// Falls back to the embedded list on missing/corrupt/sanity-failed
// file. Logs the source on startup so a human triaging breadth values
// can confirm which list is in play.
func (s *Server) installBreadthEngine() {
	dir, err := spx.DefaultDir()
	if err != nil {
		s.logger.Warnf("breadth: resolve cache dir: %v (engine disabled)", err)
		return
	}
	fetcher := newBreadthFetcher(s.breadthGatewayConnector)

	var members []string
	if path, perr := spx.MembersDefaultPath(); perr == nil {
		if loaded, asOf, ok := spx.LoadExternal(path); ok {
			members = loaded
			s.infof("breadth: loaded %d members from cache (as_of %s)", len(loaded), asOf.Format("2006-01-02"))
		}
	}
	if len(members) == 0 {
		embedded, asOf := spx.MemberList()
		members = embedded
		s.infof("breadth: using embedded members list (%d names, as_of %s)", len(embedded), asOf.Format("2006-01-02"))
	}

	s.breadth = spx.New(spx.NewStore(dir), fetcher, spx.Options{Logger: s.logger, Members: members})
}

// installMembersRefresher stands up the runtime SPX-members refresher.
// The refresher fetches Wikipedia's constituent list daily at 02:30 ET
// plus on startup catch-up, writes the result to the XDG cache file,
// and pushes new lists into the breadth engine.
//
// Construction is best-effort:
//   - breadth engine missing (cache-dir failure) → no refresher.
//   - members cache path unresolvable (HOME unset) → no refresher.
//
// The IBKR_SPX_MEMBERS_AUTO_REFRESH env var (symmetric override:
// "1" force-on, "0" force-off, unset defers) takes precedence over the
// TOML [spx] members_auto_refresh field. The status renderer surfaces
// which gate is active. Run() is launched separately in
// postConnectSetup so the refresher's goroutine lifetime tracks
// serverCtx.
func (s *Server) installMembersRefresher() {
	if s.breadth == nil {
		return
	}
	cachePath, err := spx.MembersDefaultPath()
	if err != nil {
		s.warnf("members refresh: resolve cache path: %v (refresher disabled)", err)
		return
	}

	// Resolve env+config to a single enabled/disabled decision plus
	// the reason. Env wins when present; otherwise TOML governs. The
	// reason flags drive the status row's `disabled (env)` /
	// `disabled (config)` suffix so a confused user knows which knob
	// to flip.
	envEnabled, envForced := config.SPXMembersAutoRefreshFromEnv()
	configEnabled := s.cfg.SPX.MembersAutoRefreshEnabled()

	var enabled, pinnedByEnv, pinnedByConfig bool
	switch {
	case envForced:
		enabled = envEnabled
		pinnedByEnv = !envEnabled
	default:
		enabled = configEnabled
		pinnedByConfig = !configEnabled
	}
	_ = enabled // refresher derives state from the Pinned* flags

	version := s.version
	fetch := func(ctx context.Context) ([]string, time.Time, error) {
		return spx.FetchAndParse(ctx, spx.WikipediaURL, version)
	}
	s.membersRefresher = spx.NewRefresher(spx.RefresherOptions{
		Engine:         s.breadth,
		CachePath:      cachePath,
		Fetch:          fetch,
		Logger:         s.logger,
		PinnedByConfig: pinnedByConfig,
		PinnedByEnv:    pinnedByEnv,
	})
	s.membersCachePath = cachePath
}

// breadthGatewayConnector returns the bulk-historical IBKR connector
// dedicated to the breadth refresh, or nil if it hasn't completed
// its handshake yet. Snapshot under s.mu mirrors gatewayConnector's
// read pattern so handlers reading the pointer never see it mid-Stop.
//
// Distinct from gatewayConnector: this one does NOT trigger a
// reconnect on nil. Bulk-connector failure is non-fatal — breadth
// surfaces "no gateway" for the affected refresh and the next 16:35
// ET tick retries from scratch. Auto-reconnect machinery for the
// bulk lane would double the lifecycle surface without changing the
// observable outcome (breadth still retries).
func (s *Server) breadthGatewayConnector() *ibkrlib.Connector {
	s.mu.Lock()
	c := s.breadthConnector
	s.mu.Unlock()
	if c == nil || !c.IsReady() {
		return nil
	}
	return c
}

// startBreadthConnector launches the bulk-historical IBKR client and
// blocks until handshake completes or breadthClientStartBudget elapses.
// Called once per Server lifetime from postConnectSetup — gated by
// breadthClientStarted so a reconnect-driven second postConnectSetup
// doesn't spin up a duplicate bulk connection.
//
// The handshake is intentionally synchronous: breadth.Run() launches
// shortly after on a sibling goroutine and would otherwise race a
// not-yet-ready bulk connector, dropping all 503 fetches against nil
// on the first refresh. 12 s mirrors the primary's per-candidate
// budget — long enough for a healthy local gateway, short enough to
// surface a misconfigured second cid promptly.
//
// On failure (collision past MaxClientIDRetries, gateway unreachable,
// handshake timeout) the function logs and returns without setting
// s.breadthConnector. Breadth's refresh sees a nil bulk connector
// and aborts gracefully; the daemon as a whole continues running.
func (s *Server) startBreadthConnector(ctx context.Context, primaryEp discover.Endpoint) {
	bulkEp := primaryEp
	bulkEp.ClientID = s.cfg.Gateway.BreadthClientIDOrDefault()

	attempter := s.newConnector(bulkEp)

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		if err := attempter.Start(ctx); err != nil {
			s.logger.Warnf("breadth bulk connector start: %v", err)
		}
	}()

	select {
	case <-startDone:
	case <-time.After(breadthClientStartBudget):
		s.logger.Warnf("breadth bulk connector: handshake timeout after %s (cid=%d); refresh will use 'no gateway' fallback until next tick", breadthClientStartBudget, bulkEp.ClientID)
		_ = attempter.Stop()
		return
	case <-ctx.Done():
		_ = attempter.Stop()
		return
	}

	if !attempter.IsReady() {
		s.logger.Warnf("breadth bulk connector: not ready after Start (cid=%d); skipping", bulkEp.ClientID)
		_ = attempter.Stop()
		return
	}

	s.mu.Lock()
	s.breadthConnector = attempter
	s.mu.Unlock()
	s.logger.Infof("breadth bulk connector ready (cid=%d, separate 40-msg/sec budget from primary)", bulkEp.ClientID)

	// Seed the bulk lane from the same persisted contract cache that
	// the primary lane was seeded from in postConnectSetup. ConIDs are
	// globally unique so both lanes get the same wire identity for
	// every symbol — no contract resolution churn on the bulk side
	// across daemon restarts. seedFromContractStore is a no-op if the
	// store wasn't successfully loaded.
	s.seedConnectorFromCache(attempter)
}

// stopBreadthConnector tears down the bulk-historical connector if one
// was successfully started. Safe to call when nil. Mirror of
// stopConnector's lifecycle discipline: nil under mu so handlers can't
// observe a half-stopped pointer.
func (s *Server) stopBreadthConnector() {
	s.mu.Lock()
	c := s.breadthConnector
	s.breadthConnector = nil
	s.mu.Unlock()
	if c == nil {
		return
	}
	if err := c.Stop(); err != nil {
		s.logger.Warnf("breadth bulk connector Stop: %v", err)
	}
}

// breadthClientStartBudget bounds the bulk-historical handshake. Set
// to match the primary's perCandidateConnectBudget so a misconfigured
// second cid (collision, gateway rejecting client IDs, etc.) doesn't
// stretch postConnectSetup beyond a sensible boot delay.
const breadthClientStartBudget = 12 * time.Second

// contractCacheSaveInterval is how often the background loop reads
// both connectors' contractCache, merges the entries, and writes the
// result to ContractStore. 60s balances I/O cost against the
// "how much recent work would we lose on a crash?" risk — at one
// full file write (~50 KB) per minute the disk cost is invisible,
// and the worst-case loss is a minute of resolutions which the next
// daemon restart re-fetches transparently.
const contractCacheSaveInterval = 60 * time.Second

// seedFromContractStore loads the persisted contract cache from disk
// and seeds the supplied connector with each non-stale entry. Stale
// is defined relative to the current SPX members list (entries whose
// symbol isn't in the current sp500Members AND isn't one of the
// well-known seeds get pruned at load — they're delisted, renamed,
// or simply replaced by reconstitution).
//
// Also caches the loaded map in s.contractCacheLoaded for
// startBreadthConnector to seed the bulk lane from without a second
// disk read. No-op when contractStore is nil (XDG/HOME unresolved at
// New() time) or when the disk file doesn't exist (cold install).
func (s *Server) seedFromContractStore(c *ibkrlib.Connector) {
	if s.contractStore == nil || c == nil {
		return
	}
	loaded, savedHash, err := s.contractStore.Load()
	if err != nil {
		s.logger.Warnf("contract cache: load: %v (will start cold)", err)
		return
	}
	if loaded == nil {
		s.logger.Infof("contract cache: no on-disk file yet, starting cold")
		s.contractCacheLoaded = map[string]ibkrlib.ContractDetailsLite{}
		return
	}
	members, _ := spx.MemberList()
	currentHash := ibkrlib.MembersHash(members)
	if savedHash != "" && savedHash != currentHash {
		// SPX reconstituted since the last save. Prune entries whose
		// symbol isn't in the current list — keep the well-known
		// seeds (SPX, VIX, etc.) regardless since they aren't SPX
		// members but are still useful for regime / gamma paths.
		loaded = pruneNonMembers(loaded, members)
		s.logger.Infof("contract cache: SPX members hash changed (%s → %s); pruned to %d current-member entries", savedHash, currentHash, len(loaded))
	}
	s.contractCacheLoaded = loaded
	seeded := 0
	for sym, detail := range loaded {
		if c.SeedContractDetails(sym, detail) {
			seeded++
		}
	}
	s.logger.Infof("contract cache: seeded %d entries from disk", seeded)

	// Option contracts (added in store v2). Expired entries are GC'd
	// at the store layer; whatever remains is still valid for at least
	// today's NY session. Pre-seeds the connector's optionContractCache
	// so the next gamma prewarm finds most strikes already resolved
	// and avoids the 40s-per-fan-out round-trip cost on warm restarts.
	if opts, err := s.contractStore.LoadOptions(); err != nil {
		s.logger.Warnf("contract cache: load options: %v", err)
	} else if seededOpts := c.SeedOptionContracts(opts); seededOpts > 0 {
		s.logger.Infof("contract cache: seeded %d option entries from disk", seededOpts)
	}
}

// seedConnectorFromCache seeds c from the already-loaded
// s.contractCacheLoaded map. Called by startBreadthConnector when the
// bulk lane comes up; avoids a second disk read and guarantees both
// lanes see the same seed set. No-op if seedFromContractStore hasn't
// run yet (primary connector failed to come up) or if c is nil.
func (s *Server) seedConnectorFromCache(c *ibkrlib.Connector) {
	if c == nil || len(s.contractCacheLoaded) == 0 {
		return
	}
	seeded := 0
	for sym, detail := range s.contractCacheLoaded {
		if c.SeedContractDetails(sym, detail) {
			seeded++
		}
	}
	s.logger.Infof("contract cache: seeded %d entries into bulk connector", seeded)
}

// contractCacheSaveLoop runs for the daemon's lifetime, periodically
// snapshotting both connectors' contractCache and saving the merged
// result to ContractStore. The merge prefers entries from whichever
// connector has them — they're identical when both lanes have
// resolved the same symbol (conIDs are globally unique), and the
// merge picks any one. Returns when ctx is cancelled (daemon shutting
// down); Stop() runs a final saveContractCache after this to capture
// the last minute's work.
func (s *Server) contractCacheSaveLoop(ctx context.Context) {
	t := time.NewTicker(contractCacheSaveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.saveContractCache()
		}
	}
}

// saveContractCache snapshots both connectors' contractCaches, merges
// them, and writes the result. Errors are logged but not propagated —
// a transient I/O failure shouldn't take the daemon down; the next
// tick retries. Safe to call when ContractStore is nil or when
// neither connector is up (no-op).
func (s *Server) saveContractCache() {
	if s.contractStore == nil {
		return
	}
	merged := map[string]ibkrlib.ContractDetailsLite{}
	for _, c := range []*ibkrlib.Connector{s.gatewayConnector(), s.breadthGatewayConnector()} {
		if c == nil {
			continue
		}
		// Both connectors should resolve to the same ConID for a
		// given symbol; if they ever diverge (corrupted state on
		// one lane), the merge picks whichever was iterated last
		// — a benign tie-break that doesn't poison the file
		// because both values are valid IBKR contract identities.
		maps.Copy(merged, c.SnapshotContracts())
	}
	// Option contracts come from the primary connector only (gamma's
	// bulk prewarm runs against the primary's option cache). Empty when
	// no prewarm has fired this session — that's fine, an empty options
	// map persists as an empty JSON object.
	options := map[string]ibkrlib.ContractDetailsLite{}
	if primary := s.gatewayConnector(); primary != nil {
		options = primary.SnapshotOptionContracts()
	}
	if len(merged) == 0 && len(options) == 0 {
		return
	}
	members, _ := spx.MemberList()
	hash := ibkrlib.MembersHash(members)
	if err := s.contractStore.Save(merged, options, hash); err != nil {
		s.logger.Warnf("contract cache: save: %v", err)
	}
}

// pruneNonMembers returns a new map containing only entries whose
// symbol is in the current SPX members list OR is one of the
// well-known seeds (SPX, VIX, VIX3M, HYG, SPY, USD.JPY — the regime
// dashboard symbols with verified IBKR contracts). Caller uses this to strip delisted / renamed
// tickers from the persisted cache when SPX has reconstituted since
// the last save. The well-known seeds aren't SPX members but are
// still useful for the regime path, so they survive the prune.
func pruneNonMembers(loaded map[string]ibkrlib.ContractDetailsLite, members []string) map[string]ibkrlib.ContractDetailsLite {
	keep := make(map[string]struct{}, len(members)+8)
	for _, m := range members {
		keep[m] = struct{}{}
	}
	for _, sym := range []string{"SPX", "VIX", "VIX3M", "HYG", "SPY", "USD.JPY", "DXY", "NDX"} {
		keep[sym] = struct{}{}
	}
	out := make(map[string]ibkrlib.ContractDetailsLite, len(loaded))
	for sym, detail := range loaded {
		if _, ok := keep[sym]; ok {
			out[sym] = detail
		}
	}
	return out
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
	defer s.stopBreadthConnector()

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
	// Breadth scheduler launches from postConnectSetup behind a
	// sync.Once — the cold-start bootstrap fan-out depends on a live
	// gateway connector, and launching here would race against the
	// in-flight connect goroutine. Once Run() is up it survives
	// subsequent gateway disconnects via the fetcher's connector
	// thunk.
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
	// Capture the last minute of contract resolutions before tearing
	// the connectors down. The save loop runs every 60s; without this
	// final flush, anything resolved in the trailing tick would be
	// lost across the restart. Best-effort: a save error here just
	// logs, since shutdown shouldn't fail because a disk write
	// couldn't complete.
	s.saveContractCache()

	s.stopConnector()
	s.stopBreadthConnector()
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
	SubscribeAccountPnL(account string) error
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

	cc := &ibkrlib.ConnectorConfig{
		ServiceName:       "ibkrd",
		PreferredClientID: ep.ClientID,
		BaseConfig:        conn,
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
	// Subscribe to the account-level Daily P&L stream (TWS msg 94). Failure
	// is non-fatal: account.summary keeps working without daily fields,
	// and per-position daily lookups still degrade gracefully because the
	// connector cache returns nil pointers on miss. Empty account means
	// the discover/config path couldn't pin one — skip the call entirely
	// since reqPnL requires an account.
	if ep.Account != "" {
		if err := a.SubscribeAccountPnL(ep.Account); err != nil {
			s.logger.Warnf("SubscribeAccountPnL failed (account.summary will lack daily P&L): %v", err)
		}
	}

	// Load the persisted contract cache and seed the primary connector
	// BEFORE prewarm / breadth / gamma fire. Every cache hit here is
	// an IBKR rate-limit-bucket token saved: without persistence, each
	// daemon restart re-resolves all 503 SPX members + the 6 regime
	// seeds, draining IBKR's per-account reqContractDetails bucket
	// over and over. The members-hash check prunes stale entries when
	// SPX has reconstituted since the last save — added members fall
	// through to fresh resolution via the normal path.
	s.seedFromContractStore(s.connector)

	// Spawn the periodic save loop. Guarded by contractCacheSaveStarted
	// so reconnect-driven postConnectSetup re-runs don't multiply the
	// goroutine. The loop runs for the daemon's lifetime, picking up
	// new resolutions from both connectors every minute.
	if s.contractStore != nil && s.serverCtx != nil {
		s.contractCacheSaveStarted.Do(func() {
			go s.contractCacheSaveLoop(s.serverCtx)
		})
	}

	// Pre-warm contract-details cache for the regime-dashboard symbols
	// in the background. Without this the first regime call of the day
	// races five parallel goroutines against fresh contract resolution
	// — confirmed observed at v0.22.0: first cold call returned with
	// hyg_50dma and weekly_change_pct missing; second call (caches
	// warm) returned all fields. Pre-warming eliminates the variance
	// without changing the regime request path.
	//
	// Sentinel set HERE (before `go`), not inside the goroutine: a
	// status RPC arriving in the brief window between this function
	// returning and the goroutine being scheduled would otherwise see
	// Connected=true but no background tasks. The goroutine's deferred
	// Store(false) is the load-bearing clear.
	s.regimePrewarming.Store(true)
	go s.prewarmRegimeSymbols()

	// Stand up the dedicated bulk-historical IBKR client BEFORE
	// launching breadth.Run(). The scheduler's cold-start bootstrap
	// fan-outs 500 historical-bar fetches and would otherwise read
	// nil from breadthGatewayConnector on every leg until the bulk
	// handshake lands. Synchronous start (12-s ceiling) blocks
	// postConnectSetup briefly; in exchange, breadth.Run() launches
	// with a guaranteed-or-nil view of the bulk connector.
	if s.breadth != nil && s.serverCtx != nil {
		s.breadthClientStarted.Do(func() {
			s.startBreadthConnector(s.serverCtx, ep)
		})
	}

	// Launch the breadth scheduler now that the gateway has handshaken.
	// The cold-start bootstrap inside Run() fan-outs 500 historical-bar
	// fetches via the bulk connector started above and would fail-and-
	// poison the cache if either connector weren't ready yet (v0.27.0
	// regression — see engine.finalise's Coverage==0 guard). The Once
	// guard means a reconnect-driven second postConnectSetup is a no-op
	// rather than spawning a duplicate scheduler loop.
	//
	// MarkPendingBootstrap is called BEFORE `go` so a status RPC arriving
	// in the goroutine-spawn window sees breadth-spx in BackgroundTasks
	// when a bootstrap Refresh is about to fire. No-op when the cached
	// snapshot is fresh enough that Run() would skip straight to the
	// daily-tick wait — so the flag never sticks.
	if s.breadth != nil && s.serverCtx != nil {
		s.breadthStarted.Do(func() {
			s.breadth.MarkPendingBootstrap()
			go s.breadth.Run(s.serverCtx)
		})
	}

	// Launch the runtime members refresher alongside the breadth
	// scheduler. The refresher's Run() no-ops immediately when the
	// pin gates fire — keeps the start path uniform regardless of
	// config. Once-guarded so reconnect-driven second postConnectSetup
	// is a no-op rather than spawning a duplicate ticker loop.
	if s.membersRefresher != nil && s.serverCtx != nil {
		s.membersRefresherStarted.Do(func() {
			go s.membersRefresher.Run(s.serverCtx)
		})
	}

	// Prewarm dealer zero-gamma so the first `ibkr regime` / `ibkr
	// gamma` of the day doesn't block on a cold ~4-min compute. The
	// user sees "computing dealer zero-gamma" in `ibkr status`
	// immediately. No periodic loop here — kickOrJoin's soft-TTL
	// machinery handles staleness from RPC handlers thereafter, so we
	// only need the initial kick. The Once gates against
	// reconnect-driven duplicate kicks; later staleness is the soft
	// TTL's job.
	//
	// Called synchronously (no `go`): kickOrJoin only acquires the
	// cache mutex and assigns c.current under it; the multi-minute
	// fan-out runs on the goroutine spawnJob spawns internally. By the
	// time postConnectSetup returns, IsComputing() reflects the
	// in-flight compute — closing the race where the first `ibkr
	// status` after restart would see Connected=true but no background
	// tasks.
	if s.zeroGamma != nil && s.serverCtx != nil {
		s.gammaStarted.Do(func() {
			s.prewarmZeroGamma(s.serverCtx)
		})
	}

	// Latch the postConnectSetup-done barrier. handleStatusHealth gates
	// its Connected field on this so a status RPC arriving in the brief
	// window between c.ready flipping true (set by the connection read
	// loop in pkg/ibkr) and the synchronous sentinel-setting above
	// finishing reports Connected=false — the CLI keeps polling and the
	// first user-visible response shows the full background-task list.
	s.postConnectSetupDone.Store(true)
}

// prewarmZeroGamma kicks the first dealer zero-gamma compute of a
// daemon's lifetime so the first `ibkr regime` / `ibkr gamma` call
// doesn't block on the cold compute. Mirrors handleGammaZeroSPX's
// compute closure construction (connector + normalized default params
// + production leg fetcher) so the prewarm result and a subsequent
// user-triggered compute are interchangeable from the cache's
// perspective.
//
// No sequencing against breadth: with breadth running on a dedicated
// bulk-historical client (separate clientID, separate rate-limit
// budget), the two fan-outs no longer share the primary's 40-msg/sec
// or 60-historical/10-min budgets. Gamma can prewarm concurrently
// with breadth's cold-start refresh and still finish in its normal
// ~20 s wall-clock.
//
// Returns silently on connector unavailability — postConnectSetup
// runs after handshake so the connector is normally ready, but a
// race against immediate disconnect shouldn't crash startup. The
// next user-triggered kickOrJoin will kick the compute instead.
func (s *Server) prewarmZeroGamma(ctx context.Context) {
	c := s.gatewayConnector()
	if c == nil {
		s.logger.Warnf("gamma prewarm: gateway connector unavailable, skipping initial compute")
		return
	}
	s.logger.Infof("gamma prewarm: kicking initial compute (caller=startup, scope=combined)")
	params := normalizeGammaParams(rpc.GammaZeroParams{})
	// Startup prewarm builds the canonical combined cache so the first
	// user-driven `ibkr gamma` (default scope) hits ready instead of
	// kicking another 7-minute fan-out. computeGammaCombined holds
	// SPY then SPX serially via runUnderlyingPhase, and degrades to
	// SPY-only with a structured warning when SPX is unreachable —
	// the same path the handler uses for scope="spy+spx". Refcounted
	// Holds via subManager so a concurrent regime snapshot on either
	// symbol is safe.
	compute := func(bgCtx context.Context, prog *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return computeGammaCombined(bgCtx, s, c, params, prog)
	}
	s.zeroGamma.kickOrJoin(ctx, rpc.GammaZeroScopeCombined, time.Now(), computeETA, compute)
}

// regimeSymbolSeed is the static fallback used by prewarmRegimeSymbols
// when the gateway's reqContractDetails is unresponsive (per-account
// rate limit, post-restart cold state, or any other transient
// silence). These conIDs identify the spot instruments by their IBKR
// contract identity, which is stable across years for spot ETFs/FX/CBOE
// indices — observed unchanged in the daemon log over weeks. A live
// gateway response always wins over a seeded value (the
// SeedContractDetails guard refuses to overwrite a non-zero entry), so
// the seed is purely a fallback for the broken-gateway case. If IBKR
// ever renumbers one of these (extremely rare for spot instruments),
// the only symptom is the regime row's historical-derived field going
// stale until someone updates the constant.
var regimeSymbolSeed = map[string]ibkrlib.ContractDetailsLite{
	"VIX":     {Symbol: "VIX", ConID: 13455763, Exchange: "CBOE", PrimaryExch: "CBOE"},
	"VIX3M":   {Symbol: "VIX3M", ConID: 47511905, Exchange: "CBOE", PrimaryExch: "CBOE"},
	"HYG":     {Symbol: "HYG", ConID: 43652089, Exchange: "SMART", PrimaryExch: "ARCA"},
	"SPY":     {Symbol: "SPY", ConID: 756733, Exchange: "SMART", PrimaryExch: "ARCA"},
	"USD.JPY": {Symbol: "USD", ConID: 15016059, Exchange: "IDEALPRO", PrimaryExch: "IDEALPRO"},
	// SPX anchors the zero-gamma compute (Indicator 4). Without a
	// seeded underlying conID, the compute's first step — fetching
	// the option chain — can stall on the same reqContractDetails
	// silence that blocks the historical fetchers above.
	"SPX": {Symbol: "SPX", ConID: 416904, Exchange: "CBOE", PrimaryExch: "CBOE"},
}

// prewarmRegimeSymbols populates the connector's contract-details
// cache for the verified regime-dashboard symbols so the first
// user-initiated regime call's historical fetches don't depend on a
// fresh reqContractDetails round-trip.
//
// Two layers, applied in order:
//
//  1. Seed each symbol from regimeSymbolSeed (above) — fast and works
//     even when the gateway's reqContractDetails is silent (rate
//     limit, cold state). Without this the seed, a slow / wedged
//     reqContractDetails leaves hyg_50dma and weekly_change_pct
//     missing for the entire daemon lifetime.
//
//  2. Fire FetchContractDetails in parallel anyway. On a healthy
//     gateway the live response overwrites the seed in
//     SeedContractDetails' caller (the cleanup goroutine's "ConID==0
//     can be overwritten" guard ensures live wins over seed). On a
//     degraded gateway the live request times out at 30 s but the
//     seed remains in place, and the regime row still ranks.
//
// USD.JPY normalises to the IDEALPRO FX-pair contract via the same
// classifySymbol path the regime USD/JPY fetcher uses; the seed entry
// is keyed on the dotted-pair form so the look-up matches.
func (s *Server) prewarmRegimeSymbols() {
	// Clear the sentinel on every exit path. The matching Set is in the
	// caller (postConnectSetup) BEFORE the `go` so a status RPC arriving
	// during the goroutine-spawn window sees the in-flight prewarm.
	// The connector nil-return below depends on this defer running.
	defer s.regimePrewarming.Store(false)
	c := s.gatewayConnector()
	if c == nil {
		return
	}
	syms := []string{"VIX", "VIX3M", "HYG", "SPY", "USD.JPY", "SPX"}
	for _, sym := range syms {
		if seed, ok := regimeSymbolSeed[sym]; ok {
			c.SeedContractDetails(sym, seed)
		}
	}
	var wg sync.WaitGroup
	for _, sym := range syms {
		wg.Go(func() {
			// 30 s budget per symbol. Empirically a healthy
			// weekday gateway resolves contract details in well
			// under a second; weekend/frozen state can stretch
			// past 20 s for some symbols (observed: HYG at 21 s
			// post-connect). The pre-warm goroutine is
			// fire-and-forget and doesn't gate any user-facing
			// path, so a generous budget here is cheap and the
			// payoff (first regime call lands without 5-way
			// contract-resolve contention) is real.
			if _, err := c.FetchContractDetails(sym, 30*time.Second); err != nil {
				s.logger.Warnf("regime pre-warm: %s contract details: %v (using seeded conID)", sym, err)
			}
		})
	}
	wg.Wait()
	s.logger.Infof("regime pre-warm: contract cache primed for %v", syms)
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
	var cancel context.CancelFunc
	ctx, cancel = requestCtx(ctx, req.Method)
	defer cancel()
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
	case rpc.MethodBreadthSPX:
		s.unary(req, enc, func() (any, error) { return s.handleBreadthSPX(ctx, req) })
	case rpc.MethodGammaZeroSPX:
		s.unary(req, enc, func() (any, error) { return s.handleGammaZeroSPX(ctx, req) })
	case rpc.MethodRegimeSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleRegimeSnapshot(ctx, req) })
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

// requestCtx returns a derived context with the per-method unary
// deadline applied, plus its cancel func. Streaming methods (deadline
// == 0) pass parent through with a no-op cancel so callers can defer
// uniformly.
func requestCtx(parent context.Context, method string) (context.Context, context.CancelFunc) {
	if d := unaryDeadline(method); d > 0 {
		return context.WithTimeout(parent, d)
	}
	return parent, func() {}
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
		// 50 s = 5 s spot snapshot + 4 waves × ~10 s/wave cold-start. The
		// cold path on a fresh-client-ID TWS session pays per-leg contract-
		// details warmup (2-3 s × first-wave timeouts → retries) on top of
		// the per-leg market-data tick window; observed cold cost on a
		// 14-leg AAPL fetch is 25-30 s and at 25 s the handler is cut off
		// mid-leg-fanout. CLI ceiling is 60 s ([cmd/ibkr/main.go:125]) so
		// 50 s leaves room for the classified daemon error to surface
		// before the socket times out.
		return 50 * time.Second
	case rpc.MethodHistoryDaily, rpc.MethodPositionsList:
		return 30 * time.Second
	case rpc.MethodBreadthSPX:
		// 2 s — handleBreadthSPX is a pure projection of in-memory
		// engine state (Get() + History()) since v0.27.0. No gateway
		// calls happen on this code path; the long-running compute
		// runs on the engine's scheduler goroutine. The 20 s budget
		// this method inherited from the pre-v0.27.0 INDEX-feed
		// implementation papered over a different bug class. A handler
		// taking >2 s here is a sign of mutex contention or a stuck
		// scheduler, not legitimate work.
		return 2 * time.Second
	case rpc.MethodGammaZeroSPX:
		// 55 s — under the CLI's 60 s ceiling but generous enough to
		// support meaningful WaitMs values. The actual compute runs
		// on a daemon-internal goroutine outside this budget; the
		// only thing under the deadline is the snapshot + (optional)
		// wait for the cached result. A caller that sets WaitMs >
		// 50000 ms will get a clean "computing" envelope back once
		// the deadline ticks, not a socket timeout.
		return 55 * time.Second
	case rpc.MethodRegimeSnapshot:
		// 45 s — the regime aggregator fans out 5 fetches concurrently;
		// slowest leg bounds the wall clock. VIX/HYG/SPY/USD-JPY spot
		// snapshots run at 5 s each, HYG's 50-day SMA history pulls in
		// ~10-15 s on a cold cache, gamma returns from its own cache
		// instantly after the first call of the day. 45 s leaves slack
		// for the historical-bars worst-case on first call.
		return 45 * time.Second
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
			if active == 0 && !s.isBusy() {
				s.logger.Infof("Idle timeout reached (%s); shutting down", timeout)
				return
			}
			s.mu.Lock()
			s.idleTimer.Reset(timeout)
			s.mu.Unlock()
		}
	}
}

// backgroundTasks returns the set of daemon-internal long-running
// computes that are running RIGHT NOW. Always returns a non-nil slice
// (possibly empty) so consumers can rely on len()==0 to mean idle.
//
// This is the single source of truth for "what background work is in
// flight." Three callers ride it: isBusy() (idle-watcher gate),
// handleStatusHealth (wire-emitted BackgroundTasks list), and the
// regime partial-envelope path (names the contending task in the
// ErrorMessage). Adding a new long-running task means adding one
// clause here; the three caller surfaces stay coherent automatically.
//
// Distinct from activeConns: those track open client sockets, but the
// engine-style background tasks run without any client connection held
// open. The breadth bootstrap takes ~60 min against IBKR's historical-
// data pacing limit (60 reqs/10 min sliding window) and gamma compute
// runs ~1–2 min — both can outlive a CLI invocation that triggered
// them.
func (s *Server) backgroundTasks() []rpc.BackgroundTaskStatus {
	tasks := []rpc.BackgroundTaskStatus{}
	if s.breadth != nil && s.breadth.IsRefreshing() {
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "breadth-spx"})
	}
	if s.zeroGamma != nil && s.zeroGamma.IsComputing() {
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "gamma-zero"})
	}
	if s.regimePrewarming.Load() {
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "regime-prewarm"})
	}
	return tasks
}

// isBusy reports whether the daemon has daemon-internal background work
// that should defer idle shutdown. Derived from backgroundTasks() so
// the idle watcher and the status surface never disagree about what's
// running.
func (s *Server) isBusy() bool {
	return len(s.backgroundTasks()) > 0
}
