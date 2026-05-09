// Package dial is the CLI's small Unix-socket client for the daemon.
//
// One Conn handles one request at a time: either a unary call (Call) or a
// streaming subscription (Stream). The newline-delimited JSON wire format
// is symmetric with what internal/daemon writes.
package dial

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// ErrSocketMissing indicates the daemon socket file does not exist.
var ErrSocketMissing = errors.New("ibkrd socket missing")

// DefaultSocketPath returns the canonical socket location.
func DefaultSocketPath() string {
	if v := os.Getenv("IBKRD_SOCKET"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, "ibkr", "ibkrd.sock")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "ibkr", "ibkrd.sock")
}

// DefaultLogPath returns the canonical daemon log location.
func DefaultLogPath() string {
	if v := os.Getenv("IBKRD_LOG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ibkr", "ibkrd.log")
}

// Conn is a single client connection over the Unix socket.
type Conn struct {
	c   net.Conn
	mu  sync.Mutex
	r   *bufio.Reader
	enc *json.Encoder
}

// Connect opens the socket. Returns ErrSocketMissing if path doesn't exist.
func Connect(path string) (*Conn, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrSocketMissing
		}
		return nil, err
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return &Conn{
		c:   c,
		r:   bufio.NewReaderSize(c, 64<<10),
		enc: json.NewEncoder(c),
	}, nil
}

// WaitForSocket polls until the socket appears or the deadline expires, then
// dials it.
func WaitForSocket(path string, timeout time.Duration) (*Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		c, err := Connect(path)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, ErrSocketMissing) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon socket did not appear within %s", timeout)
		}
		time.Sleep(75 * time.Millisecond)
	}
}

// Close releases the socket.
func (c *Conn) Close() error {
	if c == nil {
		return nil
	}
	return c.c.Close()
}

// Call performs a unary request/response round trip and decodes result into
// out. ctx cancellation closes the underlying connection.
func (c *Conn) Call(ctx context.Context, method string, params any, out any) error {
	req, err := newRequest(method, params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.applyDeadline(ctx); err != nil {
		return err
	}

	if err := c.enc.Encode(req); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var resp rpc.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !resp.Ok {
		if resp.Error != nil {
			return resp.Error
		}
		return errors.New("daemon returned !ok with no error payload")
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

// Stream sends a subscribe-style request and invokes onFrame for each frame
// until {"end":true}, the underlying socket closes, or ctx is cancelled.
func (c *Conn) Stream(ctx context.Context, method string, params any, onFrame func(json.RawMessage) error) error {
	req, err := newRequest(method, params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.enc.Encode(req); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	// Cancellation closes the socket so the read loop returns.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.c.SetReadDeadline(time.Now())
		case <-stop:
		}
	}()

	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return ctx.Err()
			}
			return err
		}
		var resp rpc.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		if !resp.Ok {
			if resp.Error != nil {
				return resp.Error
			}
			return errors.New("daemon returned !ok with no error payload")
		}
		if resp.End {
			return nil
		}
		if len(resp.Frame) > 0 && onFrame != nil {
			if err := onFrame(resp.Frame); err != nil {
				return err
			}
		}
	}
}

func newRequest(method string, params any) (*rpc.Request, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	r := &rpc.Request{ID: id, Method: method}
	if params != nil {
		buf, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		r.Params = buf
	}
	return r, nil
}

func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "r-" + hex.EncodeToString(b[:]), nil
}

func (c *Conn) applyDeadline(ctx context.Context) error {
	dl, ok := ctx.Deadline()
	if !ok {
		return c.c.SetDeadline(time.Time{})
	}
	return c.c.SetDeadline(dl)
}
