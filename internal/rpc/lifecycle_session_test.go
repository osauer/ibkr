package rpc

import (
	"testing"
	"time"
)

// nyTest builds a fixed America/New_York timestamp for classifier tests.
func nyTest(t *testing.T, y int, m time.Month, d, hh, mm int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(y, m, d, hh, mm, 0, 0, loc)
}

// TestTapeSessionFor pins the shared classifier both the canary and the
// regime lifecycle key on: weekends and holidays are closed dates, weekday
// overnight hours belong to their trading date, and dates outside embedded
// calendar coverage stay empty (fail-open).
func TestTapeSessionFor(t *testing.T) {
	t.Parallel()

	state, reason, nextOpen := TapeSessionFor(nyTest(t, 2026, time.July, 18, 12, 0)) // Saturday
	if state != TapeSessionClosedDate || reason == "" || nextOpen == nil {
		t.Fatalf("saturday = %q/%q/nextOpen=%v, want closed_date with reason and next open", state, reason, nextOpen)
	}

	state, reason, _ = TapeSessionFor(nyTest(t, 2026, time.July, 3, 10, 30)) // Independence Day observed
	if state != TapeSessionClosedDate || reason == "" {
		t.Fatalf("holiday = %q/%q, want closed_date with a holiday reason", state, reason)
	}

	// Weekday overnight (02:00 ET Friday): VIX prints overnight on weekdays —
	// this is a trading date and keeps full tape effect.
	if state, _, _ = TapeSessionFor(nyTest(t, 2026, time.July, 17, 2, 0)); state != TapeSessionTradingDate {
		t.Fatalf("weekday overnight = %q, want trading_date", state)
	}

	if state, _, _ = TapeSessionFor(nyTest(t, 2024, time.May, 10, 12, 0)); state != "" {
		t.Fatalf("outside coverage = %q, want empty (fail-open)", state)
	}
}

// closedDateSnapshot returns the crash fixture from the panic-ladder test
// (all clusters green, tape at spy/vix) with the given tape-session state.
func tapeSnapshot(spy, vix float64, session string) RegimeSnapshotResult {
	return RegimeSnapshotResult{
		TapeSessionState: session,
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta(), VIXChangePct: &vix},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta(), SPYChangePct: &spy},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: greenMeta()},
	}
}

// TestRegimeLifecycleClosedDateTapeGating pins the closed-date rule on every
// tape-driven stage arm: frozen prints cannot enter (or, on re-evaluation,
// hold) panic, confirmed_stress, early_warning, opportunity, or
// stabilization; empty session state fails open; trading dates keep full
// effect at any hour.
func TestRegimeLifecycleClosedDateTapeGating(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		spy, vix  float64
		session   string
		wantStage string
	}{
		{"crash enters panic on a trading date", -7.2, 0, TapeSessionTradingDate, LifecyclePanic},
		{"crash fails open outside coverage", -7.2, 0, "", LifecyclePanic},
		{"frozen crash cannot enter panic", -7.2, 0, TapeSessionClosedDate, LifecycleQuiet},
		{"spy drop warns on a trading date", -1.8, 0, TapeSessionTradingDate, LifecycleEarlyWarning},
		{"frozen spy drop cannot warn", -1.8, 0, TapeSessionClosedDate, LifecycleQuiet},
		{"vix spike warns on a trading date", 0.1, 12, TapeSessionTradingDate, LifecycleEarlyWarning},
		{"frozen vix spike cannot warn", 0.1, 12, TapeSessionClosedDate, LifecycleQuiet},
		{"rally enters opportunity on a trading date", 2.0, -12, TapeSessionTradingDate, LifecycleOpportunity},
		{"frozen rally cannot enter opportunity", 2.0, -12, TapeSessionClosedDate, LifecycleQuiet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap := tapeSnapshot(tc.spy, tc.vix, tc.session)
			got := BuildRegimeLifecycle(&snap)
			if got.Stage != tc.wantStage {
				t.Fatalf("stage = %q, want %q", got.Stage, tc.wantStage)
			}
		})
	}
}

// TestRegimeLifecycleClosedDateClusterPathsUnaffected pins that the gate is
// tape-only: cluster evidence warns and confirms identically on closed dates.
func TestRegimeLifecycleClosedDateClusterPathsUnaffected(t *testing.T) {
	t.Parallel()
	eligible := func() RegimeIndicatorMeta {
		return RegimeIndicatorMeta{Band: "red", Eligibility: &RegimeEligibility{Eligible: true}}
	}
	calm := 0.1
	confirmed := RegimeSnapshotResult{
		TapeSessionState: TapeSessionClosedDate,
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta(), SPYChangePct: &calm},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: eligible()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: eligible()},
	}
	if got := BuildRegimeLifecycle(&confirmed); got.Stage != LifecycleConfirmedStress {
		t.Fatalf("two eligible reds on a closed date = %q, want confirmed_stress (cluster path untouched)", got.Stage)
	}

	provisional := tapeSnapshot(0.1, 0, TapeSessionClosedDate)
	provisional.FundingStress = RegimeFundingStress{RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "red"}}
	if got := BuildRegimeLifecycle(&provisional); got.Stage != LifecycleEarlyWarning {
		t.Fatalf("provisional red on a closed date = %q, want early_warning (cluster path untouched)", got.Stage)
	}
}

