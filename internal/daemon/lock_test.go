package daemon

import (
	"errors"
	"path/filepath"
	"testing"
)

// First holder gets the lock; the second holder must observe ErrAlreadyRunning
// rather than blocking or returning a generic error.
func TestAcquireInstanceLockExclusive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ibkrd.sock")

	first, err := acquireInstanceLock(socketPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	second, err := acquireInstanceLock(socketPath)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning on second acquire, got %v (lock=%v)", err, second)
	}
	if second != nil {
		t.Fatalf("expected nil lock when contended, got %v", second)
	}
}

// Releasing the first holder must let the second succeed. Verifies we
// actually unlock + close the fd rather than leaking it (which would leave
// the lock dangling until process exit).
func TestAcquireInstanceLockReleaseAllowsReacquire(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ibkrd.sock")

	first, err := acquireInstanceLock(socketPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	first.Release()

	second, err := acquireInstanceLock(socketPath)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	defer second.Release()
}

// Release on a nil receiver / already-released lock must not panic. Server
// hot-paths call Release() during error unwinds; tolerate idempotent calls.
func TestInstanceLockReleaseIdempotent(t *testing.T) {
	t.Parallel()
	var nilLock *instanceLock
	nilLock.Release()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ibkrd.sock")
	l, err := acquireInstanceLock(socketPath)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Release()
	l.Release() // second release must be a no-op
}

// Concurrent acquisition: of N goroutines, exactly one wins, the rest
// observe ErrAlreadyRunning. This is the production failure mode (CLI race
// auto-spawning daemons) made deterministic.
func TestAcquireInstanceLockConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ibkrd.sock")
	const N = 8

	type result struct {
		lock *instanceLock
		err  error
	}
	results := make(chan result, N)
	start := make(chan struct{})
	for range N {
		go func() {
			<-start
			l, err := acquireInstanceLock(socketPath)
			results <- result{lock: l, err: err}
		}()
	}
	close(start)

	var winners, losers int
	var winner *instanceLock
	for range N {
		r := <-results
		switch {
		case r.err == nil && r.lock != nil:
			winners++
			winner = r.lock
		case errors.Is(r.err, ErrAlreadyRunning):
			losers++
		default:
			t.Fatalf("unexpected outcome: lock=%v err=%v", r.lock, r.err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner, got %d (losers=%d)", winners, losers)
	}
	if losers != N-1 {
		t.Fatalf("expected %d losers, got %d", N-1, losers)
	}
	winner.Release()
}
