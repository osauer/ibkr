// Package ibkr implements the Interactive Brokers TWS API protocol.
//
// TARGET API VERSION: IBKR Java API CLIENT_VERSION 66
// MINIMUM SERVER VERSION REQUIRED: 124 (MIN_SERVER_VER_SYNT_REALTIME_BARS)
// TESTED WITH: TWS API Gateway v10.30+ (serverVersion 203)
//
// This implementation follows the official IBKR Java API specification exactly,
// with NO conditional compatibility code for legacy server versions < 124.
//
// REFERENCE: the canonical IBKR Java client at
// https://github.com/InteractiveBrokers/tws-api-public — specifically
// IBJts/source/JavaClient/com/ib/client/EClient.java. This Go package is
// an independent re-implementation of the wire protocol; no IBKR code is
// included or redistributed here.
//
// Protocol details:
// - Binary message framing for serverVersion >= 100 (4-byte big-endian length prefix)
// - ASCII field encoding with NULL byte (\x00) delimiters
// - Historical data requests include explicit VERSION field (8) to align optional slots
// - ConID field required for serverVersion >= 68 in contract specifications
//
// IMPORTANT: Do NOT add version-specific workarounds without consulting the official
// IBKR API source code. Empirical testing alone can be misleading.
package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/pkg/ibkr/internal/logging"
)

// ConnectionStatus represents the state of an IBKR connection
type ConnectionStatus int

const (
	StatusDisconnected ConnectionStatus = iota
	StatusConnecting
	StatusConnected
	StatusReconnecting
	StatusFailed
)

var (
	errStartAPIFailed  = errors.New("ibkr start api failure")
	errHandshakeNoData = errors.New("ibkr handshake: no response")
	ibkrLogger         = logging.Component("IBKR")
	connectLogger      = logging.Component("IBKR Connect")
	wireLogger         = logging.Component("IBKR Wire")
	handshakeLogger    = logging.Component("IBKR Handshake")
	portfolioLogger    = logging.Component("IBKR Portfolio")
	marketLogger       = logging.Component("IBKR MarketData")
)

func (s ConnectionStatus) String() string {
	switch s {
	case StatusDisconnected:
		return "DISCONNECTED"
	case StatusConnecting:
		return "CONNECTING"
	case StatusConnected:
		return "CONNECTED"
	case StatusReconnecting:
		return "RECONNECTING"
	case StatusFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// ConnectionConfig holds IBKR connection parameters
type ConnectionConfig struct {
	Host     string
	Port     int
	ClientID int
	Account  string

	// PacketLogPath enables the optional packet logger when non-empty. The path
	// may contain a %d placeholder that will be formatted with the client ID by
	// the connection pool.
	PacketLogPath string
	LogWireHex    bool

	// WireInterceptor is an optional shared wire interceptor for the connection.
	// If nil, a new interceptor will be created per connection (legacy behavior).
	// If provided, all connections in the pool share the same interceptor (recommended).
	WireInterceptor *WireInterceptor

	// Client ID retry settings
	MaxClientIDRetries int // Max attempts to find free client ID (default 5)
	ClientIDIncrement  int // How to increment client ID on retry (1=linear, 2=exponential)

	// Reconnection settings (from hedge patterns)
	AutoReconnect     bool
	MaxRetries        int
	InitialDelay      time.Duration // Initial reconnect delay (5s)
	MaxDelay          time.Duration // Max reconnect delay (60s)
	BackoffMultiplier float64       // Exponential backoff multiplier (2.0)
	Jitter            bool          // Add random jitter to delays

	// Connection timeouts
	ConnectTimeout    time.Duration
	HeartbeatInterval time.Duration

	// TLS options
	UseTLS                bool
	EnableTLSFallback     bool
	TLSInsecureSkipVerify bool
	TLSServerName         string
}

// RawPosition represents a position from IBKR
type RawPosition struct {
	Account       string
	Contract      Contract
	Position      float64
	MarketPrice   float64
	MarketValue   float64
	AverageCost   float64
	UnrealizedPNL float64
	RealizedPNL   float64
}

// Contract represents an IBKR contract
type Contract struct {
	ConID        int
	Symbol       string
	SecType      string  // STK, OPT, FUT, etc.
	Expiry       string  // For options/futures
	Strike       float64 // For options
	Right        string  // P or C for options
	Multiplier   int
	Exchange     string
	PrimaryExch  string // Primary exchange for routing
	Currency     string
	LocalSymbol  string
	TradingClass string
	SecIDType    string
	SecID        string
}

// DefaultConfig returns production-ready connection config
func DefaultConfig() *ConnectionConfig {
	return &ConnectionConfig{
		Host:                  "127.0.0.1",
		Port:                  4001, // IB Gateway port
		ClientID:              1,
		MaxClientIDRetries:    5,
		ClientIDIncrement:     1, // Sequential increment (simpler and more predictable)
		AutoReconnect:         true,
		MaxRetries:            10,
		InitialDelay:          5 * time.Second,
		MaxDelay:              60 * time.Second,
		BackoffMultiplier:     2.0,
		Jitter:                true,
		ConnectTimeout:        10 * time.Second,
		HeartbeatInterval:     30 * time.Second,
		UseTLS:                false,
		EnableTLSFallback:     true,
		TLSInsecureSkipVerify: true,
		TLSServerName:         "",
		LogWireHex:            false,
	}
}

func lookupEnvBool(key string) (bool, bool) {
	if val, ok := os.LookupEnv(key); ok {
		s := strings.TrimSpace(strings.ToLower(val))
		switch s {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off", "":
			return false, true
		default:
			return false, true
		}
	}
	return false, false
}

func (c *Connection) tlsAttempts() []bool {
	if c == nil || c.config == nil {
		return []bool{false}
	}
	base := c.config.UseTLS
	seq := []bool{base}
	if c.config.EnableTLSFallback {
		alt := !base
		if alt != base {
			seq = append(seq, alt)
		}
	}
	return seq
}

// Connection represents a single IBKR connection
type Connection struct {
	config   *ConnectionConfig
	status   ConnectionStatus
	statusMu sync.RWMutex

	// Connection state
	connectedAt   time.Time
	lastHeartbeat time.Time
	errorCount    int
	lastError     error

	// Reconnection control
	reconnectChan chan struct{}
	stopChan      chan struct{}
	wg            sync.WaitGroup

	// TCP connection
	conn    net.Conn
	reader  *bufio.Reader
	writer  *bufio.Writer
	scanner *bufio.Scanner

	// Protocol state
	serverVersion  int
	connTime       string
	reqIDSeq       int
	reqIDMu        sync.Mutex
	nextOrderID    int
	account        string
	handshakeMu    sync.RWMutex
	handshakeReady chan struct{}
	useTLS         bool

	// Outbound sequencing to guarantee single-writer semantics per client ID
	transportMu     sync.Mutex
	transportCond   *sync.Cond
	transportPaused bool

	packetLogger       PacketLogger
	packetLoggerMu     sync.RWMutex
	packetLoggerCloser func() error

	wireTap *WireInterceptor

	// Order tracking (scaffold for tests and local state)
	ordersMu    sync.RWMutex
	openOrders  map[int]*IBKROrder
	orderStatus map[int]string

	// Request aliasing (reqID -> metadata) for logging/system notices
	aliasMu  sync.RWMutex
	reqAlias map[int]reqAliasEntry

	logWireHex bool

	// Ensure write path runs serially
	writeMu         sync.Mutex
	writeInProgress atomic.Bool

	// Guard against repeated suspicious logs per symbol/payload.
	suspectMu        sync.Mutex
	suspectFlags     map[string]struct{}
	suspectSummaries map[string]string
	contractTimingMu sync.Mutex
	contractTimings  map[string]time.Duration

	// Read loop coordination so outbound requests wait until reader is ready
	readStartMu sync.Mutex
	readStartCh chan struct{}

	// Callbacks for status changes
	onConnect    func()
	onDisconnect func(error)

	// Message handling
	msgHandlers map[int][]handlerEntry
	handlersMu  sync.RWMutex
	handlerSeq  uint64

	// Market data type per reqID (1=RealTime,2=Frozen,3=Delayed,4=DelayedFrozen)
	mktDataType   map[int]int
	mktDataTypeMu sync.RWMutex

	optionContractMu    sync.RWMutex
	optionContractCache map[string]ContractDetailsLite

	systemNoticeMu      sync.RWMutex
	systemNoticeHandler func(note *systemNotification, alias reqAliasEntry)

	// Competing live session detection (error 10197)
	competingMu          sync.RWMutex
	competingLiveSession bool

	// Portfolio data storage
	positions      map[string]*RawPosition
	positionsMu    sync.RWMutex
	accountSummary map[string]string
	accountMu      sync.RWMutex

	// Completion signals for async operations
	positionsEndChan   chan struct{} // Signals when position sync is complete
	acctSummaryEndChan chan struct{} // Signals when account summary is complete

	// Rate limiting
	rateLimiter *RateLimiter
	ctx         context.Context
	cancel      context.CancelFunc

	// Start API failure tracking for adaptive backoff
	startAPIMu          sync.Mutex
	startAPIFailures    int
	lastStartAPIFailure time.Time
}

type handlerEntry struct {
	id uint64
	fn func([]string)
}

type reqAliasEntry struct {
	symbol       string
	secType      string
	localSymbol  string
	tradingClass string
}

func (c *Connection) registerReqAlias(reqID int, contract Contract) {
	if reqID <= 0 || contract.Symbol == "" {
		return
	}
	entry := reqAliasEntry{
		symbol:       strings.ToUpper(contract.Symbol),
		secType:      strings.ToUpper(contract.SecType),
		localSymbol:  contract.LocalSymbol,
		tradingClass: contract.TradingClass,
	}
	c.aliasMu.Lock()
	c.reqAlias[reqID] = entry
	c.aliasMu.Unlock()
}

func (c *Connection) lookupReqAlias(reqID int) (reqAliasEntry, bool) {
	if reqID <= 0 {
		return reqAliasEntry{}, false
	}
	c.aliasMu.RLock()
	alias, ok := c.reqAlias[reqID]
	c.aliasMu.RUnlock()
	return alias, ok
}

func (c *Connection) SetSystemNoticeHandler(handler func(note *systemNotification, alias reqAliasEntry)) {
	c.systemNoticeMu.Lock()
	c.systemNoticeHandler = handler
	c.systemNoticeMu.Unlock()
}

func (c *Connection) dispatchSystemNotice(note *systemNotification, alias reqAliasEntry) {
	c.systemNoticeMu.RLock()
	handler := c.systemNoticeHandler
	c.systemNoticeMu.RUnlock()
	if handler != nil {
		handler(note, alias)
	}
}

func (c *Connection) resetReadStartCh() {
	c.readStartMu.Lock()
	c.readStartCh = make(chan struct{})
	c.readStartMu.Unlock()
}

func (c *Connection) signalReadStarted() {
	c.readStartMu.Lock()
	ch := c.readStartCh
	c.readStartMu.Unlock()
	if ch != nil {
		select {
		case <-ch:
			// already closed
		default:
			close(ch)
		}
	}
}

func (c *Connection) waitForReadStart(timeout time.Duration) {
	c.readStartMu.Lock()
	ch := c.readStartCh
	c.readStartMu.Unlock()
	if ch == nil {
		return
	}
	if timeout <= 0 {
		<-ch
		return
	}
	select {
	case <-ch:
	case <-time.After(timeout):
		connectLogger.Warnf("Client %d: read loop start wait timed out after %s", c.config.ClientID, timeout)
	}
}

// NewConnection creates a new IBKR connection
func NewConnection(config *ConnectionConfig) *Connection {
	if config == nil {
		config = DefaultConfig()
	} else {
		// Fill in missing timeouts/intervals with safe defaults to avoid zero-value panics
		def := DefaultConfig()
		if config.ConnectTimeout == 0 {
			config.ConnectTimeout = def.ConnectTimeout
		}
		if config.HeartbeatInterval == 0 {
			config.HeartbeatInterval = def.HeartbeatInterval
		}
		if config.MaxClientIDRetries == 0 {
			config.MaxClientIDRetries = def.MaxClientIDRetries
		}
		if config.ClientIDIncrement == 0 {
			config.ClientIDIncrement = def.ClientIDIncrement
		}
		if config.MaxRetries == 0 {
			config.MaxRetries = def.MaxRetries
		}
		if config.InitialDelay == 0 {
			config.InitialDelay = def.InitialDelay
		}
		if config.MaxDelay == 0 {
			config.MaxDelay = def.MaxDelay
		}
		if config.BackoffMultiplier == 0 {
			config.BackoffMultiplier = def.BackoffMultiplier
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	conn := &Connection{
		config:              config,
		status:              StatusDisconnected,
		reconnectChan:       make(chan struct{}, 1),
		stopChan:            make(chan struct{}),
		msgHandlers:         make(map[int][]handlerEntry),
		mktDataType:         make(map[int]int),
		positions:           make(map[string]*RawPosition),
		accountSummary:      make(map[string]string),
		reqIDSeq:            1,
		openOrders:          make(map[int]*IBKROrder),
		orderStatus:         make(map[int]string),
		reqAlias:            make(map[int]reqAliasEntry),
		logWireHex:          config.LogWireHex,
		suspectFlags:        make(map[string]struct{}),
		suspectSummaries:    make(map[string]string),
		contractTimings:     make(map[string]time.Duration),
		optionContractCache: make(map[string]ContractDetailsLite),
		readStartCh:         make(chan struct{}),
		ctx:                 ctx,
		cancel:              cancel,
		rateLimiter:         NewRateLimiter(ctx),
		positionsEndChan:    make(chan struct{}, 1),
		acctSummaryEndChan:  make(chan struct{}, 1),
		serverVersion:       0,
		useTLS:              config.UseTLS,
	}
	if val, ok := lookupEnvBool("IBKR_LOG_WIRE_HEX"); ok {
		conn.logWireHex = val
	}

	// Use shared wire interceptor if provided, otherwise create per-connection (legacy)
	if config.WireInterceptor != nil {
		conn.wireTap = config.WireInterceptor
	} else if interceptor, err := NewWireInterceptorFromEnv(config.ClientID); err != nil {
		wireLogger.Warnf("Client %d: failed to initialize wire interceptor: %v", config.ClientID, err)
		ibkrLogger.Warnf("[WIRE] Client %d: failed to initialize wire interceptor: %v", config.ClientID, err)
	} else {
		conn.wireTap = interceptor
	}

	conn.transportCond = sync.NewCond(&conn.transportMu)
	conn.resetHandshakeReady()

	if config.PacketLogPath != "" {
		path := config.PacketLogPath
		if strings.Contains(path, "%d") {
			path = fmt.Sprintf(path, config.ClientID)
		}
		if logger, err := NewHexPacketLogger(path); err != nil {
			ibkrLogger.Warnf("Client %d: failed to initialize packet logger: %v", config.ClientID, err)
		} else {
			conn.SetPacketLogger(logger)
			config.PacketLogPath = path
		}
	}

	return conn
}

func (c *Connection) dialEndpoint(ctx context.Context, useTLS bool) (net.Conn, error) {
	addr := fmt.Sprintf("%s:%d", c.config.Host, c.config.Port)
	dialer := net.Dialer{Timeout: c.config.ConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to IBKR at %s: %w", addr, err)
	}

	// Disable Nagle's algorithm (TCP_NODELAY) to ensure immediate transmission.
	// Without this, small packets (like reqContractData ~40 bytes) are buffered by TCP,
	// causing IBKR to receive nothing until buffer fills or connection closes.
	// This was causing "Invalid incoming request type" errors when disconnecting.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to set TCP_NODELAY: %w", err)
		}
	}

	if !useTLS {
		return conn, nil
	}
	tlsCfg := &tls.Config{
		InsecureSkipVerify: c.config.TLSInsecureSkipVerify,
	}
	serverName := c.config.TLSServerName
	if serverName == "" && !c.config.TLSInsecureSkipVerify {
		serverName = c.config.Host
	}
	if serverName != "" {
		tlsCfg.ServerName = serverName
	}
	tlsConn := tls.Client(conn, tlsCfg)
	// HandshakeContext (Go 1.17+) makes the TLS handshake honor ctx
	// cancellation. The plain Handshake() variant blocks on the read
	// from the server until the kernel TCP timeout fires — so a wedged
	// server that accepts TCP but never replies to ClientHello would
	// leave the goroutine alive past Server.Stop and the daemon's idle
	// timer. With HandshakeContext, ctx-cancel during the handshake
	// closes the underlying conn and returns promptly. (Issue #3 AC#4.)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tls handshake failed: %w", err)
	}
	return tlsConn, nil
}

func (c *Connection) closeConnection() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.reader = nil
	c.writer = nil
	c.scanner = nil
}

// SetPacketLogger installs a packet logger invoked for every outbound frame.
// Passing nil disables logging. Intended for short-lived debugging sessions.
func (c *Connection) SetPacketLogger(logger PacketLogger) {
	c.packetLoggerMu.Lock()
	if c.packetLoggerCloser != nil {
		if err := c.packetLoggerCloser(); err != nil {
			ibkrLogger.Warnf("packet logger close error: %v", err)
		}
		c.packetLoggerCloser = nil
	}
	c.packetLogger = logger
	if logger != nil {
		if closer, ok := logger.(interface{ Close() error }); ok {
			c.packetLoggerCloser = closer.Close
		}
	}
	c.packetLoggerMu.Unlock()
}

func (c *Connection) ensurePacketLogger() {
	if c.config == nil || c.config.PacketLogPath == "" {
		return
	}
	c.packetLoggerMu.RLock()
	loggerPresent := c.packetLogger != nil
	c.packetLoggerMu.RUnlock()
	if loggerPresent {
		return
	}
	logger, err := NewHexPacketLogger(c.config.PacketLogPath)
	if err != nil {
		ibkrLogger.Warnf("Client %d: unable to open packet logger: %v", c.config.ClientID, err)
		return
	}
	c.SetPacketLogger(logger)
}

