package daemon

import (
	"fmt"
	"sort"
	"time"

	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// pickedExpiration carries one (date, trading-class, strikes) tuple
// the gamma compute walks to build per-leg jobs. Uniform shape across
// single-class underlyings (SPY: tradingClass == sym, one entry per
// date) and multi-class (SPX: SPX-class third-Friday + SPXW-class
// weeklies, possibly multiple entries per date).
type pickedExpiration struct {
	date         string // YYYY-MM-DD
	expiryYMD    string // YYYYMMDD
	tradingClass string
	strikes      []float64
}

// buildPickedExpirations enumerates the expirations + strikes to compute
// over. For sym=="SPX" it walks the classed enumeration (SPX-AM monthlies
// + SPXW-PM weeklies) under per-class settlement cutoffs. For everything
// else it falls back to the existing single-class path (FetchOptionExpiry
// Strikes + selectExpirations with empty class).
//
// Surfaces the per-class budget (params.ExpiryCount) GLOBALLY across
// classes — for SPX, the 6-expiry cap covers SPX + SPXW combined, so a
// quarterly third-Friday + 5 nearest SPXW weeklies is the typical
// picked set.
func buildPickedExpirations(c *ibkrlib.Connector, sym string, spotAt time.Time, expiryCount int) ([]pickedExpiration, error) {
	if sym == "SPX" {
		classed, err := c.FetchOptionExpiryStrikesClassed(sym, 30*time.Second)
		if err != nil {
			return nil, err
		}
		if len(classed) == 0 {
			return nil, fmt.Errorf("gateway returned no SPX expirations")
		}
		specs := selectSPXExpirationsClassed(classed, spotAt, expiryCount)
		out := make([]pickedExpiration, 0, len(specs))
		for _, s := range specs {
			out = append(out, pickedExpiration{
				date:         s.Date,
				expiryYMD:    compactExpiry(s.Date),
				tradingClass: s.TradingClass,
				strikes:      s.Strikes,
			})
		}
		return out, nil
	}

	// Single-class path — SPY, equities. Empty trading class to
	// selectExpirations preserves the SPY-only 16:15 ET cutoff
	// bit-for-bit. tradingClass on each leg is the underlying symbol
	// (matches what IBKR returns for SPY-class options).
	allStrikes, err := c.FetchOptionExpiryStrikes(sym, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if len(allStrikes) == 0 {
		return nil, fmt.Errorf("gateway returned no %s expirations", sym)
	}
	pickedDates := selectExpirations(allStrikes, "", spotAt, expiryCount)
	out := make([]pickedExpiration, 0, len(pickedDates))
	for _, d := range pickedDates {
		out = append(out, pickedExpiration{
			date:         d,
			expiryYMD:    compactExpiry(d),
			tradingClass: sym,
			strikes:      allStrikes[d],
		})
	}
	return out, nil
}

// pickedDatesFromPicked extracts the deduped, sorted set of dates from a
// picked-expiration slice. SPX runs with both class enumerations on the
// same third-Friday yield one entry per (date, class); the result
// envelope's Expirations field carries the unique dates only (matches
// today's []string shape for back-compat).
func pickedDatesFromPicked(picked []pickedExpiration) []string {
	seen := make(map[string]struct{}, len(picked))
	out := make([]string, 0, len(picked))
	for _, p := range picked {
		if _, ok := seen[p.date]; ok {
			continue
		}
		seen[p.date] = struct{}{}
		out = append(out, p.date)
	}
	sort.Strings(out)
	return out
}

// spxExpirySpec is a single (date, tradingClass) pair the SPX classed
// enumeration emits. Same date listed under both SPX and SPXW
// (third-Friday quarterlies) yields two spxExpirySpec entries with
// disjoint strike grids and disjoint settlement instants.
//
// Strikes carries the per-class grid as returned by
// FetchOptionExpiryStrikesClassed — does NOT merge across classes
// because the IV surface, ConID, and settlement convention differ.
type spxExpirySpec struct {
	Date         string // YYYY-MM-DD
	TradingClass string // "SPX" | "SPXW"
	Strikes      []float64
}

// selectSPXExpirationsClassed picks the nearest N (date, tradingClass)
// pairs that are NOT past their class-specific settlement window. The
// 6-expiry budget is global across classes — each (date, class)
// counts as a distinct listing because SPX-class third-Friday and
// SPXW-class third-Friday are economically distinct contracts (AM SET
// vs PM close, different ConIDs).
//
// Sorting is by date ascending, with SPX class winning ties before
// SPXW to keep the leg order deterministic. The 6-cap is then applied
// in that order — so on a Monday with both an SPX-class quarterly
// and SPXW weeklies, we pick the quarterly + 5 nearest weeklies, not
// all weeklies, which reflects the user-facing "6 expirations" budget
// honestly.
//
// classed is the per-date per-class strike grid emitted by
// FetchOptionExpiryStrikesClassed. now is the wall-clock reference
// for the settlement cutoff. count caps the returned slice.
func selectSPXExpirationsClassed(classed map[string][]ibkrlib.ExpiryClassedStrikes, now time.Time, count int) []spxExpirySpec {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	nyNow := now.In(loc)
	today := nyNow.Format("2006-01-02")

	var candidates []spxExpirySpec
	for date, entries := range classed {
		if date < today {
			continue // pre-today is always expired regardless of class
		}
		for _, entry := range entries {
			// Class-specific settlement cutoff. SPX-class third-Friday
			// settles at 09:30 ET; SPXW and any other class settle at
			// 16:00 ET. The 15-minute buffer mirrors selectExpirations'
			// PM convention for symmetry.
			day, parseErr := time.ParseInLocation("2006-01-02", date, loc)
			if parseErr != nil {
				continue
			}
			cutoff := classSettlementInstant(entry.TradingClass, day.Year(), day.Month(), day.Day(), loc).Add(classSettlementBuffer)
			if date == today && nyNow.After(cutoff) {
				continue // 0DTE post-settle for this specific class
			}
			candidates = append(candidates, spxExpirySpec{
				Date:         date,
				TradingClass: entry.TradingClass,
				Strikes:      entry.Strikes,
			})
		}
	}

	// Sort: date ascending, then trading-class ascending (SPX before
	// SPXW) for ties. Stable so re-ordering doesn't churn the leg list
	// across runs.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Date != candidates[j].Date {
			return candidates[i].Date < candidates[j].Date
		}
		return candidates[i].TradingClass < candidates[j].TradingClass
	})

	if len(candidates) > count {
		candidates = candidates[:count]
	}
	return candidates
}
