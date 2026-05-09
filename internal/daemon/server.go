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
	"sync"
	"syscall"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"

	"github.com/osauer/ibkr/internal/cache"
	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

// Server is the daemon process state.
type Server struct {
	cfg        *config.Resolved
	socketPath string
	startedAt  time.Time
	version    string

	listener      net.Listener
	connector     *ibkrlib.Connector
	contractCache *cache.JSONCache
	inactive      *cache.InactiveStore

	mu               sync.Mutex
	streams          map[string]context.CancelFunc
	lastConnectError string

	idleTimer   *time.Timer
	idleStop    chan struct{}
	activeConns int

	lock *instanceLock

	logger *Logger
}

// Options configures a Server.
type Options struct {
	Config        *config.Resolved
	SocketPath    string
	Version       string
	ContractCache *cache.JSONCache
	Inactive      *cache.InactiveStore
	Logger        *Logger
}

// New constructs a Server with the supplied options.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = NewLogger(os.Stderr, opts.Config.Daemon.LogLevel)
	}
	return &Server{
		cfg:           opts.Config,
		socketPath:    opts.SocketPath,
		version:       opts.Version,
		contractCache: opts.ContractCache,
		inactive:      opts.Inactive,
		streams:       map[string]context.CancelFunc{},
		idleStop:      make(chan struct{}),
		logger:        opts.Logger,
	}
}

// Start opens the IB Gateway connection, listens on the Unix socket, and
// blocks until ctx is cancelled or Stop is called. Returns the first fatal
// error encountered. Returns ErrAlreadyRunning (without touching the
// gateway) if another ibkrd holds the instance lock for this socket path.
func (s *Server) Start(ctx context.Context) error {
	lock, err := acquireInstanceLock(s.socketPath)
	if err != nil {
		return err
	}
	s.lock = lock

	if err := s.startConnector(ctx); err != nil {
		s.lock.Release()
		s.lock = nil
		return err
	}
	defer s.stopConnector()

	if err := s.openSocket(); err != nil {
		s.lock.Release()
		s.lock = nil
		return err
	}
	s.startedAt = time.Now()
	s.logger.Infof("ibkrd v%s listening on %s (profile=%s, gateway=%s:%d, clientID=%d)",
		s.version, s.socketPath, s.cfg.ProfileName,
		s.cfg.Profile.Host, s.cfg.Profile.Port, s.cfg.Profile.ClientID)

	go s.acceptLoop(ctx)
	s.runIdleWatcher(ctx)

	return nil
}

// Stop closes the listener and IBKR connection. Safe to call multiple times.
// A Server that never reached openSocket (e.g. lock contention exit) must
// not touch the socket file — that would unlink the active peer's socket
// and break the running daemon.
func (s *Server) Stop() {
	s.mu.Lock()
	for _, c := range s.streams {
		c()
	}
	s.streams = map[string]context.CancelFunc{}
	owned := s.listener != nil
	s.mu.Unlock()
	if owned {
		_ = s.listener.Close()
		_ = os.Remove(s.socketPath)
	}
	s.stopConnector()
	if s.contractCache != nil {
		_ = s.contractCache.Flush(context.Background())
	}
	if s.inactive != nil {
		_ = s.inactive.Flush(context.Background())
	}
	if s.lock != nil {
		s.lock.Release()
		s.lock = nil
	}
}

