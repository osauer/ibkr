package ibkr

import (
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"
)

// PacketLogger captures raw IBKR frames for offline inspection. It is intended
// for narrow debugging sessions (e.g. verifying wire encoding) and is disabled
// by default to avoid noisy disk writes on production paths.
type PacketLogger interface {
	// Outbound logs a fully framed payload (length-prefix excluded) that is about
	// to be written to the socket.
	Outbound(label string, payload []byte)
}

// HexPacketLogger writes outbound frames to a local file in a human-friendly
// hex representation. Each line contains:
//
//	<timestamp> <label> <byte-length> <hex>
type HexPacketLogger struct {
	mu   sync.Mutex
	out  *os.File
	path string
}

// NewHexPacketLogger creates a packet logger that appends to the given path.
// The caller is responsible for closing the returned logger when finished.
func NewHexPacketLogger(path string) (*HexPacketLogger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open packet log %q: %w", path, err)
	}
	return &HexPacketLogger{out: f, path: path}, nil
}

// Close releases the underlying file handle.
func (l *HexPacketLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out == nil {
		return nil
	}
	err := l.out.Close()
	l.out = nil
	return err
}

// Outbound logs a payload if the logger is still active.
func (l *HexPacketLogger) Outbound(label string, payload []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out == nil {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	line := fmt.Sprintf("%s %s %d %s\n", ts, label, len(payload), hex.EncodeToString(payload))
	_, _ = l.out.WriteString(line)
}
