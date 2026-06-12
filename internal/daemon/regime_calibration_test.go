package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// TestRegimeIncident20260612Regression replays the 2026-06-12 pre-open false
// positive through the full post-fanout pipeline: HYG 7 bps below its 50DMA
// (one session, thin pre-open context) plus a prior-evening gamma cache
// mutually confirmed "Broad stress regime / confirmed_stress / act" against a
// green tape. Under the confirmation gates both reds are provisional: the
// engine must warn (early_warning / watch, "Stress signal present") and
// disclose both clusters as unconfirmed — never confirm, never demand act.
func TestRegimeIncident20260612Regression(t *testing.T) {
	t.Parallel()
	ratio := 18.84 / 21.42
	spyChange := 0.3
	vixChange := -3.45
	vvix := 100.6
	r := &rpc.RegimeSnapshotResult{
		AsOf: time.Now(),
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK,
			VIX:    new(18.84), VIX3M: new(21.42), Ratio: &ratio,
			VIXChangePct: &vixChange,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK, Last: &vvix,
			AsOfDate: time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:   rpc.RegimeStatusOK,
			HYGPrice: new(79.95), HYG50DMA: new(80.008),
			SPYPrice: new(740.06), SPY52WHigh: new(760.39),
			SPYChangePct: &spyChange,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK, HYOAS: new(2.80), HY20DChange: new(0.04)},
		FundingStress: rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK, SpreadBps: new(float64(9))},
		USDJPY:        rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, WeeklyChange: new(0.13)},
		GammaZero: rpc.RegimeGammaZero{
			// fetchRegimeGamma downgrades a prior-trading-date compute to
			// stale; the band stays visible for awareness.
			Status: rpc.RegimeStatusStale,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					GammaSign: "negative",
					Quality:   &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable},
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status:   rpc.RegimeStatusOK,
			Envelope: rpc.BreadthSPXResult{State: rpc.BreadthStateReady, PctAbove50DMA: 52},
		},
	}
	c := regimeTestFinalize(t, r)

	if r.Lifecycle.Stage != rpc.LifecycleEarlyWarning || r.Lifecycle.Severity != "watch" {
		t.Fatalf("lifecycle = %s/%s, want early_warning/watch (incident produced confirmed_stress/act)", r.Lifecycle.Stage, r.Lifecycle.Severity)
	}
	if len(r.Lifecycle.ConfirmedBy) != 0 {
		t.Fatalf("confirmed_by = %v, want empty — marginal reds must not confirm", r.Lifecycle.ConfirmedBy)
	}
	for _, want := range []string{"credit", "gamma"} {
		if !slices.Contains(r.Lifecycle.Unconfirmed, want) {
			t.Fatalf("unconfirmed = %v, want %s disclosed", r.Lifecycle.Unconfirmed, want)
		}
	}
	if c.Verdict != "Stress signal present" {
		t.Fatalf("verdict = %q, want %q (incident headlined Broad stress regime)", c.Verdict, "Stress signal present")
	}
	if c.ClusterEligibleRedCount != 0 {
		t.Fatalf("eligible reds = %d, want 0", c.ClusterEligibleRedCount)
	}
	if r.Posture.Label != c.Verdict {
		t.Fatalf("posture label %q != verdict %q — headline drift", r.Posture.Label, c.Verdict)
	}
	// The stale gamma row degrades readiness (as it did in the real
	// incident), so the non-confirmed stage renders the data_quality tone —
	// a muted "verify inputs" read, never the red stress headline.
	if r.Posture.Tone != rpc.RegimeToneDataQuality || r.Posture.Severity != "watch" {
		t.Fatalf("posture = %s/%s, want data_quality/watch", r.Posture.Tone, r.Posture.Severity)
	}
	if r.Lifecycle.Readiness != "degraded" {
		t.Fatalf("readiness = %q, want degraded with a stale confirming input", r.Lifecycle.Readiness)
	}
	// Row-level disclosures: the gamma red stays visible on the stale row,
	// and HYG's eligibility names the failed depth gate.
	if r.GammaZero.Band != "red" || r.GammaZero.Status != rpc.RegimeStatusStale {
		t.Fatalf("gamma row = band %q status %q, want red/stale awareness", r.GammaZero.Band, r.GammaZero.Status)
	}
	if e := r.HYGSPYDivergence.Eligibility; e == nil || e.Eligible || !slices.Contains(e.Reasons, "depth_below_min") {
		t.Fatalf("hyg eligibility = %+v, want provisional with depth_below_min", e)
	}
	if e := r.GammaZero.Eligibility; e == nil || e.Eligible || !slices.Contains(e.Reasons, "data_overdue") {
		t.Fatalf("gamma eligibility = %+v, want provisional with data_overdue", e)
	}
}

