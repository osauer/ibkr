package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistenceLockSerializesDatabaseAcrossSocketPaths(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "state", "daemon.db")
	first, err := acquirePersistenceLock(databasePath)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	t.Cleanup(first.Release)

	if _, err := acquirePersistenceLock(databasePath); !errors.Is(err, ErrPersistenceInUse) {
		t.Fatalf("second lock error = %v, want %v", err, ErrPersistenceInUse)
	}

	first.Release()
	second, err := acquirePersistenceLock(databasePath)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second.Release()

	if _, err := os.Stat(databasePath + ".lock"); err != nil {
		t.Fatalf("stable lock file: %v", err)
	}
}

func TestPersistenceLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "daemon.db")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, databasePath+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := acquirePersistenceLock(databasePath); err == nil {
		t.Fatal("expected symlink refusal")
	}
}
