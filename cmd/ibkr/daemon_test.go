package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDaemonLogSecuresExistingPath(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ibkr-daemon.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := openDaemonLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}

	assertMode(t, dir, 0o700)
	assertMode(t, path, 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
