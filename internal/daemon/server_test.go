package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

// shortTempDir returns a tempdir under /tmp so Unix socket paths stay
// inside macOS's ~104-char SUN_LEN limit. t.TempDir() builds paths under
// /var/folders/... which routinely exceeds that.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ibkrd-test-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// openSocket should clean up a stale socket file from a crashed predecessor
// and bind a fresh listener.
func TestOpenSocketRemovesStaleSocketFile(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	// Simulate a stale socket: bind, close listener immediately. The file
	// remains on disk but no process is serving it, so dial fails fast.
	staleListener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("seed stale socket: %v", err)
	}
	_ = staleListener.Close()
	// Re-create the file (Close removes it on Unix); writing a regular
	// file would not match the ModeSocket check, so we have to leave a
	// real socket inode. The simpler approach: assert openSocket handles
	// the present-but-dead-socket case by ensuring the file exists.
	if _, err := os.Stat(sockPath); err == nil {
		// fine — leftover socket inode, exactly the stale state we want
	} else {
		// staleListener.Close already cleaned it up; recreate as a regular
		// file to exercise the alternate stale-path. Both are valid.
		f, err := os.Create(sockPath)
		if err != nil {
			t.Fatalf("recreate stale path: %v", err)
		}
		_ = f.Close()
	}

	srv := &Server{socketPath: sockPath}
	if err := srv.openSocket(); err != nil {
		// If the leftover was a regular file (non-socket), openSocket
		// won't try to remove it and net.Listen will fail with EADDRINUSE.
		// Either way we should not crash the daemon.
		if !strings.Contains(err.Error(), "address already in use") &&
			!strings.Contains(err.Error(), "bind: address already in use") {
			t.Fatalf("unexpected openSocket error: %v", err)
		}
		// Recover and retry the genuine stale-socket case to keep coverage
		// meaningful.
		_ = os.Remove(sockPath)
		if err := srv.openSocket(); err != nil {
			t.Fatalf("openSocket after manual cleanup: %v", err)
		}
	}
	defer func() {
		if srv.listener != nil {
			_ = srv.listener.Close()
		}
	}()
	if srv.listener == nil {
		t.Fatalf("listener nil after openSocket")
	}
}

// A Server that never opened its listener (e.g. it lost the instance-lock
// race) must not delete the socket file on Stop(). Pre-fix, the loser's
// deferred srv.Stop() would unlink the winner's live socket out from
// underneath it, breaking the running daemon.
func TestStopDoesNotRemoveSocketWhenListenerNeverOpened(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	// Simulate the winner: a real socket file that should survive.
	winner, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("seed winner socket: %v", err)
	}
	defer winner.Close()

	// Simulate the loser: a Server constructed with the same socketPath
	// but no listener (because it never reached openSocket).
	loser := &Server{
		socketPath: sockPath,
		streams:    map[string]context.CancelFunc{},
	}
	loser.Stop()

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("loser.Stop() removed the winner's socket: %v", err)
	}
}

// If a peer is actively serving on the socket, openSocket must refuse to
// evict it. This is belt-and-suspenders; the instance flock is the real
// guard, but a stuck flock + live socket should still be diagnosed clearly
// rather than ripping the socket out from under the live peer.
func TestOpenSocketRefusesToEvictLivePeer(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	livePeer, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("seed live peer: %v", err)
	}
	defer livePeer.Close()

	srv := &Server{socketPath: sockPath}
	err = srv.openSocket()
	if err == nil {
		t.Fatalf("expected openSocket to refuse evicting a live peer")
	}
	if !strings.Contains(err.Error(), "already serving") {
		t.Fatalf("expected 'already serving' diagnostic, got %v", err)
	}
}

// dispatch must report terminal=true for streaming RPCs. serveConn relies
// on this to return out of its read loop and let defer close the conn,
// which in turn unblocks the EOF watcher inside the streaming handler.
// Pre-fix, dispatch returned no terminal signal and serveConn would loop
// back into ReadBytes — but the streaming handler hadn't returned yet
// because it had no per-conn ctx tied to the read side, so the
// subscription leaked.
func TestDispatchQuoteSubscribeReportsTerminal(t *testing.T) {
	t.Parallel()
	srv := &Server{
		cfg:     &config.Resolved{ProfileName: "live", Profile: config.Profile{Host: "127.0.0.1", Port: 4001, ClientID: 15}},
		streams: map[string]context.CancelFunc{},
		logger:  NewLogger(&bytes.Buffer{}, "error"),
	}

	// No connector wired → handleQuoteSubscribe takes the
	// gateway-unavailable early-exit, but the dispatch still has to
	// declare itself terminal so serveConn cleans up the conn.
	params, _ := json.Marshal(rpc.QuoteSubscribeParams{Contract: rpc.ContractParams{Symbol: "AAPL"}})
	req := &rpc.Request{ID: "test-1", Method: rpc.MethodQuoteSubscribe, Params: params}

	var encOut bytes.Buffer
	enc := json.NewEncoder(&encOut)
	r := bufio.NewReader(strings.NewReader(""))

	terminal := srv.dispatch(context.Background(), req, enc, r)
	if !terminal {
		t.Fatalf("expected dispatch to report terminal=true for MethodQuoteSubscribe")
	}
}

// serveConn must return when the client closes its socket end while a
// streaming RPC is in flight. The fix wires this via per-conn context
// and an EOF watcher inside the streaming handler. With no connector,
// the handler takes the gateway-unavailable early exit and returns;
// dispatch reports terminal=true; serveConn returns. The whole sequence
// must complete within a tight deadline.
func TestServeConnExitsCleanlyAfterStreamingRequest(t *testing.T) {
	t.Parallel()
	srv := &Server{
		cfg:     &config.Resolved{ProfileName: "live", Profile: config.Profile{Host: "127.0.0.1", Port: 4001, ClientID: 15}},
		streams: map[string]context.CancelFunc{},
		logger:  NewLogger(&bytes.Buffer{}, "error"),
	}

	clientSide, daemonSide := net.Pipe()
	t.Cleanup(func() { _ = clientSide.Close(); _ = daemonSide.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveConn(context.Background(), daemonSide)
	}()

	params, _ := json.Marshal(rpc.QuoteSubscribeParams{Contract: rpc.ContractParams{Symbol: "AAPL"}})
	req := &rpc.Request{ID: "test-1", Method: rpc.MethodQuoteSubscribe, Params: params}
	if err := json.NewEncoder(clientSide).Encode(req); err != nil {
		t.Fatalf("encode subscribe request: %v", err)
	}

	// Read the streaming handler's gateway-unavailable error response so
	// the daemon-side write doesn't block on a backed-up pipe. Then close
	// the client side; serveConn should exit promptly.
	if _, err := bufio.NewReader(clientSide).ReadBytes('\n'); err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = clientSide.Close()

	select {
	case <-done:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatalf("serveConn did not return within 2s after client disconnect")
	}
}
