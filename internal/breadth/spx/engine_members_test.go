package spx

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// TestEngineSetMembersDetectsChange: identical list → false, no
// downstream signal. Different list → true and Members() reflects.
func TestEngineSetMembersDetectsChange(t *testing.T) {
	e := freshEngine(t)
	initial := e.Members()
	if changed := e.SetMembers(slices.Clone(initial)); changed {
		t.Error("SetMembers reported change for identical list")
	}

	updated := slices.Clone(initial)
	updated[0] = "ZZZ"
	if changed := e.SetMembers(updated); !changed {
		t.Error("SetMembers should report change when list differs")
	}
	got := e.Members()
	if !slices.Equal(got, updated) {
		t.Error("Members() did not reflect updated list")
	}
}

// TestEngineMembersDefensiveCopy confirms callers can't mutate engine
// state by editing the returned slice.
func TestEngineMembersDefensiveCopy(t *testing.T) {
	e := freshEngine(t)
	got := e.Members()
	if len(got) == 0 {
		t.Skip("no embedded members available")
	}
	got[0] = "TAMPERED"
	again := e.Members()
	if again[0] == "TAMPERED" {
		t.Error("Members() returned aliased slice — caller mutation leaked into engine state")
	}
}

// TestEngineNewWithMembersOption: explicit Members option overrides
// the embedded list. Used by the daemon to seed from the on-disk
// cache file at startup.
func TestEngineNewWithMembersOption(t *testing.T) {
	custom := []string{"AAA", "BBB", "CCC"}
	e := New(NewStore(t.TempDir()), stubBarFetcher{}, Options{Members: custom})
	got := e.Members()
	if !slices.Equal(got, custom) {
		t.Errorf("Engine.Members not seeded from Options.Members: got %v", got)
	}
}

// TestEngineNewFallsBackToEmbedded: empty Members option falls back
// to MemberList() — preserves existing callers and test fixtures
// constructed without the new field.
func TestEngineNewFallsBackToEmbedded(t *testing.T) {
	e := New(NewStore(t.TempDir()), stubBarFetcher{}, Options{})
	got := e.Members()
	embedded, _ := MemberList()
	if !slices.Equal(got, embedded) {
		t.Error("Engine should fall back to MemberList() when Options.Members is empty")
	}
}

// TestEngineMembersFnDeferredOnce pins the lazy members resolution:
// construction must not invoke MembersFn (autospawn race losers build
// an Engine but never use it), concurrent first uses share exactly one
// invocation, and a SetMembers push after the load stays final.
func TestEngineMembersFnDeferredOnce(t *testing.T) {
	var calls atomic.Int32
	custom := []string{"AAA", "BBB", "CCC"}
	e := New(NewStore(t.TempDir()), stubBarFetcher{}, Options{MembersFn: func() []string {
		calls.Add(1)
		return slices.Clone(custom)
	}})
	if got := calls.Load(); got != 0 {
		t.Fatalf("MembersFn ran at construction: %d calls, want 0", got)
	}

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { e.Members() })
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("MembersFn calls after concurrent first use: got %d, want exactly 1", got)
	}
	if got := e.Members(); !slices.Equal(got, custom) {
		t.Errorf("Members() = %v, want deferred list %v", got, custom)
	}

	pushed := []string{"ZZZ"}
	if changed := e.SetMembers(pushed); !changed {
		t.Error("SetMembers should report change against the deferred list")
	}
	if got := e.Members(); !slices.Equal(got, pushed) {
		t.Errorf("Members() after refresher push = %v, want %v", got, pushed)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("MembersFn re-ran after SetMembers: got %d calls, want 1", got)
	}
}

// TestEngineMembersOptionWinsOverFn: an explicit Members list resolves
// eagerly and the deferred fn is never consulted.
func TestEngineMembersOptionWinsOverFn(t *testing.T) {
	var calls atomic.Int32
	custom := []string{"AAA", "BBB"}
	e := New(NewStore(t.TempDir()), stubBarFetcher{}, Options{
		Members:   custom,
		MembersFn: func() []string { calls.Add(1); return []string{"NOPE"} },
	})
	if got := e.Members(); !slices.Equal(got, custom) {
		t.Errorf("Members() = %v, want explicit list %v", got, custom)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("MembersFn ran despite explicit Members: %d calls, want 0", got)
	}
}
