package cache

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestJSONCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "contracts.json")
	c, err := OpenJSONCache(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Put(Contract{Symbol: "AAPL", ConID: 265598, SecType: "STK"})
	c.Put(Contract{Symbol: "MSFT", ConID: 272093, SecType: "STK"})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	c2, err := OpenJSONCache(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c2.Get("AAPL")
	if !ok || got.ConID != 265598 {
		t.Fatalf("AAPL after reload = %+v ok=%v", got, ok)
	}
	if len(c2.All()) != 2 {
		t.Fatalf("len(All) = %d, want 2", len(c2.All()))
	}
}

func TestJSONCache_FlushIdempotentWhenClean(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenJSONCache(filepath.Join(dir, "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestJSONCache_ConcurrentReadsAndWrites(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenJSONCache(filepath.Join(dir, "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(2)
		go func(i int) { defer wg.Done(); c.Put(Contract{Symbol: "S", ConID: i}) }(i)
		go func() { defer wg.Done(); _, _ = c.Get("S") }()
	}
	wg.Wait()
	if _, ok := c.Get("S"); !ok {
		t.Fatal("S missing after concurrent writes")
	}
}

// Pre-fix, Flush snapshotted under RLock then cleared dirty under WLock
// AFTER the file write. A Put landing in that window updated dirty=true
// then was clobbered to false, so the new entry was lost on disk. Force
// the interleave by hooking writeJSONAtomic to call back into Put while
// the write is in flight; reopen to assert the late Put survived.
func TestJSONCache_FlushDoesNotClobberConcurrentWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "contracts.json")
	c, err := OpenJSONCache(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Put(Contract{Symbol: "AAPL", ConID: 1, SecType: "STK"})

	orig := writeJSONAtomic
	t.Cleanup(func() { writeJSONAtomic = orig })
	writeJSONAtomic = func(p string, v any) error {
		c.Put(Contract{Symbol: "MSFT", ConID: 2, SecType: "STK"})
		return orig(p, v)
	}

	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeJSONAtomic = orig
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	c2, err := OpenJSONCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.Get("AAPL"); !ok {
		t.Fatal("AAPL missing after flush+reload")
	}
	if _, ok := c2.Get("MSFT"); !ok {
		t.Fatal("MSFT (concurrent Put) lost on disk — dirty bit was clobbered")
	}
}

func TestInactiveStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inactive.json")
	s, err := OpenInactiveStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Mark("XYZ", "delisted")
	s.Mark("XYZ", "ignored second reason")
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenInactiveStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := s2.Reason("XYZ")
	if !ok || r != "delisted" {
		t.Fatalf("XYZ reason after reload = %q ok=%v", r, ok)
	}
}
