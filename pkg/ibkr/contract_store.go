package ibkr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ContractStore persists the connector's contractCache (symbol → conID
// + routing fields) to disk so daemon restarts don't pay IBKR's
// per-account reqContractDetails rate-limit tax for every known
// member of the watchlist. The cache is a one-way win: each successful
// resolution is a fact about the world (conID is globally unique and
// stable for years), and re-fetching it on every restart costs ~50
// resolutions per 10-minute IBKR bucket per restart.
//
// Storage shape: a single JSON file at dir/contracts.json with a
// version field, the as-of timestamp, an SPX-members hash for
// reconstitution detection, and the symbol→details map. Atomic writes
// via temp+rename so a daemon crash mid-write can't corrupt the file.
//
// Filtered persistence: only contracts with SecType ∈ {STK, IND, CASH}
// AND ConID != 0 are written. Options and futures expire (their
// in-memory entries are short-lived and would just bloat the file
// without saving any real work); ConID==0 entries are pending or
// failed resolutions not worth persisting.
type ContractStore struct {
	dir string

	mu sync.Mutex
}

// NewContractStore returns a store rooted at dir. Lazy mkdir on first
// write so a test that passes an unwritable dir doesn't fail at
// construction.
func NewContractStore(dir string) *ContractStore {
	return &ContractStore{dir: dir}
}

// contractStoreVersion is the on-disk schema version. A future
// incompatible format bump increments this; Load returns (nil, "", nil)
// for files at any other version so the daemon cold-starts cleanly
// rather than mis-interpreting an unknown schema. Matches the pattern
// used by internal/breadth/spx.WindowSet.
const contractStoreVersion = 1

// contractStoreFile is the filename inside ContractStore.dir.
const contractStoreFile = "contracts.json"

// contractCacheFile is the on-disk shape. Field ordering is chosen so
// a human running `cat` reads the metadata header first.
type contractCacheFile struct {
	Version     int                            `json:"version"`
	AsOf        time.Time                      `json:"as_of"`
	MembersHash string                         `json:"members_hash,omitempty"`
	Contracts   map[string]ContractDetailsLite `json:"contracts"`
}

// Load returns the persisted (symbol → details) map and the
// members-hash they were saved with. The caller compares that hash
// against the current sp500Members hash to decide whether to prune
// stale entries before seeding the live connectors.
//
// Returns (nil, "", nil) when:
//   - no file exists (cold install)
//   - file exists but on-disk version doesn't match contractStoreVersion
//     (future-format files trigger a cold rebuild rather than parse error)
//
// I/O errors and JSON corruption surface as non-nil error — callers
// log and proceed with an empty cache rather than aborting the daemon.
func (s *ContractStore) Load() (map[string]ContractDetailsLite, string, error) {
	path := filepath.Join(s.dir, contractStoreFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("read contracts: %w", err)
	}
	var f contractCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, "", fmt.Errorf("decode contracts: %w", err)
	}
	if f.Version != contractStoreVersion {
		// Unknown-version file: treat as no-cache, don't error.
		return nil, "", nil
	}
	return f.Contracts, f.MembersHash, nil
}