func isClientIDInUseError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "error code 326") ||
		strings.Contains(msg, "client id is already in use") ||
		strings.Contains(msg, "unable to connect as the client id")
}

// Connect establishes connection to IBKR Gateway with automatic client ID retry
func (c *Connection) Connect(ctx context.Context) error {
	originalClientID := c.config.ClientID
	currentClientID := originalClientID

	// Limit to 5 retries max
	maxRetries := c.config.MaxClientIDRetries
	if maxRetries > 5 {
		maxRetries = 5
	}
	if maxRetries < 1 {
		maxRetries = 1
	}

	connectLogger.Infof("Starting connection process with Client ID %d, MaxRetries=%d",
		originalClientID, maxRetries)

	for attempt := 0; attempt < maxRetries; attempt++ {
		c.config.ClientID = currentClientID
		connectLogger.Infof("Attempting connection with Client ID %d (attempt %d/%d)",
			currentClientID, attempt+1, maxRetries)

		err := c.connectWithClientID(ctx)
		if err == nil {
			// Success!
			if currentClientID != originalClientID {
				connectLogger.Infof("Successfully connected with Client ID %d (original was %d)",
					currentClientID, originalClientID)
			}
			return nil
		}

		if errors.Is(err, errStartAPIFailed) {
			connectLogger.Warnf("startAPI failed for Client ID %d; retrying", currentClientID)
			continue
		}

		// Check if error is client ID related (error 326)
		// Match various forms of the client ID error message
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "error code 326") ||
			strings.Contains(errStr, "client id is already in use") ||
			strings.Contains(errStr, "client id already in use") ||
			strings.Contains(errStr, "unable to connect as client id") {

			// Client ID in use, try next sequential ID
			prevClientID := currentClientID
			currentClientID++

			// IBKR has a max client ID limit (typically 999)
			// Wrap around to 1 if we exceed it, but skip the original ID
			if currentClientID > 999 {
				currentClientID = 1
			}
			// Skip original ID if we wrapped around to it
			if currentClientID == originalClientID && originalClientID < 999 {
				currentClientID++
			}

			connectLogger.Warnf("Client ID %d already in use, trying next: %d",
				prevClientID, currentClientID)
		} else {
			// Non-client ID error, return immediately
			connectLogger.Errorf("Connection failed with non-client ID error: %v", err)
			return err
		}
	}

	return fmt.Errorf("failed to connect after %d attempts, all client IDs tried were in use", maxRetries)
}

// connectWithClientID attempts connection with specific client ID
func (c *Connection) connectWithClientID(ctx context.Context) error {
	c.setStatus(StatusConnecting)
	c.ensurePacketLogger()

	attempts := c.tlsAttempts()
	var lastErr error

	for idx, useTLS := range attempts {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if idx > 0 {
			connectLogger.Warnf("Client %d: retrying with tls=%v after error: %v", c.config.ClientID, useTLS, lastErr)
		}

		c.pauseTransport()
		c.resetHandshakeReady()

		if err := c.connectAttempt(ctx, useTLS); err != nil {
			lastErr = err
			c.closeConnection()
			c.resumeTransport()
			if ctx != nil {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
			}
			if errors.Is(err, errHandshakeNoData) && idx+1 < len(attempts) {
				connectLogger.Warnf("Client %d: handshake returned no data (tls=%v); attempting fallback", c.config.ClientID, useTLS)
				continue
			}
			return err
		}

		return nil
	}

	if lastErr != nil {
		return lastErr
	}

	return fmt.Errorf("failed to connect to IBKR (client %d)", c.config.ClientID)
}

func (c *Connection) connectAttempt(ctx context.Context, useTLS bool) error {
	addr := fmt.Sprintf("%s:%d", c.config.Host, c.config.Port)
	connectLogger.Infof("Client %d: Connecting to %s (tls=%v)...", c.config.ClientID, addr, useTLS)

	netConn, err := c.dialEndpoint(ctx, useTLS)
	if err != nil {
		c.setStatus(StatusDisconnected)
		return err
	}

	var cancelOnce sync.Once
	var cancelWatcherDone chan struct{}
	if ctx != nil {
		if cerr := ctx.Err(); cerr != nil {
			_ = netConn.Close()
			return cerr
		}
		cancelWatcherDone = make(chan struct{})
		go func(conn net.Conn, done <-chan struct{}, watchCtx context.Context) {
			select {
			case <-watchCtx.Done():
				cancelOnce.Do(func() { _ = conn.Close() })
			case <-done:
			}
		}(netConn, cancelWatcherDone, ctx)
	}
	defer func() {
		if cancelWatcherDone != nil {
			close(cancelWatcherDone)
		}
	}()

	c.conn = netConn
	c.reader = bufio.NewReader(netConn)
	c.writer = bufio.NewWriter(netConn)

	c.scanner = bufio.NewScanner(netConn)
	c.scanner.Split(c.scanMessages)
	c.scanner.Buffer(make([]byte, 4096), 1024*1024) // 1MB max message

	connectLogger.Infof("Client %d: Starting handshake...", c.config.ClientID)
	if err := c.handshake(); err != nil {
		connectLogger.Errorf("Client %d: Handshake failed: %v", c.config.ClientID, err)
		c.setStatus(StatusDisconnected)
		cancelOnce.Do(func() { _ = netConn.Close() })
		return fmt.Errorf("handshake failed: %w", err)
	}
	connectLogger.Infof("Client %d: Handshake successful (serverVersion=%d)", c.config.ClientID, c.serverVersion)

	connectLogger.Infof("Client %d: Starting API...", c.config.ClientID)
	if err := c.startAPI(); err != nil {
		if isClientIDInUseError(err) {
			connectLogger.Warnf("Client %d: startAPI rejected client ID in use: %v", c.config.ClientID, err)
			c.setStatus(StatusDisconnected)
			return err
		}
		delay := c.registerStartAPIFailure()
		connectLogger.Warnf("Client %d: Failed to start API: %v (backing off %s)", c.config.ClientID, err, delay)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return fmt.Errorf("%w: context cancelled during startAPI backoff: %w", errStartAPIFailed, ctx.Err())
			}
		}
		return fmt.Errorf("%w: %v", errStartAPIFailed, err)
	}
	connectLogger.Infof("Client %d: API started successfully", c.config.ClientID)
	c.resetStartAPIFailure()
	c.resumeTransport()

	c.statusMu.Lock()
	c.status = StatusConnected
	c.connectedAt = time.Now()
	c.lastHeartbeat = time.Now()
	c.errorCount = 0
	c.lastError = nil
	c.statusMu.Unlock()

	c.useTLS = useTLS

	connectLogger.Infof("Connection established (Client ID: %d, Server Version: %d, tls=%v)", c.config.ClientID, c.serverVersion, c.useTLS)

	c.resetReadStartCh()
	c.wg.Add(2)
	go c.heartbeatMonitor()
	go c.readMessages()
	c.waitForReadStart(500 * time.Millisecond)
	c.signalHandshakeReady()

	if c.onConnect != nil {
		c.onConnect()
	}

	return nil
}

// Disconnect closes the IBKR connection
func (c *Connection) Disconnect() error {
	c.statusMu.Lock()
	if c.status != StatusConnected {
		c.statusMu.Unlock()
		return nil
	}
	c.status = StatusDisconnected
	c.statusMu.Unlock()

	// Signal shutdown first - this stops new requests from being queued
	close(c.stopChan)

	// Stop the rate limiter - this drains the queue and waits for in-flight requests
	if c.rateLimiter != nil {
		c.rateLimiter.Stop()
	}

	// DO NOT flush the writer buffer here! There may be partial message data
	// from a write that was interrupted. TCP close will cleanly discard buffered data.
	// Flushing partial messages causes "Invalid incoming request type" errors at IBKR.
	// The transportMu lock in withTransport() ensures completed messages are already flushed.

	// Cancel context
	if c.cancel != nil {
		c.cancel()
	}

	// Wait for goroutines to finish
	c.wg.Wait()

	// Close TCP connection - safe now that all writes are complete
	if c.conn != nil {
		c.conn.Close()
	}

	connectLogger.Infof("Connection closed (Client ID: %d)", c.config.ClientID)

	if c.onDisconnect != nil {
		c.onDisconnect(nil)
	}

	if c.wireTap != nil {
		_ = c.wireTap.Close()
	}

	c.SetPacketLogger(nil)

	return nil
}

// reconnectWithBackoff implements exponential backoff reconnection
// Pattern adapted from hedge's connection_pool.py
func (c *Connection) reconnectWithBackoff(ctx context.Context) {
	defer c.wg.Done()

	attempt := 0

	for {
		select {
		case <-c.stopChan:
			return
		case <-c.reconnectChan:
			// Reset attempt counter on new reconnect request
			attempt = 0
		case <-time.After(c.calculateBackoff(attempt)):
			if attempt >= c.config.MaxRetries {
				c.setStatus(StatusFailed)
				connectLogger.Errorf("Reconnection failed after %d attempts", attempt)
				return
			}

			attempt++
			connectLogger.Warnf("Reconnection attempt %d/%d (Client ID: %d)",
				attempt, c.config.MaxRetries, c.config.ClientID)

			c.setStatus(StatusReconnecting)

			connectCtx, cancel := context.WithTimeout(ctx, c.config.ConnectTimeout)
			err := c.Connect(connectCtx)
			cancel()

			if err == nil {
				connectLogger.Infof("Reconnection successful (Client ID: %d)", c.config.ClientID)
				return
			}

			c.statusMu.Lock()
			c.errorCount++
			c.lastError = err
			c.statusMu.Unlock()
		}
	}
}

// calculateBackoff calculates delay with exponential backoff and optional jitter
func (c *Connection) calculateBackoff(attempt int) time.Duration {
	if attempt == 0 {
		return 0
	}

	// Calculate exponential backoff
	delay := float64(c.config.InitialDelay) * math.Pow(c.config.BackoffMultiplier, float64(attempt-1))

	// Cap at max delay
	if delay > float64(c.config.MaxDelay) {
		delay = float64(c.config.MaxDelay)
	}

	// Add jitter if enabled (±10% randomization)
	if c.config.Jitter {
		jitter := delay * 0.1 * (rand.Float64()*2 - 1)
		delay += jitter
	}

	return time.Duration(delay)
}

// heartbeatMonitor checks connection health periodically
func (c *Connection) heartbeatMonitor() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			c.statusMu.RLock()
			status := c.status
			lastHeartbeat := c.lastHeartbeat
			c.statusMu.RUnlock()

			if status != StatusConnected {
				continue
			}

			// Check if heartbeat is stale (no response for 2x interval)
			if time.Since(lastHeartbeat) > c.config.HeartbeatInterval*2 {
				connectLogger.Warnf("Heartbeat timeout (Client ID: %d)", c.config.ClientID)
				c.handleDisconnection(fmt.Errorf("heartbeat timeout"))
				return
			}

			// Send heartbeat request to IBKR
			if err := c.RequestCurrentTime(); err != nil {
				connectLogger.Warnf("Failed to send heartbeat: %v", err)
				// Don't disconnect immediately on heartbeat failure,
				// let the timeout mechanism handle it
			} else {
				// Update heartbeat timestamp on successful send
				c.statusMu.Lock()
				c.lastHeartbeat = time.Now()
				c.statusMu.Unlock()
			}
		}
	}
}

// handleDisconnection triggers reconnection if auto-reconnect is enabled
func (c *Connection) handleDisconnection(err error) {
	c.statusMu.Lock()
	if c.status != StatusConnected {
		c.statusMu.Unlock()
		return
	}
	c.status = StatusDisconnected
	c.lastError = err
	c.statusMu.Unlock()
	c.pauseTransport()

	connectLogger.Warnf("Disconnection detected (Client ID: %d): %v", c.config.ClientID, err)

	if c.onDisconnect != nil {
		c.onDisconnect(err)
	}

	if c.config.AutoReconnect {
		select {
		case c.reconnectChan <- struct{}{}:
			c.wg.Add(1)
			go c.reconnectWithBackoff(context.Background())
		default:
			// Reconnection already in progress
		}
	}
}

