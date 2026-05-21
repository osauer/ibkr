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
// Stock + index + cash entries (long-lived, ConID stable across years)
// live under the top-level "contracts" key. Option entries live under
// "options" — same ContractDetailsLite shape but keyed by the OPRA-style
// optionContractKey (symbol|expiry|strike|right). Options are garbage-
// collected on save: any entry whose Expiry has passed is dropped, so
// the file stabilises around ~1-3K live entries even though gamma
// computes touch ~1600 strikes per session.
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
// incompatible format bump increments this; Load returns a cold result
// for files at any other version so the daemon cold-starts cleanly
// rather than mis-interpreting an unknown schema. Matches the pattern
// used by internal/breadth/spx.WindowSet.
//
// v1 → v2 added the top-level "options" map, keyed by optionContractKey,
// for bulk-prewarmed option contracts. v1 files are read transparently:
// the options map appears as nil and the daemon refills it from the
// next prewarm.
//
// v2 → v3 widened optionContractKey to include the trading class
// (`symbol|class|expiry|strike|right` instead of `symbol|expiry|strike|right`).
// Required for SPX/SPXW disambiguation — see
// docs/design/gamma-spx-coverage.md §4.4. v2 keys are migrated forward
// on read: each `S|E|K|R` becomes `S||E|K|R` (empty class slot). Empty
// class never collides with a real entry because the connector always
// fills TradingClass for OPT contracts; only the v2-read migration
// produces empty-class entries, and the next prewarm overwrites them
// with class-qualified keys.
//
// New writes always go out at v3.
const contractStoreVersion = 3

// contractStoreFile is the filename inside ContractStore.dir.
const contractStoreFile = "contracts.json"

// contractCacheFile is the on-disk shape. Field ordering is chosen so
// a human running `cat` reads the metadata header first.
type contractCacheFile struct {
	Version     int                            `json:"version"`
	AsOf        time.Time                      `json:"as_of"`
	MembersHash string                         `json:"members_hash,omitempty"`
	Contracts   map[string]ContractDetailsLite `json:"contracts"`
	Options     map[string]ContractDetailsLite `json:"options,omitempty"`
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
	// Accept any version ≤ contractStoreVersion. v1 files have no
	// "options" key; they load cleanly because the field is optional.
	// Newer-version files (forward compatibility, hypothetical) trigger
	// a cold rebuild — the daemon doesn't try to interpret a schema
	// it doesn't know.
	if f.Version > contractStoreVersion {
		return nil, "", nil
	}
	return f.Contracts, f.MembersHash, nil
}

