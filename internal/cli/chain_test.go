package cli

import "testing"

// fmtOI renders open interest compactly for the chain table. Mirrors the
// abbreviation policy used for bid/ask sizes (formatSize), but with the
// chain's "0 = unavailable" convention so empty cells match how zero
// bid/ask render in the same row.
func TestFmtOI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "—"},
		{-1, "—"},
		{1, "1"},
		{42, "42"},
		{999, "999"},
		{1234, "1.2K"},
		{9999, "10.0K"},
		{12345, "12K"},
		{999_999, "999K"},
		{1_234_567, "1.2M"},
		{12_500_000, "12M"},
	}
	for _, tc := range cases {
		got := fmtOI(tc.in)
		if got != tc.want {
			t.Errorf("fmtOI(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