// TestRegimeLifecycleClosedDateStabilizationGated pins the remaining
// tape-driven recovery arm: a frozen bounce print cannot enter stabilization.
func TestRegimeLifecycleClosedDateStabilizationGated(t *testing.T) {
	t.Parallel()
	snap := tapeSnapshot(1.2, 0, TapeSessionTradingDate)
	snap.FundingStress = RegimeFundingStress{RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "yellow"}}
	if got := BuildRegimeLifecycle(&snap); got.Stage != LifecycleStabilization {
		t.Fatalf("live bounce with one yellow = %q, want stabilization", got.Stage)
	}
	snap.TapeSessionState = TapeSessionClosedDate
	if got := BuildRegimeLifecycle(&snap); got.Stage != LifecycleQuiet {
		t.Fatalf("frozen bounce with one yellow = %q, want quiet", got.Stage)
	}
}

// TestRegimeLifecycleClosedDateEvidenceDemoted pins the tape evidence rows:
// the frozen print keeps its factual bucket but reads forward_warning /
// observe / unconfirmed, mirroring the canary tape row's demotion.
func TestRegimeLifecycleClosedDateEvidenceDemoted(t *testing.T) {
	t.Parallel()
	snap := tapeSnapshot(-7.2, 14, TapeSessionClosedDate)
	got := BuildRegimeLifecycle(&snap)
	seen := map[string]bool{}
	for _, ev := range got.Evidence {
		if ev.Signal != "tape" {
			continue
		}
		seen[ev.Source] = true
		if ev.Bucket == "green" {
			t.Fatalf("%s bucket = green, want the frozen print's factual magnitude kept", ev.Source)
		}
		if ev.Severity != "observe" || ev.Confirmed || ev.Timing != LifecycleTimingForwardWarning {
			t.Fatalf("%s demoted row = severity %q confirmed %v timing %q, want observe/false/forward_warning", ev.Source, ev.Severity, ev.Confirmed, ev.Timing)
		}
	}
	if !seen["spy"] || !seen["vix"] {
		t.Fatalf("tape evidence rows missing: %v", seen)
	}

	live := tapeSnapshot(-7.2, 14, TapeSessionTradingDate)
	got = BuildRegimeLifecycle(&live)
	for _, ev := range got.Evidence {
		if ev.Signal == "tape" && ev.Source == "spy" {
			if ev.Severity != "act" || !ev.Confirmed || ev.Timing != LifecycleTimingContemporary {
				t.Fatalf("live spy crash row = %q/%v/%q, want act/confirmed/contemporaneous", ev.Severity, ev.Confirmed, ev.Timing)
			}
		}
	}
}

// TestRegimeSeverityGovernorClosedDateArms pins the governor interaction:
// frozen SPY/VIX change prints can neither co-sign heuristic confirmation
// (arms 1-2) nor claim the pure-tape-panic exemption, while arm 3 (the
// status-gated VIX-term inversion) keeps its existing behavior.
func TestRegimeSeverityGovernorClosedDateArms(t *testing.T) {
	t.Parallel()
	pending := func() RegimeIndicatorMeta {
		return RegimeIndicatorMeta{
			Band:        "red",
			Eligibility: &RegimeEligibility{Eligible: true},
			Thresholds:  &RegimeThresholds{Label: "test_v1", Heuristic: true, PendingBacktest: true},
		}
	}
	drop := -1.8
	cosigned := RegimeSnapshotResult{
		TapeSessionState: TapeSessionClosedDate,
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta(), SPYChangePct: &drop},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: pending()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: pending()},
	}
	got := BuildRegimeLifecycle(&cosigned)
	if got.Stage != LifecycleConfirmedStress || got.Severity != "watch" {
		t.Fatalf("frozen co-sign = %q/%q, want confirmed_stress/watch (arms 1-2 cannot co-sign on a closed date)", got.Stage, got.Severity)
	}
	if len(got.Governors) == 0 || got.Governors[0].Reason != "pending_backtest_no_tape_cosign" {
		t.Fatalf("governors = %+v, want disclosed pending_backtest cap", got.Governors)
	}

	// Arm 3 — a fresh status-ok VIX-term inversion — still co-signs; its own
	// status gate is unchanged by this pass.
	ratio := 1.02
	armThree := cosigned
	armThree.VIXTermStructure = RegimeVIXTerm{RegimeIndicatorMeta: greenMeta(), Ratio: &ratio, Status: RegimeStatusOK}
	if got := BuildRegimeLifecycle(&armThree); got.Severity != "act" {
		t.Fatalf("arm-3 co-sign on a closed date = %q, want act (status-gated arm unchanged)", got.Severity)
	}

	// The pure-tape-panic governor exemption needs a confirmable tape too: a
	// cluster-driven panic beside a frozen -4.5% print stays governable.
	threeReds := RegimeSnapshotResult{
		TapeSessionState: TapeSessionClosedDate,
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta(), SPYChangePct: new(-4.5)},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: pending()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: pending()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: pending()},
	}
	got = BuildRegimeLifecycle(&threeReds)
	if got.Stage != LifecyclePanic || got.Severity != "act" {
		t.Fatalf("closed-date cluster panic = %q/%q, want panic/act (exemption withheld)", got.Stage, got.Severity)
	}
	live := threeReds
	live.TapeSessionState = TapeSessionTradingDate
	got = BuildRegimeLifecycle(&live)
	if got.Stage != LifecyclePanic || got.Severity != "urgent" {
		t.Fatalf("live tape panic = %q/%q, want panic/urgent (exemption intact)", got.Stage, got.Severity)
	}
}
