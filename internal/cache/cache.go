// Package cache holds the small persistent stores the daemon owns: the
// resolved-contract map and the inactive-symbol set. v1 backs both with JSON
// files written via temp + atomic rename.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Contract is the cached resolution for a symbol → IBKR contract metadata.
// Kept narrow on purpose; full ibkr.Contract is much wider but most fields
// are only needed at request time.
type Contract struct {
	Symbol       string  `json:"symbol"`
	ConID        int     `json:"con_id"`
	SecType      string  `json:"sec_type"`
	Exchange     string  `json:"exchange"`
	PrimaryExch  string  `json:"primary_exch"`
	Currency     string  `json:"currency"`
	Multiplier   int     `json:"multiplier"`
	TradingClass string  `json:"trading_class"`
	LocalSymbol  string  `json:"local_symbol"`
	Expiry       string  `json:"expiry,omitempty"`
	Strike       float64 `json:"strike,omitempty"`
	Right        string  `json:"right,omitempty"`
}

// ContractCache is the storage interface; v1 ships JSONCache, future engines
// (bbolt) can be dropped in without touching the daemon.
type ContractCache interface {
	Get(symbol string) (Contract, bool)
	Put(c Contract)
	Delete(symbol string)
	All() map[string]Contract
	Flush(ctx context.Context) error
}

// JSONCache is the v1 file-backed implementation.
type JSONCache struct {
	path  string
	mu    sync.RWMutex
	data  map[string]Contract
	dirty bool
}

// OpenJSONCache loads (or creates) a JSON cache rooted at the given file path.
// A missing file is not an error.
func OpenJSONCache(path string) (*JSONCache, error) {
	c := &JSONCache{path: path, data: map[string]Contract{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(data, &c.data); err != nil {
		return nil, err
	}
	return c, nil
}

// Get returns the cached contract for symbol if present.
func (c *JSONCache) Get(symbol string) (Contract, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[symbol]
	return v, ok
}

// Put inserts or updates a contract; identical writes are no-ops.
func (c *JSONCache) Put(x Contract) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.data[x.Symbol]; ok && existing == x {
		return
	}
	c.data[x.Symbol] = x
	c.dirty = true
}

// Delete removes a symbol if present.
func (c *JSONCache) Delete(symbol string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[symbol]; !ok {
		return
	}
	delete(c.data, symbol)
	c.dirty = true
}

// All returns a copy of the cached contents.
func (c *JSONCache) All() map[string]Contract {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return maps.Clone(c.data)
}

// Flush atomically writes the cache to disk. No-op if no changes since the
// last successful flush. dirty is cleared inside the same critical section
// as the snapshot so a concurrent Put after the snapshot keeps dirty=true
// for the next Flush — pre-fix, the bool was reset after I/O which lost
// any write that landed during the file write.
func (c *JSONCache) Flush(ctx context.Context) error {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	snap := maps.Clone(c.data)
	c.dirty = false
	c.mu.Unlock()

	if err := writeJSONAtomic(c.path, snap); err != nil {
		c.mu.Lock()
		c.dirty = true
		c.mu.Unlock()
		return err
	}
	return nil
}

// InactiveStore tracks symbols IBKR has reported as unavailable.
type InactiveStore struct {
	path  string
	mu    sync.RWMutex
	data  map[string]InactiveEntry
	dirty bool
}

// InactiveEntry records why and when a symbol was first observed inactive.
type InactiveEntry struct {
	Reason      string    `json:"reason"`
	FirstSeenAt time.Time `json:"first_seen_at"`
}

// OpenInactiveStore reads (or creates) the inactive-symbol JSON file.
func OpenInactiveStore(path string) (*InactiveStore, error) {
	s := &InactiveStore{path: path, data: map[string]InactiveEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return nil, err
	}
	return s, nil
}

// Mark records a symbol as inactive; idempotent.
func (s *InactiveStore) Mark(symbol, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[symbol]; ok {
		return
	}
	s.data[symbol] = InactiveEntry{Reason: reason, FirstSeenAt: time.Now()}
	s.dirty = true
}

// Reason returns the recorded reason if the symbol is marked inactive.
func (s *InactiveStore) Reason(symbol string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[symbol]
	if !ok {
		return "", false
	}
	return v.Reason, true
}

// All returns a snapshot of the entire set.
func (s *InactiveStore) All() map[string]InactiveEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.data)
}

// Flush atomically writes the store to disk. No-op if unchanged. See
// JSONCache.Flush for the dirty-bit ordering rationale.
func (s *InactiveStore) Flush(ctx context.Context) error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snap := maps.Clone(s.data)
	s.dirty = false
	s.mu.Unlock()

	if err := writeJSONAtomic(s.path, snap); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}

// writeJSONAtomic writes v as indented JSON via tempfile + rename. Replaceable
// in tests so the dirty-bit race regression test can interleave a concurrent
// Put deterministically with the file write.
var writeJSONAtomic = func(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