// Status returns the current connection status
func (c *Connection) Status() ConnectionStatus {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

// IsConnected returns true if connected
func (c *Connection) IsConnected() bool {
	return c.Status() == StatusConnected
}

// setStatus updates connection status safely
func (c *Connection) setStatus(status ConnectionStatus) {
	c.statusMu.Lock()
	c.status = status
	c.statusMu.Unlock()
}

// SetOnConnect sets callback for successful connections
func (c *Connection) SetOnConnect(fn func()) {
	c.onConnect = fn
}

// SetOnDisconnect sets callback for disconnections
func (c *Connection) SetOnDisconnect(fn func(error)) {
	c.onDisconnect = fn
}

// GetConnectionInfo returns current connection details
func (c *Connection) GetConnectionInfo() map[string]interface{} {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()

	info := map[string]interface{}{
		"client_id":      c.config.ClientID,
		"host":           c.config.Host,
		"port":           c.config.Port,
		"status":         c.status.String(),
		"error_count":    c.errorCount,
		"connected_at":   c.connectedAt,
		"last_heartbeat": c.lastHeartbeat,
		"server_version": c.serverVersion,
	}

	if c.lastError != nil {
		info["last_error"] = c.lastError.Error()
	}

	return info
}

func (c *Connection) ServerVersion() int {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.serverVersion
}

// UsingTLS reports whether the established session negotiated TLS. When
// EnableTLSFallback flips the configured value, this exposes the actual mode.
func (c *Connection) UsingTLS() bool {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.useTLS
}

// Protocol constants aligned with IBKR CLIENT_VERSION 66
const (
	// Handshake advertises compatibility starting at client version 100
	minClientVersion = 100

	// Minimum server version we accept: 124 = MIN_SERVER_VER_SYNT_REALTIME_BARS
	minServerVersionRequired = 124

	// Maximum tested version (TWS API Gateway v10.30+)
	maxClientVersion = 203

	// Version 203 = MIN_SERVER_VER_PROTOBUF_PLACE_ORDER
	// Required for protobuf-encoded placeOrder messages
	minServerVerProtoBufPlaceOrder = 203

	minServerVerExecutionDataChain         = 42
	minServerVerModelsSupport              = 103
	minServerVerLastLiquidity              = 136
	minServerVerPendingPriceRevision       = 178
	minServerVerSubmitter                  = 198
	minServerVerParametrizedDaysExecutions = 200

	// Required for startApi optional capabilities
	minServerVerStartAPICapab = 72

	// IBKR protobuf-encoded inbound messages use an offset of 200
	protoMsgOffset = 200

	// Message IDs from IBKR protocol
	msgTickPrice                              = 1
	msgTickSize                               = 2
	msgOrderStatus                            = 3
	msgErrMsg                                 = 4
	msgOpenOrder                              = 5
	msgAcctValue                              = 6
	msgPortfolioValue                         = 7
	msgAcctUpdateTime                         = 8
	msgNextValidID                            = 9
	msgContractData                           = 10
	msgExecutionData                          = 11
	msgMarketDepth                            = 12
	msgMarketDepthL2                          = 13
	msgNewsBulletins                          = 14
	msgManagedAccts                           = 15
	msgReceiveFA                              = 16
	msgHistoricalData                         = 17
	msgHistoricalDataEnd                      = 108
	msgCurrentTimeMillis                      = 109
	msgBondContractData                       = 18
	msgScannerParameters                      = 19
	msgScannerData                            = 20
	msgTickOptionComputation                  = 21
	msgTickGeneric                            = 45
	msgTickString                             = 46
	msgTickEFP                                = 47
	msgCurrentTime                            = 49
	msgRealTimeBars                           = 50
	msgFundamentalData                        = 51
	msgContractDataEnd                        = 52
	msgOpenOrderEnd                           = 53
	msgAcctDownloadEnd                        = 54
	msgExecDetailsEnd                         = 55
	msgDeltaNeutralValidation                 = 56
	msgTickSnapshotEnd                        = 57
	msgMarketDataType                         = 58
	msgCommissionReport                       = 59
	msgPosition                               = 61
	msgPositionEnd                            = 62
	msgAccountSummary                         = 63
	msgAccountSummaryEnd                      = 64
	msgVerifyMessageAPI                       = 65
	msgVerifyCompleted                        = 66
	msgDisplayGroupList                       = 67
	msgDisplayGroupUpdated                    = 68
	msgVerifyAndAuthMessageAPI                = 69
	msgVerifyAndAuthCompleted                 = 70
	msgPositionMulti                          = 71
	msgPositionMultiEnd                       = 72
	msgAccountUpdateMulti                     = 73
	msgAccountUpdateMultiEnd                  = 74
	msgSecurityDefinitionOptionalParameter    = 75
	msgSecurityDefinitionOptionalParameterEnd = 76
	msgSoftDollarTiers                        = 77
	msgFamilyCodes                            = 78
	msgSymbolSamples                          = 79
	msgMktDepthExchanges                      = 80
	msgTickNews                               = 81
	msgSmartComponents                        = 82
	msgTickReqParams                          = 83
	msgNewsProviders                          = 84
	msgNewsArticle                            = 85
	msgHistoricalNews                         = 86
	msgHistoricalNewsEnd                      = 87
	msgHeadTimestamp                          = 88
	msgHistogramData                          = 89
	msgHistoricalDataUpdate                   = 90
	msgRerouteMktDataReq                      = 91
	msgRerouteMktDepthReq                     = 92
	msgMarketRule                             = 93
	msgPnL                                    = 94
	msgPnLSingle                              = 95
	msgHistoricalTicks                        = 96
	msgHistoricalTicksBidAsk                  = 97
	msgHistoricalTicksLast                    = 98
	msgTickByTick                             = 99
	msgOrderBound                             = 100
	msgSystemNotification                     = 204
	msgExecutionRequestAck                    = 255 // Observed ack for reqExecutions with no payload (gateway v10.30+)

	// Outgoing message IDs
	reqMktData                  = 1
	cancelMktData               = 2
	placeOrder                  = 3
	cancelOrder                 = 4
	reqOpenOrders               = 5
	reqAcctData                 = 6
	reqExecutions               = 7
	reqIds                      = 8
	reqContractData             = 9
	reqMktDepth                 = 10
	cancelMktDepth              = 11
	reqNewsBulletins            = 12
	cancelNewsBulletins         = 13
	setServerLogLevel           = 14
	reqAutoOpenOrders           = 15
	reqAllOpenOrders            = 16
	reqManagedAccts             = 17
	reqFA                       = 18
	replaceFA                   = 19
	reqHistoricalData           = 20
	exerciseOptions             = 21
	reqScannerSubscription      = 22
	cancelScannerSubscription   = 23
	reqScannerParameters        = 24
	cancelHistoricalData        = 25
	reqCurrentTime              = 49
	reqRealTimeBars             = 50
	cancelRealTimeBars          = 51
	reqFundamentalData          = 52
	cancelFundamentalData       = 53
	reqCalcImpliedVolatility    = 54
	reqCalcOptionPrice          = 55
	cancelCalcImpliedVolatility = 56
	cancelCalcOptionPrice       = 57
	reqGlobalCancel             = 58
	reqMarketDataType           = 59
	reqPositions                = 61
	reqAccountSummary           = 62
	cancelAccountSummary        = 63
	cancelPositions             = 64
	verifyRequest               = 65
	verifyMessage               = 66
	queryDisplayGroups          = 67
	subscribeToGroupEvents      = 68
	updateDisplayGroup          = 69
	unsubscribeFromGroupEvents  = 70
	startAPI                    = 71
	reqSecDefOptParams          = 78
)

var placeOrderBaseFields = []string{
	"3", "0", "0", "", "", "", "0.0", "", "", "SMART", "", "USD", "", "", "", "", "BUY", "0", "LMT", "0", "", "DAY", "", "", "", "0", "", "1", "0", "0", "0", "0", "0", "0", "0", "", "0", "", "", "", "", "", "", "", "0", "", "-1", "0", "", "", "0", "", "", "1", "1", "", "0", "", "", "", "", "", "0", "", "", "", "", "0", "", "", "", "", "", "", "", "", "", "", "0", "", "", "0", "0", "", "", "0", "", "0", "0", "0", "0", "", "", "", "", "", "", "0", "", "", "", "", "0", "0", "0", "", ""}

// handshake performs the initial IBKR protocol handshake
func (c *Connection) handshake() error {
	attemptPayloads := []string{
		fmt.Sprintf("v%d..%d", minClientVersion, maxClientVersion),
		fmt.Sprintf("v%d", maxClientVersion),
	}

	var sawNoData bool

	for idx, payload := range attemptPayloads {
		if err := c.sendHandshakePayload(payload); err != nil {
			return fmt.Errorf("failed to send handshake payload %q: %w", payload, err)
		}

		err := c.readHandshakeResponse()
		if err == nil {
			return nil
		}
		if errors.Is(err, errHandshakeNoData) {
			sawNoData = true
			handshakeLogger.Warnf("Client %d: no response to payload %q (attempt %d/%d)", c.config.ClientID, payload, idx+1, len(attemptPayloads))
			continue
		}
		return err
	}

	if sawNoData {
		return fmt.Errorf("%w: no response from IBKR gateway after %d attempts", errHandshakeNoData, len(attemptPayloads))
	}

	return fmt.Errorf("handshake failed: no valid response format detected")
}

func (c *Connection) sendHandshakePayload(versionDescriptor string) error {
	descriptorBytes := append([]byte(versionDescriptor), '\x00')
	var lengthBuf [4]byte
	binary.BigEndian.PutUint32(lengthBuf[:], uint32(len(descriptorBytes)))

	var frame bytes.Buffer
	frame.Grow(4 + len(lengthBuf) + len(descriptorBytes))
	frame.WriteString("API\x00")
	frame.Write(lengthBuf[:])
	frame.Write(descriptorBytes)

	handshakeLogger.Infof("Client %d: sending descriptor %q", c.config.ClientID, versionDescriptor)
	return c.withTransport(true, func() error {
		_, err := c.conn.Write(frame.Bytes())
		return err
	})
}

func (c *Connection) readHandshakeResponse() error {
	const handshakeDeadline = 10 * time.Second
	if err := c.conn.SetReadDeadline(time.Now().Add(handshakeDeadline)); err != nil {
		return fmt.Errorf("handshake set deadline: %w", err)
	}
	defer c.conn.SetReadDeadline(time.Time{})

	head, err := c.reader.Peek(4)
	if err != nil {
		if isHandshakeNoDataErr(err) {
			return errHandshakeNoData
		}
		return fmt.Errorf("handshake peek failed: %w", err)
	}

	first := head[0]
	if first == '-' || (first >= '0' && first <= '9') {
		return c.readAsciiHandshake()
	}
	return c.readLengthPrefixedHandshake()
}

func (c *Connection) readLengthPrefixedHandshake() error {
	var lengthBuf [4]byte
	if _, err := io.ReadFull(c.reader, lengthBuf[:]); err != nil {
		if isHandshakeNoDataErr(err) {
			return errHandshakeNoData
		}
		return fmt.Errorf("handshake read frame length: %w", err)
	}

	frameLen := int(binary.BigEndian.Uint32(lengthBuf[:]))
	if frameLen == 0 {
		return errHandshakeNoData
	}
	if frameLen < 0 || frameLen > 4096 {
		return fmt.Errorf("handshake frame length out of bounds: %d", frameLen)
	}

	payload := make([]byte, frameLen)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		if isHandshakeNoDataErr(err) {
			return errHandshakeNoData
		}
		return fmt.Errorf("handshake read frame payload: %w", err)
	}

	fieldsRaw := bytes.Split(payload, []byte{0})
	fields := make([]string, 0, len(fieldsRaw))
	for i, raw := range fieldsRaw {
		// Drop a trailing empty field if the payload ended with a null delimiter
		if i == len(fieldsRaw)-1 && len(raw) == 0 {
			continue
		}
		fields = append(fields, string(raw))
	}

	if len(fields) == 0 || fields[0] == "" {
		return errHandshakeNoData
	}

	serverVersion, err := strconv.Atoi(fields[0])
	if err != nil {
		return fmt.Errorf("invalid server version string %q: %w", fields[0], err)
	}

	if serverVersion == -1 {
		if len(fields) < 2 {
			return fmt.Errorf("handshake redirect requested but no target provided")
		}
		return fmt.Errorf("handshake redirect requested: %s", fields[1])
	}

	connTime := ""
	if serverVersion >= 20 {
		if len(fields) >= 2 {
			connTime = fields[1]
		} else {
			handshakeLogger.Warnf("Client %d: server version %d provided no time string", c.config.ClientID, serverVersion)
		}
	}

	if serverVersion < minServerVersionRequired {
		return fmt.Errorf("server version %d is too old (minimum: %d)", serverVersion, minServerVersionRequired)
	}

	c.serverVersion = serverVersion
	c.connTime = connTime
	handshakeLogger.Infof("Client %d: Server Version %d, Time %s (v100 frame)", c.config.ClientID, c.serverVersion, c.connTime)
	return nil
}

func (c *Connection) readAsciiHandshake() error {
	verStr, err := c.readHandshakeCString()
	if err != nil {
		if isHandshakeNoDataErr(err) {
			return errHandshakeNoData
		}
		return fmt.Errorf("handshake read version string: %w", err)
	}
	if verStr == "" {
		return errHandshakeNoData
	}

	serverVersion, err := strconv.Atoi(verStr)
	if err != nil {
		return fmt.Errorf("invalid server version string %q: %w", verStr, err)
	}

	if serverVersion == -1 {
		redirect, err := c.readHandshakeCString()
		if err != nil {
			return fmt.Errorf("handshake read redirect target: %w", err)
		}
		return fmt.Errorf("handshake redirect requested: %s", redirect)
	}

	connTime := ""
	if serverVersion >= 20 {
		timeStr, err := c.readHandshakeCString()
		if err != nil {
			if !isHandshakeNoDataErr(err) {
				return fmt.Errorf("handshake read time: %w", err)
			}
		} else {
			connTime = timeStr
		}
	}

	if serverVersion < minServerVersionRequired {
		return fmt.Errorf("server version %d is too old (minimum: %d)", serverVersion, minServerVersionRequired)
	}

	c.serverVersion = serverVersion
	c.connTime = connTime
	handshakeLogger.Infof("Client %d: Server Version %d, Time %s (ascii)", c.config.ClientID, c.serverVersion, c.connTime)
	return nil
}

func (c *Connection) readHandshakeCString() (string, error) {
	data, err := c.reader.ReadString('\x00')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(data, "\x00"), nil
}

func isHandshakeNoDataErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// startAPI sends the start API message to initialize the connection
func (c *Connection) startAPI() error {
	fields := []interface{}{startAPI, 2, c.config.ClientID}
	if c.serverVersion >= minServerVerStartAPICapab {
		// Optional capabilities placeholder – currently unused but must be omitted
		// entirely when the server version predates the field.
		fields = append(fields, "")
	}

	msg := c.encodeMsg(fields...)
	lengthBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBytes, uint32(len(msg)))

	// Debug: hex dump START_API message
	c.logOutgoingMessageHex(msg)

	sendErr := c.withTransport(true, func() error {
		if c.writer == nil {
			return fmt.Errorf("%w: buffered writer not initialized before startAPI", errStartAPIFailed)
		}
		if _, err := c.writer.Write(lengthBytes); err != nil {
			return fmt.Errorf("%w: failed to send startAPI length: %w", errStartAPIFailed, err)
		}
		if _, err := c.writer.Write(msg); err != nil {
			return fmt.Errorf("%w: failed to send startAPI payload: %w", errStartAPIFailed, err)
		}
		c.logPacketOutbound(msg)
		if err := c.writer.Flush(); err != nil {
			return fmt.Errorf("%w: failed to flush startAPI payload: %w", errStartAPIFailed, err)
		}
		return nil
	})
	if sendErr != nil {
		return sendErr
	}

	// Sent startAPI message

	// Wait for initial responses (managed accounts, next valid ID, etc.)
	// Set a shorter timeout for initial messages
	c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{}) // Clear deadline

	// Track if we get error 326 (client ID already in use)
	var clientIDError error

	// Read initial responses
	for i := 0; i < 10; i++ { // Try to read up to 10 initial messages
		msgBytes, err := c.readMessage()
		if err != nil {
			// Check if we have a client ID error before returning
			c.statusMu.RLock()
			if c.lastError != nil && (strings.Contains(c.lastError.Error(), "Error code 326") ||
				strings.Contains(c.lastError.Error(), "client id is already in use")) {
				clientIDError = c.lastError
			}
			c.statusMu.RUnlock()

			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				break // Timeout is expected after initial messages
			}
			if err == io.EOF {
				// Check one more time for client ID error
				c.statusMu.RLock()
				if c.lastError != nil {
					lastErr := c.lastError
					c.statusMu.RUnlock()
					// Return the actual error if it's client ID related
					errStr := strings.ToLower(lastErr.Error())
					if strings.Contains(errStr, "client id") || strings.Contains(errStr, "error code 326") {
						return lastErr
					}
				} else {
					c.statusMu.RUnlock()
				}
				// If we got error 326, return that specific error
				if clientIDError != nil {
					return clientIDError
				}
				return fmt.Errorf("%w: connection closed by server after startAPI", errStartAPIFailed)
			}
			// Log but don't fail on read errors during initialization
			connectLogger.Errorf("Error reading initial message: %v", err)
			break
		}

		// Process the initial message
		c.processMessage(msgBytes)

		// Check if lastError contains client ID error
		c.statusMu.RLock()
		if c.lastError != nil && (strings.Contains(c.lastError.Error(), "Error code 326") ||
			strings.Contains(c.lastError.Error(), "client id is already in use")) {
			clientIDError = c.lastError
		}
		c.statusMu.RUnlock()
	}

	// If we detected client ID error, return it
	if clientIDError != nil {
		return clientIDError
	}

	return nil
}

// readMessages continuously reads messages from the connection.
//
// Wrapped in a panic guard because this goroutine is the only consumer of
// the TCP read side: if a handler or decoder panics (bad protobuf shape,
// unexpected wire field, …) without recovery, the reader dies silently
// while c.status stays StatusConnected — every subsequent Write queues
// forever waiting for a reply that no one is reading. A recovered panic
// is converted to a disconnect so the reconnect loop can take over.
func (c *Connection) readMessages() {
	defer c.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			connectLogger.Errorf("readMessages panic recovered (Client ID: %d): %v\n%s",
				c.config.ClientID, r, debug.Stack())
			c.handleDisconnection(fmt.Errorf("reader panic: %v", r))
		}
	}()
	c.signalReadStarted()

	for {
		select {
		case <-c.stopChan:
			return
		default:
			if c.conn == nil {
				c.handleDisconnection(io.EOF)
				return
			}
			// Read message with timeout
			c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

			msgBytes, err := c.readMessage()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout is expected, continue
					continue
				}
				if err == io.EOF {
					ibkrLogger.Warnf("Connection closed by server")
					c.handleDisconnection(err)
					return
				}
				// Any other error means stream alignment is uncertain —
				// "message too large" is the canonical case: a length
				// prefix that overflowed the cap leaves the reader
				// positioned in the middle of the previous frame's body,
				// so continuing reads garbage as length prefixes
				// indefinitely (one production incident hit 500k+ such
				// errors before disconnect). Fail fast: log, signal
				// disconnect, let reconnect logic rebuild a clean stream.
				ibkrLogger.Errorf("Error reading message: %v", err)
				c.handleDisconnection(err)
				return
			}

			// Process the message
			c.processMessage(msgBytes)

			// Update heartbeat
			c.statusMu.Lock()
			c.lastHeartbeat = time.Now()
			c.statusMu.Unlock()
		}
	}
}

// processMessage handles incoming messages from IBKR
func (c *Connection) processMessage(msgBytes []byte) {
	fields := c.decodeMessage(msgBytes)
	if len(fields) == 0 {
		return
	}

	// First field is always the message ID
	msgID, err := strconv.Atoi(fields[0])
	if err != nil {
		ibkrLogger.Warnf("[WARNING] Invalid message ID: %v", err)
		return
	}

	if c.wireTap != nil {
		c.wireTap.RecordInbound(msgID, msgBytes, fields)
	}

	// Log only critical messages, not routine ones
	// Suppress common message types that occur frequently
	suppressedMessages := map[int]bool{
		msgTickPrice:         true, // Tick price updates (1)
		msgTickSize:          true, // Tick size updates (2)
		msgTickString:        true, // Tick string updates (46)
		msgTickGeneric:       true, // Generic tick updates (45)
		msgMarketDataType:    true, // Market data type (58)
		msgTickNews:          true, // Tick news (81)
		msgAccountSummary:    true, // Account summary (63)
		msgAccountSummaryEnd: true, // Account summary end (64)
		msgPosition:          true, // Position updates (61)
		msgPositionEnd:       true, // Position sync complete (62)
		15:                   true, // Managed accounts
		9:                    true, // Next valid ID
		4:                    true, // Error messages (handled separately)
		msgCurrentTimeMillis: true, // Heartbeat variant with ms precision (109)
	}

	// Only log unusual messages for debugging
	if msgID != 0 && !suppressedMessages[msgID] && msgID != msgCurrentTime {
		ibkrLogger.Debugf("Received message ID %d with %d fields", msgID, len(fields))
	}

	// Handle common messages
	switch msgID {
	case msgNextValidID:
		if len(fields) > 1 {
			// Store for later use
			c.nextOrderID, _ = strconv.Atoi(fields[1])
			// Only log for first connection in pool
			if c.config.ClientID == 1 {
				ibkrLogger.Infof("Next Valid Order ID: %s", fields[1])
			}
		}
	case msgCurrentTimeMillis:
		if len(fields) > 1 {
			if ms, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				ts := time.Unix(0, ms*int64(time.Millisecond))
				c.statusMu.Lock()
				c.lastHeartbeat = ts
				c.statusMu.Unlock()
			}
		}
	case msgManagedAccts:
		if len(fields) > 1 {
			acct := strings.TrimSpace(fields[1])
			c.accountMu.Lock()
			c.account = acct
			c.accountMu.Unlock()
			// Only log for first connection in pool
			if c.config.ClientID == 1 {
				ibkrLogger.Infof("Managed Accounts: %s", fields[1])
			}
		}
	case msgErrMsg:
		c.handleErrorMessage(fields)
		// Also forward to any registered error handler for higher-level recovery
		if handlers := c.snapshotHandlers(msgErrMsg); len(handlers) > 0 {
			for _, h := range handlers {
				h(fields)
			}
		}
	case msgCurrentTime:
		// IBKR heartbeat - silently process to maintain connection
		// The timestamp is available in fields[1] if needed for debugging
	case msgPosition:
		c.handlePosition(fields)
	case msgPositionEnd:
		portfolioLogger.Infof("Position sync complete")
		// Signal that positions are complete
		select {
		case c.positionsEndChan <- struct{}{}:
		default:
			// Channel already has a signal
		}
	case msgAccountSummary:
		c.handleAccountSummary(fields)
	case msgAccountSummaryEnd:
		portfolioLogger.Infof("Account summary sync complete")
		// Signal that account summary is complete
		select {
		case c.acctSummaryEndChan <- struct{}{}:
		default:
			// Channel already has a signal
		}
	case msgPortfolioValue:
		c.handlePortfolioValue(fields)
	case msgAcctValue:
		c.handleAccountValue(fields)
	case msgAcctUpdateTime:
		if len(fields) > 1 {
			portfolioLogger.Infof("Account update time: %s", fields[1])
		}
	case msgAcctDownloadEnd:
		portfolioLogger.Infof("Account download complete")
	case msgMarketDataType:
		// Market data type notification (live/delayed/frozen)
		// Format: [msgID, version, reqID, type]
		if len(fields) >= 4 {
			rid, _ := strconv.Atoi(fields[2])
			dt, _ := strconv.Atoi(fields[3])
			c.mktDataTypeMu.Lock()
			c.mktDataType[rid] = dt
			c.mktDataTypeMu.Unlock()
			ibkrLogger.Infof("[cid=%d] MarketDataType notice: reqID=%d, type=%d", c.config.ClientID, rid, dt)
		}
	case msgSystemNotification:
		c.handleSystemNotification(fields)
	case msgTickNews:
		// News tick - handle silently for now
		// Format: [msgID, reqID, timeStamp, providerCode, articleID, headline, extraData]
	case msgTickString:
		// String tick data (e.g., last timestamp, bid/ask exchange)
		// Format: [msgID, version, reqID, tickType, value]
		// Silently handle - these are frequent
	case msgTickGeneric:
		// Generic tick data (e.g., 106 = Option Implied Volatility)
		// Format: [msgID, version, reqID, tickType, value]
		// Route to a registered handler if present so upstream components can
		// capture items like option implied volatility (tick 106).
		if handlers := c.snapshotHandlers(msgTickGeneric); len(handlers) > 0 {
			for _, handler := range handlers {
				handler(fields)
			}
			return
		}
	case msgExecutionRequestAck:
		// Gateway >= v10.30 sends an explicit ack (255) after reqExecutions when no fills are returned.
		if len(fields) > 1 && fields[1] != "" {
			ibkrLogger.Infof("[INFO] Execution request acknowledged (reqID=%s)", fields[1])
		} else {
			ibkrLogger.Infof("[INFO] Execution request acknowledged (msgID=255)")
		}
		return
	case msgExecDetailsEnd:
		if handlers := c.snapshotHandlers(msgExecDetailsEnd); len(handlers) > 0 {
			for _, handler := range handlers {
				handler(fields)
			}
		} else if len(fields) > 1 && fields[1] != "" {
			ibkrLogger.Debugf("Execution details completed (reqID=%s)", fields[1])
		} else {
			ibkrLogger.Debugf("Execution details completed")
		}
		return
	case msgSecurityDefinitionOptionalParameter, msgSecurityDefinitionOptionalParameterEnd:
		// reqSecDefOptParams (78) responses arrive once per exchange (75) before
		// a single end marker (76). Connector.FetchOptionExpiries registers
		// per-message handlers via RegisterHandler; route both IDs there.
		if handlers := c.snapshotHandlers(msgID); len(handlers) > 0 {
			for _, handler := range handlers {
				handler(fields)
			}
		}
		return
	default:
		// Check for registered handler
		if handlers := c.snapshotHandlers(msgID); len(handlers) > 0 {
			for _, handler := range handlers {
				handler(fields)
			}
		} else if isBenignUnhandledMessage(msgID) {
			// Contract details may arrive even when the connector did not register a handler.
			// Avoid logging warnings for these routine responses.
			return
		} else {
			ibkrLogger.Warnf("[WARNING] Unhandled message ID %d: %v", msgID, fields)
		}
	}
}

