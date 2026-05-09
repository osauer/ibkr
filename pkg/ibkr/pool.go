package ibkr

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/pkg/ibkr/internal/logging"
)

var poolLogger = logging.Component("IBKR Pool")

// ConnectionLease represents a leased connection
type ConnectionLease struct {
	LeaseID       string
	ClientID      int
	ServiceName   string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	LastHeartbeat time.Time
	Active        bool
}

// ConnectionPool manages multiple IBKR connections
// Adapted from hedge's connection pooling pattern
type ConnectionPool struct {
	config      *PoolConfig
	connections map[int]*Connection
	connLocks   map[int]*sync.Mutex
	connLocksMu sync.Mutex
	leases      map[string]*ConnectionLease
	mu          sync.RWMutex

	// Monitoring
	stopChan chan struct{}
	wg       sync.WaitGroup

	connectFn    func(*Connection, context.Context) error
	disconnectFn func(*Connection) error
}

// PoolConfig holds connection pool configuration
type PoolConfig struct {
	ClientIDs        []int             // IBKR client IDs to manage (1-5)
	MaxLeaseTime     time.Duration     // Maximum lease duration
	HeartbeatTimeout time.Duration     // Lease heartbeat timeout
	MonitorInterval  time.Duration     // Lease monitoring interval
	BaseConfig       *ConnectionConfig // Base config for all connections
	EagerConnect     bool              // If true, preconnect all clients on Start
}

// DefaultPoolConfig returns a production-ready pool configuration
func DefaultPoolConfig() *PoolConfig {
	return &PoolConfig{
		ClientIDs:        []int{1}, // Temporarily reduced to 1 for debugging
		MaxLeaseTime:     30 * time.Minute,
		HeartbeatTimeout: 2 * time.Minute,
		MonitorInterval:  10 * time.Second,
		BaseConfig:       DefaultConfig(),
		EagerConnect:     false,
	}
}

// NewConnectionPool creates a new connection pool
func NewConnectionPool(config *PoolConfig) *ConnectionPool {
	if config == nil {
		config = DefaultPoolConfig()
	}
	if config.BaseConfig == nil {
		config.BaseConfig = DefaultConfig()
	}

	pool := &ConnectionPool{
		config:      config,
		connections: make(map[int]*Connection),
		connLocks:   make(map[int]*sync.Mutex),
		leases:      make(map[string]*ConnectionLease),
		stopChan:    make(chan struct{}),
	}

	// Create ONE shared wire interceptor for all connections in the pool
	// This prevents 11x initialization spam and shares the Claude analyzer across all connections
	var sharedWireInterceptor *WireInterceptor
	if len(config.ClientIDs) > 0 {
		// Use first client ID for interceptor identification (doesn't matter which)
		if interceptor, err := NewWireInterceptorFromEnv(config.ClientIDs[0]); err != nil {
			poolLogger.Warnf("Failed to initialize shared wire interceptor: %v", err)
		} else {
			sharedWireInterceptor = interceptor
			poolLogger.Debugf("Initialized shared wire interceptor for %d connections", len(config.ClientIDs))
		}
	}

	// Initialize connections for each client ID
	for _, clientID := range config.ClientIDs {
		connConfig := *config.BaseConfig // Copy base config
		connConfig.ClientID = clientID
		connConfig.WireInterceptor = sharedWireInterceptor // Share the interceptor
		if connConfig.PacketLogPath != "" && strings.Contains(connConfig.PacketLogPath, "%d") {
			connConfig.PacketLogPath = fmt.Sprintf(connConfig.PacketLogPath, clientID)
		}
		pool.connections[clientID] = NewConnection(&connConfig)
		pool.connLocks[clientID] = &sync.Mutex{}
	}

	pool.connectFn = func(c *Connection, ctx context.Context) error {
		return c.Connect(ctx)
	}
	pool.disconnectFn = func(c *Connection) error {
		return c.Disconnect()
	}

	return pool
}

