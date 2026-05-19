package spx

import (
	"slices"
	"time"
)

// MemberList returns the S&P-500 membership the engine uses for
// today's compute. The list is checked into the repository
// (members_data.go) and refreshed by `make refresh-spx-members` —
// the daemon never reaches out to Wikipedia at runtime.
//
// asOf is the date the checked-in list was last refreshed. Stale
// lists are not a hard error; the engine logs and the verification
// scrape catches drift within a day of any reconstitution. The
// release flow runs `refresh-spx-members` on every release so a
// freshly-tagged binary always carries a current list.
func MemberList() (members []string, asOf time.Time) {
	out := slices.Clone(sp500Members)
	return out, sp500AsOf
}