func isBenignUnhandledMessage(msgID int) bool {
	switch msgID {
	case msgContractData, msgContractDataEnd:
		return true
	default:
		return false
	}
}

// getErrorDescription returns a human-readable description for IBKR error codes
func getErrorDescription(code int) string {
	switch code {
	// Low-numbered error codes (1-99)
	case 1:
		return "Requested market data is not available"
	case 2:
		return "Requested market data is not subscribed"
	case 3:
		return "Requested market data cannot be retrieved"
	case 4:
		return "Market data request error"

	// Connection and System Status (2100-2199)
	case 2104:
		return "Market data farm connected"
	case 2106:
		return "Historical data farm connected"
	case 2107:
		return "Historical data farm connected (inactive)"
	case 2108:
		return "Market data farm disconnected"
	case 2110:
		return "Connectivity between TWS and server is broken"
	case 2119:
		return "Market data farm connection is OK"
	case 2158:
		return "Security definition data farm connected"

	// Market Data Errors (300-399)
	case 320:
		return "Reading request error - Invalid ticker or exchange"
	case 321:
		return "Error validating request"
	case 322:
		return "Error processing request - Duplicate ticker ID"
	case 326:
		return "Unable to connect as client id is already in use. Retry with unique client id"
	case 354:
		return "Requested market data is not subscribed"

	// Order and Trading Errors (100-199)
	case 110:
		return "Price does not conform to minimum price variation"
	case 161:
		return "Cancel attempted when order is not in a cancellable state"
	case 162:
		return "Historical market data service error"

	// Connection Errors (500-599)
	case 502:
		return "Couldn't connect to TWS"
	case 503:
		return "The TWS is out of date and must be upgraded"
	case 504:
		return "Not connected to TWS"

	// Account and Position Errors (400-449)
	case 430:
		return "The account code is required for this operation"
	case 431:
		return "Invalid account code"

	default:
		return ""
	}
}

// handleErrorMessage processes error messages from IBKR
func (c *Connection) handleErrorMessage(fields []string) {
	if len(fields) < 3 {
		ibkrLogger.Warnf("[WARNING] Invalid error message: %v", fields)
		return
	}

	// Expected layout: [msgId(4), version, reqId, errorCode, errorMsg]
	reqID := ""
	errorCode := ""
	errorMsg := ""
	if len(fields) > 2 {
		reqID = fields[2]
	}
	if len(fields) > 3 {
		errorCode = fields[3]
	}
	if len(fields) > 4 {
		errorMsg = fields[4]
	} else if len(fields) > 3 {
		errorMsg = fields[3]
	}

	// Check the error code to determine if it's informational or an actual error
	code, _ := strconv.Atoi(errorCode)

	// Log important errors for debugging
	if code >= 300 && code < 400 {
		ibkrLogger.Debugf("[cid=%d] Market data error for reqID %s: code=%s, msg=%s", c.config.ClientID, reqID, errorCode, errorMsg)
	} else if code == 200 || code == 162 || code == 10197 {
		// Market data subscription errors
		ibkrLogger.Debugf("[cid=%d] Market data subscription error for reqID %s: code=%s, msg=%s", c.config.ClientID, reqID, errorCode, errorMsg)
	}

	// Sometimes IBKR sends the error code in the message field
	// Check if errorMsg is just a number (another error code)
	if msgCode, err := strconv.Atoi(errorMsg); err == nil {
		// The message is an error code, look up its description
		if desc := getErrorDescription(msgCode); desc != "" {
			errorMsg = desc
		} else {
			errorMsg = fmt.Sprintf("Error code %d", msgCode)
		}
	}

	// Get human-readable description for the main error code
	description := getErrorDescription(code)
	if description != "" && errorMsg == errorCode {
		// Only replace if errorMsg is just the code repeated
		errorMsg = description
	}

	// IBKR informational codes (not actual errors)
	// These codes indicate successful connections to various data farms
	switch code {
	case 2104, 2106, 2107, 2158, 2119, 2169:
		// These are normal connection confirmations, suppress them
		// They just confirm the data farms are connected
		return
	}

	// Warning level codes
	if code >= 2100 && code < 2200 {
		ibkrLogger.Warnf("[cid=%d] %s", c.config.ClientID, errorMsg)
	} else if code == 502 || code == 503 || code == 504 {
		// Connection errors - these are critical
		ibkrLogger.Errorf("[cid=%d] Critical Error: %s (Code %d)", c.config.ClientID, errorMsg, code)
		c.handleDisconnection(fmt.Errorf("connection error %d: %s", code, errorMsg))
	} else if code == 326 {
		// Client ID already in use - store this error so Connect can retry
		ibkrLogger.Infof("[cid=%d] System notice: %s", c.config.ClientID, errorMsg)
		c.statusMu.Lock()
		c.lastError = fmt.Errorf("Error code 326: %s", errorMsg)
		c.statusMu.Unlock()
	} else if code == 200 {
		ibkrLogger.Warnf("[cid=%d] Market Data Error (ReqID %s): %s", c.config.ClientID, reqID, errorMsg)
		c.releaseMarketDataSlot()
	} else if code == 320 || code == 321 || code == 322 || code == 354 {
		// Market data errors
		ibkrLogger.Warnf("[cid=%d] Market Data Error (ReqID %s): %s", c.config.ClientID, reqID, errorMsg)
		if code == 354 {
			c.releaseMarketDataSlot()
		}
		if code == 320 || code == 321 || code == 322 {
			c.reportParserError(reqID, code, errorMsg, fields)
		}
	} else if code == 10197 {
		// Competing live session blocks real-time data; switch to delayed
		ibkrLogger.Warnf("[cid=%d] Market Data Error (ReqID %s): %s", c.config.ClientID, reqID, errorMsg)
		c.handleCompetingLiveSession(reqID, errorMsg)
	} else if code < 0 || code == 0 {
		// Codes -1 or 0 often contain system messages
		if errorMsg != "" && errorMsg != "0" {
			// Check if this is one of the informational messages we want to suppress
			if errorMsg == "Market data farm connected" ||
				errorMsg == "Historical data farm connected" ||
				errorMsg == "Historical data farm connected (inactive)" ||
				errorMsg == "Security definition data farm connected" {
				// Suppress these repetitive connection confirmations
				return
			}
			// Only log if it's not just the code number
			if _, err := strconv.Atoi(errorMsg); err != nil {
				ibkrLogger.Infof("[cid=%d] System: %s", c.config.ClientID, errorMsg)
				// Check if this is a client ID error message without a code
				errLower := strings.ToLower(errorMsg)
				if strings.Contains(errLower, "unable to connect as client id") ||
					strings.Contains(errLower, "client id is already in use") ||
					strings.Contains(errLower, "client id already in use") {
					// Store this as an error so Connect can retry
					c.statusMu.Lock()
					c.lastError = fmt.Errorf("%s", errorMsg)
					c.statusMu.Unlock()
				}
			}
		}
	} else {
		// Other errors
		ibkrLogger.Warnf("[cid=%d] Error (ReqID %s): %s (Code %d)", c.config.ClientID, reqID, errorMsg, code)
	}
}

// HasCompetingLiveSession returns true if IBKR reported code 10197 for this connection.
func (c *Connection) HasCompetingLiveSession() bool {
	c.competingMu.RLock()
	defer c.competingMu.RUnlock()
	return c.competingLiveSession
}

func (c *Connection) reportParserError(reqIDStr string, code int, msg string, fields []string) {
	if c.wireTap == nil {
		return
	}
	reqID := parseIntSafe(reqIDStr)
	entry, _ := c.lookupReqAlias(reqID)
	symbol := entry.symbol
	c.wireTap.HandleParserError(ParseError{
		ClientID: c.config.ClientID,
		ReqID:    reqID,
		Symbol:   symbol,
		Code:     code,
		Message:  msg,
		Context:  cloneStringSlice(fields),
	})
}

// handleCompetingLiveSession forces delayed market data when IBKR reports code 10197.
func (c *Connection) handleCompetingLiveSession(reqID, errorMsg string) {
	c.competingMu.Lock()
	already := c.competingLiveSession
	c.competingLiveSession = true
	c.competingMu.Unlock()

	if already {
		return
	}

	ibkrLogger.Warnf("[cid=%d] 10197 competing live session detected (reqID=%s) – requesting delayed market data", c.config.ClientID, reqID)
	if err := c.SetMarketDataType(3); err != nil {
		ibkrLogger.Errorf("[cid=%d] Failed to request delayed market data after 10197: %v", c.config.ClientID, err)
	} else {
		ibkrLogger.Warnf("[cid=%d] Forced delayed market data after 10197 (%s)", c.config.ClientID, errorMsg)
	}
}

func (c *Connection) releaseMarketDataSlot() {
	if c.rateLimiter == nil {
		return
	}
	c.rateLimiter.ReleaseMarketDataSlot()
}

func (c *Connection) registerStartAPIFailure() time.Duration {
	c.startAPIMu.Lock()
	defer c.startAPIMu.Unlock()

	c.startAPIFailures++
	c.lastStartAPIFailure = time.Now()

	switch {
	case c.startAPIFailures == 1:
		return 2 * time.Second
	case c.startAPIFailures == 2:
		return 5 * time.Second
	case c.startAPIFailures <= 4:
		return 15 * time.Second
	case c.startAPIFailures <= 6:
		return 30 * time.Second
	default:
		return time.Minute
	}
}

func (c *Connection) resetStartAPIFailure() {
	c.startAPIMu.Lock()
	c.startAPIFailures = 0
	c.lastStartAPIFailure = time.Time{}
	c.startAPIMu.Unlock()
}

// handlePosition processes position updates from IBKR
func (c *Connection) handlePosition(fields []string) {
	// IBKR Position message format (msgID 61, version 3):
	// For stocks (13 fields):
	// 0: msgID (61)
	// 1: version (3)
	// 2: account
	// 3: conId
	// 4: symbol
	// 5: secType
	// 6: multiplier (or 0.0)
	// 7: exchange
	// 8: currency
	// 9: localSymbol
	// 10: tradingClass
	// 11: position
	// 12: avgCost
	//
	// For options (15-16 fields):
	// Additional fields for expiry, strike, right

	// The actual position message format might vary based on version
	// Let's handle both formats (with and without some optional fields)
	if len(fields) < 13 {
		portfolioLogger.Errorf("Position message too short: %d fields", len(fields))
		return
	}

	// Parse contract details
	conID, _ := strconv.Atoi(fields[3])

	var positionSize, avgCost float64
	var contract Contract

	if len(fields) == 13 {
		// Stock format (13 fields)
		multiplier, _ := strconv.ParseFloat(fields[6], 64)
		if multiplier == 0 {
			multiplier = 1
		}

		contract = Contract{
			ConID:        conID,
			Symbol:       fields[4],
			SecType:      fields[5],
			Multiplier:   int(multiplier),
			Exchange:     fields[7],
			Currency:     fields[8],
			LocalSymbol:  fields[9],
			TradingClass: fields[10],
		}

		positionSize, _ = strconv.ParseFloat(fields[11], 64)
		avgCost, _ = strconv.ParseFloat(fields[12], 64)

	} else if len(fields) == 15 {
		// Options format (15 fields) - includes expiry, strike, right
		// Fields shift: expiry at 6, strike at 7, right at 8, multiplier at 9
		strike, _ := strconv.ParseFloat(fields[7], 64)
		multiplier, _ := strconv.Atoi(fields[9])
		if multiplier == 0 {
			multiplier = 100 // Default for options
		}

		contract = Contract{
			ConID:        conID,
			Symbol:       fields[4],
			SecType:      fields[5],
			Expiry:       fields[6],
			Strike:       strike,
			Right:        fields[8],
			Multiplier:   multiplier,
			Exchange:     fields[10],
			Currency:     fields[11],
			LocalSymbol:  fields[12],
			TradingClass: fields[13],
		}

		// Position at field 14, avgCost might be missing
		if fields[14] != "" {
			positionSize, _ = strconv.ParseFloat(fields[14], 64)
		}

	} else if len(fields) >= 16 {
		// Full options format with avgCost
		strike, _ := strconv.ParseFloat(fields[7], 64)
		multiplier, _ := strconv.Atoi(fields[9])
		if multiplier == 0 {
			multiplier = 100
		}

		contract = Contract{
			ConID:        conID,
			Symbol:       fields[4],
			SecType:      fields[5],
			Expiry:       fields[6],
			Strike:       strike,
			Right:        fields[8],
			Multiplier:   multiplier,
			Exchange:     fields[10],
			Currency:     fields[11],
			LocalSymbol:  fields[12],
			TradingClass: fields[13],
		}

		positionSize, _ = strconv.ParseFloat(fields[14], 64)
		avgCost, _ = strconv.ParseFloat(fields[15], 64)

	} else {
		portfolioLogger.Errorf("Unexpected position message format with %d fields", len(fields))
		return
	}

	// Create position key
	key := contract.Symbol
	if contract.SecType == "OPT" {
		key = fmt.Sprintf("%s_%s_%s%.0f", contract.Symbol, contract.Expiry, contract.Right, contract.Strike)
	}

	// Store position
	c.positionsMu.Lock()
	c.positions[key] = &RawPosition{
		Account:     fields[2],
		Contract:    contract,
		Position:    positionSize,
		AverageCost: avgCost,
	}
	c.positionsMu.Unlock()

	portfolioLogger.Infof("Position: %s %s %.2f @ %.2f",
		fields[2], key, positionSize, avgCost)
}

// handleAccountSummary processes account summary updates from IBKR
func (c *Connection) handleAccountSummary(fields []string) {
	// Expected fields:
	// 0: msgID (63)
	// 1: version
	// 2: reqId
	// 3: account
	// 4: tag
	// 5: value
	// 6: currency

	if len(fields) < 7 {
		ibkrLogger.Warnf("[WARNING] Invalid account summary message: expected at least 7 fields, got %d", len(fields))
		return
	}

	tag := fields[4]
	value := fields[5]
	currency := fields[6]
	account := strings.TrimSpace(fields[3])

	// Store in account summary map and update active account
	c.accountMu.Lock()
	if account != "" {
		c.account = account
	}
	key := tag
	if currency != "" && currency != "BASE" {
		key = fmt.Sprintf("%s_%s", tag, currency)
	}
	c.accountSummary[key] = value
	c.accountMu.Unlock()

	// Log important values
	switch tag {
	case "NetLiquidation", "BuyingPower", "TotalCashValue", "GrossPositionValue":
		portfolioLogger.Infof("%s: %s %s", tag, value, currency)
	}
}