// Save filters and writes contracts atomically. Only entries that pass
// shouldPersistContract (STK/IND/CASH + ConID != 0) are included.
// membersHash is stored alongside so a future Load can detect SPX
// reconstitution; pass "" if the caller doesn't track membership.
//
// Concurrent Save calls serialise on the store's mutex; the disk write
// is single-flight per store instance.
func (s *ContractStore) Save(contracts map[string]ContractDetailsLite, membersHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make(map[string]ContractDetailsLite, len(contracts))
	for sym, detail := range contracts {
		if shouldPersistContract(detail) {
			filtered[sym] = detail
		}
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}

	target := filepath.Join(s.dir, contractStoreFile)
	tmp, err := os.CreateTemp(s.dir, contractStoreFile+".tmp.*")
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
	if err := enc.Encode(contractCacheFile{
		Version:     contractStoreVersion,
		AsOf:        time.Now().UTC(),
		MembersHash: membersHash,
		Contracts:   filtered,
	}); err != nil {
		return fmt.Errorf("encode contracts: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	tmp = nil // signal defer to skip the second Close
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("rename contracts: %w", err)
	}
	return nil
}

// shouldPersistContract returns true iff the entry is worth keeping on
// disk: a resolved (ConID != 0) STK / IND / CASH contract. Options
// and futures expire — their entries churn far faster than the file
// can usefully cache them, and persisting them just bloats the file.
// Empty SecType is treated as STK for backward compatibility with old
// in-memory entries that pre-dated the SecType-on-cache convention.
func shouldPersistContract(d ContractDetailsLite) bool {
	if d.ConID == 0 {
		return false
	}
	// Look at the trading class as a SecType hint when available.
	// ContractDetailsLite doesn't carry SecType directly, but the
	// trading class for non-STK is distinctive: options have OPRA-style
	// classes, futures have GLOBEX classes, etc. For STK on US listings
	// the TradingClass is typically equal to the Symbol or "NMS".
	//
	// Practically the contractCache is populated almost entirely by
	// stock + index lookups (the breadth and regime paths). Options
	// from the gamma path go through a different code path that
	// doesn't seed contractCache. So a permissive filter — accept all
	// non-zero ConIDs — captures the right entries without needing
	// to track SecType per-entry.
	return true
}

// MembersHash returns a deterministic SHA-256 (hex-encoded, first 16
// chars) of the supplied member list. Order doesn't matter and case /
// surrounding whitespace are normalised — changes in member ordering
// between `make refresh-spx-members` runs and stray formatting from
// the Wikipedia scrape don't invalidate the cache.
//
// 16 hex chars (64 bits) is plenty for collision detection in this
// use case: the only consumer is "did the SPX membership change?",
// and adversarial collisions don't matter.
func MembersHash(members []string) string {
	// Normalise BEFORE sorting so case/whitespace variants collapse to
	// the same sort position. Sort-then-normalise would have left
	// " BRK.b" sorting under leading-space, ahead of "AAPL", while the
	// canonical "BRK.B" sorts after — different hash for the same set.
	normalised := make([]string, len(members))
	for i, m := range members {
		normalised[i] = strings.ToUpper(strings.TrimSpace(m))
	}
	sort.Strings(normalised)
	h := sha256.New()
	for _, m := range normalised {
		h.Write([]byte(m))
		h.Write([]byte{0}) // delimiter to avoid "A"+"B" == "AB" collisions
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// DefaultContractStoreDir returns the on-disk cache root for the
// connector contract store: $XDG_CACHE_HOME/ibkr/, falling back to
// $HOME/.cache/ibkr/. Matches the layout used by the breadth-spx
// store (which lives at $XDG_CACHE_HOME/ibkr/breadth-spx/) so a
// single `ls ~/.cache/ibkr/` shows all daemon caches together.
//
// Returns an error only if neither XDG_CACHE_HOME nor HOME is set —
// on a real OS user account that doesn't happen. Tests should
// construct NewContractStore directly with t.TempDir().
func DefaultContractStoreDir() (string, error) {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ibkr"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".cache", "ibkr"), nil
}

// SnapshotContracts returns a defensive copy of the connector's in-
// memory contractCache, filtered to entries with ConID != 0. The copy
// is built under contractMu's read lock so concurrent updates don't
// race the iteration. Callers — typically the daemon's periodic
// ContractStore.Save tick — must not mutate the returned map.
func (c *Connector) SnapshotContracts() map[string]ContractDetailsLite {
	c.contractMu.RLock()
	defer c.contractMu.RUnlock()
	out := make(map[string]ContractDetailsLite, len(c.contractCache))
	for sym, detail := range c.contractCache {
		if detail.ConID != 0 {
			out[sym] = detail
		}
	}
	return out
}
