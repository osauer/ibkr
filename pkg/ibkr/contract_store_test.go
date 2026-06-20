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

	if err := store.Save(in, nil, hash); err != nil {
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
	if err := store.Save(in, nil, ""); err != nil {
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

// TestContractStoreOptionsRoundTripSPXvsSPXW pins the v3 cache-key shape:
// SPX and SPXW share a third-Friday date and strike but are two distinct
// listed contracts with two distinct ConIDs. The key must keep them
// separated so the gamma compute prices each leg under the correct
// AM-vs-PM settlement instant. Pre-v3 keys (symbol|expiry|strike|right)
// would collide here.
func TestContractStoreOptionsRoundTripSPXvsSPXW(t *testing.T) {
	dir := t.TempDir()
	store := NewContractStore(dir)

	// Two distinct contracts: same date, same strike, same right.
	// Trading class is the discriminator. ConIDs are fictional but
	// represent the real-world AM/PM-settled pair on third-Friday.
		spxAM := ContractDetailsLite{
			Symbol: "SPX", TradingClass: "SPX", Expiry: "20991218",
		Strike: 5400, Right: "C", ConID: 700000001, Exchange: "CBOE",
	}
		spxwPM := ContractDetailsLite{
			Symbol: "SPX", TradingClass: "SPXW", Expiry: "20991218",
		Strike: 5400, Right: "C", ConID: 700000002, Exchange: "CBOE",
	}

	options := map[string]ContractDetailsLite{
		optionContractKey(spxAM.Symbol, spxAM.TradingClass, spxAM.Expiry, spxAM.Strike, spxAM.Right):      spxAM,
		optionContractKey(spxwPM.Symbol, spxwPM.TradingClass, spxwPM.Expiry, spxwPM.Strike, spxwPM.Right): spxwPM,
	}
	if len(options) != 2 {
		t.Fatalf("v3 key collision: SPX and SPXW collapsed to %d entries (want 2): %v",
			len(options), options)
	}

	if err := store.Save(map[string]ContractDetailsLite{}, options, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.LoadOptions()
	if err != nil {
		t.Fatalf("LoadOptions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadOptions returned %d entries, want 2: %v", len(got), got)
	}
		spxKey := optionContractKey("SPX", "SPX", "20991218", 5400, "C")
		spxwKey := optionContractKey("SPX", "SPXW", "20991218", 5400, "C")
	if got[spxKey].ConID != 700000001 {
		t.Errorf("SPX-class ConID after round-trip: got %d, want 700000001", got[spxKey].ConID)
	}
	if got[spxwKey].ConID != 700000002 {
		t.Errorf("SPXW-class ConID after round-trip: got %d, want 700000002", got[spxwKey].ConID)
	}
}

// TestContractStoreOptionsMigratesV2KeysToEmptyClass pins the v2 → v3
// migration: a v2 file (no trading class in keys) loads cleanly and the
// keys are normalised to the v3 empty-class shape so the connector's
// in-memory cache can read them. The next prewarm overwrites those
// empty-class entries with class-qualified ones; the migration is a
// one-shot bridge across the schema bump.
func TestContractStoreOptionsMigratesV2KeysToEmptyClass(t *testing.T) {
	dir := t.TempDir()
	// Hand-write a v2-shaped file. v2 keys are symbol|expiry|strike|right
	// (three pipes). v2 didn't carry the class in the cache file.
	v2 := `{
		"version": 2,
		"as_of": "2026-05-21T00:00:00Z",
		"contracts": {},
		"options": {
				"SPY|20991218|500.000000|C": {
					"Symbol": "SPY", "TradingClass": "SPY", "Expiry": "20991218",
				"Strike": 500, "Right": "C", "ConID": 600000001
			},
				"SPY|20991218|500.000000|P": {
					"Symbol": "SPY", "TradingClass": "SPY", "Expiry": "20991218",
				"Strike": 500, "Right": "P", "ConID": 600000002
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, contractStoreFile), []byte(v2), 0o644); err != nil {
		t.Fatalf("seed v2 file: %v", err)
	}
	store := NewContractStore(dir)
	got, err := store.LoadOptions()
	if err != nil {
		t.Fatalf("LoadOptions (v2 migration): %v", err)
	}
		// Keys must be normalised to v3 empty-class shape: "SPY||20991218|500.000000|C".
		want := map[string]int{
			"SPY||20991218|500.000000|C": 600000001,
			"SPY||20991218|500.000000|P": 600000002,
		}
	if len(got) != len(want) {
		t.Fatalf("got %d entries after migration, want %d: %v", len(got), len(want), got)
	}
	for k, wantConID := range want {
		entry, ok := got[k]
		if !ok {
			t.Errorf("missing migrated key %q in %v", k, got)
			continue
		}
		if entry.ConID != wantConID {
			t.Errorf("ConID for %q: got %d, want %d", k, entry.ConID, wantConID)
		}
	}
}

// TestContractStoreV2ExpiredOptionStillPruned pins that the v2-migration
// path doesn't accidentally resurrect entries whose Expiry has passed in
// NY time — the GC at the head of LoadOptions runs BEFORE the key
// migration, so an expired v2 entry never gets normalised.
func TestContractStoreV2ExpiredOptionStillPruned(t *testing.T) {
	dir := t.TempDir()
	// Expiry 2020-01-01 is comfortably in the past relative to any test
	// clock; the GC must drop it without inspecting the key shape.
	v2 := `{
		"version": 2,
		"as_of": "2026-05-21T00:00:00Z",
		"contracts": {},
		"options": {
			"SPY|20200101|300.000000|C": {
				"Symbol": "SPY", "Expiry": "20200101", "Strike": 300, "Right": "C", "ConID": 1
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(dir, contractStoreFile), []byte(v2), 0o644); err != nil {
		t.Fatalf("seed v2 file: %v", err)
	}
	store := NewContractStore(dir)
	got, err := store.LoadOptions()
	if err != nil {
		t.Fatalf("LoadOptions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expired v2 entry leaked through migration: %v", got)
	}
}
