// refresh-spx-members pulls the current S&P-500 constituent list from
// Wikipedia's "List of S&P 500 companies" article and rewrites
// internal/breadth/spx/members_data.go with the result. Runs as a
// developer tool — the daemon itself never makes this request.
//
// Invoked by `make refresh-spx-members` and `make release`. The
// release flow fails-closed if the rewrite produces a diff: the
// maintainer commits the membership update separately, then re-runs
// the release. This keeps the binary's checked-in member list in
// lockstep with the git tag.
//
// Sanity bounds: refuses to write if fewer than 450 or more than 520
// tickers are extracted (Wikipedia table structure breakage would
// otherwise silently corrupt the list). The S&P 500 actually carries
// ~500–505 names with the dual-class entries (BRK.B, GOOG/GOOGL,
// etc.); the band is intentionally wider than that to absorb
// transient mid-rebalance editing on the source page.
package main

import (
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	wikipediaURL = "https://en.wikipedia.org/wiki/List_of_S%26P_500_companies"
	userAgent    = "ibkr-refresh-spx-members/1.0 (https://github.com/osauer/ibkr; +breadth indicator)"
	httpTimeout  = 15 * time.Second
	minMembers   = 450
	maxMembers   = 520
	outputPath   = "internal/breadth/spx/members_data.go"
)

func main() {
	dry := flag.Bool("dry-run", false, "parse and print the symbols but don't rewrite members_data.go")
	urlOverride := flag.String("url", "", "override Wikipedia URL (for testing against a fixture HTTP server)")
	flag.Parse()

	url := wikipediaURL
	if *urlOverride != "" {
		url = *urlOverride
	}

	body, err := fetch(url)
	if err != nil {
		log.Fatalf("fetch %s: %v", url, err)
	}
	symbols, err := ParseSymbols(body)
	if err != nil {
		log.Fatalf("parse: %v", err)
	}
	if n := len(symbols); n < minMembers || n > maxMembers {
		log.Fatalf("sanity check: parsed %d symbols, want %d-%d (Wikipedia table structure may have changed)", n, minMembers, maxMembers)
	}

	if *dry {
		for _, s := range symbols {
			fmt.Println(s)
		}
		fmt.Fprintf(os.Stderr, "\n%d symbols (would write to %s)\n", len(symbols), outputPath)
		return
	}

	asOf := time.Now().UTC()
	src, err := renderMembersData(symbols, asOf)
	if err != nil {
		log.Fatalf("render members_data.go: %v", err)
	}
	if err := os.WriteFile(outputPath, src, 0o644); err != nil {
		log.Fatalf("write %s: %v", outputPath, err)
	}
	fmt.Fprintf(os.Stderr, "Wrote %d symbols to %s (as-of %s)\n", len(symbols), outputPath, asOf.Format("2006-01-02"))
}

func fetch(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Wikipedia's bot policy expects a descriptive User-Agent with a
	// contact URL — anonymous python-requests-style UAs get blocked.
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html")

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// constituentsTableRE locates the constituents table on the Wikipedia
// page. The table is consistently tagged with id="constituents" — a
// stable anchor since the article was restructured in 2014. The non-
// greedy match stops at the closing </table>, so additional wikitables
// further down the page (e.g. "Selected changes") don't get scanned.
var constituentsTableRE = regexp.MustCompile(`(?s)<table[^>]*id="constituents"[^>]*>(.*?)</table>`)

// rowRE breaks the table body into <tr>...</tr> blocks. (?s) makes .
// match newlines so a single regex captures multi-line rows.
var rowRE = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)

// firstCellRE captures the first <td>...</td> of each row. Ticker
// cells are always the first cell since the table's first column has
// been "Symbol" since the article's 2014 restructure.
var firstCellRE = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)

// linkTextRE captures the visible text of the first <a> tag inside a
// cell. The ticker can be wrapped in either an external NYSE/NASDAQ
// quote link or an internal /wiki/ link; both forms have the symbol
// as the visible link text.
var linkTextRE = regexp.MustCompile(`(?s)<a[^>]*>(.*?)</a>`)

