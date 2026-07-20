package history

import (
	"context"
	"os"
	"testing"
	"time"
)

func mustTime(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func seedQueryStore(t *testing.T) (*Store, Options) {
	t.Helper()
	opts := testOptions(t)
	writeJournal(t, opts.RegimeJournalPath,
		`{"v":1,"ts":"2026-07-10T10:00:00Z","stage":"calm","verdict":"Stable tape"}`+"\n"+
			`{"v":1,"ts":"2026-07-11T10:00:00Z","stage":"early_warning","severity":"watch","composite":{"cluster_red_count":1,"cluster_yellow_count":2,"cluster_eligible_red_count":1}}`+"\n"+
			`{"v":1,"ts":"2026-07-12T10:00:00Z","stage":"calm"}`+"\n"+
			`{"v":1,"ts":"2026-07-13T10:00:00Z","stage":"confirmed"}`+"\n")
	writeJournal(t, opts.RulesJournalPath,
		`{"at":"2026-07-10T09:00:00Z","rule":"single_name_exposure","status":"watch","version":1}`+"\n"+
			`{"at":"2026-07-11T09:00:00Z","rule":"cash_sell_only","status":"act","version":1}`+"\n"+
			`{"at":"2026-07-12T09:00:00Z","rule":"single_name_exposure","status":"pass","was":"watch","version":1}`+"\n")
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	return s, opts
}

func TestRegimeHistoryQuerySemantics(t *testing.T) {
	t.Parallel()
	s, _ := seedQueryStore(t)

	// Window is [since, until): the 13th sits outside.
	entries, total, err := s.RegimeHistory(RegimeQuery{
		Since: mustTime(t, "2026-07-11T00:00:00Z"),
		Until: mustTime(t, "2026-07-13T00:00:00Z"),
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("RegimeHistory: %v", err)
	}
	if total != 2 || len(entries) != 2 {
		t.Fatalf("window match = %d/%d entries, want 2/2", len(entries), total)
	}
	if !entries[0].At.After(entries[1].At) {
		t.Fatalf("entries not newest-first: %v then %v", entries[0].At, entries[1].At)
	}
	if entries[0].Stage != "calm" || entries[1].Stage != "early_warning" {
		t.Fatalf("stages = %q,%q", entries[0].Stage, entries[1].Stage)
	}
	if entries[1].Severity != "watch" || entries[1].ClusterRed != 1 || entries[1].ClusterYellow != 2 || entries[1].ClusterEligibleRed != 1 {
		t.Fatalf("early_warning entry fields = %+v", entries[1])
	}

	// Stage filter.
	entries, total, err = s.RegimeHistory(RegimeQuery{
		Since: mustTime(t, "2026-07-01T00:00:00Z"),
		Until: mustTime(t, "2026-07-20T00:00:00Z"),
		Stage: "calm",
		Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(entries) != 2 || entries[0].Stage != "calm" || entries[1].Stage != "calm" {
		t.Fatalf("stage filter = %d/%d %+v", len(entries), total, entries)
	}

	// Limit cuts entries, TotalCount still counts everything matching.
	entries, total, err = s.RegimeHistory(RegimeQuery{
		Since: mustTime(t, "2026-07-01T00:00:00Z"),
		Until: mustTime(t, "2026-07-20T00:00:00Z"),
		Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || total != 4 {
		t.Fatalf("limit=1 gave %d entries, total %d; want 1 and 4", len(entries), total)
	}
	if entries[0].Stage != "confirmed" {
		t.Fatalf("limit=1 newest entry stage = %q, want confirmed", entries[0].Stage)
	}
}

func TestRulesHistoryQuerySemantics(t *testing.T) {
	t.Parallel()
	s, _ := seedQueryStore(t)
	entries, total, err := s.RulesHistory(RulesQuery{
		Since: mustTime(t, "2026-07-01T00:00:00Z"),
		Until: mustTime(t, "2026-07-20T00:00:00Z"),
		Rule:  "single_name_exposure",
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("RulesHistory: %v", err)
	}
	if total != 2 || len(entries) != 2 {
		t.Fatalf("rule filter = %d/%d, want 2/2", len(entries), total)
	}
	if entries[0].Status != "pass" || entries[0].Was != "watch" || entries[1].Status != "watch" {
		t.Fatalf("transitions newest-first = %+v", entries)
	}

	entries, total, err = s.RulesHistory(RulesQuery{
		Since: mustTime(t, "2026-07-11T00:00:00Z"),
		Until: mustTime(t, "2026-07-12T00:00:00Z"),
		Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(entries) != 1 || entries[0].Rule != "cash_sell_only" {
		t.Fatalf("window = %d/%d %+v", len(entries), total, entries)
	}
}

func TestHealthDisclosesBacklogBytes(t *testing.T) {
	t.Parallel()
	s, opts := seedQueryStore(t)
	st, err := os.Stat(opts.RegimeJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(sourceRegime)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.IngestedBytes != st.Size() || h.JournalBytes != st.Size() {
		t.Fatalf("health bytes = %d/%d, want both %d", h.IngestedBytes, h.JournalBytes, st.Size())
	}
	if h.LastIngestAt.IsZero() {
		t.Fatal("LastIngestAt is zero after an ingest")
	}

	// Un-ingested appended bytes must show as backlog, never silently
	// fresh.
	appendJournal(t, opts.RegimeJournalPath, `{"v":1,"ts":"2026-07-14T10:00:00Z","stage":"calm"}`+"\n")
	h, err = s.Health(sourceRegime)
	if err != nil {
		t.Fatal(err)
	}
	if h.JournalBytes <= h.IngestedBytes {
		t.Fatalf("backlog not disclosed: journal %d <= ingested %d", h.JournalBytes, h.IngestedBytes)
	}

	if _, err := s.Health("nope"); err == nil {
		t.Fatal("unknown source must error")
	}
}
