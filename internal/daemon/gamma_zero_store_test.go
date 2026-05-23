package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// helperGammaResult builds a populated rpc.GammaZeroComputed
// suitable for round-tripping the store. Returns a result whose
// Method matches what the daemon writes today; tests that want a
// method-mismatch case override after the call.
func helperGammaResult(asOf time.Time) *rpc.GammaZeroComputed {
	spot := 745.09
	zg := 580.0
	gap := (spot - zg) / zg * 100
	return &rpc.GammaZeroComputed{
		SpotUnderlying: spot,
		SpotAt:         asOf,
		ZeroGamma:      &zg,
		GapPct:         &gap,
		GammaSign:      "",
		Profile: []rpc.GammaProfilePoint{
			{Spot: 700, GEX: -1.5e9},
			{Spot: 745, GEX: 0.5e9},
		},
		GammaTotalAbs: 3.41e9,
		Expirations:   []string{"2026-05-22", "2026-07-17", "2026-09-18"},
		LegCount:      1040,
		Method:        gammaMethodToken,
		Source:        "computed from IBKR SPY option chain",
		AsOf:          asOf,
		DurationMS:    324000,
	}
}

// TestGammaZeroStore_RoundTrip persists a result, reads it back, and
// confirms the meaningful fields survive the JSON round-trip. Acts
// as the regression pin for any future shape change in
// rpc.GammaZeroComputed.
func TestGammaZeroStore_RoundTrip(t *testing.T) {
	store := newGammaZeroStore(t.TempDir())
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, loc)
	want := helperGammaResult(now)

	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(now), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(rpc.GammaZeroScopeCombined, now)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil after Save")
	}
	if got.SpotUnderlying != want.SpotUnderlying {
		t.Errorf("SpotUnderlying: got %v want %v", got.SpotUnderlying, want.SpotUnderlying)
	}
	if got.ZeroGamma == nil || want.ZeroGamma == nil || *got.ZeroGamma != *want.ZeroGamma {
		t.Errorf("ZeroGamma round-trip mismatch: got %v want %v", got.ZeroGamma, want.ZeroGamma)
	}
	if got.LegCount != want.LegCount {
		t.Errorf("LegCount: got %d want %d", got.LegCount, want.LegCount)
	}
	if got.Method != want.Method {
		t.Errorf("Method: got %q want %q", got.Method, want.Method)
	}
}

// TestGammaZeroStore_ScopeIsolation confirms each scope persists to
// its own file and Load for one scope doesn't bleed into another.
// This is the load-bearing guarantee that motivated the scope-keyed
// refactor — a stale --only=spy result must NOT be served as
// combined or vice versa.
func TestGammaZeroStore_ScopeIsolation(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, loc)

	combined := helperGammaResult(now)
	combined.LegCount = 1111
	spy := helperGammaResult(now)
	spy.LegCount = 2222
	spx := helperGammaResult(now)
	spx.LegCount = 3333

	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(now), combined); err != nil {
		t.Fatalf("Save combined: %v", err)
	}
	if err := store.Save(rpc.GammaZeroScopeSPY, nySessionKey(now), spy); err != nil {
		t.Fatalf("Save spy: %v", err)
	}
	if err := store.Save(rpc.GammaZeroScopeSPX, nySessionKey(now), spx); err != nil {
		t.Fatalf("Save spx: %v", err)
	}

	// Three distinct files exist.
	for _, scope := range []string{
		rpc.GammaZeroScopeCombined,
		rpc.GammaZeroScopeSPY,
		rpc.GammaZeroScopeSPX,
	} {
		path := filepath.Join(dir, gammaZeroStoreFilename(scope))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file at %s: %v", path, err)
		}
	}

	// Each scope's Load returns its own legCount, no bleeding.
	cases := map[string]int{
		rpc.GammaZeroScopeCombined: 1111,
		rpc.GammaZeroScopeSPY:      2222,
		rpc.GammaZeroScopeSPX:      3333,
	}
	for scope, wantLegs := range cases {
		got, err := store.Load(scope, now)
		if err != nil {
			t.Errorf("Load %s: %v", scope, err)
			continue
		}
		if got == nil || got.LegCount != wantLegs {
			t.Errorf("scope %s: got legs=%v, want %d", scope, got, wantLegs)
		}
	}
}

