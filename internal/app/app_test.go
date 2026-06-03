package app

import (
	"strings"
	"testing"
)

func TestNewRejectsSecondAppForSameStateDir(t *testing.T) {
	t.Parallel()

	opts := DefaultOptions("test")
	opts.StateDir = t.TempDir()
	opts.Addr = "127.0.0.1:0"
	opts.PublicURL = "http://127.0.0.1:0"

	first, err := New(opts)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := New(opts)
	if err == nil {
		_ = second.Close()
		t.Fatalf("second New unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "another ibkr app process is already running") {
		t.Fatalf("second New error = %v", err)
	}
}
