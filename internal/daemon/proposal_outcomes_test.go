package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProposalOutcomeStoreHydratesDedupCache(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "trade-proposal-outcomes.jsonl")
	existing := proposalOutcomeMark{
		At:          time.Date(2026, 7, 10, 16, 0, 0, 0, time.UTC),
		MarkDate:    "2026-07-10",
		State:       proposalOutcomeStateMarked,
		ProposalKey: "risk_reduction:existing",
		MarkPrice:   101.25,
	}
	if err := newProposalOutcomeStore(path).AppendMark(existing); err != nil {
		t.Fatalf("pre-populate outcome store: %v", err)
	}

	novel := existing
	novel.ProposalKey = "risk_reduction:novel"
	novel.MarkPrice = 102.50
	store := newProposalOutcomeStore(path)
	if err := store.AppendMark(existing); err != nil {
		t.Fatalf("append existing outcome through fresh store: %v", err)
	}
	if err := store.AppendMark(novel); err != nil {
		t.Fatalf("append novel outcome through fresh store: %v", err)
	}

	reopened := newProposalOutcomeStore(path)
	if err := reopened.AppendMark(existing); err != nil {
		t.Fatalf("append existing outcome through reopened store: %v", err)
	}
	if err := reopened.AppendMark(novel); err != nil {
		t.Fatalf("append novel outcome through reopened store: %v", err)
	}
	reopened.mu.Lock()
	gotCacheSize := len(reopened.outcomeKeys)
	cacheHasIdentity := make(map[string]bool, 2)
	for _, mark := range []proposalOutcomeMark{existing, novel} {
		identity := proposalOutcomeIdentity(mark)
		_, ok := reopened.outcomeKeys[identity]
		cacheHasIdentity[identity] = ok
	}
	reopened.mu.Unlock()
	if gotCacheSize != 2 {
		t.Fatalf("hydrated dedup cache size = %d, want 2", gotCacheSize)
	}
	for _, mark := range []proposalOutcomeMark{existing, novel} {
		if !cacheHasIdentity[proposalOutcomeIdentity(mark)] {
			t.Fatalf("hydrated dedup cache missing identity %q", proposalOutcomeIdentity(mark))
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read outcome store: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if got := len(lines); got != 2 {
		t.Fatalf("outcome rows = %d, want 2; file=%s", got, raw)
	}
	want := map[string]bool{
		proposalOutcomeIdentity(existing): false,
		proposalOutcomeIdentity(novel):    false,
	}
	for _, line := range lines {
		var mark proposalOutcomeMark
		if err := json.Unmarshal([]byte(line), &mark); err != nil {
			t.Fatalf("decode outcome row: %v", err)
		}
		identity := proposalOutcomeIdentity(mark)
		if _, ok := want[identity]; !ok {
			t.Fatalf("unexpected outcome identity %q", identity)
		}
		if want[identity] {
			t.Fatalf("duplicate outcome identity %q", identity)
		}
		want[identity] = true
	}
	for identity, found := range want {
		if !found {
			t.Fatalf("outcome file missing identity %q", identity)
		}
	}
}
