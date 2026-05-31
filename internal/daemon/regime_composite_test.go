package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestBuildRegimeComposite_AllGreenIsNormalRegime pins the happy path:
// eight green rows produce a "Normal regime" verdict with six ranked
// clusters, no unranked.
func TestBuildRegimeComposite_AllGreenIsNormalRegime(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	c := buildRegimeComposite(r)
	if c.Verdict != "Normal regime" {
		t.Errorf("verdict: got %q want %q", c.Verdict, "Normal regime")
	}
	if c.GreenCount != 8 {
		t.Errorf("green: got %d want 8", c.GreenCount)
	}
	if c.RankedCount != 8 {
		t.Errorf("ranked: got %d want 8", c.RankedCount)
	}
	if c.UnrankedCount != 0 {
		t.Errorf("unranked: got %d want 0", c.UnrankedCount)
	}
	if c.ClusterGreenCount != 6 || c.ClusterRankedCount != 6 {
		t.Errorf("clusters: got green=%d ranked=%d want 6/6", c.ClusterGreenCount, c.ClusterRankedCount)
	}
}

// TestBuildRegimeComposite_ThreeRedTriggersRegimeShift pins the spec
// interpretation table: ≥3 red bands surface as a broad stress label.
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
	if c.Verdict != "Broad stress regime" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

// TestBuildRegimeComposite_SingleRedTriggersWatch pins the
// one-red branch: a single red row reads as a stress signal.
func TestBuildRegimeComposite_SingleRedTriggersWatch(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	ratio := 1.05
	r.VIXTermStructure.Ratio = &ratio
	c := buildRegimeComposite(r)
	if c.RedCount != 1 {
		t.Errorf("red: got %d want 1", c.RedCount)
	}
	if c.Verdict != "Stress signal present" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

func TestBuildRegimeComposite_RedClusterCannotBeHiddenByGreenRows(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	vvix := 125.0
	r.VolOfVol.Last = &vvix
	c := buildRegimeComposite(r)
	if c.RedCount != 1 || c.GreenCount != 7 {
		t.Fatalf("red/green counts = %d/%d, want 1/7", c.RedCount, c.GreenCount)
	}
	if c.ClusterRedCount != 1 {
		t.Fatalf("equity-vol cluster should be red despite green VIX term row, got red clusters=%d", c.ClusterRedCount)
	}
	if c.Verdict != "Stress signal present" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

func TestBuildRegimeComposite_IsolatedVVIXRedStaysVisibleButClusterIsWatch(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	vvix := 112.0
	r.VolOfVol.Last = &vvix
	c := buildRegimeComposite(r)
	if c.RedCount != 1 {
		t.Fatalf("indicator red count = %d, want isolated VVIX row still red", c.RedCount)
	}
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("isolated non-severe VVIX red should count as cluster yellow, got %+v", c)
	}
	if c.Verdict != "Normal regime" {
		t.Fatalf("verdict = %q, want normal with a single unconfirmed vol watch", c.Verdict)
	}
}

func TestBuildRegimeComposite_MixedClusterDisagreementUsesWorstBand(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	spy52 := 740.0
	r.HYGSPYDivergence.HYGPrice = &hyg
	r.HYGSPYDivergence.HYG50DMA = &hyg50
	r.HYGSPYDivergence.SPYPrice = &spy
	r.HYGSPYDivergence.SPY52WHigh = &spy52
	c := buildRegimeComposite(r)
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("unconfirmed HYG/SPY proxy red should be cluster yellow with green OAS companion, got %+v", c)
	}
	if c.Verdict != "Normal regime" {
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
	if c.GreenCount != 7 {
		t.Errorf("green: got %d want 7 (gamma row should not count)", c.GreenCount)
	}
	if c.UnrankedCount != 1 {
		t.Errorf("unranked: got %d want 1", c.UnrankedCount)
	}
	if c.RankedCount != 7 {
		t.Errorf("ranked: got %d want 7", c.RankedCount)
	}
}

func TestBuildRegimeComposite_HYGSPYNearHighCountsRed(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	spy52 := 749.0
	r.HYGSPYDivergence.HYGPrice = &hyg
	r.HYGSPYDivergence.HYG50DMA = &hyg50
	r.HYGSPYDivergence.SPYPrice = &spy
	r.HYGSPYDivergence.SPY52WHigh = &spy52
	c := buildRegimeComposite(r)
	if c.RedCount != 1 {
		t.Errorf("HYG/SPY current divergence should count as one red row, got %d", c.RedCount)
	}
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("unconfirmed HYG/SPY proxy red should be cluster yellow, got %+v", c)
	}
	if c.Verdict != "Normal regime" {
		t.Errorf("verdict: got %q", c.Verdict)
	}
}