// handlePortfolioValue handles portfolio position updates (from reqAccountUpdates)
func (c *Connection) handlePortfolioValue(fields []string) {
	// Expected fields for msgPortfolioValue (7):
	// 0: msgID
	// 1: version
	// 2: contract.conId
	// 3: contract.symbol
	// 4: contract.secType
	// 5: contract.expiry
	// 6: contract.strike
	// 7: contract.right
	// 8: contract.multiplier
	// 9: contract.primaryExchange
	// 10: contract.currency
	// 11: contract.localSymbol
	// 12: contract.tradingClass
	// 13: position
	// 14: marketPrice
	// 15: marketValue
	// 16: averageCost
	// 17: unrealizedPNL
	// 18: realizedPNL
	// 19: accountName

	if len(fields) < 20 {
		ibkrLogger.Warnf("[WARNING] Invalid portfolio value message: expected at least 20 fields, got %d", len(fields))
		return
	}

	// Parse contract
	conID, _ := strconv.Atoi(fields[2])
	strike, _ := strconv.ParseFloat(fields[6], 64)
	multiplier, _ := strconv.Atoi(fields[8])
	if multiplier == 0 {
		multiplier = 1
	}

	contract := Contract{
		ConID:        conID,
		Symbol:       fields[3],
		SecType:      fields[4],
		Expiry:       fields[5],
		Strike:       strike,
		Right:        fields[7],
		Multiplier:   multiplier,
		Exchange:     fields[9],
		Currency:     fields[10],
		LocalSymbol:  fields[11],
		TradingClass: fields[12],
	}

	// Parse position data
	position, _ := strconv.ParseFloat(fields[13], 64)
	marketPrice, _ := strconv.ParseFloat(fields[14], 64)
	marketValue, _ := strconv.ParseFloat(fields[15], 64)
	averageCost, _ := strconv.ParseFloat(fields[16], 64)
	unrealizedPNL, _ := strconv.ParseFloat(fields[17], 64)
	realizedPNL, _ := strconv.ParseFloat(fields[18], 64)

	// Create position key
	key := contract.Symbol
	if contract.SecType == "OPT" {
		key = fmt.Sprintf("%s_%s_%s%.0f", contract.Symbol, contract.Expiry, contract.Right, contract.Strike)
	}

	// Store position with full data
	c.positionsMu.Lock()
	c.positions[key] = &RawPosition{
		Account:       fields[19],
		Contract:      contract,
		Position:      position,
		MarketPrice:   marketPrice,
		MarketValue:   marketValue,
		AverageCost:   averageCost,
		UnrealizedPNL: unrealizedPNL,
		RealizedPNL:   realizedPNL,
	}
	c.positionsMu.Unlock()

	portfolioLogger.Infof("Updated: %s %.2f @ %.2f, PnL: %.2f",
		key, position, marketPrice, unrealizedPNL)
}

// handleAccountValue handles account value updates (from reqAccountUpdates)
func (c *Connection) handleAccountValue(fields []string) {
	// Expected fields for msgAcctValue (6):
	// 0: msgID
	// 1: version
	// 2: key (e.g., "NetLiquidation", "BuyingPower")
	// 3: value
	// 4: currency
	// 5: accountName

	if len(fields) < 6 {
		ibkrLogger.Warnf("[WARNING] Invalid account value message: expected at least 6 fields, got %d", len(fields))
		return
	}

	key := fields[2]
	value := fields[3]
	currency := fields[4]

	// Store in account summary
	c.accountMu.Lock()
	mapKey := key
	if currency != "" && currency != "BASE" {
		mapKey = fmt.Sprintf("%s_%s", key, currency)
	}
	c.accountSummary[mapKey] = value
	c.accountMu.Unlock()

	// Log important values
	switch key {
	case "NetLiquidation", "BuyingPower", "TotalCashValue", "UnrealizedPnL", "RealizedPnL":
		portfolioLogger.Infof("%s: %s %s", key, value, currency)
	}
}

func (c *Connection) handleSystemNotification(fields []string) {
	if len(fields) < 2 {
		ibkrLogger.Warnf("[IBKR cid=%d] System notice: missing payload", c.config.ClientID)
		return
	}

	note, err := parseSystemNotificationPayload([]byte(fields[1]))
	if err != nil {
		ibkrLogger.Warnf("[IBKR cid=%d] System notice decode error: %v", c.config.ClientID, err)
		return
	}

	scope := "global"
	symbolAlias := ""
	aliasEntry := reqAliasEntry{}
	if note.tickerID >= 0 {
		if entry, ok := c.lookupReqAlias(int(note.tickerID)); ok {
			aliasEntry = entry
			symbolAlias = entry.symbol
			if symbolAlias != "" {
				scope = fmt.Sprintf("reqID=%d (%s)", note.tickerID, symbolAlias)
			} else {
				scope = fmt.Sprintf("reqID=%d", note.tickerID)
			}
		} else {
			scope = fmt.Sprintf("reqID=%d", note.tickerID)
		}
	}

	codeLabel := fmt.Sprintf("code=%d", note.code)
	if desc := getErrorDescription(note.code); desc != "" && !strings.Contains(note.message, desc) {
		codeLabel = fmt.Sprintf("code=%d (%s)", note.code, desc)
	}

	// Treat documented market-data error codes (300-399) as warnings so they
	// surface even when operators run with log level set to info.
	shouldWarn := note.code == 200 || (note.code >= 300 && note.code < 400 && note.code != 366)
	if !shouldWarn {
		msgLower := strings.ToLower(note.message)
		// For non-cataloged codes, fall back to a substring check; this keeps
		// unexpected notices containing "error" from being silently downgraded.
		shouldWarn = strings.Contains(msgLower, "error")
	}
	upperMsg := strings.ToUpper(note.message)
	parserMisalign := strings.Contains(upperMsg, "MART") || strings.Contains(upperMsg, "'BOE") || strings.Contains(upperMsg, "\"BOE") || strings.Contains(upperMsg, " BOE")
	context := ""
	if parserMisalign {
		context = c.parserContext(symbolAlias)
		if c.wireTap != nil {
			reqID := int(note.tickerID)
			if reqID < 0 {
				reqID = 0
			}
			c.wireTap.HandleParserError(ParseError{
				ClientID: c.config.ClientID,
				ReqID:    reqID,
				Symbol:   symbolAlias,
				Code:     note.code,
				Message:  note.message,
				Context:  cloneStringSlice(fields),
			})
		}
	}

	msgText := note.message
	if context != "" {
		msgText = fmt.Sprintf("%s | frame=%s", msgText, context)
	}

	if note.timestamp.IsZero() {
		format := "[IBKR cid=%d] System notice %s %s: %s"
		args := []interface{}{c.config.ClientID, scope, codeLabel, msgText}
		switch {
		case parserMisalign:
			ibkrLogger.Errorf(format, args...)
		case shouldWarn:
			ibkrLogger.Warnf(format, args...)
		default:
			ibkrLogger.Infof(format, args...)
		}
		c.dispatchSystemNotice(note, aliasEntry)
		return
	}

	format := "[IBKR cid=%d] System notice %s %s @ %s: %s"
	args := []interface{}{c.config.ClientID, scope, codeLabel, note.timestamp.UTC().Format(time.RFC3339), msgText}
	switch {
	case parserMisalign:
		ibkrLogger.Errorf(format, args...)
	case shouldWarn:
		ibkrLogger.Warnf(format, args...)
	default:
		ibkrLogger.Infof(format, args...)
	}
	c.dispatchSystemNotice(note, aliasEntry)
}

// Message encoding/decoding methods

// sendRawMessage sends a raw string message with rate limiting
func (c *Connection) sendRawMessage(msg string) error {
	// Check connection status before queueing - reject if disconnecting
	c.statusMu.RLock()
	status := c.status
	c.statusMu.RUnlock()
	if status != StatusConnected {
		return fmt.Errorf("cannot send message: connection status is %v", status)
	}

	// Use rate limiter for all messages
	return c.rateLimiter.Submit(RequestTypeGeneral, func() error {
		if err := c.waitForHandshakeReady(); err != nil {
			return err
		}
		return c.withTransport(false, func() error {
			_, err := c.writer.WriteString(msg)
			if err != nil {
				return err
			}
			return c.writer.Flush()
		})
	})
}

// sendMessage sends a length-prefixed message with rate limiting
func (c *Connection) sendMessage(msg []byte) error {
	// Check connection status before queueing - reject if disconnecting
	c.statusMu.RLock()
	status := c.status
	c.statusMu.RUnlock()
	if status != StatusConnected {
		return fmt.Errorf("cannot send message: connection status is %v", status)
	}

	// Use rate limiter for all messages
	return c.rateLimiter.Submit(RequestTypeGeneral, func() error {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()

		if c.writeInProgress.Load() {
			wireLogger.Errorf("CONCURRENT WRITE DETECTED: previous send still in progress")
		}
		c.writeInProgress.Store(true)
		defer c.writeInProgress.Store(false)

		lengthBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(lengthBytes, uint32(len(msg)))

		if err := c.waitForHandshakeReady(); err != nil {
			return err
		}
		return c.withTransport(false, func() error {
			fields := c.decodeMessage(msg)
			msgID := determineMessageID(c.serverVersion, msg)

			if c.wireTap != nil && len(fields) > 0 {
				if updatedFields, changed := c.wireTap.ApplyOutboundOverrides(msgID, cloneStringSlice(fields)); changed {
					if updatedMsg, err := c.encodeFromFields(updatedFields); err != nil {
						wireLogger.Errorf("Failed to re-encode override for %s: %v", resolveMessageName(msgID), err)
					} else {
						msg = updatedMsg
						fields = updatedFields
						msgID = determineMessageID(c.serverVersion, msg)
					}
				}
			}

			c.logSuspiciousOutbound(msgID, fields)
			if c.wireTap != nil {
				c.wireTap.RecordOutbound(msgID, msg, fields)
			}

			// Debug: hex dump outgoing message
			c.logOutgoingMessageHex(msg)

			if _, err := c.writer.Write(lengthBytes); err != nil {
				return err
			}

			if _, err := c.writer.Write(msg); err != nil {
				return err
			}

			c.logPacketOutbound(msg)

			if err := c.writer.Flush(); err != nil {
				return err
			}
			if c.writer.Buffered() > 0 {
				wireLogger.Errorf("flush incomplete: %d bytes still buffered after Flush()", c.writer.Buffered())
			}

			return nil
		})
	})
}

// sendMessageWithType sends a message with specific request type for rate limiting
func (c *Connection) sendMessageWithType(msg []byte, reqType RequestType) error {
	// Check connection status before queueing - reject if disconnecting
	c.statusMu.RLock()
	status := c.status
	c.statusMu.RUnlock()
	if status != StatusConnected {
		return fmt.Errorf("cannot send message: connection status is %v", status)
	}

	return c.rateLimiter.Submit(reqType, func() error {
		if err := c.waitForHandshakeReady(); err != nil {
			return err
		}
		return c.withTransport(false, func() error {
			fields := c.decodeMessage(msg)
			msgID := determineMessageID(c.serverVersion, msg)

			if c.wireTap != nil && len(fields) > 0 {
				if updatedFields, changed := c.wireTap.ApplyOutboundOverrides(msgID, cloneStringSlice(fields)); changed {
					if updatedMsg, err := c.encodeFromFields(updatedFields); err != nil {
						wireLogger.Errorf("Failed to re-encode override for %s: %v", resolveMessageName(msgID), err)
					} else {
						msg = updatedMsg
						fields = updatedFields
						msgID = determineMessageID(c.serverVersion, msg)
					}
				}
			}

			c.logSuspiciousOutbound(msgID, fields)
			if c.wireTap != nil {
				c.wireTap.RecordOutbound(msgID, msg, fields)
			}

			lengthBytes := make([]byte, 4)
			binary.BigEndian.PutUint32(lengthBytes, uint32(len(msg)))

			// Debug: hex dump outgoing message
			c.logOutgoingMessageHex(msg)

			if _, err := c.writer.Write(lengthBytes); err != nil {
				return err
			}

			if _, err := c.writer.Write(msg); err != nil {
				return err
			}

			c.logPacketOutbound(msg)

			return c.writer.Flush()
		})
	})
}

func (c *Connection) logOutgoingMessageHex(msg []byte) {
	if !c.logWireHex || len(msg) < 4 {
		return
	}

	var msgType int32
	var hasNullAfterType bool

	if c.serverVersion >= 100 && len(msg) >= 4 {
		msgType = int32(binary.BigEndian.Uint32(msg[:4]))
		if len(msg) > 4 && msg[4] == 0x00 {
			hasNullAfterType = true
		}
	}

	// Show first 80 bytes or entire message if shorter
	dumpLen := len(msg)
	if dumpLen > 80 {
		dumpLen = 80
	}

	hexStr := ""
	for i := 0; i < dumpLen; i++ {
		hexStr += fmt.Sprintf("%02x ", msg[i])
		if (i+1)%16 == 0 {
			hexStr += "\n                "
		}
	}
	if dumpLen < len(msg) {
		hexStr += fmt.Sprintf("... (%d more bytes)", len(msg)-dumpLen)
	}

	log.Printf("[WIRE OUT] msgType=%d len=%d nullAfterType=%v\n                %s",
		msgType, len(msg), hasNullAfterType, hexStr)
}

// logSuspiciousOutbound inspects encoded payloads to highlight frames that
// frequently trigger IBKR MART/320 parser faults (e.g., reqID/conID set to 0).
// It intentionally works on already-encoded messages to avoid allocating in the
// request builders and focuses on reqMktData, reqHistoricalData, and
// reqContractData frames where misaligned fields are most disruptive.
type protocolWarning struct {
	Summary string
	Key     string
	Symbol  string
}

func (c *Connection) logSuspiciousOutbound(msgID int, fields []string) {
	if len(fields) == 0 {
		return
	}
	var warning protocolWarning
	var ok bool
	var category string

	switch msgID {
	case reqMktData:
		warning, ok = summarizeReqMktDataFields(fields)
		category = "reqMktData"
	case reqContractData:
		warning, ok = summarizeReqContractFields(fields)
		category = "reqContractData"
	case reqHistoricalData:
		warning, ok = summarizeReqHistoricalFields(fields)
		category = "reqHistoricalData"
	default:
		return
	}

	if !ok {
		return
	}

	if warning.Symbol != "" {
		c.recordSuspiciousSummary(warning.Symbol, warning.Summary)
	}

	if c.shouldLogSuspicious(warning.Key) {
		if warning.Symbol != "" {
			ibkrLogger.Warnf("[WARNING] Protocol misalignment for %s via %s: %s", warning.Symbol, category, warning.Summary)
		} else {
			ibkrLogger.Warnf("[WARNING] Protocol misalignment (%s): %s", category, warning.Summary)
		}
	}
}

func (c *Connection) recordSuspiciousSummary(symbol, summary string) {
	if symbol == "" || summary == "" {
		return
	}
	c.suspectMu.Lock()
	c.suspectSummaries[symbol] = summary
	c.suspectMu.Unlock()
}

func (c *Connection) latestSuspiciousSummary(symbol string) string {
	if symbol == "" {
		return ""
	}
	c.suspectMu.Lock()
	summary := c.suspectSummaries[symbol]
	c.suspectMu.Unlock()
	return summary
}

func (c *Connection) allSuspiciousSummaries() []string {
	c.suspectMu.Lock()
	defer c.suspectMu.Unlock()
	if len(c.suspectSummaries) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c.suspectSummaries))
	for sym := range c.suspectSummaries {
		keys = append(keys, sym)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, sym := range keys {
		result = append(result, fmt.Sprintf("%s: %s", sym, c.suspectSummaries[sym]))
	}
	return result
}

func (c *Connection) parserContext(symbol string) string {
	if symbol != "" {
		if summary := c.latestSuspiciousSummary(symbol); summary != "" {
			return summary
		}
	}
	if summaries := c.allSuspiciousSummaries(); len(summaries) > 0 {
		return strings.Join(summaries, "; ")
	}
	return ""
}

func (c *Connection) observeContractTiming(symbol string, elapsed time.Duration, resolved bool) {
	if symbol == "" || elapsed <= 0 {
		return
	}

	c.contractTimingMu.Lock()
	if prev, ok := c.contractTimings[symbol]; !ok || elapsed > prev {
		c.contractTimings[symbol] = elapsed
	}
	c.contractTimingMu.Unlock()

	if elapsed >= 500*time.Millisecond || !resolved {
		status := "resolved"
		if !resolved {
			status = "pending"
		}
		ibkrLogger.Infof("Contract detail latency %s: %s (%s)", symbol, elapsed, status)
	}
}

func (c *Connection) shouldLogSuspicious(key string) bool {
	if key == "" {
		return false
	}
	c.suspectMu.Lock()
	defer c.suspectMu.Unlock()
	if _, exists := c.suspectFlags[key]; exists {
		return false
	}
	c.suspectFlags[key] = struct{}{}
	return true
}

func summarizeReqMktDataFields(fields []string) (protocolWarning, bool) {
	if len(fields) < 8 {
		return protocolWarning{}, false
	}
	reqID := fieldValue(fields, 2)
	conID := fieldValue(fields, 3)
	symbol := fieldValue(fields, 4)
	exchange := fieldValue(fields, 10)
	primary := fieldValue(fields, 11)
	generic := fieldValue(fields, 18)
	snapshot := fieldValue(fields, 19)
	regSnap := fieldValue(fields, 20)
	if reqID != "0" && reqID != "" && conID != "0" {
		return protocolWarning{}, false
	}
	summary := fmt.Sprintf("reqID=%s conID=%s symbol=%s exch=%s primary=%s ticks=%s snap=%s regSnap=%s",
		reqID, conID, symbol, exchange, primary, generic, snapshot, regSnap)
	if conID == "0" {
		summary += " (contract details pending)"
	}
	key := fmt.Sprintf("mkt:%s:%s", symbol, conID)
	return protocolWarning{Summary: summary, Key: key, Symbol: symbol}, true
}

