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
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); c.Put(Contract{Symbol: "S", ConID: i}) }(i)
		go func() { defer wg.Done(); _, _ = c.Get("S") }()
	}
	wg.Wait()
	if _, ok := c.Get("S"); !ok {
		t.Fatal("S missing after concurrent writes")
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
