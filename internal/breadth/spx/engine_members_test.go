package spx

import (
	"slices"
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