func summarizeReqContractFields(fields []string) (protocolWarning, bool) {
	// Contract detail REQUESTS are supposed to have conID=0 - that's how you ASK for the conID!
	// Only market data and historical requests with conID=0 are problematic.
	// Suppress all warnings for reqContractData to avoid false positives during pre-warming.
	return protocolWarning{}, false
}

func summarizeReqHistoricalFields(fields []string) (protocolWarning, bool) {
	if len(fields) < 6 {
		return protocolWarning{}, false
	}
	reqID := fieldValue(fields, 1)
	if fieldValue(fields, 2) == "" {
		return protocolWarning{}, false
	}
	conID := fieldValue(fields, 2)
	symbol := fieldValue(fields, 3)
	whatToShow := fieldValue(fields, 19)
	if reqID != "0" && conID != "0" {
		return protocolWarning{}, false
	}
	summary := fmt.Sprintf("reqID=%s conID=%s symbol=%s what=%s", reqID, conID, symbol, whatToShow)
	if conID == "0" {
		summary += " (contract details pending)"
	}
	key := fmt.Sprintf("hist:%s:%s", symbol, reqID)
	return protocolWarning{Summary: summary, Key: key, Symbol: symbol}, true
}

func fieldValue(fields []string, idx int) string {
	if idx < 0 || idx >= len(fields) {
		return ""
	}
	return fields[idx]
}

func (c *Connection) logPacketOutbound(payload []byte) {
	c.packetLoggerMu.RLock()
	logger := c.packetLogger
	c.packetLoggerMu.RUnlock()
	if logger == nil || len(payload) == 0 {
		return
	}
	msgID := determineMessageID(c.serverVersion, payload)
	label := fmt.Sprintf("out msgID=%d", msgID)
	clone := make([]byte, len(payload))
	copy(clone, payload)
	logger.Outbound(label, clone)
}

func determineMessageID(serverVersion int, payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	if serverVersion >= 100 && len(payload) >= 4 {
		return int(binary.BigEndian.Uint32(payload[:4]))
	}
	idx := bytes.IndexByte(payload, '\x00')
	if idx == -1 {
		idx = len(payload)
	}
	id, err := strconv.Atoi(string(payload[:idx]))
	if err != nil {
		return -1
	}
	return id
}

// readMessage reads a length-prefixed message
func (c *Connection) readMessage() ([]byte, error) {
	// Read message length (4 bytes)
	lengthBytes := make([]byte, 4)
	// Debug: Reading message length
	if _, err := io.ReadFull(c.reader, lengthBytes); err != nil {
		// Only log non-timeout errors (timeouts are expected when no messages)
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			connectLogger.Warnf("Client %d: Failed to read length: %v", c.config.ClientID, err)
		}
		return nil, err
	}

	msgLength := binary.BigEndian.Uint32(lengthBytes)
	// Debug: Message length = %d bytes

	if msgLength == 0 {
		return []byte{}, nil
	}

	// Sanity cap: 16 MB. The IBKR scanner-parameters XML on a US Pro
	// account with many subscriptions can run 1-3 MB; an old 1 MB cap
	// truncated that response and desynced the stream. 16 MB is well
	// above any realistic IBKR frame and still slams the door on a
	// gateway that's gone rogue (or a wire that's been hijacked).
	if msgLength > 16*1024*1024 {
		return nil, fmt.Errorf("message too large: %d bytes", msgLength)
	}

	// Read message body
	msgBytes := make([]byte, msgLength)
	// Debug: Reading message body
	if _, err := io.ReadFull(c.reader, msgBytes); err != nil {
		connectLogger.Warnf("Client %d: Failed to read body: %v", c.config.ClientID, err)
		return nil, err
	}

	// Debug: Successfully read message
	return msgBytes, nil
}

func (c *Connection) resetHandshakeReady() {
	c.handshakeMu.Lock()
	c.handshakeReady = make(chan struct{})
	c.handshakeMu.Unlock()
}

func (c *Connection) signalHandshakeReady() {
	c.handshakeMu.RLock()
	ch := c.handshakeReady
	c.handshakeMu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (c *Connection) waitForHandshakeReady() error {
	c.handshakeMu.RLock()
	ch := c.handshakeReady
	c.handshakeMu.RUnlock()
	if ch == nil {
		return fmt.Errorf("handshake readiness channel not initialized")
	}

	select {
	case <-ch:
		return nil
	default:
	}

	timeout := c.config.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	select {
	case <-ch:
		return nil
	case <-c.ctx.Done():
		return fmt.Errorf("connection context closed before handshake ready: %w", c.ctx.Err())
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for handshake readiness")
	}
}

// encodeMsg encodes fields into IBKR message format.
//
// The IBKR protocol uses null-terminated fields within a length-prefixed frame.
// We strictly maintain field order per the TWS API reference (e.g., reqMktData v11)
// and avoid introducing extra placeholders that would shift subsequent fields.
func (c *Connection) encodeMsg(fields ...interface{}) []byte {
	var buf bytes.Buffer

	for i, field := range fields {
		if i == 0 && c.serverVersion >= 100 {
			// For v100+: encode msgID as 4-byte binary, NO null terminator
			// (adding null causes field shift per cffaed9 analysis)
			switch v := field.(type) {
			case int:
				binary.Write(&buf, binary.BigEndian, int32(v))
			case int32:
				binary.Write(&buf, binary.BigEndian, v)
			case int64:
				binary.Write(&buf, binary.BigEndian, int32(v))
			default:
				ibkrLogger.Warnf("Non-integer message type %T: %v", field, field)
				buf.WriteString(fmt.Sprintf("%v", v))
				buf.WriteByte('\x00')
			}
			continue
		}

		switch v := field.(type) {
		case int:
			buf.WriteString(strconv.Itoa(v))
		case int64:
			buf.WriteString(strconv.FormatInt(v, 10))
		case float64:
			buf.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		case string:
			buf.WriteString(v)
		case bool:
			if v {
				buf.WriteString("1")
			} else {
				buf.WriteString("0")
			}
		default:
			buf.WriteString(fmt.Sprintf("%v", v))
		}
		buf.WriteByte('\x00')
	}

	return append([]byte(nil), buf.Bytes()...)
}

func (c *Connection) encodeFromFields(fields []string) ([]byte, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("encodeFromFields: empty field slice")
	}
	msgID, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil, fmt.Errorf("encodeFromFields: invalid msg id %q: %w", fields[0], err)
	}
	args := make([]interface{}, len(fields))
	args[0] = msgID
	for i := 1; i < len(fields); i++ {
		args[i] = fields[i]
	}
	return c.encodeMsg(args...), nil
}

func ensureASCII(label, value string) error {
	if value == "" {
		return nil
	}
	if !isASCIIPrintable(value) {
		return fmt.Errorf("%s contains non-ASCII characters", label)
	}
	return nil
}

func isASCIIPrintable(s string) bool {
	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// decodeMessage decodes an IBKR message payload into trimmed string fields.
// Empty fields are dropped; tests that need exact placeholder positions use
// a helper to preserve empties.
func (c *Connection) decodeMessage(msgBytes []byte) []string {
	if len(msgBytes) == 0 {
		return []string{}
	}

	if c.serverVersion >= 100 && len(msgBytes) >= 4 {
		msgType := binary.BigEndian.Uint32(msgBytes[:4])

		if fields, ok := c.tryDecodeProtoMessage(msgType, msgBytes[4:]); ok {
			return fields
		}

		result := []string{strconv.Itoa(int(msgType))}
		remaining := msgBytes[4:]
		if msgType == uint32(msgSystemNotification) {
			result = append(result, string(remaining))
			return result
		}
		raw := bytes.Split(remaining, []byte{'\x00'})
		for _, field := range raw {
			result = append(result, string(field))
		}
		return result
	}

	var result []string
	raw := bytes.Split(msgBytes, []byte{'\x00'})
	for _, field := range raw {
		result = append(result, string(field))
	}
	return result
}

func (c *Connection) tryDecodeProtoMessage(msgType uint32, payload []byte) ([]string, bool) {
	if c.serverVersion < minServerVerProtoBufPlaceOrder {
		return nil, false
	}
	switch msgType {
	case uint32(protoMsgOffset + msgExecutionData):
		fields, err := decodeExecutionDetailsProto(payload, c.serverVersion)
		if err != nil {
			ibkrLogger.Warnf("Failed to decode protobuf execution message: %v", err)
			return nil, false
		}
		return fields, true
	case uint32(protoMsgOffset + msgExecDetailsEnd):
		fields, err := decodeExecutionDetailsEndProto(payload)
		if err != nil {
			ibkrLogger.Warnf("Failed to decode protobuf execution end message: %v", err)
			return nil, false
		}
		return fields, true
	default:
		return nil, false
	}
}

type systemNotification struct {
	tickerID  int64
	timestamp time.Time
	code      int
	message   string
}

func parseSystemNotificationPayload(payload []byte) (*systemNotification, error) {
	var note systemNotification
	buf := payload

	for len(buf) > 0 {
		tag, n := binary.Uvarint(buf)
		if n <= 0 {
			return nil, fmt.Errorf("invalid protobuf tag for system notification")
		}
		buf = buf[n:]
		fieldNum := int(tag >> 3)
		wireType := int(tag & 0x7)

		switch wireType {
		case 0: // varint
			val, m := binary.Uvarint(buf)
			if m <= 0 {
				return nil, fmt.Errorf("invalid varint for system notification field %d", fieldNum)
			}
			buf = buf[m:]
			switch fieldNum {
			case 1:
				if val == math.MaxUint64 {
					note.tickerID = -1
				} else {
					note.tickerID = int64(val)
				}
			case 2:
				note.timestamp = time.Unix(0, int64(val)*int64(time.Millisecond))
			case 3:
				note.code = int(val)
			}
		case 2: // length-delimited (message string)
			length, m := binary.Uvarint(buf)
			if m <= 0 {
				return nil, fmt.Errorf("invalid length for system notification field %d", fieldNum)
			}
			buf = buf[m:]
			if length > uint64(len(buf)) {
				return nil, fmt.Errorf("system notification field %d length overflow", fieldNum)
			}
			val := buf[:length]
			buf = buf[length:]
			if fieldNum == 4 {
				note.message = string(val)
			}
		default:
			return nil, fmt.Errorf("unsupported wire type %d in system notification", wireType)
		}
	}

	if note.message == "" {
		return nil, fmt.Errorf("system notification missing message text")
	}

	return &note, nil
}

// snapshotHandlers returns a copy of the handler list for a message ID.
func (c *Connection) snapshotHandlers(msgID int) []func([]string) {
	c.handlersMu.RLock()
	entries := c.msgHandlers[msgID]
	c.handlersMu.RUnlock()
	if len(entries) == 0 {
		return nil
	}
	fns := make([]func([]string), 0, len(entries))
	for _, entry := range entries {
		if entry.fn != nil {
			fns = append(fns, entry.fn)
		}
	}
	return fns
}

// UnregisterHandler removes a previously registered handler for a message type.
func (c *Connection) UnregisterHandler(msgID int, handlerID uint64) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	entries := c.msgHandlers[msgID]
	if len(entries) == 0 {
		return
	}
	for i, entry := range entries {
		if entry.id == handlerID {
			entries = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	if len(entries) == 0 {
		delete(c.msgHandlers, msgID)
	} else {
		c.msgHandlers[msgID] = entries
	}
}

// RequestContractDetails sends a request to retrieve contract details for a contract.
// Returns the reqID used for the request.
func (c *Connection) RequestContractDetails(contract Contract) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}

	reqID := c.GetNextRequestID()
	if err := c.sendContractDetailsRequest(contract, reqID); err != nil {
		return 0, err
	}
	return reqID, nil
}

func (c *Connection) sendContractDetailsRequest(contract Contract, reqID int) error {
	c.registerReqAlias(reqID, contract)

	// Handle strike field: IB API expects empty string (not "0") for non-option contracts
	strikeField := ""
	if contract.Strike != 0 {
		strikeField = strconv.FormatFloat(contract.Strike, 'f', -1, 64)
	}

	// Primary exchange must be empty during discovery (conID=0).
	// Only populate it after we have a known conID from IBKR.
	primaryField := ""
	if contract.ConID != 0 && contract.PrimaryExch != "" {
		primaryField = contract.PrimaryExch
	}

	// LocalSymbol and TradingClass can be empty during discovery.
	// The official IB API sends these fields but leaves them blank for initial requests.
	localSymbol := contract.LocalSymbol
	tradingClass := contract.TradingClass

	fields := []interface{}{
		reqContractData,
		8,     // version
		reqID, // request id
		0,     // contractId (0 when using fields)
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		strikeField, // use empty string for stocks, actual value for options
		contract.Right,
		"", // multiplier as string
		ifEmpty(contract.Exchange, "SMART"),
		primaryField,
		ifEmpty(contract.Currency, "USD"),
		localSymbol,
		tradingClass,
		0,  // includeExpired = false
		"", // secIdType
		"", // secId
		"", // issuerId (required for server >= 147, added for server 203)
	}

	msg := c.encodeMsg(fields...)

	if contract.SecType == "STK" && contract.PrimaryExch == "" {
		stack := make([]byte, 16384)
		stackLen := runtime.Stack(stack, false)
		decoded := c.decodeMessage(msg)
		wireLogger.Warnf("[cid=%d] reqContractData anomaly symbol=%s reqID=%d primary='' local=%q class=%q fields=%v\n%s",
			c.config.ClientID, contract.Symbol, reqID, localSymbol, tradingClass, decoded, string(stack[:stackLen]))
	}

	return c.sendMessage(msg)
}

func ifEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Public API methods

// SetMarketDataType sets the market data type (live, delayed, etc.)
func (c *Connection) SetMarketDataType(dataType int) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	// Market data types:
	// 1 = Live (real-time)
	// 2 = Frozen (last available data)
	// 3 = Delayed (15-20 min delayed)
	// 4 = Delayed frozen
	// Include protocol version (1) followed by dataType per IB API
	msg := c.encodeMsg(reqMarketDataType, 1, dataType)

	marketLogger.Infof("[cid=%d] Setting market data type to %d (1=Live, 3=Delayed)", c.config.ClientID, dataType)
	return c.sendMessage(msg)
}

// GetMarketDataType returns the current market data type for a reqID.
// 1=RealTime, 2=Frozen, 3=Delayed, 4=DelayedFrozen. 0 if unknown.
func (c *Connection) GetMarketDataType(reqID int) int {
	c.mktDataTypeMu.RLock()
	defer c.mktDataTypeMu.RUnlock()
	if v, ok := c.mktDataType[reqID]; ok {
		return v
	}
	return 0
}

// PlaceOrder sends a placeOrder request to IBKR using the v45+ wire format.
func (c *Connection) PlaceOrder(order *IBKROrder) error {
	if order == nil {
		return fmt.Errorf("order is nil")
	}
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	if c.serverVersion > 0 && c.serverVersion < minServerVerProtoBufPlaceOrder {
		return fmt.Errorf("server version %d is too old for placeOrder v45+ encoding; upgrade TWS/IB Gateway", c.serverVersion)
	}

	if err := ValidateOrder(order); err != nil {
		return err
	}

	stringFields := []struct {
		name  string
		value string
	}{
		{"symbol", order.Symbol},
		{"secType", order.SecType},
		{"exchange", order.Exchange},
		{"currency", order.Currency},
		{"primary exchange", order.PrimaryExch},
		{"local symbol", order.LocalSymbol},
		{"trading class", order.TradingClass},
		{"account", order.Account},
		{"orderRef", order.OrderRef},
		{"tif", order.TIF},
		{"action", order.Action},
	}
	for _, field := range stringFields {
		if err := ensureASCII(field.name, field.value); err != nil {
			return err
		}
	}

	if order.OrderID == 0 {
		order.OrderID = c.GetNextOrderID()
	}
	if order.ClientID == 0 {
		order.ClientID = c.config.ClientID
	}
	if order.Account == "" {
		order.Account = c.account
	}
	if order.Account == "" {
		order.Account = c.config.Account
	}
	if order.OpenClose == "" {
		order.OpenClose = "O"
	}
	if !order.Transmit {
		order.Transmit = true
	}

	fields := clonePlaceOrderFields()
	assignPlaceOrderFields(fields, order)

	interfaces := make([]interface{}, len(fields))
	interfaces[0] = placeOrder
	for i := 1; i < len(fields); i++ {
		interfaces[i] = fields[i]
	}

	msg := c.encodeMsg(interfaces...)
	if err := c.sendMessageWithType(msg, RequestTypeOrder); err != nil {
		return err
	}

	now := time.Now()
	c.ordersMu.Lock()
	order.Status = "Submitted"
	order.SubmittedTime = now
	if order.CreatedTime.IsZero() {
		order.CreatedTime = now
	}
	if order.Remaining == 0 {
		order.Remaining = order.TotalQty
	}
	c.openOrders[order.OrderID] = order
	c.ordersMu.Unlock()

	return nil
}

// CancelOrder sends a cancelOrder request for an existing order ID.
func (c *Connection) CancelOrder(orderID int) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	fields := []interface{}{cancelOrder, 1, orderID}
	msg := c.encodeMsg(fields...)
	if err := c.sendMessageWithType(msg, RequestTypeOrder); err != nil {
		return err
	}

	c.ordersMu.Lock()
	if ord, ok := c.openOrders[orderID]; ok {
		ord.Status = "Cancelled"
		now := time.Now()
		if ord.CancelledTime == nil {
			ord.CancelledTime = &now
		}
		delete(c.openOrders, orderID)
	}
	c.ordersMu.Unlock()

	return nil
}

func clonePlaceOrderFields() []string {
	fields := make([]string, len(placeOrderBaseFields))
	copy(fields, placeOrderBaseFields)
	return fields
}

