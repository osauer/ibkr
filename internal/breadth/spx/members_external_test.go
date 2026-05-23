package spx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestLoadExternalRoundTrip pins SaveExternal → LoadExternal:
// pretty-printed JSON on disk, identical bytes back out.
func TestLoadExternalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	members := makeMembers(503)
	asOf := time.Date(2026, time.May, 23, 10, 0, 0, 0, time.UTC)

	if err := SaveExternal(path, members, asOf); err != nil {
		t.Fatalf("SaveExternal: %v", err)
	}
	got, gotAsOf, ok := LoadExternal(path)
	if !ok {
		t.Fatal("LoadExternal returned not-ok on a freshly saved file")
	}
	if !slices.Equal(got, members) {
		t.Errorf("members round-trip mismatch")
	}
	if !gotAsOf.Equal(asOf) {
		t.Errorf("asOf round-trip: want %v got %v", asOf, gotAsOf)
	}
}

// TestLoadExternalMissingFile returns ok=false, no error surface
// (the daemon falls back to embedded silently in this case).
func TestLoadExternalMissingFile(t *testing.T) {
	_, _, ok := LoadExternal(filepath.Join(t.TempDir(), "missing.json"))
	if ok {
		t.Error("missing file should return not-ok")
	}
}

// TestLoadExternalCorruptJSON: invalid JSON → ok=false; daemon falls
// back. No panic, no error escalation.
func TestLoadExternalCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	_, _, ok := LoadExternal(path)
	if ok {
		t.Error("corrupt JSON should return not-ok")
	}
}

// TestLoadExternalSanityBoundRejection: count below MinMembers →
// ok=false. The runtime gate matches what the refresher checks, so a
// file written by a v0.32 daemon (no bounds at load) loaded by v0.33
// stays usable as long as the count is in band.
func TestLoadExternalSanityBoundRejection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	short := makeMembers(100)
	if err := SaveExternal(path, short, time.Now()); err != nil {
		t.Fatalf("SaveExternal: %v", err)
	}
	_, _, ok := LoadExternal(path)
	if ok {
		t.Error("file with count below MinMembers should be rejected")
	}
}

// TestLoadExternalVersionMismatch: future schema bump → ok=false,
// daemon cold-rebuilds from embedded. Mirrors the same gate pattern
// the other stores in this repo use (gamma-zero, breadth-spx).
func TestLoadExternalVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MembersFilename)
	if err := os.WriteFile(path, []byte(`{"version":99,"as_of":"2026-05-22T00:00:00Z","members":["AAA"]}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, ok := LoadExternal(path)
	if ok {
		t.Error("version mismatch should return not-ok")
	}
}

// TestMembersDefaultPath confirms the XDG-aware path resolution
// matches the documented layout: $XDG_CACHE_HOME/ibkr/spx-members/...
// or $HOME/.cache/ibkr/spx-members/... on fallback.
func TestMembersDefaultPath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdg")
	t.Setenv("HOME", "/home/test")
	got, err := MembersDefaultPath()
	if err != nil {
		t.Fatalf("MembersDefaultPath: %v", err)
	}
	want := filepath.Join("/tmp/xdg", "ibkr", "spx-members", MembersFilename)
	if got != want {
		t.Errorf("MembersDefaultPath:\n  want %s\n  got  %s", want, got)
	}
}
