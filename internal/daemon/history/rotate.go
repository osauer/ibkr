package history

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rotation engine (docs/design/history-index.md, phase 2). Only the three
// decision journals (regime/rules/canary) are rotatable. Rotation
// compresses a fully-ingested byte prefix of the live journal into
// immutable per-month gzip archives under rotated/ and rewrites the live
// file to its tail — it relocates evidence, it NEVER deletes it. The
// stored `offset` stays a logical-stream offset (base += cut), so no row
// is ever updated and archives-in-name-order ++ live file reconstruct the
// original byte stream exactly.

// errArchiveBoundaryConflict reports an archive on disk whose bytes the
// live-file bookkeeping has not accounted for — the signature of a
// rotation that swapped the live file but died before its finalize
// transaction. Ingest heals it with a rebuild rather than wedging: left
// unhandled, the backfill refusal would abort every later pass before the
// truncation check that would otherwise recover.
var errArchiveBoundaryConflict = errors.New("archive boundary conflict")

// RotationSource binds one rotatable journal name to the daemon-side lock
// that excludes its writer while the live file is swapped. The history
// package stays daemon-import-free: the daemon hands the lockers in.
type RotationSource struct {
	// Name is the ingest source name ("regime", "rules", "canary").
	Name string
	// Locker serializes the journal's open-write-close append path.
	Locker sync.Locker
}

// rotationArchive is one planned or written archive inside a rotation_log
// intent record. SHA256 is the hex digest of the DECOMPRESSED member
// content, so recovery can prove an archive byte-equal to the journal
// prefix it was cut from.
type rotationArchive struct {
	Name     string `json:"name"`
	Months   string `json:"months"`
	GzBytes  int64  `json:"gz_bytes"`
	RawBytes int64  `json:"raw_bytes"`
	SHA256   string `json:"sha256"`
}

// archiveRun is one contiguous same-month byte range of the live-file
// prefix, destined for one archive file.
type archiveRun struct {
	name       string
	month      string
	start, end int64 // physical byte range in the live file
}

// rotationPlan is the computed cut for one source.
type rotationPlan struct {
	cut         int64
	preGenesis  string
	postGenesis string // "" = the planned tail is empty
	runs        []archiveRun
}

// archiveNameRE pins the sanctioned archive shape:
// <journal-base>-YYYY-MM[.partN].jsonl.gz with N in 2..9.
var archiveNameRE = regexp.MustCompile(`^\d{4}-\d{2}(\.part[2-9])?\.jsonl\.gz$`)

// journalArchiveBase returns the archive name stem for a journal path
// (e.g. regime-decisions.jsonl → "regime-decisions").
func journalArchiveBase(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// RotateAll runs one rotation pass over the given sources. Per-source
// failures are warned and never propagate — journaling is never blocked
// (each source's writer lock is held only across its own verified swap).
// keepMonths < 1 is clamped to 1; the current UTC month counts as month 1.
func (s *Store) RotateAll(ctx context.Context, sources []RotationSource, keepMonths int, now time.Time) {
	if s == nil {
		return
	}
	keepMonths = max(keepMonths, 1)
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return
		}
		def, ok := s.sourceByName(src.Name)
		if !ok || !def.rotatable || def.path == "" {
			s.warnf("history: rotation source %q is not rotatable; skipping", src.Name)
			continue
		}
		if src.Locker == nil {
			s.warnf("history: rotation source %q has no writer lock; skipping", src.Name)
			continue
		}
		if err := s.rotateSource(ctx, def, src.Locker, keepMonths, now); err != nil {
			s.warnf("history: rotate %s: %v", def.name, err)
		}
	}
}

