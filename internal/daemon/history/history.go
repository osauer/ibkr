// Package history maintains history.db, a derived and always-rebuildable
// SQLite index over the daemon's append-only evidence journals
// (regime/rules/canary decisions, capital events, risk-policy governance,
// proposal outcomes, the order journal) plus the retained Flex statements.
// The journals stay the evidence of record; this package mirrors their
// complete lines into queryable tables so operator timelines
// (regime.history / rules.history / canary.history / recon.equity) and
// offline sqlite3 calibration work stop requiring jq over tens of
// megabytes, and so the order read model can serve indexed reads with an
// automatic fallback to the journal scan.
//
// Phase 2 adds rotation: the three decision journals may have fully
// ingested byte prefixes compressed into immutable gzip archives under
// rotated/. Rotation relocates evidence, it never deletes it — archives
// plus the live file concatenate back to the original byte stream, and
// `src_offset` values are offsets in that logical stream (bookkeeping
// column `base` = bytes rotated out of the live file).
//
// Ownership and safety (docs/design/history-index.md): the daemon is the
// sole writer and the sole runtime opener. Deleting history.db (plus its
// -wal/-shm sidecars) is always safe — the next daemon start re-ingests
// archives and journals from byte 0. Idempotency is a per-source
// ingested-byte offset advanced in the same SQLite transaction as its
// rows, so a crash replays from the last committed boundary and can
// neither drop nor duplicate a line. Nothing here may touch submit
// eligibility, freeze, or any broker-write path; the order journal itself
// is never rotated, truncated, or rewritten.
package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // pure-Go database/sql driver, name "sqlite"
)

// Options locates the derived index file, its journal sources, the
// retained-statement directory, and the rotation archive directory.
type Options struct {
	// DBPath is the history.db file. Pre-created 0600 before the driver
	// touches it so WAL sidecars inherit a private mode.
	DBPath string
	// RegimeJournalPath is regime-decisions.jsonl. The file may not exist
	// yet; ingest treats a missing journal as zero lines.
	RegimeJournalPath string
	// RulesJournalPath is rules-decisions.jsonl. Same missing-file
	// semantics as the regime journal.
	RulesJournalPath string
	// CanaryJournalPath is canary-decisions.jsonl (phase-2 evidence).
	CanaryJournalPath string
	// CapitalJournalPath is capital-events.jsonl (never rotated).
	CapitalJournalPath string
	// RiskPolicyJournalPath is risk-policy-journal.jsonl (never rotated).
	RiskPolicyJournalPath string
	// ProposalOutcomesPath is trade-proposal-outcomes.jsonl (never rotated).
	ProposalOutcomesPath string
	// OrderJournalPath is order-journal.jsonl. Trading evidence: indexed in
	// place, never rotated, truncated, or rewritten by this package.
	OrderJournalPath string
	// StatementsDir is the retained Flex statement directory; *.xml files
	// there derive statement_equity_days (file-set ingest, newest
	// whenGenerated wins per day).
	StatementsDir string
	// RotatedDir is where rotation writes its immutable gzip archives
	// (<journal>-YYYY-MM.jsonl.gz). Created 0700 on first rotation.
	RotatedDir string
	// Logf receives warnings (skipped corrupt lines, rebuilds, recovery,
	// rotation failures). Ingest failures never propagate to journaling or
	// snapshots — they are logged here and swallowed. nil discards.
	Logf func(format string, args ...any)
	// Infof receives the one-line rotation disclosures. nil falls back to
	// Logf.
	Infof func(format string, args ...any)
}

// Store is the daemon-owned handle on history.db: one SQLite connection
// that serializes ingest batches and read queries, plus the kick channel
// the journal writers nudge after an append.
type Store struct {
	db   *sql.DB
	opts Options
	kick chan struct{}

	// ingestMu serializes mutation passes (ingestAll, rotation, recovery)
	// so the rotation live-file swap can never race a concurrent tail
	// ingest into a spurious truncation rebuild. Read queries do not take
	// it — the single SQLite connection already serializes statements.
	ingestMu sync.Mutex

	// wmMu guards the in-memory per-source logical ingest watermarks and
	// the cached orders parse-marker flag. Watermarks are refreshed after
	// every ingest batch commit; freshness checks (OrdersFresh) read them
	// without touching SQLite. Deliberately NOT seeded at Open: the first
	// serves after a restart fall back to the legacy journal scan until the
	// first ingest pass verifies size and genesis, so the fallback path is
	// exercised on every cold start.
	wmMu       sync.Mutex
	watermarks map[string]int64
	ordersBad  bool

	// rotateFailpoint, when set (tests only), is invoked at named rotation
	// and backfill stages and aborts the operation when it errors —
	// simulating a crash for the recovery matrix.
	rotateFailpoint func(stage string) error

	closeOnce sync.Once
	closeErr  error
}