func assignPlaceOrderFields(fields []string, order *IBKROrder) {
	setIntField(fields, 1, order.OrderID)
	setIntField(fields, 2, order.ConID)
	setStringField(fields, 3, order.Symbol)
	setStringField(fields, 4, order.SecType)
	setStringField(fields, 5, order.Expiry)
	if order.Strike != 0 {
		setFloatField(fields, 6, order.Strike)
	}
	setStringField(fields, 7, order.Right)
	setStringField(fields, 8, order.Multiplier)
	if order.Exchange != "" {
		setStringField(fields, 9, order.Exchange)
	}
	setStringField(fields, 10, order.PrimaryExch)
	if order.Currency != "" {
		setStringField(fields, 11, order.Currency)
	}
	setStringField(fields, 12, order.LocalSymbol)
	setStringField(fields, 13, order.TradingClass)
	setStringField(fields, 14, order.SecIDType)
	setStringField(fields, 15, order.SecID)
	setStringField(fields, 16, strings.ToUpper(order.Action))
	setIntField(fields, 17, order.TotalQty)
	setStringField(fields, 18, strings.ToUpper(order.OrderType))
	if order.OrderType != "MKT" && order.LmtPrice != 0 {
		setFloatField(fields, 19, order.LmtPrice)
	}
	if order.AuxPrice != 0 {
		setFloatField(fields, 20, order.AuxPrice)
	}
	setStringField(fields, 21, strings.ToUpper(order.TIF))
	setStringField(fields, 22, order.OcaGroup)
	setStringField(fields, 23, order.Account)
	if order.OpenClose != "" {
		setStringField(fields, 24, order.OpenClose)
	}
	setIntFieldWithZero(fields, 25, order.Origin)
	setStringField(fields, 26, order.OrderRef)
	setBoolField(fields, 27, order.Transmit)
	setIntFieldWithZero(fields, 28, order.ParentID)
	setBoolField(fields, 29, order.BlockOrder)
	setBoolField(fields, 30, order.SweepToFill)
	setIntField(fields, 31, order.DisplaySize)
	setIntFieldWithZero(fields, 32, order.TriggerMethod)
	setBoolField(fields, 33, order.OutsideRth)
	setBoolField(fields, 34, order.Hidden)
	if order.DiscretionaryAmt != 0 {
		setFloatField(fields, 36, order.DiscretionaryAmt)
	}
	setStringField(fields, 37, order.GoodAfterTime)
	setStringField(fields, 38, order.GoodTillDate)
	setStringField(fields, 39, order.FaGroup)
	setStringField(fields, 40, order.FaMethod)
	setStringField(fields, 41, order.FaPercentage)
	setStringField(fields, 42, order.FaProfile)
	setStringField(fields, 43, order.ModelCode)
	setIntFieldWithZero(fields, 44, order.ShortSaleSlot)
	setStringField(fields, 45, order.DesignatedLocation)
	if order.ExemptCode != 0 {
		setIntFieldWithZero(fields, 46, order.ExemptCode)
	}
	setIntFieldWithZero(fields, 47, order.OcaType)
	setStringField(fields, 48, order.Rule80A)
	setStringField(fields, 49, order.SettlingFirm)
	setBoolField(fields, 50, order.AllOrNone)
	setIntField(fields, 51, order.MinQty)
	if order.PercentOffset != 0 {
		setFloatField(fields, 52, order.PercentOffset)
	}
	setBoolField(fields, 53, order.ETradeOnly)
	setBoolField(fields, 54, order.FirmQuoteOnly)
	if order.NbboPriceCap != 0 {
		setFloatField(fields, 55, order.NbboPriceCap)
	}
	setIntField(fields, 56, order.AuctionStrategy)
	if order.StartingPrice != 0 {
		setFloatField(fields, 57, order.StartingPrice)
	}
	if order.StockRefPrice != 0 {
		setFloatField(fields, 58, order.StockRefPrice)
	}
	if order.Delta != 0 {
		setFloatField(fields, 59, order.Delta)
	}
	if order.StockRangeLower != 0 {
		setFloatField(fields, 60, order.StockRangeLower)
	}
	if order.StockRangeUpper != 0 {
		setFloatField(fields, 61, order.StockRangeUpper)
	}
	setBoolField(fields, 62, order.OverridePercentageConstraints)
	if order.Volatility != 0 {
		setFloatField(fields, 63, order.Volatility)
	}
	setIntField(fields, 64, order.VolatilityType)
	setStringField(fields, 65, order.DeltaNeutralOrderType)
	if order.DeltaNeutralAuxPrice != 0 {
		setFloatField(fields, 66, order.DeltaNeutralAuxPrice)
	}
	setIntField(fields, 67, order.DeltaNeutralConID)
	setStringField(fields, 68, order.DeltaNeutralSettlingFirm)
	setStringField(fields, 69, order.DeltaNeutralClearingAccount)
	setStringField(fields, 70, order.DeltaNeutralClearingIntent)
	setStringField(fields, 71, order.DeltaNeutralOpenClose)
	setBoolField(fields, 72, order.DeltaNeutralShortSale)
	setIntField(fields, 73, order.DeltaNeutralShortSaleSlot)
	setStringField(fields, 74, order.DeltaNeutralDesignatedLocation)
	setIntField(fields, 75, order.ContinuousUpdate)
	setIntField(fields, 76, order.ReferencePriceType)
	if order.TrailStopPrice != 0 {
		setFloatField(fields, 77, order.TrailStopPrice)
	}
	if order.TrailingPercent != 0 {
		setFloatField(fields, 78, order.TrailingPercent)
	}
	if order.BasisPoints != 0 {
		setFloatField(fields, 79, order.BasisPoints)
	}
	setIntField(fields, 80, order.BasisPointsType)
	setIntField(fields, 81, order.ScaleInitLevelSize)
	setIntField(fields, 82, order.ScaleSubsLevelSize)
	if order.ScalePriceIncrement != 0 {
		setFloatField(fields, 83, order.ScalePriceIncrement)
	}
	if order.ScalePriceAdjustValue != 0 {
		setFloatField(fields, 84, order.ScalePriceAdjustValue)
	}
	setIntField(fields, 85, order.ScalePriceAdjustInterval)
	if order.ScaleProfitOffset != 0 {
		setFloatField(fields, 86, order.ScaleProfitOffset)
	}
	setBoolField(fields, 87, order.ScaleAutoReset)
	setIntField(fields, 88, order.ScaleInitPosition)
	setIntField(fields, 89, order.ScaleInitFillQty)
	setBoolField(fields, 90, order.ScaleRandomPercent)
	setStringField(fields, 91, order.HedgeType)
	setStringField(fields, 92, order.HedgeParam)
	setBoolField(fields, 93, order.OptOutSmartRouting)
	setStringField(fields, 94, order.ClearingAccount)
	setStringField(fields, 95, order.ClearingIntent)
	setBoolField(fields, 96, order.NotHeld)
}

func setStringField(fields []string, idx int, value string) {
	if idx >= len(fields) || value == "" {
		return
	}
	fields[idx] = value
}

func setIntField(fields []string, idx int, value int) {
	if idx >= len(fields) || value == 0 {
		return
	}
	fields[idx] = strconv.Itoa(value)
}

func setIntFieldWithZero(fields []string, idx int, value int) {
	if idx >= len(fields) {
		return
	}
	fields[idx] = strconv.Itoa(value)
}

func setFloatField(fields []string, idx int, value float64) {
	if idx >= len(fields) {
		return
	}
	fields[idx] = strconv.FormatFloat(value, 'f', -1, 64)
}

func setBoolField(fields []string, idx int, value bool) {
	if idx >= len(fields) {
		return
	}
	if value {
		fields[idx] = "1"
	} else {
		fields[idx] = "0"
	}
}

// RequestOpenOrders requests the broker to send all open orders (scaffold no-op)
func (c *Connection) RequestOpenOrders() error { return nil }

// GetNextOrderID returns the next synthetic order ID used by tests and scaffolding.
func (c *Connection) GetNextOrderID() int {
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	if c.nextOrderID <= 0 {
		c.nextOrderID = 1
	}
	id := c.nextOrderID
	c.nextOrderID++
	return id
}

// RequestMarketData subscribes to market data for a symbol
func (c *Connection) RequestMarketData(symbol string) (int, error) {
	secType, exchange, currency, primaryExchange := classifySymbol(symbol)
	localSymbol, tradingClassHint := contractDisplayHints(symbol, secType)

	contract := Contract{
		Symbol:       symbol,
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  primaryExchange,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClassHint,
	}

	// For equities IBKR expects primary exchange blank unless explicitly requested.
	if contract.SecType == "STK" {
		contract.PrimaryExch = ""
	}

	return c.RequestMarketDataWithContract(contract, "100,101,104,106,165,221,233", false, false)
}

func (c *Connection) RequestMarketDataWithContract(contract Contract, genericTicks string, snapshot bool, regulatorySnap bool) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestMarketData"); err != nil {
		return 0, err
	}
	if contract.Symbol == "" {
		return 0, fmt.Errorf("contract symbol is required for market data")
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if err := ensureASCII("symbol", contract.Symbol); err != nil {
		return 0, err
	}

	c.reqIDMu.Lock()
	reqID := c.reqIDSeq
	c.reqIDSeq++
	c.reqIDMu.Unlock()

	// Copy the contract to avoid caller mutations affecting queued send.
	contractCopy := contract
	c.registerReqAlias(reqID, contractCopy)

	fields := c.buildReqMktDataFields(contractCopy, reqID, genericTicks, snapshot, regulatorySnap)
	msg := c.encodeMsg(fields...)

	if err := c.rateLimiter.AcquireMarketDataSlot(c.ctx); err != nil {
		return 0, fmt.Errorf("market data subscription limit reached: %w", err)
	}

	marketLogger.Infof("Requesting market data for %s (ReqID: %d, SecType: %s, Exchange: %s, Primary: %s, ConID: %d)",
		contractCopy.Symbol, reqID, contractCopy.SecType, contractCopy.Exchange, contractCopy.PrimaryExch, contractCopy.ConID)

	if err := c.sendMessageWithType(msg, RequestTypeMarketData); err != nil {
		c.rateLimiter.ReleaseMarketDataSlot()
		return 0, fmt.Errorf("failed to request market data: %w", err)
	}

	return reqID, nil
}

func (c *Connection) requireServerVersion(method string) error {
	if c.serverVersion == 0 {
		return fmt.Errorf("%s: server version not negotiated", method)
	}
	if c.serverVersion < minServerVersionRequired {
		return fmt.Errorf("%s: server version %d is too old (minimum: %d)", method, c.serverVersion, minServerVersionRequired)
	}
	return nil
}

func (c *Connection) buildReqMktDataFields(contract Contract, reqID int, genericTicks string, snapshot bool, regulatorySnap bool) []interface{} {
	// All fields required for serverVersion >= 124
	// Per official IBKR API reqMktData message version 11

	strikeField := ""
	if contract.Strike != 0 {
		strikeField = strconv.FormatFloat(contract.Strike, 'f', -1, 64)
	}

	multiplierField := ""
	if contract.Multiplier != 0 {
		multiplierField = strconv.Itoa(contract.Multiplier)
	}

	fields := []interface{}{
		reqMktData,
		11, // message version
		reqID,
		contract.ConID,
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		strikeField,
		contract.Right,
		multiplierField,
		contract.Exchange,
		contract.PrimaryExch,
		contract.Currency,
		contract.LocalSymbol,
		contract.TradingClass,
	}

	if contract.SecType == "BAG" {
		fields = append(fields, 0) // combo legs count
	}

	fields = append(fields,
		false, // deltaNeutral
		genericTicks,
		snapshot,
		regulatorySnap,
		"", // mktDataOptions (chart options)
	)

	return fields
}

func optionContractKey(symbol, expiry string, strike float64, right string) string {
	key := strings.ToUpper(strings.TrimSpace(symbol)) + "|" + strings.TrimSpace(expiry) + "|" + strconv.FormatFloat(strike, 'f', 6, 64) + "|" + strings.ToUpper(strings.TrimSpace(right))
	return key
}

func applyContractDetailLite(detail ContractDetailsLite, contract *Contract) {
	if contract == nil {
		return
	}
	if detail.Exchange != "" {
		contract.Exchange = detail.Exchange
	}
	if detail.PrimaryExch != "" {
		contract.PrimaryExch = detail.PrimaryExch
	}
	if detail.ConID != 0 {
		contract.ConID = detail.ConID
	}
	if detail.LocalSymbol != "" {
		contract.LocalSymbol = detail.LocalSymbol
	}
	if detail.TradingClass != "" {
		contract.TradingClass = detail.TradingClass
	}
}

// RequestHistoricalData submits an HMDS request for historical data.
// The beforeSend callback is invoked after the reqID is allocated but before
// the message is sent, allowing callers to register tracking state safely.
func (c *Connection) RequestHistoricalData(contract Contract, endDateTime, duration, barSize, whatToShow string, useRTH bool, includeExpired bool, formatDate int, keepUpToDate bool, beforeSend func(int)) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestHistoricalData"); err != nil {
		return 0, err
	}

	// Defensive assertion: Prevent byte-shift MART/BOE errors by blocking conID=0 requests
	// at the protocol encoder level. This is the third layer of defense (after guards in
	// FetchHistoricalDailyBars:2532 and fetchHistoricalWithContract:2668).
	if contract.ConID == 0 {
		ibkrLogger.Errorf("[cid=%d] PROTOCOL VIOLATION: attempted historical request with conID=0 for symbol=%s exchange=%s (would cause MART byte-shift error at IBKR gateway)",
			c.config.ClientID, contract.Symbol, contract.Exchange)
		return 0, fmt.Errorf("PROTOCOL VIOLATION: attempted historical request with conID=0 for symbol=%s exchange=%s (would cause MART byte-shift error at IBKR gateway)",
			contract.Symbol, contract.Exchange)
	}

	duration = normalizeHistoricalDuration(duration)

	c.reqIDMu.Lock()
	reqID := c.reqIDSeq
	c.reqIDSeq++
	c.reqIDMu.Unlock()

	multiplier := ""
	if contract.Multiplier != 0 {
		multiplier = strconv.Itoa(contract.Multiplier)
	}

	// Handle strike field: IB API expects empty string (not "0") for non-option contracts
	strikeField := ""
	if contract.Strike != 0 {
		strikeField = strconv.FormatFloat(contract.Strike, 'f', -1, 64)
	}

	fields := make([]interface{}, 0, 34)
	fields = append(fields,
		reqHistoricalData,
		reqID,
		contract.ConID,
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		strikeField, // use empty string for stocks/indices, actual value for options
		contract.Right,
		multiplier,
		contract.Exchange,
		contract.PrimaryExch,
		contract.Currency,
		contract.LocalSymbol,
		contract.TradingClass, // Always sent (MIN_SERVER_VER_TRADING_CLASS=68 < 124)
	)

	fields = append(fields, includeExpired)
	if contract.SecIDType != "" || contract.SecID != "" {
		fields = append(fields, contract.SecIDType, contract.SecID)
	}

	fields = append(fields,
		endDateTime,
		barSize,    // IBKR API encodes barSizeSetting before durationStr (see twsapi v10)
		duration,   // durationStr follows barSizeSetting
		useRTH,     // useRTH flag is encoded before whatToShow
		whatToShow, // whatToShow string must follow useRTH
		formatDate,
	)

	if contract.SecType == "BAG" {
		fields = append(fields, 0) // combo legs count (unsupported)
	}

	// Always sent for serverVersion >= 124
	fields = append(fields, keepUpToDate)

	// Always sent (MIN_SERVER_VER_LINKING=70 < 124)
	fields = append(fields, "") // chart options (unused)

	msg := c.encodeMsg(fields...)

	// Enhanced diagnostics: Log contract details when wire hex logging is enabled
	// This helps diagnose byte-shift issues (MART/BOE errors)
	if c.logWireHex {
		wireLogger.Debugf("[cid=%d] Historical reqID=%d conID=%d symbol=%s exchange=%s primary=%s fields=%d msgLen=%d",
			c.config.ClientID, reqID, contract.ConID, contract.Symbol, contract.Exchange, contract.PrimaryExch, len(fields), len(msg))
	}

	if beforeSend != nil {
		beforeSend(reqID)
	}

	if err := c.sendMessageWithType(msg, RequestTypeHistorical); err != nil {
		return 0, fmt.Errorf("failed to request historical data: %w", err)
	}

	return reqID, nil
}

// normalizeHistoricalDuration coerces legacy day-based durations into IBKR-compliant
// year tokens when they exceed the 365-day limit. This prevents error 321 without
// forcing callers to manually convert lookbacks.
func normalizeHistoricalDuration(duration string) string {
	parts := strings.Fields(strings.TrimSpace(duration))
	if len(parts) != 2 {
		return duration
	}

	value, err := strconv.Atoi(parts[0])
	if err != nil || value <= 0 {
		return duration
	}

	unit := strings.ToUpper(parts[1])
	switch unit {
	case "D", "DAY", "DAYS":
		if value > 365 {
			return formatHistoricalDuration(value)
		}
		return fmt.Sprintf("%d D", value)
	default:
		return duration
	}
}

// CancelHistoricalData cancels an active historical data subscription/request.
func (c *Connection) CancelHistoricalData(reqID int) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(cancelHistoricalData, 1, reqID)
	return c.sendMessageWithType(msg, RequestTypeHistorical)
}

// RequestSecDefOptParams issues msg 78 (reqSecDefOptParams) to enumerate the
// option chain (expirations + strikes) for an underlying. The IBKR wire format
// (verified against ibapi.client.EClient.reqSecDefOptParams) has no version
// field — six total fields: msgID, reqID, underlyingSymbol, futFopExchange
// (empty for STK options), underlyingSecType, underlyingConId. The beforeSend
// callback runs after the reqID is allocated but before the message hits the
// wire so callers can register their per-request handler atomically.
func (c *Connection) RequestSecDefOptParams(underlyingSymbol, futFopExchange, underlyingSecType string, underlyingConId int, beforeSend func(int)) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if underlyingConId == 0 {
		return 0, fmt.Errorf("reqSecDefOptParams: underlying conID required")
	}

	c.reqIDMu.Lock()
	reqID := c.reqIDSeq
	c.reqIDSeq++
	c.reqIDMu.Unlock()

	msg := c.encodeMsg(
		reqSecDefOptParams,
		reqID,
		underlyingSymbol,
		futFopExchange,
		underlyingSecType,
		underlyingConId,
	)

	if beforeSend != nil {
		beforeSend(reqID)
	}

	if err := c.sendMessage(msg); err != nil {
		return 0, fmt.Errorf("failed to request sec def opt params: %w", err)
	}
	return reqID, nil
}