func TestBuildRegimeComposite_HYGSPYRedRequiresConfirmation(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	spy52 := 740.0
	r.HYGSPYDivergence.HYGPrice = &hyg
	r.HYGSPYDivergence.HYG50DMA = &hyg50
	r.HYGSPYDivergence.SPYPrice = &spy
	r.HYGSPYDivergence.SPY52WHigh = &spy52

	c := buildRegimeComposite(r)
	if c.RedCount != 1 {
		t.Fatalf("indicator red count = %d, want HYG/SPY row still red", c.RedCount)
	}
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("cluster counts = red %d yellow %d, want 0/1 for unconfirmed proxy red", c.ClusterRedCount, c.ClusterYellowCount)
	}
	if c.Verdict != "Normal regime" {
		t.Fatalf("verdict = %q, want normal until an independent cluster confirms", c.Verdict)
	}

	ratio := 1.05
	r.VIXTermStructure.Ratio = &ratio
	c = buildRegimeComposite(r)
	if c.ClusterRedCount != 2 {
		t.Fatalf("confirmed proxy red should count once vol is also red, got %+v", c)
	}
	if c.Verdict != "Stress signal present" {
		t.Fatalf("verdict = %q, want stress signal with confirmed proxy red", c.Verdict)
	}
}

func TestBuildRegimeComposite_USDJPYRedRequiresConfirmation(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	weekly := -2.5
	r.USDJPY.WeeklyChange = &weekly

	c := buildRegimeComposite(r)
	if c.RedCount != 1 {
		t.Fatalf("indicator red count = %d, want USD/JPY row still red", c.RedCount)
	}
	if c.ClusterRedCount != 0 || c.ClusterYellowCount != 1 {
		t.Fatalf("cluster counts = red %d yellow %d, want 0/1 for unconfirmed FX proxy red", c.ClusterRedCount, c.ClusterYellowCount)
	}
	if c.Verdict != "Normal regime" {
		t.Fatalf("verdict = %q, want normal until an independent cluster confirms", c.Verdict)
	}

	breadth := 35.0
	r.Breadth.Envelope.PctAbove50DMA = breadth
	c = buildRegimeComposite(r)
	if c.ClusterRedCount != 2 {
		t.Fatalf("confirmed FX red should count once breadth is also red, got %+v", c)
	}
	if c.Verdict != "Stress signal present" {
		t.Fatalf("verdict = %q, want stress signal with confirmed FX red", c.Verdict)
	}
}

