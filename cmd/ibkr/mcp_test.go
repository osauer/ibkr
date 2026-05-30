package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchMCPParentCancelsWhenParentChanges(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var parent atomic.Int32
	parent.Store(42)
	getppid := func() int { return int(parent.Load()) }
	go watchMCPParent(ctx, cancel, 42, getppid, 5*time.Millisecond)

	parent.Store(43)
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("watchMCPParent did not cancel after parent PID changed")
	}
}

func TestMCPServeOptionsDoNotIdleTimeout(t *testing.T) {
	t.Parallel()

	opts := mcpServeOptions()
	if opts.IdleTimeout != 0 {
		t.Fatalf("MCP stdio server must not idle-exit while the host keeps stdin open; IdleTimeout=%s", opts.IdleTimeout)
	}
}

func TestDialMCPDaemonHonorsCanceledContextBeforeAutospawn(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := dialMCPDaemon(ctx, filepath.Join(t.TempDir(), "missing.sock"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dialMCPDaemon with canceled ctx = %v, want context.Canceled", err)
	}
}