// Start initializes all connections and starts monitoring
func (p *ConnectionPool) Start(ctx context.Context) error {
	poolLogger.Infof("Starting connection pool with %d configured client IDs", len(p.config.ClientIDs))
	if !p.config.EagerConnect {
		poolLogger.Infof("Using lazy connection mode; connections establish when leases are granted")
	}

	var connectErrors []error
	if p.config.EagerConnect {
		poolLogger.Infof("Eager-connect enabled; establishing all connections upfront")

		var wg sync.WaitGroup
		errChan := make(chan error, len(p.connections))

		for clientID := range p.connections {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				if _, err := p.ensureConnection(ctx, id); err != nil {
					poolLogger.Warnf("Failed to eager-connect client %d: %v", id, err)
					errChan <- fmt.Errorf("client %d: %w", id, err)
				}
			}(clientID)
		}

		wg.Wait()
		close(errChan)

		for err := range errChan {
			connectErrors = append(connectErrors, err)
		}
	}

	// Start lease monitor regardless of eager connect outcome
	p.wg.Add(1)
	go p.monitorLeases()

	if len(connectErrors) > 0 {
		successCount := len(p.config.ClientIDs) - len(connectErrors)
		poolLogger.Infof("Connection pool started with %d/%d eager connections", successCount, len(p.config.ClientIDs))
		if successCount == 0 {
			return fmt.Errorf("all eager connection attempts failed")
		}
	} else if p.config.EagerConnect {
		poolLogger.Infof("All %d eager connections established successfully", len(p.config.ClientIDs))
	}

	return nil
}

// Stop gracefully shuts down all connections
func (p *ConnectionPool) Stop() error {
	poolLogger.Infof("Stopping connection pool")

	// Signal shutdown
	select {
	case <-p.stopChan:
		// already closed
	default:
		close(p.stopChan)
	}

	// Wait for monitor to stop
	p.wg.Wait()

	// Disconnect all connections
	var disconnectErrors []error
	for clientID := range p.connections {
		if err := p.disconnectAndReset(clientID, false); err != nil {
			disconnectErrors = append(disconnectErrors,
				fmt.Errorf("client %d: %w", clientID, err))
		}
	}

	if len(disconnectErrors) > 0 {
		return fmt.Errorf("disconnection errors: %v", disconnectErrors)
	}

	return nil
}

// RequestLease requests a connection lease for a service
func (p *ConnectionPool) RequestLease(ctx context.Context, serviceName string, preferredClientID int) (*ConnectionLease, error) {
	p.mu.Lock()
	for _, lease := range p.leases {
		if lease.ServiceName == serviceName && lease.Active {
			p.mu.Unlock()
			return nil, fmt.Errorf("service %s already has an active lease", serviceName)
		}
	}

	clientID := p.findAvailableConnection(preferredClientID)
	if clientID == 0 {
		p.mu.Unlock()
		return nil, fmt.Errorf("no available connections")
	}

	lease := &ConnectionLease{
		LeaseID:       fmt.Sprintf("%s-%d-%d", serviceName, clientID, time.Now().Unix()),
		ClientID:      clientID,
		ServiceName:   serviceName,
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(p.config.MaxLeaseTime),
		LastHeartbeat: time.Now(),
		Active:        false,
	}

	p.leases[lease.LeaseID] = lease
	p.mu.Unlock()

	// Ensure connection is established before activating the lease
	conn, err := p.ensureConnection(ctx, clientID)
	if err != nil {
		p.mu.Lock()
		delete(p.leases, lease.LeaseID)
		p.mu.Unlock()
		return nil, fmt.Errorf("failed to connect client %d: %w", clientID, err)
	}

	if !conn.IsConnected() {
		p.mu.Lock()
		delete(p.leases, lease.LeaseID)
		p.mu.Unlock()
		return nil, fmt.Errorf("client %d did not reach connected state", clientID)
	}

	p.mu.Lock()
	lease.Active = true
	lease.LastHeartbeat = time.Now()
	lease.ExpiresAt = time.Now().Add(p.config.MaxLeaseTime)
	p.mu.Unlock()

	poolLogger.Infof("Granted lease %s to %s (Client ID: %d)", lease.LeaseID, serviceName, clientID)

	return lease, nil
}