// TestGammaZeroStore_ColdMissingFile confirms Load returns (nil, nil)
// when the cache directory exists but no file is present — the
// expected first-boot state.
func TestGammaZeroStore_ColdMissingFile(t *testing.T) {
	store := newGammaZeroStore(t.TempDir())
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	got, err := store.Load(rpc.GammaZeroScopeCombined, now)
	if err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if got != nil {
		t.Errorf("Load on empty dir: got %v, want nil", got)
	}
}

// TestGammaZeroStore_VersionMismatch confirms a persisted file with
// a future Version is treated as cold cache rather than erroring.
func TestGammaZeroStore_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)

	env := gammaZeroPersistEnvelope{
		Version:    99,
		SessionKey: nySessionKey(now),
		Scope:      rpc.GammaZeroScopeCombined,
		Method:     gammaMethodToken,
		Result:     helperGammaResult(now),
	}
	if err := writeTestEnvelope(dir, rpc.GammaZeroScopeCombined, env); err != nil {
		t.Fatalf("seed envelope: %v", err)
	}

	got, err := store.Load(rpc.GammaZeroScopeCombined, now)
	if err != nil {
		t.Errorf("version mismatch should return nil-nil, got err: %v", err)
	}
	if got != nil {
		t.Errorf("version mismatch: got %v, want nil", got)
	}
}

// TestGammaZeroStore_SessionKeyMismatch confirms a persisted file
// from a prior NY session is gracefully ignored on load.
func TestGammaZeroStore_SessionKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	loc, _ := time.LoadLocation("America/New_York")
	yesterday := time.Date(2026, 5, 21, 10, 0, 0, 0, loc)
	today := time.Date(2026, 5, 22, 10, 0, 0, 0, loc)

	want := helperGammaResult(yesterday)
	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(yesterday), want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load(rpc.GammaZeroScopeCombined, today)
	if err != nil {
		t.Errorf("session-key mismatch should return nil-nil, got err: %v", err)
	}
	if got != nil {
		t.Errorf("session-key mismatch: got %v, want nil", got)
	}
}

// TestGammaZeroStore_ScopeMismatch confirms a file whose envelope's
// Scope doesn't match the load request's scope is treated as cold.
// Defense against a renamed file.
func TestGammaZeroStore_ScopeMismatch(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)

	// Hand-write a file at gamma-zero-spy.json whose envelope claims
	// scope = combined. Load(spy) must reject.
	env := gammaZeroPersistEnvelope{
		Version:    currentGammaPersistVersion,
		SessionKey: nySessionKey(now),
		Scope:      rpc.GammaZeroScopeCombined, // wrong-shape envelope at this path
		Method:     gammaMethodToken,
		Result:     helperGammaResult(now),
	}
	if err := writeTestEnvelope(dir, rpc.GammaZeroScopeSPY, env); err != nil {
		t.Fatalf("seed envelope: %v", err)
	}

	got, err := store.Load(rpc.GammaZeroScopeSPY, now)
	if err != nil {
		t.Errorf("scope mismatch should return nil-nil, got err: %v", err)
	}
	if got != nil {
		t.Errorf("scope mismatch: got %v, want nil", got)
	}
}

// TestGammaZeroStore_MethodMismatch confirms a persisted file whose
// Result.Method differs from the envelope's claimed Method is
// treated as cold. Defense-in-depth against a hand-edited cache.
func TestGammaZeroStore_MethodMismatch(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)

	result := helperGammaResult(now)
	result.Method = "perfiliev-bs-sweep-v1-stickyiv"
	env := gammaZeroPersistEnvelope{
		Version:    currentGammaPersistVersion,
		SessionKey: nySessionKey(now),
		Scope:      rpc.GammaZeroScopeCombined,
		Method:     gammaMethodToken,
		Result:     result,
	}
	if err := writeTestEnvelope(dir, rpc.GammaZeroScopeCombined, env); err != nil {
		t.Fatalf("seed envelope: %v", err)
	}

	got, err := store.Load(rpc.GammaZeroScopeCombined, now)
	if err != nil {
		t.Errorf("method mismatch should return nil-nil, got err: %v", err)
	}
	if got != nil {
		t.Errorf("method mismatch: got %v, want nil", got)
	}
}

