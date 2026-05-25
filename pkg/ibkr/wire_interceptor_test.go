package ibkr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWireInterceptorLogPermissions(t *testing.T) {
	t.Setenv(envWireEnable, "1")

	dir := filepath.Join(t.TempDir(), "wire")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "wire.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWireLogPath, path)

	w, err := NewWireInterceptorFromEnv(17)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	assertPerm(t, dir, 0o700)
	assertPerm(t, path, 0o600)
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
