package daemon

import (
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestBuildRegimeComposite_AllGreenIsNormalRegime pins the happy path:
// five green rows produce a "Normal regime" verdict with five ranked
// rows, no unranked.
func TestBuildRegimeComposite_AllGreenIsNormalRegime(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	c := buildRegimeComposite(r)
	if c.Verdict != "Normal regime" {
		t.Errorf("verdict: got %q want %q", c.Verdict, "Normal regime")
	}
	if c.GreenCount != 5 {
		t.Errorf("green: got %d want 5", c.GreenCount)
	}
	if c.RankedCount != 5 {
		t.Errorf("ranked: got %d want 5", c.RankedCount)
	}
	if c.UnrankedCount != 0 {
		t.Errorf("unranked: got %d want 0", c.UnrankedCount)
	}
}

// TestBuildRegimeComposite_ThreeRedTriggersRegimeShift pins the spec
// interpretation table: ≥3 red bands surface as "Regime shift likely".
func TestBuildRegimeComposite_ThreeRedTriggersRegimeShift(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	// Force VIX, USD/JPY, breadth into red bands.
	ratio := 1.05
	r.VIXTermStructure.Ratio = &ratio
	weekly := -3.0 // yen strengthening 3% = red
	r.USDJPY.WeeklyChange = &weekly
	r.Breadth.Envelope.PctAbove50DMA = 30 // <40 = red
	c := buildRegimeComposite(r)
	if c.RedCount != 3 {
		t.Errorf("red: got %d want 3", c.RedCount)
	}
	if c.Verdict != "Regime shift likely — execute pre-committed plan" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

// TestBuildRegimeComposite_SingleRedTriggersWatch pins the
// one-red branch: a single red row reads as "Watch closely".
func TestBuildRegimeComposite_SingleRedTriggersWatch(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	ratio := 1.05
	r.VIXTermStructure.Ratio = &ratio
	c := buildRegimeComposite(r)
	if c.RedCount != 1 {
		t.Errorf("red: got %d want 1", c.RedCount)
	}
	if c.Verdict != "Watch closely, prep defensive moves" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

// TestBuildRegimeComposite_UnrankedDoesntCountTowardBand pins the
// honesty contract: computing / unavailable rows don't contribute to
// the green tally — they sit in unranked so a consumer sees the
// reduced coverage explicitly.
func TestBuildRegimeComposite_UnrankedDoesntCountTowardBand(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	r.GammaZero.Status = rpc.RegimeStatusComputing
	r.GammaZero.Envelope.Result = nil
	c := buildRegimeComposite(r)
	if c.GreenCount != 4 {
		t.Errorf("green: got %d want 4 (gamma row should not count)", c.GreenCount)
	}
	if c.UnrankedCount != 1 {
		t.Errorf("unranked: got %d want 1", c.UnrankedCount)
	}
	if c.RankedCount != 4 {
		t.Errorf("ranked: got %d want 4", c.RankedCount)
	}
}

// TestBuildRegimeComposite_BelowFloorIsInsufficient pins the
// honesty-floor: a verdict can't be claimed when fewer than 3 rows
// are ranked.
func TestBuildRegimeComposite_BelowFloorIsInsufficient(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	// Kill HYG/SPY, USD/JPY, breadth to leave only VIX + gamma ranked.
	r.HYGSPYDivergence.Status = rpc.RegimeStatusError
	r.USDJPY.Status = rpc.RegimeStatusError
	r.Breadth.Status = rpc.RegimeStatusError
	c := buildRegimeComposite(r)
	if c.RankedCount >= verdictFloor {
		t.Fatalf("test setup wrong: ranked %d, want < %d", c.RankedCount, verdictFloor)
	}
	if c.Verdict != "Insufficient signal — too few indicators ranked" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

// TestBuildRegimeComposite_NilReturnsHonestVerdict guards the
// defensive nil path so a handler that returns the helper directly
// doesn't crash on a fresh result envelope.
func TestBuildRegimeComposite_NilReturnsHonestVerdict(t *testing.T) {
	t.Parallel()
	c := buildRegimeComposite(nil)
	if c.RankedCount != 0 || c.UnrankedCount != 0 {
		t.Errorf("nil should produce zero counts: %+v", c)
	}
	if c.Verdict == "" {
		t.Errorf("nil verdict should be non-empty")
	}
}

// mkAllGreenRegime returns a fixture where every row classifies green
// under the spec defaults. Used as the baseline so each test can
// perturb one indicator and verify the rollup tracks.
func mkAllGreenRegime() *rpc.RegimeSnapshotResult {
	ratio := 0.85
	hyg := 80.0
	hyg50 := 78.0
	spy := 580.0
	spy52 := 600.0
	weekly := -0.5 // <1% weekly = green
	zg := 580.0
	gap := 5.0 // >2% above γ-zero = green
	return &rpc.RegimeSnapshotResult{
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK,
			Ratio:  &ratio,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:     rpc.RegimeStatusOK,
			HYGPrice:   &hyg,
			HYG50DMA:   &hyg50,
			SPYPrice:   &spy,
			SPY52WHigh: &spy52,
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status:       rpc.RegimeStatusOK,
			WeeklyChange: &weekly,
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					SpotUnderlying: 580.0,
					ZeroGamma:      &zg,
					GapPct:         &gap,
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.BreadthSPXResult{
				State:         rpc.BreadthStateReady,
				PctAbove50DMA: 65,
			},
		},
	}
}