// rotateSource performs the pinned 8-step rotation sequence for one
// source under its writer lock and the store's ingest lock. Every failure
// path releases both locks; a crash at any point is resolved by
// RecoverRotations on the next start.
func (s *Store) rotateSource(ctx context.Context, def sourceDef, locker sync.Locker, keepMonths int, now time.Time) error {
	locker.Lock()
	defer locker.Unlock()
	// The ingest lock keeps the tail-ingest goroutine out for the whole
	// critical section: after the live-file swap and before the finalize
	// transaction, a concurrent ingest would misread the shrunken file as
	// a truncation and trigger a spurious rebuild.
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()

	st, err := os.Stat(def.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	mode := st.Mode().Perm()

	// Precondition: the index must be caught up (offset-base == size), so
	// rotation only ever archives bytes already ingested into history.db
	// and the prefix ends on a complete line. One synchronous inline
	// ingest is allowed — the maintenance goroutine may block here, hot
	// paths never do.
	offset, base, err := s.sourceBookkeeping(def.name)
	if err != nil {
		return err
	}
	if offset-base != st.Size() {
		if err := s.ingestSource(ctx, def); err != nil {
			return fmt.Errorf("inline catch-up: %w", err)
		}
		if st, err = os.Stat(def.path); err != nil {
			return err
		}
		if offset, base, err = s.sourceBookkeeping(def.name); err != nil {
			return err
		}
		if offset-base != st.Size() {
			return fmt.Errorf("index still %d bytes behind the journal after inline catch-up; skipping this pass", st.Size()-(offset-base))
		}
	}
	liveSize := st.Size()
	if liveSize == 0 {
		return nil
	}

	plan, err := s.planRotation(def, liveSize, keepMonths, now)
	if err != nil {
		return err
	}
	if plan == nil || plan.cut == 0 {
		return nil // quiet no-op: nothing outside the keep window
	}

	// Step 3: write archives as temps, fsync each.
	if err := os.MkdirAll(s.opts.RotatedDir, 0o700); err != nil {
		return err
	}
	archives := make([]rotationArchive, 0, len(plan.runs))
	for _, run := range plan.runs {
		arch, err := s.writeArchiveTemp(def.path, run)
		if err != nil {
			return err
		}
		archives = append(archives, arch)
	}
	if err := s.failpoint("temps-written"); err != nil {
		return err
	}

	// Step 4: intent transaction.
	archivesJSON, err := json.Marshal(archives)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`INSERT INTO rotation_log
(source, started_at, state, cut_bytes, live_size, base_before, pre_genesis, post_genesis, archives_json)
VALUES (?, ?, 'pending', ?, ?, ?, ?, ?, ?)`,
		def.name, nowUTC(), plan.cut, liveSize, base, plan.preGenesis, plan.postGenesis, string(archivesJSON))
	if err != nil {
		return err
	}
	rotID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if err := s.failpoint("intent"); err != nil {
		return err
	}

	// Step 5: temps → final names; fsync the archive dir.
	for _, arch := range archives {
		tmp := filepath.Join(s.opts.RotatedDir, ".tmp-"+arch.Name)
		if err := os.Rename(tmp, filepath.Join(s.opts.RotatedDir, arch.Name)); err != nil {
			return err
		}
		if err := s.failpoint("rename:" + arch.Name); err != nil {
			return err
		}
	}
	syncDir(s.opts.RotatedDir)
	if err := s.failpoint("renamed"); err != nil {
		return err
	}

	// Step 6: rewrite the live tail atomically, preserving the original
	// file mode. This rename is the file-side commit point.
	if err := swapLiveTail(def.path, plan.cut, liveSize, mode); err != nil {
		return err
	}
	if err := s.failpoint("swapped"); err != nil {
		return err
	}

	// Step 7: finalize transaction (base advance, archive inventory,
	// rotation_log done) and watermark refresh.
	if err := s.finalizeRotation(rotID, def.name, plan.cut, plan.postGenesis, archives); err != nil {
		return err
	}

	months := make([]string, 0, len(archives))
	names := make([]string, 0, len(archives))
	var rawTotal int64
	for _, a := range archives {
		months = append(months, a.Months)
		names = append(names, a.Name)
		rawTotal += a.RawBytes
	}
	s.infof("history: rotated %s: months %s, %d bytes archived to %s, live file now %d bytes",
		def.name, strings.Join(months, ","), rawTotal, strings.Join(names, ", "), liveSize-plan.cut)
	return nil
}

