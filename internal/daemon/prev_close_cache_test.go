package daemon

import (
	"math"
	"testing"
	"time"
)

func TestPrevCloseCacheRoundTrip(t *testing.T) {
	t.Parallel()
	c := newPrevCloseCache()
	now := time.Now()
	c.put("AAPL", prevCloseEntry{value: 207.34}, now)

	got, ok := c.get("AAPL", now.Add(1*time.Minute))
	if !ok {
		t.Fatalf("get returned !ok immediately after put")
	}
	if got.value != 207.34 {
		t.Errorf("value = %f, want 207.34", got.value)
	}
}

// Entries past the TTL must miss. 12h covers a normal overnight gap;
// anything older than that should refresh.
func TestPrevCloseCacheStaleEntryMisses(t *testing.T) {
	t.Parallel()
	c := newPrevCloseCache()
	old := time.Now().Add(-24 * time.Hour)
	c.put("AAPL", prevCloseEntry{value: 207.34}, old)

	if _, ok := c.get("AAPL", time.Now()); ok {
		t.Errorf("stale entry should miss after TTL window")
	}
}

// Negative cache (value=0) should survive at the same TTL so a known-
// dead symbol isn't re-polled every positions call within the session.
func TestPrevCloseCacheNegativeEntryPersists(t *testing.T) {
	t.Parallel()
	c := newPrevCloseCache()
	now := time.Now()
	c.put("ZZZZ", prevCloseEntry{value: 0}, now)

	got, ok := c.get("ZZZZ", now.Add(30*time.Minute))
	if !ok {
		t.Fatalf("negative entry should hit within TTL")
	}
	if got.value != 0 {
		t.Errorf("value = %f, want 0", got.value)
	}
}

// computePositionDayChange honours the nil-pointer contract — only when
// BOTH mark and prevClose are positive do we get non-nil deltas. Pre-
// market with mark=0 must show em-dash, not a fake -100 % loss.
func TestComputePositionDayChange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mark    float64
		prev    float64
		wantChg *float64
		wantPct *float64
	}{
		{"both positive +", 150.50, 148.00, ptrFloat(2.50), ptrFloat(1.6892)},
		{"both positive -", 95.00, 100.00, ptrFloat(-5), ptrFloat(-5)},
		{"flat", 100, 100, ptrFloat(0), ptrFloat(0)},
		{"mark zero", 0, 100, nil, nil},
		{"prev zero", 100, 0, nil, nil},
		{"prev negative", 100, -1, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chg, pct := computePositionDayChange(tc.mark, tc.prev)
			assertFloatPtr(t, "chg", chg, tc.wantChg)
			assertFloatPtr(t, "pct", pct, tc.wantPct)
		})
	}
}

func ptrFloat(v float64) *float64 { return &v }

func assertFloatPtr(t *testing.T, label string, got, want *float64) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s nil mismatch: got=%v want=%v", label, got, want)
	}
	if got == nil {
		return
	}
	if math.Abs(*got-*want) > 0.001 {
		t.Errorf("%s = %f, want %f", label, *got, *want)
	}
}