// TestGammaZeroStore_AtomicReplace confirms Save replaces an existing
// file in place and leaves no orphan temp files in the dir.
func TestGammaZeroStore_AtomicReplace(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)

	first := helperGammaResult(now)
	first.LegCount = 100
	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(now), first); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	second := helperGammaResult(now)
	second.LegCount = 999
	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(now), second); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := store.Load(rpc.GammaZeroScopeCombined, now)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.LegCount != 999 {
		t.Errorf("expected second Save to replace first: got LegCount=%v, want 999", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("orphan temp file left in cache dir: %s", e.Name())
		}
	}
}

// TestGammaZeroStore_DefaultDirHonoursXDG checks the env-var
// resolution path.
func TestGammaZeroStore_DefaultDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/test-xdg")
	got, err := gammaZeroStoreDefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	want := "/tmp/test-xdg/ibkr/gamma-zero"
	if got != want {
		t.Errorf("XDG_CACHE_HOME-rooted dir: got %q, want %q", got, want)
	}
}

// TestNewGammaZeroCacheWithStore_LoadsPersistedScopes confirms that
// when the store holds per-scope files keyed to today, each scope's
// slot is installed as current — the first caller for each scope
// after restart skips its compute and serves the cached value.
func TestNewGammaZeroCacheWithStore_LoadsPersistedScopes(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 5, 22, 10, 0, 0, 0, loc)

	combined := helperGammaResult(now)
	combined.LegCount = 1111
	spy := helperGammaResult(now)
	spy.LegCount = 2222
	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(now), combined); err != nil {
		t.Fatalf("seed combined: %v", err)
	}
	if err := store.Save(rpc.GammaZeroScopeSPY, nySessionKey(now), spy); err != nil {
		t.Fatalf("seed spy: %v", err)
	}

	cache := newGammaZeroCacheWithStore(store, now, nil)
	combinedSlot, ok := cache.slots[rpc.GammaZeroScopeCombined]
	if !ok || combinedSlot.current == nil {
		t.Fatal("expected combined slot to be seeded from persisted result")
	}
	if combinedSlot.current.result == nil || combinedSlot.current.result.LegCount != 1111 {
		t.Errorf("combined: got %+v, want LegCount=1111", combinedSlot.current.result)
	}
	if combinedSlot.current.scope != rpc.GammaZeroScopeCombined {
		t.Errorf("combined scope on computation: got %q, want %q",
			combinedSlot.current.scope, rpc.GammaZeroScopeCombined)
	}

	spySlot, ok := cache.slots[rpc.GammaZeroScopeSPY]
	if !ok || spySlot.current == nil {
		t.Fatal("expected spy slot to be seeded from persisted result")
	}
	if spySlot.current.result == nil || spySlot.current.result.LegCount != 2222 {
		t.Errorf("spy: got %+v, want LegCount=2222", spySlot.current.result)
	}

	// SPX wasn't persisted: no slot for it.
	if _, ok := cache.slots[rpc.GammaZeroScopeSPX]; ok {
		t.Error("spx slot should not exist when no persisted file for that scope")
	}
}

// TestNewGammaZeroCacheWithStore_IgnoresYesterdaysSession confirms a
// daemon that rolls over an NY midnight doesn't serve yesterday's
// per-scope persistence as today's.
func TestNewGammaZeroCacheWithStore_IgnoresYesterdaysSession(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	loc, _ := time.LoadLocation("America/New_York")
	yesterday := time.Date(2026, 5, 21, 10, 0, 0, 0, loc)
	today := time.Date(2026, 5, 22, 10, 0, 0, 0, loc)
	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(yesterday), helperGammaResult(yesterday)); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	cache := newGammaZeroCacheWithStore(store, today, nil)
	if slot, ok := cache.slots[rpc.GammaZeroScopeCombined]; ok && slot.current != nil {
		t.Errorf("expected cold combined slot after session rollover, got %+v", slot.current)
	}
}

// writeTestEnvelope writes a pre-built envelope without going through
// Save. Used by tests that need to seed a malformed/mismatched
// envelope (Save would always write a "correct" one).
func writeTestEnvelope(dir, scope string, env gammaZeroPersistEnvelope) error {
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, gammaZeroStoreFilename(scope)), data, 0o644)
}