// LoadOptions returns the persisted (optionContractKey → details) map.
// Expired entries are pruned in-place: any entry whose Expiry has
// passed in NY time is dropped silently. The returned map can be empty
// (cold install or all entries expired) but never nil.
//
// v2 → v3 key-format migration runs here: v2 files keyed entries as
// `symbol|expiry|strike|right` (4 fields, 3 pipes); v3 added trading
// class as a second field (5 fields, 4 pipes). v2 keys are rewritten
// on the fly to `symbol||expiry|strike|right` (empty class slot) so
// the daemon picks up the persisted entries; the next prewarm
// overwrites them with class-qualified keys.
//
// Same return convention as Load: missing file or future-version
// mismatch yields an empty map without error.
func (s *ContractStore) LoadOptions() (map[string]ContractDetailsLite, error) {
	path := filepath.Join(s.dir, contractStoreFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]ContractDetailsLite{}, nil
		}
		return nil, fmt.Errorf("read contracts: %w", err)
	}
	var f contractCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode contracts: %w", err)
	}
	if f.Version > contractStoreVersion {
		return map[string]ContractDetailsLite{}, nil
	}
	out := make(map[string]ContractDetailsLite, len(f.Options))
	today := nyDateString(time.Now())
	for k, v := range f.Options {
		if v.Expiry != "" && v.Expiry < today {
			continue
		}
		// v2 → v3 key migration. A v2 key has 3 pipes (symbol|expiry|
		// strike|right); v3 has 4 (symbol|class|expiry|strike|right).
		// Anything else is malformed and skipped (defensive — the
		// file came from our own writer, so this branch fires only
		// on hand-edited / corrupted files).
		if pipes := strings.Count(k, "|"); pipes == 3 {
			parts := strings.SplitN(k, "|", 2)
			if len(parts) == 2 {
				k = parts[0] + "||" + parts[1]
			}
		} else if pipes != 4 {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// nyDateString returns the New York trading-session date as YYYYMMDD.
// Used to compare option Expiry strings (also YYYYMMDD) for GC.
// Falls back to UTC date if the zone fails to load — keeps the GC
// pass conservative (might keep a freshly-expired entry one extra
// day, but won't drop a still-live one).
func nyDateString(now time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return now.In(loc).Format("20060102")
	}
	return now.UTC().Format("20060102")
}

// Save filters and writes contracts atomically. Only entries that pass
// shouldPersistContract are included. options is the parallel OPT
// entry map (keyed by optionContractKey); entries whose Expiry has
// passed are dropped in the GC pass. membersHash is stored alongside
// so a future Load can detect SPX reconstitution; pass "" if the
// caller doesn't track membership.
//
// Concurrent Save calls serialise on the store's mutex; the disk write
// is single-flight per store instance.
func (s *ContractStore) Save(contracts map[string]ContractDetailsLite, options map[string]ContractDetailsLite, membersHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make(map[string]ContractDetailsLite, len(contracts))
	for sym, detail := range contracts {
		if shouldPersistContract(detail) {
			filtered[sym] = detail
		}
	}

	// Option GC: drop entries whose Expiry has already passed in NY
	// time. Keeps the file size bounded — a session's worth of
	// strikes (~1700) replaces yesterday's strikes rather than
	// accumulating forever.
	filteredOptions := make(map[string]ContractDetailsLite, len(options))
	today := nyDateString(time.Now())
	for k, detail := range options {
		if detail.ConID == 0 {
			continue
		}
		if detail.Expiry != "" && detail.Expiry < today {
			continue
		}
		filteredOptions[k] = detail
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
		Options:     filteredOptions,
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

// SnapshotOptionContracts returns a defensive copy of the connection's
// in-memory optionContractCache (keyed by optionContractKey), filtered
// to entries with ConID != 0. Used by the daemon's periodic save tick
// to persist resolved option contracts to disk so a daemon restart
// within the same trading session skips the prewarm cost.
func (c *Connector) SnapshotOptionContracts() map[string]ContractDetailsLite {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil
	}
	conn.optionContractMu.RLock()
	defer conn.optionContractMu.RUnlock()
	out := make(map[string]ContractDetailsLite, len(conn.optionContractCache))
	for key, detail := range conn.optionContractCache {
		if detail.ConID != 0 {
			out[key] = detail
		}
	}
	return out
}

// IsOptionContractCached reports whether the connection's option contract
// cache has a resolved entry for (symbol, tradingClass, expiry, strike,
// right). Used by the gamma compute to filter its enumerated job list to
// strikes that actually exist as listed contracts — secDefOptParams
// returns a superset across exchanges, and asking reqMktData for a
// non-listed (Strike, Right) just burns time and trips the throttle
// detector.
//
// tradingClass is required for SPX-style multi-class underlyings (SPX vs
// SPXW share expiry+strike on third-Fridays). For single-class
// underlyings (SPY, equities) callers pass the symbol — that matches the
// TradingClass IBKR fills in for those contracts.
func (c *Connector) IsOptionContractCached(symbol, tradingClass, expiry string, strike float64, right string) bool {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return false
	}
	key := optionContractKey(symbol, tradingClass, expiry, strike, right)
	conn.optionContractMu.RLock()
	defer conn.optionContractMu.RUnlock()
	entry, ok := conn.optionContractCache[key]
	return ok && entry.ConID != 0
}

// SeedOptionContracts pre-populates the connection's optionContractCache
// from the persisted store. Called once on daemon startup before any
// gamma compute kicks. Entries already in the cache (from in-flight
// resolution races) are preserved; only empty slots get seeded.
func (c *Connector) SeedOptionContracts(options map[string]ContractDetailsLite) int {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || len(options) == 0 {
		return 0
	}
	conn.optionContractMu.Lock()
	defer conn.optionContractMu.Unlock()
	seeded := 0
	for key, detail := range options {
		if detail.ConID == 0 {
			continue
		}
		if existing, ok := conn.optionContractCache[key]; ok && existing.ConID != 0 {
			continue
		}
		conn.optionContractCache[key] = detail
		seeded++
	}
	return seeded
}
