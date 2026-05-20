package spx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if got, err := s.LoadSnapshot(); err != nil || got != nil {
		t.Fatalf("cold load: want (nil, nil), got (%v, %v)", got, err)
	}

	want := Snapshot{
		Value:       58.7,
		AsOf:        time.Date(2026, 5, 17, 20, 35, 0, 0, time.UTC),
		SessionKey:  "2026-05-16",
		Method:      methodConstituentFanout,
		MemberCount: 503,
		Coverage:    501,
		Excluded:    []ExcludedMember{{Symbol: "NEW", Reason: "thin_history(3)"}},
	}
	if err := s.SaveSnapshot(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LoadSnapshot()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil {
		t.Fatal("load returned nil after save")
	}
	if got.Value != want.Value || got.SessionKey != want.SessionKey || got.Coverage != want.Coverage {
		t.Errorf("snapshot roundtrip mismatch:\n  want %+v\n  got  %+v", want, *got)
	}
	if len(got.Excluded) != 1 || got.Excluded[0].Symbol != "NEW" {
		t.Errorf("excluded list lost in round-trip: got %+v", got.Excluded)
	}
}

// TestLoadSnapshotMethodGate pins the version gate. A snapshot.json
// written by an older methodology must be treated as no-cache so the
// next refresh tick rebuilds with the current schema. Without this gate
// a v1 file (Value-only payload) decoded silently into a v2 struct with
// the new fields zeroed, and the engine reported state=ready with a
// phantom "0% above 50-DMA / 0 new highs" reading.
func TestLoadSnapshotMethodGate(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	stale := Snapshot{
		Value:       58.7,
		AsOf:        time.Date(2026, 5, 17, 20, 35, 0, 0, time.UTC),
		SessionKey:  "2026-05-16",
		Method:      "constituent-fanout-50dma", // the pre-v2 token
		MemberCount: 503,
		Coverage:    501,
	}
	if err := s.SaveSnapshot(stale); err != nil {
		t.Fatalf("save stale-method snapshot: %v", err)
	}
	got, err := s.LoadSnapshot()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Fatalf("stale-method snapshot returned non-nil; the version gate failed open. Got: %+v", *got)
	}
}

func TestStoreWindowsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	closes := make([]float64, WindowSize)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	want := map[string]ConstituentWindow{
		"AAPL": {Symbol: "AAPL", Closes: closes, LastBarAt: "2026-05-16"},
		"MSFT": {Symbol: "MSFT", Closes: closes, LastBarAt: "2026-05-16"},
	}
	if err := s.SaveWindows(want, time.Now()); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LoadWindows()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d windows, want 2", len(got))
	}
	if len(got["AAPL"].Closes) != WindowSize {
		t.Errorf("AAPL closes: want %d entries, got %d", WindowSize, len(got["AAPL"].Closes))
	}
}

// TestStoreCorruptionRecovery covers the daemon-crash-mid-write case:
// a half-written file should not poison startup. We don't claim
// recovery from arbitrary corruption — we just need the error to be
// surfaced so the engine can cold-rebuild rather than silently use
// stale state.
func TestStoreCorruptionRecovery(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte("not json {"), 0o644); err != nil {
		t.Fatalf("seed corruption: %v", err)
	}
	got, err := s.LoadSnapshot()
	if err == nil {
		t.Errorf("expected decode error for corrupt file, got %+v", got)
	}
	if !strings.Contains(err.Error(), "decode snapshot") {
		t.Errorf("error should mention decode failure, got %v", err)
	}
}

// TestStoreUnknownVersionTriggersColdStart pins the schema-bump
// behaviour: an on-disk file with a Version field the engine doesn't
// recognise must be treated as no-cache, not as an error or as
// data-to-load. Forward-compat: a future binary can write Version=2,
// an older binary cold-rebuilds instead of corrupting its view.
func TestStoreUnknownVersionTriggersColdStart(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	future := WindowSet{Version: 99, AsOf: time.Now(), Windows: map[string]ConstituentWindow{
		"AAPL": {Symbol: "AAPL", Closes: []float64{1, 2, 3}},
	}}
	if err := s.writeAtomic("windows.json", future); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.LoadWindows()
	if err != nil {
		t.Errorf("unknown version should not error: got %v", err)
	}
	if got != nil {
		t.Errorf("unknown version should yield no-cache (nil map), got %v", got)
	}
}

// TestStoreAtomicWriteCleanupOnDecodeFailure pins that a failed save
// doesn't leave a partial snapshot.json visible. Since SaveSnapshot
// encodes-then-renames, a successful temp write followed by a rename
// failure should still result in no target file. Hard to simulate
// rename failure portably; this test instead just confirms the temp
// pattern doesn't litter the dir on the success path.
func TestStoreAtomicWriteNoLitter(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for i := range 5 {
		snap := Snapshot{Value: float64(i), Method: methodConstituentFanout}
		if err := s.SaveSnapshot(snap); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	// Only snapshot.json should remain — no stray .tmp.* files from
	// the atomic-write path.
	for _, e := range entries {
		name := e.Name()
		if name == "snapshot.json" {
			continue
		}
		if strings.HasPrefix(name, "snapshot.json.tmp") {
			t.Errorf("stale tempfile left behind: %s", name)
		}
	}
}

func TestDefaultDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/test-xdg")
	got, err := DefaultDir()
	if err != nil {
		t.Fatalf("default dir: %v", err)
	}
	if got != "/tmp/test-xdg/ibkr/breadth-spx" {
		t.Errorf("XDG dir: want /tmp/test-xdg/ibkr/breadth-spx, got %q", got)
	}
}

func TestDefaultDirFallsBackToHome(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/tmp/test-home")
	got, err := DefaultDir()
	if err != nil {
		t.Fatalf("default dir: %v", err)
	}
	if got != "/tmp/test-home/.cache/ibkr/breadth-spx" {
		t.Errorf("HOME fallback: want /tmp/test-home/.cache/ibkr/breadth-spx, got %q", got)
	}
}
