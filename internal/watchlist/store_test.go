package watchlist

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeSymbolsMatchesQuoteInput(t *testing.T) {
	t.Parallel()
	got := NormalizeSymbols(" ibm,brk.b, USD.JPY ,,")
	want := []string{"IBM", "BRK.B", "USD.JPY"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeSymbols = %#v, want %#v", got, want)
	}
}

func TestStoreAddRemoveClear(t *testing.T) {
	t.Parallel()
	store := New(filepath.Join(t.TempDir(), "watchlist.json"))

	snap, err := store.Add([]string{"ibm", "SPY", "ibm"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if want := []string{"IBM", "SPY"}; !reflect.DeepEqual(snap.Symbols, want) {
		t.Fatalf("after add = %#v, want %#v", snap.Symbols, want)
	}

	snap, err = store.Remove([]string{"missing", "IBM"})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if want := []string{"SPY"}; !reflect.DeepEqual(snap.Symbols, want) {
		t.Fatalf("after remove = %#v, want %#v", snap.Symbols, want)
	}

	snap, err = store.Clear()
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(snap.Symbols) != 0 {
		t.Fatalf("after clear = %#v; want empty", snap.Symbols)
	}
}

func TestStoreCorruptJSONReturnsClearError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "watchlist.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := New(path).Snapshot()
	if err == nil {
		t.Fatal("Snapshot succeeded on corrupt JSON")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error = %v, want parse context", err)
	}
}
