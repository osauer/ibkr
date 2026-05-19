package spx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Store persists the engine's on-disk artefacts: the latest snapshot
// (~200 bytes), the constituent-window map (~250 KB), and the
// membership list (~5 KB). Files live under a single directory which
// the caller chooses — typically $XDG_CACHE_HOME/ibkr/breadth-spx/.
//
// The store is a thin wrapper, not a general-purpose KV: each field
// has its own filename, single-writer (the engine), and atomic writes
// via temp+rename so a daemon crash mid-write can't corrupt the file.
// Reads do not lock — the engine holds its own RWMutex around the
// in-memory copy.
type Store struct {
	dir string
}

// NewStore returns a Store rooted at dir. The directory is created on
// first write (lazy mkdir keeps tests that pass an unwritable dir
// from failing at construction time). dir must be an absolute path
// or relative to the daemon's working directory; the store does not
// resolve XDG paths itself — that's the caller's job (see DefaultDir).
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// LoadSnapshot returns the persisted snapshot or (nil, nil) when no
// file exists yet (cold start). Returns an error only for actual I/O
// problems or JSON corruption — neither should happen in steady state
// but both must surface clearly when they do.
func (s *Store) LoadSnapshot() (*Snapshot, error) {
	path := filepath.Join(s.dir, "snapshot.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return &snap, nil
}

// SaveSnapshot writes snap atomically. Existing file is replaced.
func (s *Store) SaveSnapshot(snap Snapshot) error {
	return s.writeAtomic("snapshot.json", snap)
}

// LoadWindows returns the persisted constituent windows, or (nil, nil)
// when no file exists or the on-disk schema version doesn't match. The
// version-mismatch case is intentionally non-fatal: a future format
// bump triggers a cold-rebuild rather than a daemon error.
func (s *Store) LoadWindows() (map[string]ConstituentWindow, error) {
	path := filepath.Join(s.dir, "windows.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read windows: %w", err)
	}
	var set WindowSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("decode windows: %w", err)
	}
	if set.Version != CurrentWindowSetVersion {
		// Future-version files are treated as no-cache: safer to
		// cold-rebuild than to mis-interpret an unknown schema.
		return nil, nil
	}
	return set.Windows, nil
}

// SaveWindows persists the full window map. Pass nil to wipe.
func (s *Store) SaveWindows(windows map[string]ConstituentWindow, asOf time.Time) error {
	set := WindowSet{
		Version: CurrentWindowSetVersion,
		AsOf:    asOf,
		Windows: windows,
	}
	return s.writeAtomic("windows.json", set)
}

// LoadHistory returns the persisted rolling-history series or (nil,
// nil) when no file exists yet. Like the other loaders, an unknown
// schema version triggers a cold rebuild rather than an error so a
// future format bump doesn't poison startup.
func (s *Store) LoadHistory() ([]HistoryPoint, error) {
	path := filepath.Join(s.dir, "history.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read history: %w", err)
	}
	var set HistorySet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("decode history: %w", err)
	}
	if set.Version != CurrentHistorySetVersion {
		return nil, nil
	}
	return set.Points, nil
}

// SaveHistory persists the rolling history. Pass an empty slice to
// wipe.
func (s *Store) SaveHistory(points []HistoryPoint) error {
	set := HistorySet{Version: CurrentHistorySetVersion, Points: points}
	return s.writeAtomic("history.json", set)
}

// writeAtomic encodes v as JSON and replaces dir/name with the result
// in a single atomic os.Rename. Pretty-printed (indent=2) so a human
// debugging the cache can `cat` the file and read it.
func (s *Store) writeAtomic(name string, v any) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	target := filepath.Join(s.dir, name)
	tmp, err := os.CreateTemp(s.dir, name+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// On any error past this point, remove the orphaned temp file so
	// we don't litter the cache dir.
	defer func() {
		if tmp != nil {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil // signal defer to skip the second Close
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename %s: %w", name, err)
	}
	return nil
}

// DefaultDir returns the on-disk cache root the daemon uses by
// default: $XDG_CACHE_HOME/ibkr/breadth-spx/, falling back to
// $HOME/.cache/ibkr/breadth-spx/ when XDG_CACHE_HOME is unset (the
// XDG spec's documented default).
//
// Returns an error only if neither XDG_CACHE_HOME nor HOME is set,
// which on a real OS user account doesn't happen. Tests that need
// a deterministic path should construct NewStore directly with
// t.TempDir() rather than relying on this function.
func DefaultDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "breadth-spx"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr", "breadth-spx"), nil
}