// tickerRE validates the extracted text looks like a ticker: 1-6
// characters, starting with an uppercase letter, allowing digits and
// `.` (BRK.B, BF.B) or `-` (occasional class suffixes). This is the
// last line of defence against the regex chain swallowing stray
// non-ticker link text (e.g. a citation marker).
var tickerRE = regexp.MustCompile(`^[A-Z][A-Z0-9.\-]{0,5}$`)

// ParseSymbols extracts the S&P-500 ticker list from a Wikipedia
// "List of S&P 500 companies" page body. The result is uppercase,
// deduplicated, and sorted ascending — the form the runtime expects
// from members_data.go.
//
// Exported (and broken out from main) so parse_test.go can run it
// against checked-in HTML fixtures with no network involvement.
func ParseSymbols(html []byte) ([]string, error) {
	tbl := constituentsTableRE.FindSubmatch(html)
	if tbl == nil {
		return nil, fmt.Errorf("constituents table not found (Wikipedia structure may have changed)")
	}
	rows := rowRE.FindAllSubmatch(tbl[1], -1)
	if len(rows) == 0 {
		return nil, fmt.Errorf("no rows found inside constituents table")
	}

	seen := make(map[string]struct{}, 520)
	out := make([]string, 0, 520)
	for _, row := range rows {
		cell := firstCellRE.FindSubmatch(row[1])
		if cell == nil {
			// Header row (only <th> cells), or malformed — skip.
			continue
		}
		link := linkTextRE.FindSubmatch(cell[1])
		if link == nil {
			// Some cells contain plain text rather than a link.
			// Try the cell body itself, stripped of tags.
			text := stripTags(string(cell[1]))
			if tickerRE.MatchString(text) {
				if _, dup := seen[text]; !dup {
					seen[text] = struct{}{}
					out = append(out, text)
				}
			}
			continue
		}
		ticker := strings.TrimSpace(stripTags(string(link[1])))
		// Wikipedia uses ASCII hyphens internally — but a class-share
		// ticker like BRK.B can sometimes appear as BRK·B with a
		// non-breaking dot, or with extra whitespace from the source
		// markup. Normalise before validation.
		ticker = strings.ReplaceAll(ticker, " ", "")
		if !tickerRE.MatchString(ticker) {
			continue
		}
		if _, dup := seen[ticker]; dup {
			continue
		}
		seen[ticker] = struct{}{}
		out = append(out, ticker)
	}
	sort.Strings(out)
	return slices.Clip(out), nil
}

// tagRE strips inline HTML tags so a cell like `<b>FOO</b>` collapses
// to `FOO`. Unsophisticated but sufficient — the cells we parse
// contain anchor and span tags, nothing complex.
var tagRE = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string {
	return strings.TrimSpace(tagRE.ReplaceAllString(s, ""))
}

// renderMembersData formats the symbols + as-of timestamp into a
// gofmt-clean Go source file. Deterministic: same inputs always
// produce byte-identical output, so re-running the script when
// nothing's changed leaves the working tree clean.
func renderMembersData(symbols []string, asOf time.Time) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`// Code generated by scripts/refresh-spx-members. DO NOT EDIT BY HAND.
//
// To refresh: ` + "`make refresh-spx-members`" + `. The release flow runs this
// automatically; manual edits will be overwritten on the next release.

package spx

import "time"

// sp500AsOf is the timestamp at which this list was last regenerated
// from Wikipedia's "List of S&P 500 companies" article. The
// verification scrape compares against $SPXA50R daily, so drift here
// shows up as a divergence in verify.log within one trading day of any
// reconstitution.
var sp500AsOf = time.Date(`)
	fmt.Fprintf(&b, "%d, time.%s, %d, 0, 0, 0, 0, time.UTC)\n", asOf.Year(), asOf.Month().String(), asOf.Day())
	b.WriteString(`
// sp500Members is the S&P-500 constituent list pulled from Wikipedia
// and rewritten by ` + "`make refresh-spx-members`" + ` on every release.
var sp500Members = []string{
`)
	for _, s := range symbols {
		fmt.Fprintf(&b, "\t%q,\n", s)
	}
	b.WriteString("}\n")

	// gofmt the result so the checked-in file matches `make fmt`'s
	// idea of canonical formatting. Skipping this means every release
	// would dirty the working tree with cosmetic whitespace edits.
	return format.Source([]byte(b.String()))
}