// TestBuildRegimeComposite_BelowFloorIsInsufficient pins the
// honesty-floor: a verdict can't be claimed when fewer than 3 rows
// are ranked.
func TestBuildRegimeComposite_BelowFloorIsInsufficient(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	// Kill enough clusters to leave only equity-vol + gamma ranked.
	r.HYGSPYDivergence.Status = rpc.RegimeStatusError
	r.CreditSpreads.Status = rpc.RegimeStatusError
	r.FundingStress.Status = rpc.RegimeStatusError
	r.USDJPY.Status = rpc.RegimeStatusError
	r.Breadth.Status = rpc.RegimeStatusError
	c := buildRegimeComposite(r)
	if c.ClusterRankedCount >= verdictFloor {
		t.Fatalf("test setup wrong: ranked clusters %d, want < %d", c.ClusterRankedCount, verdictFloor)
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

func TestBuildRegimeSummaryAndWarnings(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	r.USDJPY.Status = rpc.RegimeStatusUnavailable
	r.USDJPY.WeeklyChange = nil
	r.USDJPY.ErrorMessage = "USD.JPY: gateway delivered no FX tick"
	r.GammaZero.Status = rpc.RegimeStatusComputing
	r.GammaZero.Envelope = rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusComputing, EtaSeconds: 42}
	r.Composite = buildRegimeComposite(r)

	s := buildRegimeSummary(r)
	if s.Label != "Normal regime" {
		t.Errorf("summary label: got %q", s.Label)
	}
	if s.Evidence != "4 green clusters / 2 unranked clusters" {
		t.Errorf("summary evidence: got %q", s.Evidence)
	}
	if s.IndicatorEvidence != "6 green / 2 unranked" {
		t.Errorf("summary indicator evidence: got %q", s.IndicatorEvidence)
	}
	if s.Confidence != "medium" {
		t.Errorf("summary confidence: got %q", s.Confidence)
	}
	if s.NotAdvice == "" {
		t.Errorf("summary should carry non-advice disclosure")
	}
	if s.PunchLine == "" || !containsAll(s.PunchLine, "constructive", "unavailable", "computing") {
		t.Errorf("summary punch line missing evidence states: %q", s.PunchLine)
	}

	warnings := buildRegimeWarnings(r)
	if len(warnings) != 2 {
		t.Fatalf("warnings len=%d want 2: %+v", len(warnings), warnings)
	}
	if warnings[0].Scope == "" || warnings[0].Impact == "" || warnings[0].Action == "" {
		t.Errorf("warnings should be scoped prose with impact/action: %+v", warnings[0])
	}
}

func TestBuildRegimeSummaryStaleRankedRowsLowerConfidence(t *testing.T) {
	t.Parallel()
	r := mkAllGreenRegime()
	r.CreditSpreads.Status = rpc.RegimeStatusStale
	r.Composite = buildRegimeComposite(r)

	s := buildRegimeSummary(r)
	if s.Confidence != "medium" {
		t.Fatalf("confidence=%q, want medium when a ranked row is stale", s.Confidence)
	}
	if !strings.Contains(s.PunchLine, "cash credit spreads is ranked from stale data") {
		t.Errorf("punch line should disclose stale ranked row: %q", s.PunchLine)
	}
}

func TestAnnotateRegimeMetadataAddsBandThresholdsAndAsOf(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("CEST", 2*60*60)
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, loc)
	r := mkAllGreenRegime()
	r.AsOf = now
	r.VIXTermStructure.DataType = rpc.MarketDataLive
	r.VIXTermStructure.VIXQuality = &rpc.Quality{AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm, Source: "VIX tick"}
	r.VolOfVol.AsOfDate = "2026-05-22"
	r.Breadth.Envelope.AsOf = now.Add(-18 * time.Minute)
	r.Breadth.Envelope.Source = "local breadth cache"
	r.Breadth.Envelope.Method = "constituent-fanout"

	annotateRegimeMetadata(r)

	if r.VIXTermStructure.Band != "green" || r.VIXTermStructure.AsOf.Label != "live" {
		t.Fatalf("VIX metadata = band %q as_of %#v, want green/live", r.VIXTermStructure.Band, r.VIXTermStructure.AsOf)
	}
	if r.VIXTermStructure.Thresholds == nil || !r.VIXTermStructure.Thresholds.Heuristic || !r.VIXTermStructure.Thresholds.PendingBacktest {
		t.Fatalf("threshold metadata should mark heuristic pending backtest: %#v", r.VIXTermStructure.Thresholds)
	}
	if r.VolOfVol.AsOf.Label != "2d old" {
		t.Fatalf("VVIX as_of=%#v, want 2d old", r.VolOfVol.AsOf)
	}
	if r.Breadth.AsOf.Label == "" || !strings.HasPrefix(r.Breadth.AsOf.Label, "cached ") {
		t.Errorf("breadth as_of should be cached clock label, got %#v", r.Breadth.AsOf)
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
	vvix := 75.0
	hyOAS := 3.2
	igOAS := 1.2
	hyIG := 2.0
	hy20d := 0.1
	funding := 12.0
	zg := 580.0
	gap := 5.0 // >2% above γ-zero = green
	return &rpc.RegimeSnapshotResult{
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK,
			Ratio:  &ratio,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK,
			Last:   &vvix,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:     rpc.RegimeStatusOK,
			HYGPrice:   &hyg,
			HYG50DMA:   &hyg50,
			SPYPrice:   &spy,
			SPY52WHigh: &spy52,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			Status:      rpc.RegimeStatusOK,
			HYOAS:       &hyOAS,
			IGOAS:       &igOAS,
			HYIGSpread:  &hyIG,
			HY20DChange: &hy20d,
		},
		FundingStress: rpc.RegimeFundingStress{
			Status:    rpc.RegimeStatusOK,
			SpreadBps: &funding,
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

func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}