// RequestMarketDataWithPrimary subscribes to market data with an explicit primary exchange hint.
// This helps IBKR route to venues that provide better pre/after-hours coverage.
func (c *Connection) RequestMarketDataWithPrimary(symbol string, primaryExchange string) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestMarketDataWithPrimary"); err != nil {
		return 0, err
	}
	if err := ensureASCII("symbol", symbol); err != nil {
		return 0, err
	}
	if err := ensureASCII("primary exchange", primaryExchange); err != nil {
		return 0, err
	}

	c.reqIDMu.Lock()
	reqID := c.reqIDSeq
	c.reqIDSeq++
	c.reqIDMu.Unlock()

	// Determine security type and base exchange based on symbol
	secType, exchange, currency, primaryHint := classifySymbol(symbol)
	if primaryExchange == "" {
		primaryExchange = primaryHint
	}

	localSymbol, tradingClassHint := contractDisplayHints(symbol, secType)

	contract := Contract{
		Symbol:       symbol,
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  primaryExchange,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClassHint,
	}

	msg := c.encodeMsg(c.buildReqMktDataFields(contract, reqID, "100,101,104,106,165,221,233", false, false)...)

	if err := c.rateLimiter.AcquireMarketDataSlot(c.ctx); err != nil {
		return 0, fmt.Errorf("market data subscription limit reached: %w", err)
	}
	marketLogger.Infof("Requesting market data for %s (ReqID: %d, SecType: %s, Exch: %s, Primary: %s)",
		symbol, reqID, secType, exchange, contract.PrimaryExch)

	if err := c.sendMessageWithType(msg, RequestTypeMarketData); err != nil {
		c.rateLimiter.ReleaseMarketDataSlot()
		return 0, fmt.Errorf("failed to request market data: %w", err)
	}
	return reqID, nil
}

// RequestOptionsMarketData subscribes to market data for an option contract.
// ctx cancellation aborts the contract-resolution round trip, which would
// otherwise block up to 5 s × N exchange attempts even if the caller has
// already given up.
func (c *Connection) RequestOptionsMarketData(ctx context.Context, symbol string, expiry string, strike float64, right string) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.requireServerVersion("RequestOptionsMarketData"); err != nil {
		return 0, err
	}

	c.reqIDMu.Lock()
	reqID := c.reqIDSeq
	c.reqIDSeq++
	c.reqIDMu.Unlock()

	secType := "OPT"
	exchange := "SMART"
	primaryExchange := "CBOE"
	currency := "USD"

	expiryFormatted := expiry
	if len(expiry) == 10 && strings.Contains(expiry, "-") {
		expiryFormatted = strings.ReplaceAll(expiry, "-", "")
	}

	localSymbol, tradingClassHint := contractDisplayHints(symbol, secType)

	contract := Contract{
		Symbol:       symbol,
		SecType:      secType,
		Expiry:       expiryFormatted,
		Strike:       strike,
		Right:        strings.ToUpper(right),
		Multiplier:   100,
		Exchange:     exchange,
		PrimaryExch:  primaryExchange,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClassHint,
	}

	if err := c.resolveOptionContract(ctx, &contract, 5*time.Second); err != nil {
		return 0, fmt.Errorf("resolve option contract failed: %w", err)
	}

	msg := c.encodeMsg(c.buildReqMktDataFields(contract, reqID, "100,101,104,106,221", false, false)...)

	if err := c.rateLimiter.AcquireMarketDataSlot(c.ctx); err != nil {
		return 0, fmt.Errorf("market data subscription limit reached: %w", err)
	}

	marketLogger.Infof("Requesting options market data for %s %s %.2f %s (ReqID: %d)",
		symbol, expiryFormatted, strike, right, reqID)

	if err := c.sendMessageWithType(msg, RequestTypeMarketData); err != nil {
		c.rateLimiter.ReleaseMarketDataSlot()
		return 0, fmt.Errorf("failed to request options market data: %w", err)
	}

	return reqID, nil
}

func (c *Connection) resolveOptionContract(ctx context.Context, contract *Contract, timeout time.Duration) error {
	if contract == nil {
		return fmt.Errorf("option contract is nil")
	}
	if contract.ConID != 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}

	key := optionContractKey(contract.Symbol, contract.Expiry, contract.Strike, contract.Right)

	c.optionContractMu.RLock()
	if cached, ok := c.optionContractCache[key]; ok && cached.ConID != 0 {
		c.optionContractMu.RUnlock()
		applyContractDetailLite(cached, contract)
		return nil
	}
	c.optionContractMu.RUnlock()

	type attempt struct {
		Contract Contract
		Label    string
	}
	buildAttempts := func() []attempt {
		attempts := []attempt{{Contract: *contract, Label: contract.Exchange}}

		if contract.PrimaryExch != "" && !strings.EqualFold(contract.Exchange, contract.PrimaryExch) {
			alt := *contract
			alt.Exchange = contract.PrimaryExch
			alt.PrimaryExch = ""
			attempts = append(attempts, attempt{Contract: alt, Label: contract.PrimaryExch})
		}

		if !strings.EqualFold(contract.Exchange, "CBOE") {
			alt := *contract
			alt.Exchange = "CBOE"
			alt.PrimaryExch = ""
			attempts = append(attempts, attempt{Contract: alt, Label: "CBOE"})
		}

		if !strings.EqualFold(contract.Exchange, "SMART") {
			alt := *contract
			alt.Exchange = "SMART"
			alt.PrimaryExch = ""
			attempts = append(attempts, attempt{Contract: alt, Label: "SMART"})
		}

		seen := make(map[string]struct{})
		dedup := make([]attempt, 0, len(attempts))
		for _, att := range attempts {
			key := strings.ToUpper(att.Contract.Exchange) + "|" + strings.ToUpper(att.Contract.PrimaryExch)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			dedup = append(dedup, att)
		}
		return dedup
	}

	var lastErr error
	for _, att := range buildAttempts() {
		if err := ctx.Err(); err != nil {
			return err
		}
		detail, err := c.fetchOptionContractDetail(ctx, att.Contract, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue
		}
		if detail == nil || detail.ConID == 0 {
			lastErr = fmt.Errorf("contract details unavailable for option %s %s %.2f%s (exchange=%s)", contract.Symbol, contract.Expiry, contract.Strike, contract.Right, att.Label)
			continue
		}

		applyContractDetailLite(*detail, contract)

		c.optionContractMu.Lock()
		c.optionContractCache[key] = *detail
		c.optionContractMu.Unlock()
		return nil
	}

	if lastErr != nil {
		return lastErr
	}

	return fmt.Errorf("contract details unavailable for option %s %s %.2f%s", contract.Symbol, contract.Expiry, contract.Strike, contract.Right)
}

func (c *Connection) fetchOptionContractDetail(ctx context.Context, contract Contract, timeout time.Duration) (*ContractDetailsLite, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}

	detailsCh := make(chan ContractDetailsLite, 8)
	doneCh := make(chan struct{})

	serverVersion := c.serverVersion
	reqID := c.GetNextRequestID()

	dataHandlerID := c.RegisterHandler(msgContractData, func(fields []string) {
		if lite, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			select {
			case detailsCh <- *lite:
			default:
			}
		}
	})

	endHandlerID := c.RegisterHandler(msgContractDataEnd, func(fields []string) {
		if len(fields) < 3 {
			return
		}
		if id, err := strconv.Atoi(fields[2]); err == nil && id == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})

	err := c.sendContractDetailsRequest(contract, reqID)
	if err != nil {
		c.UnregisterHandler(msgContractData, dataHandlerID)
		c.UnregisterHandler(msgContractDataEnd, endHandlerID)
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	defer c.UnregisterHandler(msgContractData, dataHandlerID)
	defer c.UnregisterHandler(msgContractDataEnd, endHandlerID)

	var selected *ContractDetailsLite
	prefer := func(candidate ContractDetailsLite) bool {
		if candidate.ConID == 0 {
			return false
		}
		if selected == nil {
			return true
		}
		// Prefer details that match the requested exchange or primary.
		if !strings.EqualFold(selected.Exchange, contract.Exchange) && strings.EqualFold(candidate.Exchange, contract.Exchange) {
			return true
		}
		if !strings.EqualFold(selected.PrimaryExch, contract.PrimaryExch) && strings.EqualFold(candidate.PrimaryExch, contract.PrimaryExch) {
			return true
		}
		return false
	}

	for {
		select {
		case detail := <-detailsCh:
			if prefer(detail) {
				// copy
				d := detail
				selected = &d
			}
		case <-doneCh:
			if selected != nil {
				return selected, nil
			}
			return nil, fmt.Errorf("contract details unavailable for option %s %s %.2f%s", contract.Symbol, contract.Expiry, contract.Strike, contract.Right)
		case <-timer.C:
			return nil, fmt.Errorf("timeout waiting for option contract details for %s %s %.2f%s", contract.Symbol, contract.Expiry, contract.Strike, contract.Right)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// CancelMarketData cancels market data subscription
func (c *Connection) CancelMarketData(reqID int) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(cancelMktData, 1, reqID)
	err := c.sendMessageWithType(msg, RequestTypeMarketData)

	// Release market data slot when canceling subscription
	c.rateLimiter.ReleaseMarketDataSlot()

	return err
}

// RequestPositions requests current positions
func (c *Connection) RequestPositions() error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	// Clear existing positions before requesting new ones
	c.positionsMu.Lock()
	c.positions = make(map[string]*RawPosition)
	c.positionsMu.Unlock()

	// Clear the end channel to ensure we wait for new data
	select {
	case <-c.positionsEndChan:
	default:
	}

	msg := c.encodeMsg(reqPositions, "1")
	return c.sendMessage(msg)
}

// WaitForPositionsEnd waits for position sync to complete with timeout
func (c *Connection) WaitForPositionsEnd(timeout time.Duration) error {
	select {
	case <-c.positionsEndChan:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for positions end")
	}
}

// RequestAccountSummary requests account summary data
func (c *Connection) RequestAccountSummary(reqID int, tags string) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	// If no tags specified, request all important ones
	if tags == "" {
		tags = "NetLiquidation,BuyingPower,TotalCashValue,GrossPositionValue,UnrealizedPnL,RealizedPnL"
	}

	// Clear the end channel to ensure we wait for new data
	select {
	case <-c.acctSummaryEndChan:
	default:
	}

	// reqAccountSummary message:
	// 0: msgID (62)
	// 1: version (1)
	// 2: reqId
	// 3: group ("All" to get all accounts)
	// 4: tags (comma-separated list)
	msg := c.encodeMsg(reqAccountSummary, "1", reqID, "All", tags)
	return c.sendMessage(msg)
}

// WaitForAccountSummaryEnd waits for account summary to complete with timeout
func (c *Connection) WaitForAccountSummaryEnd(timeout time.Duration) error {
	select {
	case <-c.acctSummaryEndChan:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for account summary end")
	}
}

// CancelAccountSummary cancels account summary subscription
func (c *Connection) CancelAccountSummary(reqID int) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(cancelAccountSummary, "1", reqID)
	return c.sendMessage(msg)
}

// GetPositions returns all current positions
func (c *Connection) GetPositions() map[string]*RawPosition {
	c.positionsMu.RLock()
	defer c.positionsMu.RUnlock()

	// Return a copy to prevent external modification
	result := make(map[string]*RawPosition)
	for k, v := range c.positions {
		result[k] = v
	}
	return result
}

// GetPosition returns a specific position by key
func (c *Connection) GetPosition(key string) (*RawPosition, bool) {
	c.positionsMu.RLock()
	defer c.positionsMu.RUnlock()

	pos, exists := c.positions[key]
	return pos, exists
}

// GetAccountCode returns the last known managed account code.
func (c *Connection) GetAccountCode() string {
	c.accountMu.RLock()
	defer c.accountMu.RUnlock()
	return c.account
}

// GetAccountSummary returns the account summary data
func (c *Connection) GetAccountSummary() map[string]string {
	c.accountMu.RLock()
	defer c.accountMu.RUnlock()

	// Return a copy to prevent external modification
	result := make(map[string]string)
	for k, v := range c.accountSummary {
		result[k] = v
	}
	return result
}

// GetAccountValue returns a specific account value
func (c *Connection) GetAccountValue(key string) (string, bool) {
	c.accountMu.RLock()
	defer c.accountMu.RUnlock()

	value, exists := c.accountSummary[key]
	return value, exists
}

// RequestAccountUpdates subscribes to account updates
func (c *Connection) RequestAccountUpdates(account string) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(reqAcctData, "2", "1", account)
	return c.sendMessage(msg)
}

// RequestCurrentTime requests the current server time (used for heartbeat)
func (c *Connection) RequestExecutions(filter ExecutionFilter) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestExecutions"); err != nil {
		return 0, err
	}

	reqID := filter.ReqID
	if c.serverVersion >= minServerVerExecutionDataChain {
		if reqID == 0 {
			c.reqIDMu.Lock()
			reqID = c.reqIDSeq
			c.reqIDSeq++
			c.reqIDMu.Unlock()
		}
	} else {
		reqID = 0
	}

	if (filter.LastNDays > 0 || len(filter.SpecificDates) > 0) && c.serverVersion < minServerVerParametrizedDaysExecutions {
		return reqID, fmt.Errorf("server version %d does not support parametrized execution requests", c.serverVersion)
	}

	fields := make([]interface{}, 0, 16)
	fields = append(fields, reqExecutions)
	fields = append(fields, 3) // message version

	if c.serverVersion >= minServerVerExecutionDataChain {
		fields = append(fields, reqID)
	}

	if c.serverVersion >= 9 {
		fields = append(fields,
			filter.ClientID,
			filter.Account,
			filter.Time,
			filter.Symbol,
			filter.SecType,
			filter.Exchange,
			filter.Side,
		)

		if c.serverVersion >= minServerVerParametrizedDaysExecutions {
			lastNDays := filter.LastNDays
			if lastNDays <= 0 {
				lastNDays = 2147483647
			}
			fields = append(fields, lastNDays)

			if len(filter.SpecificDates) > 0 {
				fields = append(fields, len(filter.SpecificDates))
				for _, date := range filter.SpecificDates {
					fields = append(fields, date)
				}
			} else {
				fields = append(fields, 0)
			}
		}
	}

	msg := c.encodeMsg(fields...)
	if err := c.sendMessage(msg); err != nil {
		return reqID, err
	}
	return reqID, nil
}

func (c *Connection) RequestCurrentTime() error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(reqCurrentTime, "1")
	// Use high priority for heartbeats
	return c.rateLimiter.SubmitWithPriority(RequestTypeHeartbeat, func() error {
		return c.withTransport(false, func() error {
			lengthBytes := make([]byte, 4)
			binary.BigEndian.PutUint32(lengthBytes, uint32(len(msg)))

			if _, err := c.writer.Write(lengthBytes); err != nil {
				return err
			}

			if _, err := c.writer.Write(msg); err != nil {
				return err
			}

			return c.writer.Flush()
		})
	}, 1000, 0) // High priority, no retries for heartbeats
}

// pauseTransport prevents non-handshake writers from accessing the socket.
// It is idempotent and safe to call multiple times.
func (c *Connection) pauseTransport() {
	if c.transportCond == nil {
		return
	}
	c.transportMu.Lock()
	c.transportPaused = true
	c.transportMu.Unlock()
}

// resumeTransport unblocks any goroutines waiting to send IBKR messages.
func (c *Connection) resumeTransport() {
	if c.transportCond == nil {
		return
	}
	c.transportMu.Lock()
	if !c.transportPaused {
		c.transportMu.Unlock()
		return
	}
	c.transportPaused = false
	c.transportCond.Broadcast()
	c.transportMu.Unlock()
}

// withTransport provides exclusive, sequential access to the underlying writer.
// When allowDuringPause is false the call will block until the transport is resumed,
// ensuring handshake and reconnect flows can run without interleaving payloads from
// other goroutines.
func (c *Connection) withTransport(allowDuringPause bool, fn func() error) error {
	if c.transportCond == nil {
		return fn()
	}
	c.transportMu.Lock()
	for c.transportPaused && !allowDuringPause {
		c.transportCond.Wait()
	}
	defer c.transportMu.Unlock()
	return fn()
}

// RegisterHandler registers a handler for a specific message type and returns an identifier.
func (c *Connection) RegisterHandler(msgID int, handler func([]string)) uint64 {
	if handler == nil {
		return 0
	}
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.handlerSeq++
	entry := handlerEntry{id: c.handlerSeq, fn: handler}
	c.msgHandlers[msgID] = append(c.msgHandlers[msgID], entry)
	return entry.id
}

// GetNextRequestID returns the next available request ID
func (c *Connection) GetNextRequestID() int {
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()

	reqID := c.reqIDSeq
	c.reqIDSeq++
	return reqID
}

// scanMessages is a split function for the scanner to handle IBKR messages
func (c *Connection) scanMessages(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	// Need at least 4 bytes for the length
	if len(data) < 4 {
		return 0, nil, nil
	}

	// Read message length
	msgLength := int(binary.BigEndian.Uint32(data[:4]))
	totalLength := 4 + msgLength

	// Check if we have the complete message
	if len(data) < totalLength {
		return 0, nil, nil
	}

	// Return the message (without length prefix)
	return totalLength, data[4:totalLength], nil
}
