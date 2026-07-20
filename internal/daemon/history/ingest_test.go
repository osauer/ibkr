package history

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func writeJournal(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendJournal(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return string(data)
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func sourceOffset(t *testing.T, s *Store, source string) int64 {
	t.Helper()
	var offset int64
	if err := s.db.QueryRow(`SELECT offset FROM ingest_sources WHERE source = ?`, source).Scan(&offset); err != nil {
		t.Fatalf("offset %s: %v", source, err)
	}
	return offset
}

func TestGoldenIngestRegime(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	golden := readTestdata(t, "regime-decisions.jsonl")
	writeJournal(t, opts.RegimeJournalPath, golden)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	if got := countRows(t, s, "regime_decisions"); got != 3 {
		t.Fatalf("regime_decisions rows = %d, want 3", got)
	}
	if got := countRows(t, s, "regime_indicators"); got != 4 {
		t.Fatalf("regime_indicators rows = %d, want 4 (2 indicators x 2 full lines)", got)
	}
	if got, want := sourceOffset(t, s, sourceRegime), int64(len(golden)); got != want {
		t.Fatalf("regime offset = %d, want %d", got, want)
	}

	lines := strings.Split(strings.TrimSuffix(golden, "\n"), "\n")
	// Full line: verbatim at, normalized at_unix_ms, decision fields,
	// raw_json byte-equality.
	var at, stage, severity, verdict, fingerprint, raw string
	var atMS int64
	var red, yellow, eligibleRed int
	err := s.db.QueryRow(`SELECT at, at_unix_ms, stage, severity, verdict, fingerprint,
 cluster_red_count, cluster_yellow_count, cluster_eligible_red_count, raw_json
FROM regime_decisions WHERE src_offset = 0`).Scan(&at, &atMS, &stage, &severity, &verdict, &fingerprint, &red, &yellow, &eligibleRed, &raw)
	if err != nil {
		t.Fatalf("read full row: %v", err)
	}
	if at != "2026-07-01T15:04:05.123456+02:00" {
		t.Errorf("at = %q, want verbatim journal ts", at)
	}
	wantTime, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatalf("parse fixture ts: %v", err)
	}
	if atMS != wantTime.UnixMilli() {
		t.Errorf("at_unix_ms = %d, want %d (UTC-normalized)", atMS, wantTime.UnixMilli())
	}
	if stage != "early_warning" || severity != "watch" || verdict != "Stress signal present" || fingerprint != "sha256:golden-full" {
		t.Errorf("decision fields = %q/%q/%q/%q", stage, severity, verdict, fingerprint)
	}
	if red != 2 || yellow != 1 || eligibleRed != 1 {
		t.Errorf("cluster counts = %d/%d(%d), want 2/1(1)", red, yellow, eligibleRed)
	}
	if raw != lines[0] {
		t.Errorf("raw_json is not byte-equal to the journal line:\n got %q\nwant %q", raw, lines[0])
	}

	// Indicator rows for the full line: gamma_zero has NULL depth, eligible
	// true, latched true; vix_term has value+depth, NULL eligible.
	var gStatus, gBand string
	var gValue, gDepth *float64
	var gStreak *int
	var gEligible *bool
	var gLatched bool
	err = s.db.QueryRow(`SELECT status, band, value, depth, streak_sessions, eligible, latched
FROM regime_indicators WHERE decision_id = (SELECT id FROM regime_decisions WHERE src_offset = 0) AND indicator = 'gamma_zero'`).
		Scan(&gStatus, &gBand, &gValue, &gDepth, &gStreak, &gEligible, &gLatched)
	if err != nil {
		t.Fatalf("read gamma_zero indicator: %v", err)
	}
	if gStatus != "ok" || gBand != "red" || gValue == nil || gDepth != nil || gStreak == nil || *gStreak != 18 ||
		gEligible == nil || !*gEligible || !gLatched {
		t.Errorf("gamma_zero row = %q/%q value=%v depth=%v streak=%v eligible=%v latched=%v", gStatus, gBand, gValue, gDepth, gStreak, gEligible, gLatched)
	}

	// Minimal line: absent fields land as empty/zero, no indicator rows.
	var minStage, minSeverity, minRaw string
	var minAtMS int64
	if err := s.db.QueryRow(`SELECT stage, severity, at_unix_ms, raw_json FROM regime_decisions WHERE src_offset = ?`,
		len(lines[0])+1).Scan(&minStage, &minSeverity, &minAtMS, &minRaw); err != nil {
		t.Fatalf("read minimal row: %v", err)
	}
	minWant, _ := time.Parse(time.RFC3339Nano, "2026-07-02T09:00:00Z")
	if minStage != "calm" || minSeverity != "" || minAtMS != minWant.UnixMilli() || minRaw != lines[1] {
		t.Errorf("minimal row = %q/%q/%d raw match %t", minStage, minSeverity, minAtMS, minRaw == lines[1])
	}

	// Heartbeat duplicate: byte-identical line at a later offset must
	// index as its own row (offset idempotency never dedupes evidence).
	var dupCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM regime_decisions WHERE raw_json = ?`, lines[0]).Scan(&dupCount); err != nil {
		t.Fatalf("count duplicates: %v", err)
	}
	if dupCount != 2 {
		t.Errorf("byte-identical heartbeat lines indexed = %d, want 2", dupCount)
	}
}

func TestGoldenIngestRules(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	golden := readTestdata(t, "rules-decisions.jsonl")
	writeJournal(t, opts.RulesJournalPath, golden)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())

	if got := countRows(t, s, "rule_transitions"); got != 3 {
		t.Fatalf("rule_transitions rows = %d, want 3", got)
	}
	if got, want := sourceOffset(t, s, sourceRules), int64(len(golden)); got != want {
		t.Fatalf("rules offset = %d, want %d", got, want)
	}
	lines := strings.Split(strings.TrimSuffix(golden, "\n"), "\n")

	var at, rule, status, was, evidence, policyID, policyFingerprint, raw string
	var atMS int64
	var policyVersion *int
	err := s.db.QueryRow(`SELECT at, at_unix_ms, rule_id, status, was, evidence, policy_id, policy_version, policy_fingerprint, raw_json
FROM rule_transitions WHERE src_offset = 0`).Scan(&at, &atMS, &rule, &status, &was, &evidence, &policyID, &policyVersion, &policyFingerprint, &raw)
	if err != nil {
		t.Fatalf("read first transition: %v", err)
	}
	wantTime, err := time.Parse(time.RFC3339Nano, "2026-07-07T09:00:34.304235+02:00")
	if err != nil {
		t.Fatal(err)
	}
	if at != "2026-07-07T09:00:34.304235+02:00" || atMS != wantTime.UnixMilli() {
		t.Errorf("at = %q (%d), want verbatim + normalized", at, atMS)
	}
	if rule != "single_name_exposure" || status != "watch" || was != "" || evidence != "synthetic evidence line one" {
		t.Errorf("transition fields = %q/%q/%q/%q", rule, status, was, evidence)
	}
	if policyID != "rulebook-v1" || policyVersion == nil || *policyVersion != 1 || policyFingerprint != "sha256:golden-rules" {
		t.Errorf("policy fields = %q/%v/%q", policyID, policyVersion, policyFingerprint)
	}
	if raw != lines[0] {
		t.Errorf("raw_json not byte-equal:\n got %q\nwant %q", raw, lines[0])
	}

	// Minimal third line: policy_version stays NULL, was/evidence empty.
	var minVersion *int
	var minWas string
	if err := s.db.QueryRow(`SELECT policy_version, was FROM rule_transitions WHERE rule_id = 'cash_sell_only'`).Scan(&minVersion, &minWas); err != nil {
		t.Fatalf("read minimal transition: %v", err)
	}
	if minVersion != nil || minWas != "" {
		t.Errorf("minimal transition policy_version=%v was=%q, want NULL/empty", minVersion, minWas)
	}
}

func TestBackfillIdempotency(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.RegimeJournalPath, readTestdata(t, "regime-decisions.jsonl"))
	writeJournal(t, opts.RulesJournalPath, readTestdata(t, "rules-decisions.jsonl"))
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	decisions, indicators, transitions := countRows(t, s, "regime_decisions"), countRows(t, s, "regime_indicators"), countRows(t, s, "rule_transitions")
	regimeOffset, rulesOffset := sourceOffset(t, s, sourceRegime), sourceOffset(t, s, sourceRules)

	s.ingestAll(context.Background())
	if got := countRows(t, s, "regime_decisions"); got != decisions {
		t.Errorf("second ingest changed regime_decisions: %d != %d", got, decisions)
	}
	if got := countRows(t, s, "regime_indicators"); got != indicators {
		t.Errorf("second ingest changed regime_indicators: %d != %d", got, indicators)
	}
	if got := countRows(t, s, "rule_transitions"); got != transitions {
		t.Errorf("second ingest changed rule_transitions: %d != %d", got, transitions)
	}
	if got := sourceOffset(t, s, sourceRegime); got != regimeOffset {
		t.Errorf("second ingest moved regime offset: %d != %d", got, regimeOffset)
	}
	if got := sourceOffset(t, s, sourceRules); got != rulesOffset {
		t.Errorf("second ingest moved rules offset: %d != %d", got, rulesOffset)
	}
}

func TestCrashReplayResumesFromCommittedOffset(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.RegimeJournalPath, readTestdata(t, "regime-decisions.jsonl"))
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	before := countRows(t, s, "regime_decisions")

	// Simulate the daemon coming back after journal writes the index never
	// saw: reopen the same DB and ingest again.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	appendJournal(t, opts.RegimeJournalPath,
		`{"v":1,"ts":"2026-07-03T10:00:00Z","stage":"calm"}`+"\n"+
			`{"v":1,"ts":"2026-07-03T11:00:00Z","stage":"watch"}`+"\n")
	s2 := openTestStore(t, opts)
	s2.ingestAll(context.Background())
	if got := countRows(t, s2, "regime_decisions"); got != before+2 {
		t.Fatalf("rows after replay = %d, want %d (+2 exactly)", got, before+2)
	}
}

func TestTornTailIsLeftForNextIngest(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	complete := `{"v":1,"ts":"2026-07-03T10:00:00Z","stage":"calm"}` + "\n"
	torn := `{"v":1,"ts":"2026-07-03T11:00:00Z","st`
	writeJournal(t, opts.RegimeJournalPath, complete+torn)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	if got := countRows(t, s, "regime_decisions"); got != 1 {
		t.Fatalf("rows with torn tail = %d, want 1", got)
	}
	if got, want := sourceOffset(t, s, sourceRegime), int64(len(complete)); got != want {
		t.Fatalf("offset stopped at %d, want last newline %d", got, want)
	}

	appendJournal(t, opts.RegimeJournalPath, `age":"watch"}`+"\n")
	s.ingestAll(context.Background())
	if got := countRows(t, s, "regime_decisions"); got != 2 {
		t.Fatalf("rows after completing the line = %d, want exactly 2", got)
	}
	var stage string
	if err := s.db.QueryRow(`SELECT stage FROM regime_decisions WHERE src_offset = ?`, len(complete)).Scan(&stage); err != nil {
		t.Fatalf("read completed line: %v", err)
	}
	if stage != "watch" {
		t.Fatalf("completed line stage = %q, want watch", stage)
	}
}

func TestCorruptCompleteLineIsSkipped(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	content := `{"v":1,"ts":"2026-07-03T10:00:00Z","stage":"calm"}` + "\n" +
		"this line is not JSON at all\n" +
		`{"v":1,"ts":"not-a-timestamp","stage":"calm"}` + "\n" +
		`{"v":1,"ts":"2026-07-03T12:00:00Z","stage":"watch"}` + "\n"
	writeJournal(t, opts.RegimeJournalPath, content)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	if got := countRows(t, s, "regime_decisions"); got != 2 {
		t.Fatalf("rows = %d, want 2 (two corrupt lines skipped)", got)
	}
	if got, want := sourceOffset(t, s, sourceRegime), int64(len(content)); got != want {
		t.Fatalf("offset = %d, want %d (advances past corrupt lines)", got, want)
	}
	var stage string
	if err := s.db.QueryRow(`SELECT stage FROM regime_decisions ORDER BY src_offset DESC LIMIT 1`).Scan(&stage); err != nil {
		t.Fatal(err)
	}
	if stage != "watch" {
		t.Fatalf("line after corrupt ones = %q, want watch", stage)
	}
}

func TestTruncationTriggersRebuild(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.RegimeJournalPath, readTestdata(t, "regime-decisions.jsonl"))
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	if got := countRows(t, s, "regime_decisions"); got != 3 {
		t.Fatalf("precondition rows = %d, want 3", got)
	}

	// Shrink: size < offset.
	shrunk := `{"v":1,"ts":"2026-07-04T09:00:00Z","stage":"calm"}` + "\n"
	writeJournal(t, opts.RegimeJournalPath, shrunk)
	s.ingestAll(context.Background())
	if got := countRows(t, s, "regime_decisions"); got != 1 {
		t.Fatalf("rows after shrink rebuild = %d, want 1", got)
	}
	if got, want := sourceOffset(t, s, sourceRegime), int64(len(shrunk)); got != want {
		t.Fatalf("offset after rebuild = %d, want %d", got, want)
	}

	// Replace with a LARGER file whose first line differs: size grows past
	// the offset, so only the genesis hash can catch it.
	var replaced strings.Builder
	for i := range 4 {
		fmt.Fprintf(&replaced, `{"v":1,"ts":"2026-07-05T0%d:00:00Z","stage":"early_warning"}`+"\n", i)
	}
	writeJournal(t, opts.RegimeJournalPath, replaced.String())
	s.ingestAll(context.Background())
	if got := countRows(t, s, "regime_decisions"); got != 4 {
		t.Fatalf("rows after replace rebuild = %d, want 4", got)
	}
	var stages int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM regime_decisions WHERE stage = 'early_warning'`).Scan(&stages); err != nil {
		t.Fatal(err)
	}
	if stages != 4 {
		t.Fatalf("stale pre-replace rows survived the rebuild (early_warning rows = %d, want 4)", stages)
	}
}

func TestRunServicesKicks(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	writeJournal(t, opts.RegimeJournalPath, `{"v":1,"ts":"2026-07-03T10:00:00Z","stage":"calm"}`+"\n")
	s := openTestStore(t, opts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	waitFor := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if countRows(t, s, "regime_decisions") == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("rows never reached %d", want)
	}
	waitFor(1)
	appendJournal(t, opts.RegimeJournalPath, `{"v":1,"ts":"2026-07-03T11:00:00Z","stage":"watch"}`+"\n")
	s.Kick()
	waitFor(2)
	cancel()
	<-done
}