// Open opens (creating and migrating as needed) history.db at
// opts.DBPath. A corrupt or future-versioned file is deleted and recreated
// once — the index is derived state, so recovery is rebuild, not repair.
// A second consecutive failure returns the error; the caller leaves the
// index disabled for this run and journaling continues unaffected.
func Open(opts Options) (*Store, error) {
	if opts.DBPath == "" {
		return nil, errors.New("history: DBPath is required")
	}
	if err := os.MkdirAll(filepath.Dir(opts.DBPath), 0o700); err != nil {
		return nil, fmt.Errorf("history: mkdir state dir: %w", err)
	}
	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			logf(opts.Logf, "history: recreating %s after open failure: %v", opts.DBPath, lastErr)
			removeDBFiles(opts.DBPath)
		}
		db, err := openAndMigrate(opts.DBPath)
		if err == nil {
			s := &Store{db: db, opts: opts, kick: make(chan struct{}, 1), watermarks: map[string]int64{}}
			s.seedOrdersParseBad()
			return s, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("history: open %s: %w", opts.DBPath, lastErr)
}

// seedOrdersParseBad initializes the cached parse-marker flag from the
// reopened DB so a restart cannot serve indexed order reads over a file
// the previous run flagged as unparseable.
func (s *Store) seedOrdersParseBad() {
	var bad bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM order_events WHERE parse_ok = 0)`).Scan(&bad); err == nil {
		s.setOrdersParseBad(bad)
	}
}

// Close releases the SQLite connection. Idempotent; safe on a nil Store.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// Kick nudges the ingest goroutine without blocking or carrying data — the
// journal file itself is the only ingest input, which is what guarantees
// evidence-before-index ordering. Safe on a nil Store and from any
// goroutine; a kick while one is already pending coalesces.
func (s *Store) Kick() {
	if s == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run ingests every source once (this initial pass IS backfill and crash
// reconcile — first ingest simply starts from offset 0, streaming rotated
// archives before the live file where they exist) and then services kicks
// until ctx is cancelled. All ingest errors are logged and swallowed: the
// index must never fail or block journaling or snapshots.
func (s *Store) Run(ctx context.Context) {
	s.ingestAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
			s.ingestAll(ctx)
		}
	}
}

// setWatermark records a source's committed logical ingest offset for
// lock-free freshness checks.
func (s *Store) setWatermark(source string, offset int64) {
	if s == nil {
		return
	}
	s.wmMu.Lock()
	s.watermarks[source] = offset
	s.wmMu.Unlock()
}

// watermark returns a source's committed logical ingest offset; ok is
// false before the first committed ingest of this process lifetime.
func (s *Store) watermark(source string) (int64, bool) {
	s.wmMu.Lock()
	defer s.wmMu.Unlock()
	wm, ok := s.watermarks[source]
	return wm, ok
}

func (s *Store) setOrdersParseBad(bad bool) {
	if s == nil {
		return
	}
	s.wmMu.Lock()
	s.ordersBad = bad
	s.wmMu.Unlock()
}

func (s *Store) ordersParseBad() bool {
	s.wmMu.Lock()
	defer s.wmMu.Unlock()
	return s.ordersBad
}

// warnf logs through Options.Logf when set.
func (s *Store) warnf(format string, args ...any) {
	logf(s.opts.Logf, format, args...)
}

// infof logs operational disclosures (rotation summaries) through
// Options.Infof, falling back to Logf.
func (s *Store) infof(format string, args ...any) {
	if s.opts.Infof != nil {
		s.opts.Infof(format, args...)
		return
	}
	logf(s.opts.Logf, format, args...)
}

func logf(fn func(format string, args ...any), format string, args ...any) {
	if fn != nil {
		fn(format, args...)
	}
}

// removeDBFiles deletes history.db and its WAL sidecars. Best-effort: the
// subsequent open attempt reports any file that could not be cleared.
func removeDBFiles(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(p)
	}
}