// ReleaseLease releases a connection lease
func (p *ConnectionPool) ReleaseLease(leaseID string) error {
	var clientID int
	p.mu.Lock()
	lease, exists := p.leases[leaseID]
	if !exists {
		p.mu.Unlock()
		return fmt.Errorf("lease %s not found", leaseID)
	}

	clientID = lease.ClientID
	lease.Active = false
	delete(p.leases, leaseID)
	p.mu.Unlock()

	poolLogger.Infof("Released lease %s from %s (Client ID: %d)", leaseID, lease.ServiceName, clientID)

	if err := p.disconnectAndReset(clientID, true); err != nil {
		return err
	}

	return nil
}

// HeartbeatLease updates lease heartbeat
func (p *ConnectionPool) HeartbeatLease(leaseID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	lease, exists := p.leases[leaseID]
	if !exists {
		return fmt.Errorf("lease %s not found", leaseID)
	}

	if !lease.Active {
		return fmt.Errorf("lease %s is not active", leaseID)
	}

	lease.LastHeartbeat = time.Now()
	return nil
}

// GetConnection returns the connection for a leased client ID
func (p *ConnectionPool) GetConnection(leaseID string) (*Connection, error) {
	return p.getConnectionInternal(leaseID, nil)
}

// GetConnectionPrepared returns the connection for a leased client ID and allows
// callers to register hooks on the connection before a new Connect cycle begins.
// The prepare callback runs regardless of current connection state.
func (p *ConnectionPool) GetConnectionPrepared(leaseID string, prepare func(*Connection)) (*Connection, error) {
	return p.getConnectionInternal(leaseID, prepare)
}

func (p *ConnectionPool) getConnectionInternal(leaseID string, prepare func(*Connection)) (*Connection, error) {
	p.mu.RLock()
	lease, exists := p.leases[leaseID]
	if !exists {
		p.mu.RUnlock()
		return nil, fmt.Errorf("lease %s not found", leaseID)
	}

	if !lease.Active {
		p.mu.RUnlock()
		return nil, fmt.Errorf("lease %s is not active", leaseID)
	}

	clientID := lease.ClientID
	conn := p.connections[clientID]
	p.mu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("connection for client %d not found", clientID)
	}

	if prepare != nil {
		prepare(conn)
	}

	if conn.IsConnected() {
		return conn, nil
	}

	if _, err := p.ensureConnection(context.Background(), clientID); err != nil {
		return nil, err
	}

	return p.connections[clientID], nil
}

func (p *ConnectionPool) ensureConnection(ctx context.Context, clientID int) (*Connection, error) {
	lock := p.getConnLock(clientID)
	lock.Lock()
	defer lock.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.RLock()
	conn := p.connections[clientID]
	p.mu.RUnlock()

	if conn == nil {
		conn = p.newConnection(clientID)
		p.mu.Lock()
		p.connections[clientID] = conn
		p.mu.Unlock()
	}

	if conn.IsConnected() {
		return conn, nil
	}

	if err := p.connectFn(conn, ctx); err != nil {
		return nil, err
	}

	return conn, nil
}

func (p *ConnectionPool) disconnectAndReset(clientID int, reset bool) error {
	lock := p.getConnLock(clientID)
	lock.Lock()
	defer lock.Unlock()

	p.mu.RLock()
	conn := p.connections[clientID]
	p.mu.RUnlock()

	if conn != nil {
		if err := p.disconnectFn(conn); err != nil {
			return err
		}
	}

	if reset {
		newConn := p.newConnection(clientID)
		p.mu.Lock()
		p.connections[clientID] = newConn
		p.mu.Unlock()
	}

	return nil
}

func (p *ConnectionPool) getConnLock(clientID int) *sync.Mutex {
	p.connLocksMu.Lock()
	lock, ok := p.connLocks[clientID]
	if !ok {
		lock = &sync.Mutex{}
		p.connLocks[clientID] = lock
	}
	p.connLocksMu.Unlock()
	return lock
}

