package history

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// rotationNow is the fixed "now" for rotation tests: keep window with
// keepMonths=2 covers 2026-07 and 2026-06.
var rotationNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

// buildMonthlyJournal writes count regime-shaped lines per month and
// returns the file content.
func buildMonthlyJournal(t *testing.T, path string, months []string, perMonth int) string {
	t.Helper()
	var b strings.Builder
	for _, month := range months {
		for i := range perMonth {
			fmt.Fprintf(&b, `{"v":1,"ts":"%s-%02dT10:00:00Z","stage":"calm","fingerprint":"fp-%s-%d"}`+"\n", month, i+1, month, i)
		}
	}
	writeJournal(t, path, b.String())
	return b.String()
}

// reconstructStream concatenates the source's archives (name order,
// decompressed) with the live file — the D1 identity that must equal the
// original byte stream at all times.
func reconstructStream(t *testing.T, opts Options, journalPath string) string {
	t.Helper()
	var out bytes.Buffer
	entries, err := os.ReadDir(opts.RotatedDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	prefix := journalArchiveBase(journalPath) + "-"
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && !strings.HasPrefix(e.Name(), ".tmp-") && !strings.Contains(e.Name(), ".quarantine-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := os.Open(filepath.Join(opts.RotatedDir, name))
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("archive %s: %v", name, err)
		}
		if _, err := io.Copy(&out, gz); err != nil {
			t.Fatalf("archive %s: %v", name, err)
		}
		_ = gz.Close()
		_ = f.Close()
	}
	live, err := os.ReadFile(journalPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	out.Write(live)
	return out.String()
}

func dumpRows(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT id, src_offset, raw_json FROM " + table + " ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, off int64
		var raw string
		if err := rows.Scan(&id, &off, &raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d|%d|%s", id, off, raw))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func regimeRotationSource() []RotationSource {
	return []RotationSource{{Name: sourceRegime, Locker: &sync.Mutex{}}}
}

func TestRotationGolden(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	original := buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 3)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	offsetBefore := sourceOffset(t, s, sourceRegime)
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)

	// Cut = everything before the first 2026-06 line.
	lines := strings.SplitAfter(original, "\n")
	var prefix, april, may string
	for _, line := range lines {
		switch {
		case strings.Contains(line, `"2026-04-`):
			april += line
			prefix += line
		case strings.Contains(line, `"2026-05-`):
			may += line
			prefix += line
		}
	}
	cut := int64(len(prefix))

	// Archives: exact per-month bytes, one gzip member each.
	for name, want := range map[string]string{
		"regime-decisions-2026-04.jsonl.gz": april,
		"regime-decisions-2026-05.jsonl.gz": may,
	} {
		f, err := os.Open(filepath.Join(opts.RotatedDir, name))
		if err != nil {
			t.Fatalf("archive %s missing: %v", name, err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(gz)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if string(got) != want {
			t.Errorf("archive %s content is not the exact original bytes", name)
		}
	}

	// Live tail preserved byte-for-byte; mode preserved (0600 fixture).
	live, err := os.ReadFile(opts.RegimeJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != original[cut:] {
		t.Error("live tail differs from the original suffix")
	}
	st, err := os.Stat(opts.RegimeJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("live mode = %o, want 0600 preserved", st.Mode().Perm())
	}

	// Bookkeeping: base advanced by cut, logical offset untouched, genesis
	// reset to the tail's first line.
	var offset, base int64
	var genesis string
	if err := s.db.QueryRow(`SELECT offset, base, genesis FROM ingest_sources WHERE source = 'regime'`).Scan(&offset, &base, &genesis); err != nil {
		t.Fatal(err)
	}
	if base != cut || offset != offsetBefore {
		t.Errorf("bookkeeping = offset %d base %d, want offset %d base %d", offset, base, offsetBefore, cut)
	}
	firstTail := original[cut : cut+int64(strings.IndexByte(original[cut:], '\n'))]
	if genesis != lineHash([]byte(firstTail)) {
		t.Error("genesis was not reset to the tail's first-line hash")
	}

	// rotation_log done; archive_files recorded as rotation-origin.
	var state string
	if err := s.db.QueryRow(`SELECT state FROM rotation_log ORDER BY id DESC LIMIT 1`).Scan(&state); err != nil || state != "done" {
		t.Fatalf("rotation_log state = %q (%v), want done", state, err)
	}
	var archived int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM archive_files WHERE source = 'regime' AND origin = 'rotation'`).Scan(&archived); err != nil || archived != 2 {
		t.Fatalf("archive_files rows = %d (%v), want 2", archived, err)
	}

	// D1 identity: archives ++ live reconstruct the original stream.
	if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original {
		t.Error("archives ++ live do not reconstruct the original stream")
	}

	// Post-rotation ingest of appended lines lands at correct logical offsets.
	appendJournal(t, opts.RegimeJournalPath, `{"v":1,"ts":"2026-07-15T09:00:00Z","stage":"watch"}`+"\n")
	s.ingestAll(context.Background())
	var stage string
	if err := s.db.QueryRow(`SELECT stage FROM regime_decisions ORDER BY src_offset DESC LIMIT 1`).Scan(&stage); err != nil || stage != "watch" {
		t.Fatalf("post-rotation append not ingested: %q %v", stage, err)
	}

	// A second rotation with nothing outside the keep window is a quiet no-op.
	before := dumpRows(t, s, "regime_decisions")
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	if got := dumpRows(t, s, "regime_decisions"); len(got) != len(before) {
		t.Error("no-op rotation changed rows")
	}
}

// TestRotationPreservesRulesMode pins the 0644 rules-journal mode across
// a rotation (rules lines use the "at" timestamp field).
func TestRotationPreservesRulesMode(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	var b strings.Builder
	for i := range 3 {
		fmt.Fprintf(&b, `{"version":1,"at":"2026-04-%02dT09:00:00Z","rule":"r","status":"pass"}`+"\n", i+1)
	}
	fmt.Fprintf(&b, `{"version":1,"at":"2026-07-01T09:00:00Z","rule":"r","status":"watch"}`+"\n")
	if err := os.WriteFile(opts.RulesJournalPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.RotateAll(context.Background(), []RotationSource{{Name: sourceRules, Locker: &sync.Mutex{}}}, 2, rotationNow)

	st, err := os.Stat(opts.RulesJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("rules journal mode = %o, want 0644 preserved", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(opts.RotatedDir, "rules-decisions-2026-04.jsonl.gz")); err != nil {
		t.Fatalf("rules archive missing: %v", err)
	}
	if got := reconstructStream(t, opts, opts.RulesJournalPath); got != b.String() {
		t.Error("rules stream identity broken")
	}
}

// TestRotationRebuildIdentity is the binding backfill test: after a
// rotation, deleting history.db and reopening reproduces identical rows
// including ids (archives stream before the live file).
func TestRotationRebuildIdentity(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 4)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	want := dumpRows(t, s, "regime_decisions")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	removeDBFiles(opts.DBPath)

	s2 := openTestStore(t, opts)
	s2.ingestAll(context.Background())
	got := dumpRows(t, s2, "regime_decisions")
	if len(got) != len(want) {
		t.Fatalf("rebuild rows = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rebuild row %d differs:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
	// archive_files re-recorded as backfill.
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM archive_files WHERE origin = 'backfill'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("backfill archive_files rows = %d (%v), want 2", n, err)
	}
}

// TestRotationCrashMatrix aborts rotation at every failpoint stage, runs
// recovery, and asserts the pinned invariants: the evidence stream is
// intact, the pending row resolves to exactly rollback or roll-forward,
// and a subsequent rotation converges to the golden outcome.
func TestRotationCrashMatrix(t *testing.T) {
	t.Parallel()
	stages := []struct {
		stage       string
		wantForward bool // false = rollback expected
	}{
		{"temps-written", false},
		{"intent", false},
		{"rename:regime-decisions-2026-04.jsonl.gz", false},
		{"renamed", false},
		{"swapped", true},
	}
	for _, tc := range stages {
		t.Run(tc.stage, func(t *testing.T) {
			t.Parallel()
			opts := testOptions(t)
			original := buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 3)
			s := openTestStore(t, opts)
			s.ingestAll(context.Background())
			rowsBefore := dumpRows(t, s, "regime_decisions")

			boom := errors.New("injected crash")
			s.rotateFailpoint = func(stage string) error {
				if stage == tc.stage {
					return boom
				}
				return nil
			}
			s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow) // warns, does not propagate
			s.rotateFailpoint = nil

			// Crash state: no evidence byte is lost. Before the swap the
			// prefix may exist twice (untouched journal + archive copies);
			// after the swap the archives + tail reconstruct the stream.
			if tc.wantForward {
				if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original {
					t.Fatalf("evidence stream broken in crash state %s", tc.stage)
				}
			} else {
				live, err := os.ReadFile(opts.RegimeJournalPath)
				if err != nil || string(live) != original {
					t.Fatalf("journal modified before its swap in crash state %s (%v)", tc.stage, err)
				}
			}

			// Simulate the restart: close, reopen, recover.
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s2 := openTestStore(t, opts)
			s2.RecoverRotations(regimeRotationSource())

			if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original {
				t.Fatalf("evidence stream broken after recovery from %s", tc.stage)
			}
			var state string
			if err := s2.db.QueryRow(`SELECT state FROM rotation_log ORDER BY id DESC LIMIT 1`).Scan(&state); err != nil {
				if tc.stage == "temps-written" && errors.Is(err, errNoRotationRow(err)) {
					state = "" // no intent row was written yet
				} else if tc.stage != "temps-written" {
					t.Fatal(err)
				}
			}
			if tc.wantForward {
				if state != "done" {
					t.Fatalf("state after recovery = %q, want done (roll-forward)", state)
				}
				var base int64
				if err := s2.db.QueryRow(`SELECT base FROM ingest_sources WHERE source = 'regime'`).Scan(&base); err != nil || base == 0 {
					t.Fatalf("roll-forward did not advance base (%d, %v)", base, err)
				}
			} else if state == "pending" {
				t.Fatalf("pending rotation left unresolved after recovery from %s", tc.stage)
			}

			// No temps survive recovery.
			entries, _ := os.ReadDir(opts.RotatedDir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".tmp-") {
					t.Fatalf("orphan temp %s survived recovery", e.Name())
				}
			}

			// Convergence: the next scheduled rotation completes the job.
			s2.ingestAll(context.Background())
			s2.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
			if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original {
				t.Fatalf("evidence stream broken after converging rotation")
			}
			var doneBase int64
			if err := s2.db.QueryRow(`SELECT base FROM ingest_sources WHERE source = 'regime'`).Scan(&doneBase); err != nil || doneBase == 0 {
				t.Fatalf("converged rotation base = %d (%v), want > 0", doneBase, err)
			}
			live, err := os.ReadFile(opts.RegimeJournalPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(live), `"2026-04-`) || strings.Contains(string(live), `"2026-05-`) {
				t.Fatal("converged live file still holds keep-window-expired months")
			}

			// Row set unchanged throughout (rotation moves bytes, never rows).
			if got := dumpRows(t, s2, "regime_decisions"); len(got) != len(rowsBefore) {
				t.Fatalf("row count changed across crash/recovery: %d != %d", len(got), len(rowsBefore))
			}

			// Full-rebuild identity from the converged state.
			if err := s2.Close(); err != nil {
				t.Fatal(err)
			}
			removeDBFiles(opts.DBPath)
			s3 := openTestStore(t, opts)
			s3.ingestAll(context.Background())
			got := dumpRows(t, s3, "regime_decisions")
			if len(got) != len(rowsBefore) {
				t.Fatalf("rebuild rows = %d, want %d", len(got), len(rowsBefore))
			}
			for i := range rowsBefore {
				if got[i] != rowsBefore[i] {
					t.Fatalf("rebuild row %d differs after crash matrix %s", i, tc.stage)
				}
			}
		})
	}
}

// errNoRotationRow matches database/sql's no-rows error without importing
// database/sql here.
func errNoRotationRow(err error) error {
	if err != nil && strings.Contains(err.Error(), "no rows") {
		return err
	}
	return errors.New("other")
}

func TestRotationPreconditions(t *testing.T) {
	t.Parallel()
	t.Run("index behind runs inline catch-up", func(t *testing.T) {
		t.Parallel()
		opts := testOptions(t)
		original := buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-07"}, 2)
		s := openTestStore(t, opts)
		// No explicit ingest: rotation's precondition must catch up inline.
		s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
		if _, err := os.Stat(filepath.Join(opts.RotatedDir, "regime-decisions-2026-04.jsonl.gz")); err != nil {
			t.Fatalf("rotation after inline catch-up did not archive: %v", err)
		}
		if got := countRows(t, s, "regime_decisions"); got != 4 {
			t.Fatalf("inline catch-up rows = %d, want 4", got)
		}
		if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original {
			t.Fatal("stream identity broken")
		}
	})
	t.Run("cut zero is a quiet no-op", func(t *testing.T) {
		t.Parallel()
		opts := testOptions(t)
		buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-07"}, 3)
		s := openTestStore(t, opts)
		s.ingestAll(context.Background())
		s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
		if _, err := os.ReadDir(opts.RotatedDir); !os.IsNotExist(err) {
			entries, _ := os.ReadDir(opts.RotatedDir)
			if len(entries) != 0 {
				t.Fatalf("no-op rotation created archives: %v", entries)
			}
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM rotation_log`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("no-op rotation wrote %d rotation_log rows (%v)", n, err)
		}
	})
	t.Run("unparseable first line aborts", func(t *testing.T) {
		t.Parallel()
		opts := testOptions(t)
		content := `{"v":1,"ts":"garbage","stage":"calm"}` + "\n" +
			`{"v":1,"ts":"2026-04-01T10:00:00Z","stage":"calm"}` + "\n"
		writeJournal(t, opts.RegimeJournalPath, content)
		s := openTestStore(t, opts)
		s.ingestAll(context.Background())
		s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
		live, err := os.ReadFile(opts.RegimeJournalPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(live) != content {
			t.Fatal("abort path modified the journal")
		}
		if entries, err := os.ReadDir(opts.RotatedDir); err == nil && len(entries) != 0 {
			t.Fatalf("abort path created archives: %v", entries)
		}
	})
	t.Run("whole file rotates to empty tail", func(t *testing.T) {
		t.Parallel()
		opts := testOptions(t)
		original := buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-03"}, 3)
		s := openTestStore(t, opts)
		s.ingestAll(context.Background())
		s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
		live, err := os.ReadFile(opts.RegimeJournalPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(live) != 0 {
			t.Fatalf("empty-tail rotation left %d live bytes", len(live))
		}
		if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original {
			t.Fatal("stream identity broken")
		}
		var genesis *string
		if err := s.db.QueryRow(`SELECT genesis FROM ingest_sources WHERE source = 'regime'`).Scan(&genesis); err != nil {
			t.Fatal(err)
		}
		if genesis != nil {
			t.Fatalf("empty-tail genesis = %v, want NULL", *genesis)
		}
		// Appends after an empty-tail rotation ingest cleanly.
		appendJournal(t, opts.RegimeJournalPath, `{"v":1,"ts":"2026-07-15T10:00:00Z","stage":"calm"}`+"\n")
		s.ingestAll(context.Background())
		if got := countRows(t, s, "regime_decisions"); got != 4 {
			t.Fatalf("post-empty-tail append rows = %d, want 4", got)
		}
	})
}

// TestRotationConcurrentAppendsAndQueries exercises writer-lock appends
// and reads racing the rotation pass under -race.
func TestRotationConcurrentAppendsAndQueries(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 5)
	s := openTestStore(t, opts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	locker := &sync.Mutex{}
	sources := []RotationSource{{Name: sourceRegime, Locker: locker}}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			locker.Lock()
			f, err := os.OpenFile(opts.RegimeJournalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintf(f, `{"v":1,"ts":"2026-07-15T10:%02d:00Z","stage":"calm","fingerprint":"live-%d"}`+"\n", i%60, i)
				_ = f.Close()
			}
			locker.Unlock()
			s.Kick()
			i++
			time.Sleep(time.Millisecond)
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, err := s.RegimeHistory(RegimeQuery{Since: rotationNow.Add(-365 * 24 * time.Hour), Until: rotationNow.Add(24 * time.Hour), Limit: 10}); err != nil {
				t.Errorf("query during rotation: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	for range 3 {
		s.RotateAll(ctx, sources, 2, rotationNow)
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	cancel()

	// Everything the writers appended plus the rotated prefix reconstructs
	// a coherent stream: ingest to the end and confirm no rebuild happened
	// (a rebuild would signal the swap raced the ingester).
	s.ingestAll(context.Background())
	st, err := os.Stat(opts.RegimeJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	var offset, base int64
	if err := s.db.QueryRow(`SELECT offset, base FROM ingest_sources WHERE source = 'regime'`).Scan(&offset, &base); err != nil {
		t.Fatal(err)
	}
	if offset-base != st.Size() {
		t.Fatalf("final bookkeeping incoherent: offset %d base %d size %d", offset, base, st.Size())
	}
	if base == 0 {
		t.Fatal("no rotation completed during the race test")
	}
}

// TestBackfillResume crashes an archive backfill mid-way (failpoint after
// the row batches, before the completion transaction) and proves the
// resume completes without duplicates.
func TestBackfillResume(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 4)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	want := dumpRows(t, s, "regime_decisions")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	removeDBFiles(opts.DBPath)

	s2 := openTestStore(t, opts)
	boom := errors.New("injected backfill crash")
	s2.rotateFailpoint = func(stage string) error {
		if strings.HasPrefix(stage, "backfill-rows:regime-decisions-2026-04") {
			return boom
		}
		return nil
	}
	s2.ingestAll(context.Background()) // fails mid-backfill, warns
	var recorded int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM archive_files`).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("crashed backfill recorded %d archives (%v), want 0", recorded, err)
	}
	s2.rotateFailpoint = nil
	s2.ingestAll(context.Background())
	got := dumpRows(t, s2, "regime_decisions")
	if len(got) != len(want) {
		t.Fatalf("resume rows = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resume row %d differs (duplicate or drift):\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

// TestRotationPartNamingAcrossRotations pins the cross-rotation stray:
// a line of the last-archived month re-appears in the live file and lands
// in a .part2 archive that still sorts in stream order.
func TestRotationPartNamingAcrossRotations(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	first := `{"v":1,"ts":"2026-04-01T10:00:00Z","stage":"calm"}` + "\n"
	writeJournal(t, opts.RegimeJournalPath, first)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	if _, err := os.Stat(filepath.Join(opts.RotatedDir, "regime-decisions-2026-04.jsonl.gz")); err != nil {
		t.Fatalf("first rotation archive missing: %v", err)
	}

	// A stray April line lands after the empty-tail rotation (clock blip).
	stray := `{"v":1,"ts":"2026-04-02T10:00:00Z","stage":"calm"}` + "\n"
	appendJournal(t, opts.RegimeJournalPath, stray)
	s.ingestAll(context.Background())
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	if _, err := os.Stat(filepath.Join(opts.RotatedDir, "regime-decisions-2026-04.part2.jsonl.gz")); err != nil {
		t.Fatalf("part2 archive missing: %v", err)
	}
	if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != first+stray {
		t.Fatalf("stream identity broken across part-naming rotations:\n got %q\nwant %q", got, first+stray)
	}
}

// TestRotationOrderGuardTruncates pins the out-of-order safety net: a
// stray old-month run that cannot take a name sorting after existing
// archives truncates the rotation instead of breaking name-order
// reconstruction.
func TestRotationOrderGuardTruncates(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	first := "" +
		`{"v":1,"ts":"2026-04-01T10:00:00Z","stage":"calm"}` + "\n" +
		`{"v":1,"ts":"2026-06-01T10:00:00Z","stage":"calm"}` + "\n"
	writeJournal(t, opts.RegimeJournalPath, first)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow) // archives April

	stray := `{"v":1,"ts":"2026-04-02T10:00:00Z","stage":"calm"}` + "\n"
	appendJournal(t, opts.RegimeJournalPath, stray)
	s.ingestAll(context.Background())
	later := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) // keep 2026-08..09
	s.RotateAll(context.Background(), regimeRotationSource(), 2, later)

	entries, err := os.ReadDir(opts.RotatedDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if !slicesContainsString(names, "regime-decisions-2026-06.jsonl.gz") {
		t.Fatalf("June was not archived: %v", names)
	}
	// The stray could only archive as 04.part2, which sorts before the
	// June archive — so it must stay in the live file.
	live, err := os.ReadFile(opts.RegimeJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != stray {
		t.Fatalf("live file = %q, want the guarded stray line", string(live))
	}
	if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != first+stray {
		t.Fatalf("stream identity broken by the order guard:\n got %q\nwant %q", got, first+stray)
	}
}

func slicesContainsString(names []string, want string) bool {
	return slices.Contains(names, want)
}

// TestRotationBoundaryConflictHeals pins the recovery path for a rotation
// that swapped the live file and then failed before its finalize
// transaction while the daemon stayed up, so RecoverRotations never ran.
// The archive then exists with no bookkeeping row; every later ingest pass
// meets the backfill boundary refusal before the truncation check that
// would otherwise heal it. Ingest must rebuild instead of freezing the
// source's offset while the journal keeps growing.
func TestRotationBoundaryConflictHeals(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 3)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	s.rotateFailpoint = func(stage string) error {
		if stage == "swapped" {
			return errors.New("injected crash after swap")
		}
		return nil
	}
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	s.rotateFailpoint = nil

	// No restart: the daemon keeps journaling into the swapped live file.
	f, err := os.OpenFile(opts.RegimeJournalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"v":1,"ts":"2026-07-14T10:00:00Z","stage":"calm","fingerprint":"fp-post-crash"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	s.ingestAll(context.Background())

	stream := reconstructStream(t, opts, opts.RegimeJournalPath)
	rows := dumpRows(t, s, "regime_decisions")
	if want := strings.Count(stream, "\n"); len(rows) != want {
		t.Fatalf("indexed rows = %d, want %d (evidence stream lines): ingest wedged after the failed rotation", len(rows), want)
	}
	if !strings.Contains(rows[len(rows)-1], "fp-post-crash") {
		t.Fatal("the line journaled after the failed rotation was never indexed")
	}
}