// TestStreakStoreCountsTradingDays pins the weekend fix: a Saturday or
// Sunday poll keys to Friday's trading session and cannot inflate the
// counter; Monday increments it.
func TestStreakStoreCountsTradingDays(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStreakStore(t.TempDir())
	friday := time.Date(2026, 6, 5, 10, 0, 0, 0, loc)
	saturday := time.Date(2026, 6, 6, 12, 0, 0, 0, loc)
	sunday := time.Date(2026, 6, 7, 12, 0, 0, 0, loc)
	monday := time.Date(2026, 6, 8, 10, 0, 0, 0, loc)

	if got := s.Tick("test_ind", 1.0, "red", friday); got.Sessions != 1 {
		t.Fatalf("friday sessions = %d, want 1", got.Sessions)
	}
	if got := s.Tick("test_ind", 1.0, "red", saturday); got.Sessions != 1 {
		t.Fatalf("saturday sessions = %d, want 1 (weekend keys to Friday)", got.Sessions)
	}
	if got := s.Tick("test_ind", 1.0, "red", sunday); got.Sessions != 1 {
		t.Fatalf("sunday sessions = %d, want 1", got.Sessions)
	}
	if got := s.Tick("test_ind", 1.0, "red", monday); got.Sessions != 2 {
		t.Fatalf("monday sessions = %d, want 2", got.Sessions)
	}
}

// TestStreakStoreEligibilityLatch pins the latch lifecycle: earned on a red
// streak, held across ticks, dropped on band change.
func TestStreakStoreEligibilityLatch(t *testing.T) {
	t.Parallel()
	loc, _ := time.LoadLocation("America/New_York")
	s := NewStreakStore(t.TempDir())
	day1 := time.Date(2026, 6, 4, 10, 0, 0, 0, loc)
	day2 := time.Date(2026, 6, 5, 10, 0, 0, 0, loc)

	s.Tick("test_ind", 1.0, "red", day1)
	if s.Latched("test_ind") {
		t.Fatal("latch must not pre-exist")
	}
	s.Latch("test_ind")
	if !s.Latched("test_ind") {
		t.Fatal("latch should be set on a live red streak")
	}
	s.Tick("test_ind", 1.0, "red", day2)
	if !s.Latched("test_ind") {
		t.Fatal("latch must survive same-band ticks")
	}
	s.Tick("test_ind", 0.5, "green", day2)
	if s.Latched("test_ind") {
		t.Fatal("band change must drop the latch")
	}
	// Latch on a non-red entry is a no-op.
	s.Latch("test_ind")
	if s.Latched("test_ind") {
		t.Fatal("latch must not decorate a non-red streak")
	}
}

// TestPopulateStreaksExitHysteresisHoldsRed pins the boundary-flap guard: a
// prior red holds while the exit threshold has not cleared, and releases
// once it does.
func TestPopulateStreaksExitHysteresisHoldsRed(t *testing.T) {
	t.Parallel()
	s := &Server{streaks: NewStreakStore(t.TempDir())}
	mk := func(ratio float64) *rpc.RegimeSnapshotResult {
		r := &rpc.RegimeSnapshotResult{AsOf: time.Now()}
		r.VIXTermStructure = rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK, Ratio: new(ratio)}
		return r
	}
	// Enter red.
	res := mk(1.02)
	policies := s.populateStreaks(res)
	if policies[StreakKeyVIXTerm].band != "red" {
		t.Fatalf("entry band = %q, want red", policies[StreakKeyVIXTerm].band)
	}
	// Wobble below the entry threshold but above the 0.98 exit: held red.
	res = mk(0.99)
	policies = s.populateStreaks(res)
	if policies[StreakKeyVIXTerm].band != "red" {
		t.Fatalf("hysteresis band = %q, want red held at ratio 0.99", policies[StreakKeyVIXTerm].band)
	}
	annotateRegimeMetadata(res, policies)
	if !strings.Contains(res.VIXTermStructure.BandReason, "hysteresis") {
		t.Fatalf("band reason = %q, want hysteresis disclosure", res.VIXTermStructure.BandReason)
	}
	// Clear the exit threshold: release.
	res = mk(0.97)
	policies = s.populateStreaks(res)
	if policies[StreakKeyVIXTerm].band == "red" {
		t.Fatalf("band = %q, want released below the exit threshold", policies[StreakKeyVIXTerm].band)
	}
}

// TestRegimeDecisionJournalDedupesAndHeartbeats pins the journal contract:
// fingerprint-deduped, hourly heartbeat, one valid JSON line per write.
func TestRegimeDecisionJournalDedupesAndHeartbeats(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j := &regimeDecisionJournal{path: filepath.Join(dir, "regime-decisions.jsonl")}
	res := &rpc.RegimeSnapshotResult{
		Fingerprint: rpc.Fingerprint{Version: "v", Key: "sha256:aaa"},
		Lifecycle:   rpc.LifecycleState{Stage: rpc.LifecycleQuiet, Severity: "observe"},
		Composite:   rpc.RegimeComposite{Verdict: "Normal regime"},
	}
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	if err := j.append(now, res); err != nil {
		t.Fatal(err)
	}
	if err := j.append(now.Add(time.Minute), res); err != nil {
		t.Fatal(err)
	}
	lines := journalLines(t, j.path)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1 (same fingerprint dedupes)", len(lines))
	}
	// Heartbeat after an hour even with an unchanged fingerprint.
	if err := j.append(now.Add(61*time.Minute), res); err != nil {
		t.Fatal(err)
	}
	// Semantic change writes immediately.
	res.Fingerprint.Key = "sha256:bbb"
	if err := j.append(now.Add(62*time.Minute), res); err != nil {
		t.Fatal(err)
	}
	lines = journalLines(t, j.path)
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3 (heartbeat + semantic change)", len(lines))
	}
	for i, line := range lines {
		var decoded regimeDecisionLine
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
		if decoded.V != 1 || decoded.Stage == "" {
			t.Fatalf("line %d = %+v, want v1 with stage", i, decoded)
		}
	}
}

func journalLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
