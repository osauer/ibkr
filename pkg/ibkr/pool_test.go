package ibkr

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionPoolLazyConnectSingleLease(t *testing.T) {
	cfg := &PoolConfig{
		ClientIDs:        []int{7},
		MaxLeaseTime:     time.Minute,
		HeartbeatTimeout: time.Minute,
		MonitorInterval:  10 * time.Millisecond,
		BaseConfig:       DefaultConfig(),
	}

	pool := NewConnectionPool(cfg)

	var connectCalls int32
	var disconnectCalls int32

	pool.connectFn = func(c *Connection, ctx context.Context) error {
		atomic.AddInt32(&connectCalls, 1)
		c.setStatus(StatusConnected)
		return nil
	}
	pool.disconnectFn = func(c *Connection) error {
		atomic.AddInt32(&disconnectCalls, 1)
		c.setStatus(StatusDisconnected)
		return nil
	}

	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := pool.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}()

	if got := atomic.LoadInt32(&connectCalls); got != 0 {
		t.Fatalf("expected no eager connections, got %d", got)
	}

	lease, err := pool.RequestLease(ctx, "test-service", 7)
	if err != nil {
		t.Fatalf("RequestLease: %v", err)
	}

	if got := atomic.LoadInt32(&connectCalls); got != 1 {
		t.Fatalf("expected connect to run once, got %d", got)
	}

	conn, err := pool.GetConnection(lease.LeaseID)
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if conn == nil || !conn.IsConnected() {
		t.Fatalf("expected connected conn, got %#v", conn)
	}

	if err := pool.ReleaseLease(lease.LeaseID); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	if got := atomic.LoadInt32(&disconnectCalls); got != 1 {
		t.Fatalf("expected disconnect after release, got %d", got)
	}

	lease2, err := pool.RequestLease(ctx, "test-service", 7)
	if err != nil {
		t.Fatalf("RequestLease #2: %v", err)
	}
	if got := atomic.LoadInt32(&connectCalls); got != 2 {
		t.Fatalf("expected second connect for new lease, got %d", got)
	}

	if err := pool.ReleaseLease(lease2.LeaseID); err != nil {
		t.Fatalf("ReleaseLease #2: %v", err)
	}
}

func TestConnectionPoolLeaseExpirationDisconnects(t *testing.T) {
	cfg := &PoolConfig{
		ClientIDs:        []int{17},
		MaxLeaseTime:     time.Minute,
		HeartbeatTimeout: time.Minute,
		MonitorInterval:  10 * time.Millisecond,
		BaseConfig:       DefaultConfig(),
	}
	pool := NewConnectionPool(cfg)

	var connectCalls int32
	var disconnectCalls int32

	pool.connectFn = func(c *Connection, ctx context.Context) error {
		atomic.AddInt32(&connectCalls, 1)
		c.setStatus(StatusConnected)
		return nil
	}
	pool.disconnectFn = func(c *Connection) error {
		atomic.AddInt32(&disconnectCalls, 1)
		c.setStatus(StatusDisconnected)
		return nil
	}

	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := pool.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}()

	lease, err := pool.RequestLease(ctx, "expire-service", 0)
	if err != nil {
		t.Fatalf("RequestLease: %v", err)
	}
	if got := atomic.LoadInt32(&connectCalls); got != 1 {
		t.Fatalf("expected connect once, got %d", got)
	}

	pool.mu.Lock()
	if l, ok := pool.leases[lease.LeaseID]; ok {
		l.ExpiresAt = time.Now().Add(-time.Minute)
	}
	pool.mu.Unlock()

	pool.checkLeases()

	if got := atomic.LoadInt32(&disconnectCalls); got != 1 {
		t.Fatalf("expected disconnect after expiry, got %d", got)
	}

	pool.mu.RLock()
	if _, exists := pool.leases[lease.LeaseID]; exists {
		pool.mu.RUnlock()
		t.Fatalf("expected lease removed after expiry")
	}
	pool.mu.RUnlock()

	lease2, err := pool.RequestLease(ctx, "expire-service", 0)
	if err != nil {
		t.Fatalf("RequestLease #2: %v", err)
	}
	if got := atomic.LoadInt32(&connectCalls); got != 2 {
		t.Fatalf("expected reconnect for new lease, got %d", got)
	}

	if err := pool.ReleaseLease(lease2.LeaseID); err != nil {
		t.Fatalf("ReleaseLease #2: %v", err)
	}
}
