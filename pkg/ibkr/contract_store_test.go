package ibkr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContractStoreRoundTrip pins the basic load/save contract: what
// goes in via Save comes back out via Load, including the members-hash
// field that powers reconstitution detection.
func TestContractStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewContractStore(dir)

	in := map[string]ContractDetailsLite{
		"AAPL":  {Symbol: "AAPL", ConID: 265598, Exchange: "SMART", PrimaryExch: "NASDAQ"},
		"BRK B": {Symbol: "BRK", ConID: 76790028, Exchange: "SMART", PrimaryExch: "NYSE", LocalSymbol: "BRK B"},
		"SPY":   {Symbol: "SPY", ConID: 756733, Exchange: "SMART", PrimaryExch: "ARCA"},
	}
	hash := "deadbeef12345678"

	if err := store.Save(in, hash); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, gotHash, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotHash != hash {
		t.Errorf("members hash: want %q, got %q", hash, gotHash)
	}
	if len(got) != len(in) {
		t.Errorf("entry count: want %d, got %d", len(in), len(got))
	}
	for sym, want := range in {
		gotEntry, ok := got[sym]
		if !ok {
			t.Errorf("missing entry: %s", sym)
			continue
		}
		if gotEntry.ConID != want.ConID {
			t.Errorf("%s conID: want %d, got %d", sym, want.ConID, gotEntry.ConID)
		}
		if gotEntry.LocalSymbol != want.LocalSymbol {
			t.Errorf("%s local: want %q, got %q", sym, want.LocalSymbol, gotEntry.LocalSymbol)
		}
	}
}

// TestContractStoreLoadMissingFile pins the cold-install behaviour: an
// absent file returns (nil, "", nil) — not an error — so the daemon
// starts with an empty cache without a noisy "file missing" log.
func TestContractStoreLoadMissingFile(t *testing.T) {
	store := NewContractStore(t.TempDir())
	got, hash, err := store.Load()
	if err != nil {
		t.Errorf("expected nil error for missing file, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil map for missing file, got %v", got)
	}
	if hash != "" {
		t.Errorf("expected empty hash for missing file, got %q", hash)
	}
}

// TestContractStoreFiltersZeroConID pins that ConID==0 entries (pending
// or failed resolutions) are filtered out of the saved file, so the
// next daemon load doesn't seed connectors with useless empty entries.
func TestContractStoreFiltersZeroConID(t *testing.T) {
	dir := t.TempDir()
	store := NewContractStore(dir)

	in := map[string]ContractDetailsLite{
		"REAL":     {Symbol: "REAL", ConID: 12345, Exchange: "SMART"},
		"PENDING":  {Symbol: "PENDING", ConID: 0, Exchange: "SMART"},
		"OTHER":    {Symbol: "OTHER", ConID: 67890, Exchange: "SMART"},
		"BLANKBOT": {Symbol: "BLANKBOT", ConID: 0},
	}
	if err := store.Save(in, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, present := got["PENDING"]; present {
		t.Error("ConID==0 entry leaked into persisted file")
	}
	if _, present := got["BLANKBOT"]; present {
		t.Error("ConID==0 entry leaked into persisted file")
	}
	if len(got) != 2 {
		t.Errorf("want 2 entries after filter, got %d (%v)", len(got), got)
	}
}

// TestContractStoreVersionMismatch pins the cold-rebuild path: a file
// with an unknown version (older or future daemon) must be treated as
// no-cache rather than parsed in a way that could mis-seed connectors.
func TestContractStoreVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	bogus := `{"version": 999, "as_of": "2026-01-01T00:00:00Z", "contracts": {"AAPL": {"Symbol": "AAPL", "ConID": 999}}}`
	if err := os.WriteFile(filepath.Join(dir, contractStoreFile), []byte(bogus), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	store := NewContractStore(dir)
	got, _, err := store.Load()
	if err != nil {
		t.Errorf("future-version file should be treated as no-cache, got error %v", err)
	}
	if got != nil {
		t.Errorf("future-version file should return nil map, got %v", got)
	}
}

// TestContractStoreCorruptFileSurfacesError pins the failure mode for
// genuinely-broken on-disk state: a malformed JSON file is an actual
// error the daemon should surface (operator should investigate), not
// silently swallowed like the cold-start case.
func TestContractStoreCorruptFileSurfacesError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, contractStoreFile), []byte("not json {{{"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	store := NewContractStore(dir)
	_, _, err := store.Load()
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error should mention decode, got %v", err)
	}
}

// TestMembersHashStableAcrossOrder pins the order-independence
// guarantee: regenerating sp500Members in a different order (e.g. a
// sort-key change in scripts/refresh-spx-members) must NOT invalidate
// the cache. Two lists with the same set of symbols hash identically.
func TestMembersHashStableAcrossOrder(t *testing.T) {
	a := []string{"AAPL", "MSFT", "GOOG", "AMZN", "BRK.B"}
	b := []string{"MSFT", "BRK.B", "AAPL", "GOOG", "AMZN"}
	if MembersHash(a) != MembersHash(b) {
		t.Errorf("hashes should match regardless of order; got %s vs %s",
			MembersHash(a), MembersHash(b))
	}
}

// TestMembersHashCaseAndWhitespaceTolerant pins the normalisation
// applied before hashing. SPX tickers come from a Wikipedia scrape
// that occasionally has stray whitespace or inconsistent case; the
// hash treats those as the same membership.
func TestMembersHashCaseAndWhitespaceTolerant(t *testing.T) {
	a := []string{"AAPL", "MSFT", "BRK.B"}
	b := []string{"aapl", "MSFT ", " BRK.b"}
	if MembersHash(a) != MembersHash(b) {
		t.Errorf("hashes should match modulo case/whitespace; got %s vs %s",
			MembersHash(a), MembersHash(b))
	}
}

// TestMembersHashDifferentSetsDiffer pins the reconstitution-detection
// contract: adding or removing a member changes the hash. Without
// this the on-disk cache could outlive the membership list and keep
// seeding connectors with delisted symbols.
func TestMembersHashDifferentSetsDiffer(t *testing.T) {
	a := []string{"AAPL", "MSFT", "GOOG"}
	withAdd := []string{"AAPL", "MSFT", "GOOG", "NVDA"}
	withRemove := []string{"AAPL", "MSFT"}
	withReplace := []string{"AAPL", "MSFT", "META"}

	if MembersHash(a) == MembersHash(withAdd) {
		t.Error("hash should change on add")
	}
	if MembersHash(a) == MembersHash(withRemove) {
		t.Error("hash should change on remove")
	}
	if MembersHash(a) == MembersHash(withReplace) {
		t.Error("hash should change on substitute")
	}
}