func (p *ConnectionPool) newConnection(clientID int) *Connection {
	base := p.config.BaseConfig
	if base == nil {
		base = DefaultConfig()
	}
	connCfg := *base
	connCfg.ClientID = clientID
	return NewConnection(&connCfg)
}

// findAvailableConnection finds an available connection for leasing
func (p *ConnectionPool) findAvailableConnection(preferredClientID int) int {
	// Check preferred client ID first
	if preferredClientID > 0 {
		if _, exists := p.connections[preferredClientID]; exists {
			// Check if not already leased
			isLeased := false
			for _, lease := range p.leases {
				if lease.ClientID == preferredClientID && lease.Active {
					isLeased = true
					break
				}
			}
			if !isLeased {
				return preferredClientID
			}
		}
	}

	// Find any available client
	for clientID := range p.connections {
		// Check if not already leased
		isLeased := false
		for _, lease := range p.leases {
			if lease.ClientID == clientID && lease.Active {
				isLeased = true
				break
			}
		}

		if !isLeased {
			return clientID
		}
	}

	return 0 // No available connections
}

// monitorLeases monitors and expires leases
func (p *ConnectionPool) monitorLeases() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.config.MonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.checkLeases()
		}
	}
}

// checkLeases checks for expired or stale leases
func (p *ConnectionPool) checkLeases() {
	var toReset []int
	p.mu.Lock()
	now := time.Now()

	for leaseID, lease := range p.leases {
		if !lease.Active {
			continue
		}

		// Check expiration
		if now.After(lease.ExpiresAt) {
			lease.Active = false
			poolLogger.Warnf("Lease %s expired for %s", leaseID, lease.ServiceName)
			toReset = append(toReset, lease.ClientID)
			delete(p.leases, leaseID)
			continue
		}

		// Check heartbeat timeout
		if now.Sub(lease.LastHeartbeat) > p.config.HeartbeatTimeout {
			lease.Active = false
			poolLogger.Warnf("Lease %s timed out (no heartbeat) for %s", leaseID, lease.ServiceName)
			toReset = append(toReset, lease.ClientID)
			delete(p.leases, leaseID)
		}
	}
	p.mu.Unlock()

	for _, clientID := range toReset {
		if err := p.disconnectAndReset(clientID, true); err != nil {
			poolLogger.Errorf("Error resetting connection for client %d after lease expiry: %v", clientID, err)
		}
	}
}

// GetPoolStatus returns the current pool status
func (p *ConnectionPool) GetPoolStatus() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	connectedCount := 0
	availableCount := 0
	leasedCount := 0

	// Count connected connections
	for _, conn := range p.connections {
		if conn.IsConnected() {
			connectedCount++
		}
	}

	// Count leased connections
	leasedClients := make(map[int]bool)
	for _, lease := range p.leases {
		if lease.Active {
			leasedClients[lease.ClientID] = true
		}
	}
	leasedCount = len(leasedClients)

	// Calculate available
	availableCount = connectedCount - leasedCount

	// Determine health
	health := "healthy"
	if connectedCount == 0 {
		health = "critical"
	} else if connectedCount < len(p.connections)/2 {
		health = "degraded"
	} else if connectedCount < len(p.connections) {
		health = "warning"
	}

	// Build connection details
	connectionDetails := make(map[int]map[string]interface{})
	for clientID, conn := range p.connections {
		connectionDetails[clientID] = conn.GetConnectionInfo()

		// Add lease info if leased
		for _, lease := range p.leases {
			if lease.Active && lease.ClientID == clientID {
				connectionDetails[clientID]["leased_to"] = lease.ServiceName
				connectionDetails[clientID]["lease_expires"] = lease.ExpiresAt
				break
			}
		}
	}

	return map[string]interface{}{
		"total_connections": len(p.connections),
		"connected_count":   connectedCount,
		"available_count":   availableCount,
		"leased_count":      leasedCount,
		"pool_health":       health,
		"connections":       connectionDetails,
	}
}