func (s *Server) startConnector(ctx context.Context) error {
	conn := ibkrlib.DefaultConfig()
	conn.Host = s.cfg.Profile.Host
	conn.Port = s.cfg.Profile.Port
	conn.ClientID = s.cfg.Profile.ClientID
	conn.Account = s.cfg.Profile.Account
	conn.UseTLS = s.cfg.Profile.TLS
	// tls=true is a contract: do not silently downgrade. tls=false (the
	// default) keeps fallback so a TLS-only gateway still connects.
	conn.EnableTLSFallback = !s.cfg.Profile.TLS

	pool := ibkrlib.DefaultPoolConfig()
	pool.ClientIDs = []int{s.cfg.Profile.ClientID}
	pool.BaseConfig = conn

	cc := &ibkrlib.ConnectorConfig{
		ServiceName:       "ibkrd",
		PreferredClientID: s.cfg.Profile.ClientID,
		PoolConfig:        pool,
	}
	connector := ibkrlib.NewConnector(cc)

	if err := connector.Start(ctx); err != nil {
		return fmt.Errorf("connect to IB Gateway: %w", err)
	}
	s.connector = connector

	// pkg/ibkr's pool returns success even when the underlying TCP handshake
	// hasn't completed (e.g. gateway unreachable, API socket disabled). Probe
	// IsConnected so we log the truth and surface a hint via status.health.
	if connector.IsConnected() {
		s.logger.Infof("Connected to IB Gateway %s:%d (clientID=%d, tls=%v)",
			s.cfg.Profile.Host, s.cfg.Profile.Port, s.cfg.Profile.ClientID, s.cfg.Profile.TLS)
	} else {
		s.mu.Lock()
		s.lastConnectError = fmt.Sprintf("gateway %s:%d did not complete TWS handshake; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
			s.cfg.Profile.Host, s.cfg.Profile.Port)
		hint := s.lastConnectError
		s.mu.Unlock()
		s.logger.Warnf("Daemon up but gateway not connected: %s", hint)
	}

	// Default to type 2 (frozen-aware): IBKR returns live ticks for entitled
	// symbols during market hours and the last-known close otherwise. Snapshot
	// requests reliably terminate with tickSnapshotEnd this way; pure live
	// (type 1) can leave snapshots hanging when the market is closed.
	if connector.IsConnected() {
		if err := connector.SetMarketDataType(2); err != nil {
			s.logger.Warnf("SetMarketDataType(frozen) failed: %v", err)
		}
		// Start the streaming account+portfolio subscription so position rows
		// carry live mark/value/P&L. Failures here are non-fatal: positions still
		// return correctly-typed rows from the snapshot path, just without marks.
		if err := connector.RequestAccountUpdates(s.cfg.Profile.Account); err != nil {
			s.logger.Warnf("RequestAccountUpdates failed (positions will lack marks): %v", err)
		}
	}
	return nil
}

func (s *Server) stopConnector() {
	if s.connector == nil {
		return
	}
	if err := s.connector.Stop(); err != nil {
		s.logger.Warnf("Connector.Stop: %v", err)
	}
	s.connector = nil
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

func (s *Server) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
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
	r := bufio.NewReaderSize(conn, 64<<10)
	enc := json.NewEncoder(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
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
		s.dispatch(ctx, &req, enc)
	}
}

func (s *Server) dispatch(ctx context.Context, req *rpc.Request, enc *json.Encoder) {
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
	case rpc.MethodHistoryDaily:
		s.unary(req, enc, func() (any, error) { return s.handleHistoryDaily(ctx, req) })
	case rpc.MethodStatusHealth:
		s.unary(req, enc, func() (any, error) { return s.handleStatusHealth(), nil })
	case rpc.MethodQuoteSubscribe:
		s.handleQuoteSubscribe(ctx, req, enc)
	case rpc.MethodOrderPlace:
		_, err := handleOrderPlace(ctx, req)
		writeError(enc, req.ID, rpc.CodeTradingDisabled, err.Error())
	case rpc.MethodOrderCancel:
		_, err := handleOrderCancel(ctx, req)
		writeError(enc, req.ID, rpc.CodeTradingDisabled, err.Error())
	default:
		writeError(enc, req.ID, rpc.CodeUnknownMethod, "unknown method: "+req.Method)
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
	switch {
	case errors.As(err, &bad):
		return rpc.CodeBadRequest, err.Error()
	case errors.Is(err, ibkrlib.ErrSymbolInactive):
		return rpc.CodeSymbolInactive, err.Error()
	case errors.Is(err, ibkrlib.ErrIBKRUnavailable):
		return rpc.CodeGatewayUnavailable, err.Error()
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
	s.idleTimer = time.NewTimer(timeout)
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
				_ = s.listener.Close()
				return
			}
			// Race: client connected between timer fire and lock; reset.
			s.mu.Lock()
			s.idleTimer.Reset(timeout)
			s.mu.Unlock()
		}
	}
}
