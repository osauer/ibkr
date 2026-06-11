package ibkr

import "testing"

// TestAccountCodeConcrete pins what counts as an account code usable in
// account-scoped requests (reqAcctData, reqPnL): the aggregate "All" and
// comma-separated managedAccounts lists are session aggregates, and TWS
// rejects them with error 321 (observed 2026-06-11: the portfolio stream
// never started and positions stayed empty for a daemon lifetime).
func TestAccountCodeConcrete(t *testing.T) {
	t.Parallel()
	cases := []struct {
		account string
		want    bool
	}{
		{"", false},
		{"  ", false},
		{"All", false},
		{"all", false},
		{"ALL", false},
		{"U1234567", true},
		{" U1234567 ", true},
		{"DU3136804", true},
		{"U111,U222", false},
		{"U111, U222", false},
		{"U111 U222", false},
	}
	for _, tc := range cases {
		if got := accountCodeConcrete(tc.account); got != tc.want {
			t.Errorf("accountCodeConcrete(%q) = %v, want %v", tc.account, got, tc.want)
		}
	}
}

func TestFirstConcreteAccountCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		account string
		want    string
	}{
		{"", ""},
		{"All", ""},
		{"U1234567", "U1234567"},
		{"U111,U222", "U111"},
		{" U111 , U222", "U111"},
		{",U222", ""},
	}
	for _, tc := range cases {
		if got := firstConcreteAccountCode(tc.account); got != tc.want {
			t.Errorf("firstConcreteAccountCode(%q) = %q, want %q", tc.account, got, tc.want)
		}
	}
}

// TestAccountSummaryShowsPositions pins the resubscribe watchdog's
// trigger: GrossPositionValue keys may carry a currency suffix in
// non-USD-base accounts (e.g. "GrossPositionValue_EUR").
func TestAccountSummaryShowsPositions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		summary map[string]string
		want    bool
	}{
		{"nil", nil, false},
		{"flat", map[string]string{"GrossPositionValue": "0"}, false},
		{"base key", map[string]string{"GrossPositionValue": "250000.00"}, true},
		{"currency suffix", map[string]string{"GrossPositionValue_EUR": "250000.00"}, true},
		{"unrelated keys", map[string]string{"NetLiquidation": "100000.00"}, false},
		{"unparsable", map[string]string{"GrossPositionValue": "n/a"}, false},
	}
	for _, tc := range cases {
		if got := accountSummaryShowsPositions(tc.summary); got != tc.want {
			t.Errorf("%s: accountSummaryShowsPositions = %v, want %v", tc.name, got, tc.want)
		}
	}
}