// planRotation scans the live file and computes the cut: the byte offset
// of the first line whose timestamp falls inside the keep window.
// Everything before it is partitioned into contiguous same-month runs. A
// line with an unparseable timestamp inherits the previous line's month;
// an unparseable FIRST line aborts (safe direction). Archive names must
// sort lexically after every existing archive of the source so that
// name-order concatenation always reproduces stream order; a run that
// cannot satisfy that (out-of-order stray months) truncates the cut at
// its start with a warning.
func (s *Store) planRotation(def sourceDef, liveSize int64, keepMonths int, now time.Time) (*rotationPlan, error) {
	oldestKeep := monthIndex(now.UTC()) - (keepMonths - 1)

	f, err := os.Open(def.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := newLineScanner(f)

	plan := &rotationPlan{}
	var pos int64
	var curIdx int
	var runs []archiveRun
	first := true
	cutFound := false
	for sc.Scan() {
		line := sc.Bytes()
		lineStart := pos
		pos += int64(len(line)) + 1
		ts, ok := lineTimestamp(line, def.tsField)
		if !ok {
			if first {
				return nil, fmt.Errorf("first journal line has no parseable %q timestamp; aborting rotation for safety", def.tsField)
			}
			// inherit curMonth
		} else {
			idx := monthIndex(ts.UTC())
			if idx >= oldestKeep {
				plan.cut = lineStart
				plan.postGenesis = lineHash(line)
				cutFound = true
				break
			}
			if first || idx != curIdx {
				runs = append(runs, archiveRun{month: ts.UTC().Format("2006-01"), start: lineStart})
				curIdx = idx
			}
		}
		if first {
			plan.preGenesis = lineHash(line)
			first = false
			if len(runs) == 0 {
				// First line parsed into the keep window already handled
				// above; an inherited-month first line cannot happen (abort).
				// Reaching here means the first line opened a run.
				return nil, fmt.Errorf("internal rotation planning error (no initial run)")
			}
		}
		runs[len(runs)-1].end = pos
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !cutFound {
		// Every line is older than the keep window: the whole file rotates
		// and the planned tail is empty.
		plan.cut = liveSize
		plan.postGenesis = ""
	}
	if plan.cut == 0 {
		return plan, nil
	}
	// Close the final run at the cut.
	if len(runs) > 0 && runs[len(runs)-1].end > plan.cut {
		runs[len(runs)-1].end = plan.cut
	}

	// Assign archive names in stream order with the lexical-order guard.
	existing, maxExisting, err := s.existingArchiveNames(def)
	if err != nil {
		return nil, err
	}
	planned := map[string]bool{}
	last := maxExisting
	kept := runs[:0]
	for _, run := range runs {
		name, err := nextArchiveName(journalArchiveBase(def.path), run.month, existing, planned)
		if err != nil {
			s.warnf("history: %s month %s: %v; truncating this rotation at byte %d", def.name, run.month, err, run.start)
			plan.cut = run.start
			plan.postGenesis = "" // recomputed below
			break
		}
		if last != "" && name <= last {
			s.warnf("history: %s month %s would archive as %s, which does not sort after %s — out-of-order journal months; truncating this rotation at byte %d",
				def.name, run.month, name, last, run.start)
			plan.cut = run.start
			plan.postGenesis = ""
			break
		}
		run.name = name
		planned[name] = true
		last = name
		kept = append(kept, run)
	}
	plan.runs = kept
	if len(kept) == 0 {
		plan.cut = 0
		return plan, nil
	}
	// The cut is always the end of the last kept run — the order guard may
	// have moved it earlier. Re-derive the tail's first-line hash there
	// unconditionally: a stale or empty postGenesis would disarm
	// replaced-journal detection for this source until the next pass.
	plan.cut = kept[len(kept)-1].end
	hash, ok, err := fileLineHashAt(def.path, plan.cut)
	if err != nil {
		return nil, err
	}
	if !ok {
		plan.postGenesis = ""
	} else {
		plan.postGenesis = hash
	}
	return plan, nil
}

// existingArchiveNames lists this source's well-formed archives on disk
// (the collision set) and the lexically greatest of them (the order
// guard's floor). Wrong-shape files carrying the source prefix are warned
// and ignored.
func (s *Store) existingArchiveNames(def sourceDef) (map[string]bool, string, error) {
	names := map[string]bool{}
	maxName := ""
	entries, err := os.ReadDir(s.opts.RotatedDir)
	if err != nil {
		if os.IsNotExist(err) {
			return names, "", nil
		}
		return nil, "", err
	}
	prefix := journalArchiveBase(def.path) + "-"
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || strings.HasPrefix(name, ".tmp-") {
			continue
		}
		if !archiveNameRE.MatchString(strings.TrimPrefix(name, prefix)) {
			s.warnf("history: rotated file %s does not match the archive naming shape; ignoring it", name)
			continue
		}
		names[name] = true
		maxName = max(maxName, name)
	}
	return names, maxName, nil
}

// nextArchiveName picks <base>-<month>.jsonl.gz, falling back to
// .part2 … .part9 when taken. Beyond part9 the month is skipped with an
// error (the caller warns and truncates the rotation).
func nextArchiveName(base, month string, existing, planned map[string]bool) (string, error) {
	candidate := fmt.Sprintf("%s-%s.jsonl.gz", base, month)
	if !existing[candidate] && !planned[candidate] {
		return candidate, nil
	}
	for n := 2; n <= 9; n++ {
		candidate = fmt.Sprintf("%s-%s.part%d.jsonl.gz", base, month, n)
		if !existing[candidate] && !planned[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("all archive names through part9 are taken")
}

// writeArchiveTemp writes one run's exact bytes as a single-member gzip
// file at rotated/.tmp-<final-name>, fsyncs it, and returns its intent
// record (raw sha256, sizes).
func (s *Store) writeArchiveTemp(journalPath string, run archiveRun) (rotationArchive, error) {
	src, err := os.Open(journalPath)
	if err != nil {
		return rotationArchive{}, err
	}
	defer src.Close()
	section := io.NewSectionReader(src, run.start, run.end-run.start)

	tmp := filepath.Join(s.opts.RotatedDir, ".tmp-"+run.name)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return rotationArchive{}, err
	}
	gz := gzip.NewWriter(out)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(gz, h), section)
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return rotationArchive{}, fmt.Errorf("write archive %s: %w", run.name, err)
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return rotationArchive{}, err
	}
	return rotationArchive{
		Name:     run.name,
		Months:   run.month,
		GzBytes:  st.Size(),
		RawBytes: n,
		SHA256:   hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// swapLiveTail writes bytes [cut, size) to a temp in the journal's own
// directory, fsyncs it, restores the journal's original mode, and renames
// it over the live path — the file-side commit point of a rotation.
func swapLiveTail(path string, cut, size int64, mode os.FileMode) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := filepath.Join(filepath.Dir(path), ".tmp-rotate-"+filepath.Base(path))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.NewSectionReader(src, cut, size-cut))
	if err == nil {
		err = out.Sync()
	}
	if err == nil {
		err = out.Chmod(mode)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write live tail: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// finalizeRotation is Txn B: advance base, reset genesis to the tail's
// first-line hash, record the archives, and mark the rotation done. Shared
// by the normal path and roll-forward recovery.
func (s *Store) finalizeRotation(rotID int64, source string, cut int64, postGenesis string, archives []rotationArchive) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE ingest_sources SET base = base + ?, genesis = ?, updated_at = ? WHERE source = ?`,
		cut, nullableString(postGenesis), nowUTC(), source); err != nil {
		return err
	}
	for _, arch := range archives {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO archive_files (source, name, raw_bytes, gz_bytes, origin, created_at)
VALUES (?, ?, ?, ?, 'rotation', ?)`, source, arch.Name, arch.RawBytes, arch.GzBytes, nowUTC()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE rotation_log SET state = 'done', finished_at = ? WHERE id = ?`, nowUTC(), rotID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The logical offset is untouched by rotation; refresh the watermark
	// from bookkeeping anyway so it can never go stale across the swap.
	if offset, _, err := s.sourceBookkeeping(source); err == nil {
		s.setWatermark(source, offset)
	}
	return nil
}

// RecoverRotations resolves every rotation_log row left in state
// 'pending' by a crash. Must be called after Open and before Run — i.e.
// before writer traffic — with the same lockers rotation uses. Each
// intermediate crash state lands in exactly one branch, and the evidence
// multiset (live file ∪ archives, minus verified-duplicate rollback
// deletions) is invariant throughout.
func (s *Store) RecoverRotations(sources []RotationSource) {
	if s == nil {
		return
	}
	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()

	lockers := map[string]sync.Locker{}
	for _, src := range sources {
		if src.Locker != nil {
			lockers[src.Name] = src.Locker
		}
	}

	type pendingRow struct {
		id                      int64
		source                  string
		cut                     int64
		preGenesis, postGenesis sql.NullString
		archivesJSON            string
	}
	var pending []pendingRow
	rows, err := s.db.Query(`SELECT id, source, cut_bytes, pre_genesis, post_genesis, archives_json FROM rotation_log WHERE state = 'pending' ORDER BY id`)
	if err != nil {
		s.warnf("history: rotation recovery scan failed: %v", err)
		return
	}
	for rows.Next() {
		var row pendingRow
		if err := rows.Scan(&row.id, &row.source, &row.cut, &row.preGenesis, &row.postGenesis, &row.archivesJSON); err != nil {
			s.warnf("history: rotation recovery scan failed: %v", err)
			rows.Close()
			return
		}
		pending = append(pending, row)
	}
	rows.Close()

	for _, row := range pending {
		locker, ok := lockers[row.source]
		if !ok {
			s.warnf("history: pending rotation %d for %s has no writer lock; leaving it pending", row.id, row.source)
			continue
		}
		locker.Lock()
		s.recoverRotation(row.id, row.source, row.cut, row.preGenesis.String, row.postGenesis.String, row.archivesJSON)
		locker.Unlock()
	}

	// Orphan temps are copies, never evidence: clear them unconditionally.
	if entries, err := os.ReadDir(s.opts.RotatedDir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				_ = os.Remove(filepath.Join(s.opts.RotatedDir, e.Name()))
			}
		}
	}
}

// recoverRotation discriminates one pending rotation on the live file's
// first-line hash and rolls it back (swap never happened; archives are
// verified duplicates and deleted) or forward (swap happened; archives
// are the only copy of the prefix and the finalize transaction runs).
func (s *Store) recoverRotation(rotID int64, source string, cut int64, preGenesis, postGenesis, archivesJSON string) {
	def, ok := s.sourceByName(source)
	if !ok {
		s.warnf("history: pending rotation %d references unknown source %q; leaving it pending", rotID, source)
		return
	}
	var archives []rotationArchive
	if err := json.Unmarshal([]byte(archivesJSON), &archives); err != nil {
		s.warnf("history: pending rotation %d has an undecodable intent record: %v; leaving it pending", rotID, err)
		return
	}

	liveHash := ""
	liveOK := false
	if f, err := os.Open(def.path); err == nil {
		liveHash, liveOK = firstLineHash(f)
		_ = f.Close()
	}

	switch {
	case liveOK && liveHash == preGenesis:
		s.rollbackRotation(rotID, def, archives)
	case (liveOK && postGenesis != "" && liveHash == postGenesis) || (postGenesis == "" && (!liveOK || liveHash != preGenesis)):
		s.rollForwardRotation(rotID, def, cut, postGenesis, archives)
	default:
		s.warnf("history: pending rotation %d for %s matches neither the pre- nor post-rotation journal (external change?); archives kept, rotation marked aborted — manual review advised", rotID, source)
		s.markRotationAborted(rotID)
	}
}

// rollbackRotation handles "swap did not happen": every intent archive
// that exists is verified as an exact duplicate of the untouched journal
// prefix (gz size, member sha256, and a live-prefix re-hash) and then
// deleted — the only sanctioned archive deletion anywhere. On any
// mismatch the file is quarantined (renamed out of the archive namespace)
// instead of deleted, loudly.
func (s *Store) rollbackRotation(rotID int64, def sourceDef, archives []rotationArchive) {
	prefixOK := verifyLivePrefixMatchesArchives(def.path, archives) == nil
	for _, arch := range archives {
		final := filepath.Join(s.opts.RotatedDir, arch.Name)
		if _, err := os.Stat(final); err != nil {
			continue // never renamed from temp; temp cleanup handles it
		}
		if prefixOK && verifyArchiveFile(final, arch) == nil {
			if err := os.Remove(final); err != nil {
				s.warnf("history: rotation %d rollback could not delete duplicate archive %s: %v", rotID, arch.Name, err)
			}
			continue
		}
		quarantine := final + fmt.Sprintf(".quarantine-%d", rotID)
		s.warnf("history: rotation %d rollback: archive %s does not verify against the journal prefix; quarantining as %s instead of deleting",
			rotID, arch.Name, filepath.Base(quarantine))
		if err := os.Rename(final, quarantine); err != nil {
			s.warnf("history: quarantine of %s failed: %v (file left in place; backfill will not ingest it twice thanks to archive_files)", arch.Name, err)
		}
	}
	s.markRotationAborted(rotID)
	s.infof("history: rotation %d for %s rolled back after crash; journal untouched, next scheduled rotation retries", rotID, def.name)
}

// rollForwardRotation handles "swap happened": the archives now hold the
// only raw copy of the prefix. Their presence is verified (the DB itself
// still holds every row, so a missing archive is an evidence-copy loss to
// disclose, not a data loss) and the finalize transaction runs.
func (s *Store) rollForwardRotation(rotID int64, def sourceDef, cut int64, postGenesis string, archives []rotationArchive) {
	for _, arch := range archives {
		final := filepath.Join(s.opts.RotatedDir, arch.Name)
		if _, err := os.Stat(final); err == nil {
			continue
		}
		// The rename step is strictly before the swap, so finals must
		// exist; check temps anyway in case of an unexpected state.
		tmp := filepath.Join(s.opts.RotatedDir, ".tmp-"+arch.Name)
		if _, terr := os.Stat(tmp); terr == nil {
			if verifyArchiveFile(tmp, arch) == nil {
				if rerr := os.Rename(tmp, final); rerr == nil {
					continue
				}
			}
		}
		s.warnf("history: CRITICAL: rotation %d for %s completed its journal swap but archive %s is missing — the raw prefix copy is lost (index rows remain intact in history.db)", rotID, def.name, arch.Name)
	}
	syncDir(s.opts.RotatedDir)
	if err := s.finalizeRotation(rotID, def.name, cut, postGenesis, archives); err != nil {
		s.warnf("history: rotation %d roll-forward finalize failed: %v (left pending for the next start)", rotID, err)
		return
	}
	s.infof("history: rotation %d for %s rolled forward after crash; archives finalized", rotID, def.name)
}

func (s *Store) markRotationAborted(rotID int64) {
	if _, err := s.db.Exec(`UPDATE rotation_log SET state = 'aborted', finished_at = ? WHERE id = ?`, nowUTC(), rotID); err != nil {
		s.warnf("history: could not mark rotation %d aborted: %v", rotID, err)
	}
}

// verifyArchiveFile checks a gzip archive against its intent record: exact
// gz size, decompressed length, and member sha256.
func verifyArchiveFile(path string, want rotationArchive) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() != want.GzBytes {
		return fmt.Errorf("gz size %d != intent %d", st.Size(), want.GzBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	h := sha256.New()
	n, err := io.Copy(h, gz)
	if err != nil {
		return err
	}
	if n != want.RawBytes {
		return fmt.Errorf("raw size %d != intent %d", n, want.RawBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 {
		return fmt.Errorf("member sha256 mismatch")
	}
	return nil
}

// verifyLivePrefixMatchesArchives re-hashes the live file's prefix
// against the intent records — the stronger duplicate proof rollback
// requires before it may delete an archive.
func verifyLivePrefixMatchesArchives(path string, archives []rotationArchive) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var pos int64
	for _, arch := range archives {
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, pos, arch.RawBytes)); err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != arch.SHA256 {
			return fmt.Errorf("journal prefix does not match archive %s", arch.Name)
		}
		pos += arch.RawBytes
	}
	return nil
}

// backfillArchives streams every on-disk archive of the source that
// archive_files does not record yet — the (re)build path that keeps
// "every DB row rebuildable from files" true after rotation. During
// normal operation every archive is recorded and this is a cheap no-op;
// archives are never read outside a rebuild. Callers hold ingestMu.
func (s *Store) backfillArchives(ctx context.Context, def sourceDef) error {
	entries, err := os.ReadDir(s.opts.RotatedDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	prefix := journalArchiveBase(def.path) + "-"
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || strings.HasPrefix(name, ".tmp-") {
			continue
		}
		if !archiveNameRE.MatchString(strings.TrimPrefix(name, prefix)) {
			s.warnf("history: rotated file %s does not match the archive naming shape; skipping it", name)
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	recorded := map[string]bool{}
	rows, err := s.db.Query(`SELECT name FROM archive_files WHERE source = ?`, def.name)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		recorded[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, name := range names {
		if recorded[name] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.backfillArchive(ctx, def, name); err != nil {
			return fmt.Errorf("backfill archive %s: %w", name, err)
		}
	}
	return nil
}

// backfillArchive streams one archive's lines into the source's tables at
// logical offsets starting from the current base, with INSERT OR IGNORE
// so a crashed backfill resumes without duplicates, then records the
// archive and advances base (and offset) in one completion transaction.
func (s *Store) backfillArchive(ctx context.Context, def sourceDef, name string) error {
	offset, base, err := s.sourceBookkeeping(def.name)
	if err != nil {
		return err
	}
	if offset != base {
		return fmt.Errorf("live-file bytes are already ingested past the archive boundary (offset %d, base %d): %w", offset, base, errArchiveBoundaryConflict)
	}

	path := filepath.Join(s.opts.RotatedDir, name)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	sc := newLineScanner(gz)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO ingest_sources (source, path, offset) VALUES (?, ?, 0)`, def.name, def.path); err != nil {
		return err
	}

	lineStart := base
	linesInBatch := 0
	skipped := 0
	for sc.Scan() {
		line := sc.Bytes()
		if err := def.insertLine(tx, lineStart, line, true); err != nil {
			parseErr, ok := errors.AsType[*lineParseError](err)
			if !ok {
				return fmt.Errorf("insert archived line at logical offset %d: %w", lineStart, err)
			}
			skipped++
			s.warnf("history: %s archived line at logical offset %d is unparseable and was skipped: %v", def.name, lineStart, parseErr.err)
		}
		lineStart += int64(len(line)) + 1
		linesInBatch++
		if linesInBatch >= ingestBatchLines {
			if err := tx.Commit(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if tx, err = s.db.Begin(); err != nil {
				return err
			}
			linesInBatch = 0
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.failpoint("backfill-rows:" + name); err != nil {
		return err
	}

	rawBytes := lineStart - base
	final, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = final.Rollback() }()
	if _, err := final.Exec(`UPDATE ingest_sources SET base = ?, offset = ?, updated_at = ? WHERE source = ?`,
		base+rawBytes, base+rawBytes, nowUTC(), def.name); err != nil {
		return err
	}
	if _, err := final.Exec(`INSERT OR IGNORE INTO archive_files (source, name, raw_bytes, gz_bytes, origin, created_at)
VALUES (?, ?, ?, ?, 'backfill', ?)`, def.name, name, rawBytes, st.Size(), nowUTC()); err != nil {
		return err
	}
	if err := final.Commit(); err != nil {
		return err
	}
	s.setWatermark(def.name, base+rawBytes)
	if skipped > 0 {
		s.warnf("history: %s archive %s backfill skipped %d unparseable line(s)", def.name, name, skipped)
	}
	return nil
}

// sourceBookkeeping reads one source's (offset, base); a missing row is
// (0, 0).
func (s *Store) sourceBookkeeping(name string) (offset, base int64, err error) {
	err = s.db.QueryRow(`SELECT offset, base FROM ingest_sources WHERE source = ?`, name).Scan(&offset, &base)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return offset, base, err
}

func (s *Store) failpoint(stage string) error {
	if s.rotateFailpoint == nil {
		return nil
	}
	return s.rotateFailpoint(stage)
}

// syncDir best-effort fsyncs a directory so renames inside it are
// durable before dependent state commits.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// lineTimestamp extracts and parses the journal timestamp field ("ts" or
// "at") from one line.
func lineTimestamp(line []byte, field string) (time.Time, bool) {
	var probe struct {
		TS string `json:"ts"`
		At string `json:"at"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return time.Time{}, false
	}
	raw := probe.TS
	if field == "at" {
		raw = probe.At
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// monthIndex maps a UTC instant to a comparable calendar-month ordinal.
func monthIndex(t time.Time) int {
	return t.Year()*12 + int(t.Month()) - 1
}

func lineHash(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}

// fileLineHashAt hashes the complete line starting at the given physical
// offset; ok is false when the file ends at or before the offset.
func fileLineHashAt(path string, offset int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", false, err
	}
	sc := newLineScanner(f)
	if !sc.Scan() {
		return "", false, sc.Err()
	}
	return lineHash(sc.Bytes()), true, nil
}

// newLineScanner returns a complete-lines scanner with the package's
// buffer bounds — the same splitter live ingest uses, so archives and
// live files tokenize identically.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, scanBufInitial), scanBufMax)
	sc.Split(scanCompleteLines)
	return sc
}
