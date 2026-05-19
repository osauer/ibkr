package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestParseSymbolsFromFixture pins the parser against a checked-in
// HTML snippet captured from the live Wikipedia page. When the page's
// table structure changes, this test breaks at unit-test time so the
// release flow doesn't surprise the maintainer with a parse failure
// at tag time. To refresh the fixture: run `curl --user-agent
// 'ibkr-refresh-spx-members' https://en.wikipedia.org/wiki/List_of_S%26P_500_companies
// > testdata/wikipedia-snippet.html` and trim to a small subset of
// rows that still includes both link styles (external NYSE/NASDAQ
// quote links and internal /wiki/ links).
func TestParseSymbolsFromFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "wikipedia-snippet.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := ParseSymbols(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The fixture is hand-curated; bumping it requires updating both
	// the file and this expected list. Keeping the asserted set small
	// and explicit makes a regex regression diagnosable from the test
	// output.
	want := []string{"AAPL", "BRK.B", "GOOGL", "MMM", "MSFT", "NVDA"}
	if !slices.Equal(got, want) {
		t.Errorf("symbols:\n  want %v\n  got  %v", want, got)
	}
}

// TestParseSymbolsRejectsMissingTable covers the page-restructure
// failure mode: the regex anchor for the constituents table is gone,
// so we must return an explicit error rather than a partial / empty
// list that the release flow might silently commit.
func TestParseSymbolsRejectsMissingTable(t *testing.T) {
	html := []byte(`<html><body><p>Page redesigned, table gone.</p></body></html>`)
	_, err := ParseSymbols(html)
	if err == nil {
		t.Fatal("expected error when constituents table is absent")
	}
}

// TestParseSymbolsFiltersNonTickers covers two real footguns we've
// seen on the Wikipedia page: footnote/citation markers like [1] and
// alt-text of svg icons. Neither should be admitted as tickers.
func TestParseSymbolsFiltersNonTickers(t *testing.T) {
	html := []byte(`<table id="constituents">
<tr><th>Symbol</th><th>Security</th></tr>
<tr><td><a href="/q">AAPL</a></td><td>Apple</td></tr>
<tr><td><a href="/q">[1]</a></td><td>Footnote</td></tr>
<tr><td><a href="/q">edit</a></td><td>UI cruft</td></tr>
<tr><td><a href="/q">MSFT</a></td><td>Microsoft</td></tr>
</table>`)
	got, err := ParseSymbols(html)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"AAPL", "MSFT"}
	if !slices.Equal(got, want) {
		t.Errorf("filter:\n  want %v\n  got  %v", want, got)
	}
}

// TestParseSymbolsDeduplicates covers the rare case where the same
// ticker appears twice in the source HTML (transient Wikipedia
// edit-conflict state). Result must be deduplicated even though that
// shouldn't happen in steady state.
func TestParseSymbolsDeduplicates(t *testing.T) {
	html := []byte(`<table id="constituents">
<tr><td><a>AAPL</a></td></tr>
<tr><td><a>MSFT</a></td></tr>
<tr><td><a>AAPL</a></td></tr>
</table>`)
	got, err := ParseSymbols(html)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !slices.Equal(got, []string{"AAPL", "MSFT"}) {
		t.Errorf("dedup: got %v", got)
	}
}

// TestParseSymbolsSortsAscending pins the deterministic-output
// contract — `make refresh-spx-members` writes the symbols in sorted
// order so day-to-day no-op runs don't dirty the working tree with
// ordering noise.
func TestParseSymbolsSortsAscending(t *testing.T) {
	html := []byte(`<table id="constituents">
<tr><td><a>ZTS</a></td></tr>
<tr><td><a>AAPL</a></td></tr>
<tr><td><a>MMM</a></td></tr>
</table>`)
	got, err := ParseSymbols(html)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"AAPL", "MMM", "ZTS"}
	if !slices.Equal(got, want) {
		t.Errorf("sort:\n  want %v\n  got  %v", want, got)
	}
}
