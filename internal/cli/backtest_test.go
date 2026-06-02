package cli

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCanaryBacktestSampleProducesSignalMetrics(t *testing.T) {
	t.Parallel()
	rows := readBacktestFixture(t)
	res := runCanaryBacktest(rows, time.Date(2026, 5, 31, 9, 8, 0, 0, time.UTC))

	if got, want := res.Metrics.Observations, 18; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TargetStress, 9; got != want {
		t.Fatalf("target_stress = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalTruePositive, 9; got != want {
		t.Fatalf("signal_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalFalsePositive, 4; got != want {
		t.Fatalf("signal_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchTruePositive, 9; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchMiss, 0; got != want {
		t.Fatalf("watch_miss = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchFalsePositive, 1; got != want {
		t.Fatalf("watch_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.ActTruePositive, 6; got != want {
		t.Fatalf("act_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.RebalanceWatch, 7; got != want {
		t.Fatalf("rebalance_watch = %d, want %d", got, want)
	}
	if got, want := res.Metrics.DataQualityWatch, 3; got != want {
		t.Fatalf("data_quality_watch = %d, want %d", got, want)
	}
	if res.Metrics.SignalRecall == nil || *res.Metrics.SignalRecall != 1 {
		t.Fatalf("signal_recall = %v, want 1", res.Metrics.SignalRecall)
	}
	if !strings.Contains(strings.Join(res.Findings, "\n"), "Watch-level canary signals caught every labelled stress row") {
		t.Fatalf("findings did not record signal-level coverage: %+v", res.Findings)
	}
	if !clusterHasRebalanceWatch(res, "2023-2026 AI mega-cap concentration") {
		t.Fatalf("AI concentration cluster should expose a rebalance watch: %+v", res.Clusters)
	}
}

func TestRunBacktestCanaryRendersText(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"canary", "--input", backtestFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Canary Backtest",
		"18 observations",
		"precision 69%",
		"Watch        precision 90%",
		"2024 yen carry unwind",
		"Risk budget",
		"data-quality watch",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("backtest output missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func TestRegimeBacktestSampleProducesMarketMetrics(t *testing.T) {
	t.Parallel()
	rows := readRegimeBacktestFixture(t)
	res := runRegimeBacktest(rows, time.Date(2026, 5, 31, 11, 40, 0, 0, time.UTC))

	if got, want := res.Metrics.Observations, 17; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.ScoredObservations, 14; got != want {
		t.Fatalf("scored_observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.OutOfScope, 3; got != want {
		t.Fatalf("out_of_scope = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TargetStress, 6; got != want {
		t.Fatalf("target_stress = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchTruePositive, 6; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchFalsePositive, 2; got != want {
		t.Fatalf("watch_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.StressTruePositive, 5; got != want {
		t.Fatalf("stress_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.StressFalsePositive, 0; got != want {
		t.Fatalf("stress_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.DataQualityWatch, 1; got != want {
		t.Fatalf("data_quality_watch = %d, want %d", got, want)
	}
	if res.Metrics.WatchRecall == nil || *res.Metrics.WatchRecall != 1 {
		t.Fatalf("watch_recall = %v, want 1", res.Metrics.WatchRecall)
	}
	if res.Baseline.StressPrecision == nil || *res.Baseline.StressPrecision != 1 {
		t.Fatalf("baseline stress precision = %v, want 1", res.Baseline.StressPrecision)
	}
	if got, want := res.Lifecycle.EarlyWarning, 3; got != want {
		t.Fatalf("early_warning = %d, want %d", got, want)
	}
	if got, want := res.Lifecycle.EarlyWarningFalseCalmRally, 2; got != want {
		t.Fatalf("early_warning_false_calm_rally = %d, want %d", got, want)
	}
	findings := strings.Join(res.Findings, "\n")
	for _, want := range []string{
		"out-of-scope row(s) were excluded",
		"Regime watch caught every scored market-stress row",
		"Regime watch fired on 2 scored non-stress row",
	} {
		if !strings.Contains(findings, want) {
			t.Fatalf("findings missing %q: %+v", want, res.Findings)
		}
	}
}

func TestCanaryBacktestPortfolioStressAcceptsRebalanceWatch(t *testing.T) {
	t.Parallel()
	acc := &canaryBacktestAccumulator{}
	acc.add(CanaryBacktestRowResult{
		TargetStress:   true,
		TargetScope:    "portfolio",
		SignalWatch:    true,
		RebalanceWatch: true,
	})
	if got, want := acc.metrics.WatchTruePositive, 1; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got := acc.metrics.WatchMiss; got != 0 {
		t.Fatalf("watch_miss = %d, want 0", got)
	}

	acc = &canaryBacktestAccumulator{}
	acc.add(CanaryBacktestRowResult{
		TargetStress:   false,
		RebalanceWatch: true,
	})
	if got := acc.metrics.WatchFalsePositive; got != 0 {
		t.Fatalf("rebalance-only nonstress row should not count as defensive false positive, got %d", got)
	}
}

func TestRunBacktestRegimeRendersText(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"regime", "--input", regimeBacktestFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Regime Backtest",
		"17 observations",
		"Watch        precision 75%",
		"Stress       precision 100%",
		"Before       stress precision",
		"Early        precision",
		"Events       watch precision",
		"2020-2021 retail/reddit squ.",
		"out-of-scope",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("regime backtest output missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func TestRegimeBacktestBuilderEmitsExGammaRows(t *testing.T) {
	t.Parallel()
	f, err := os.Open(regimePointInTimeFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readRegimePointInTimeRows(f)
	if err != nil {
		t.Fatal(err)
	}
	observations := buildRegimeBacktestObservations(rows)
	if got, want := len(observations), 2; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	first := observations[0].Regime
	if first.GammaZero.Status != "unavailable" {
		t.Fatalf("gamma status = %q, want unavailable", first.GammaZero.Status)
	}
	if first.GammaZero.Band != "" {
		t.Fatalf("gamma band = %q, want unranked", first.GammaZero.Band)
	}
	if len(first.WarningDetails) != 1 || first.WarningDetails[0].Code != "gamma_zero_point_in_time_unavailable" {
		t.Fatalf("warning_details = %+v, want gamma PIT warning", first.WarningDetails)
	}
	if len(first.DataQuality) != 1 || first.DataQuality[0].Surface != "gamma" || first.DataQuality[0].Status != "degraded" {
		t.Fatalf("data_quality = %+v, want degraded gamma", first.DataQuality)
	}
	if got, want := first.Composite.ClusterUnrankedCount, 1; got != want {
		t.Fatalf("cluster_unranked_count = %d, want %d", got, want)
	}
	if got, want := first.Composite.ClusterRedCount, 5; got != want {
		t.Fatalf("cluster_red_count = %d, want %d", got, want)
	}
	res := runRegimeBacktest(observations, time.Date(2026, 5, 31, 12, 18, 0, 0, time.UTC))
	if got, want := res.Metrics.DataQualityWatch, 2; got != want {
		t.Fatalf("data_quality_watch = %d, want %d", got, want)
	}
	if got, want := res.Metrics.StressTruePositive, 1; got != want {
		t.Fatalf("stress_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.StressFalsePositive, 0; got != want {
		t.Fatalf("stress_false_positive = %d, want %d", got, want)
	}
}

func TestRunBacktestBuildRegimeEmitsJSONL(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"build-regime", "--input", regimePointInTimeFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest build-regime returned %d, stderr:\n%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
	rows, err := readRegimeBacktestObservations(strings.NewReader(stdout.String()))
	if err != nil {
		t.Fatalf("generated JSONL should feed regime backtest: %v\n%s", err, stdout.String())
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("generated rows = %d, want %d", got, want)
	}
	if !strings.Contains(stdout.String(), `"gamma_zero_point_in_time_unavailable"`) {
		t.Fatalf("generated rows missing gamma unavailable warning:\n%s", stdout.String())
	}
}

func TestRegimeBacktestBuilderPreservesTapeChangeFields(t *testing.T) {
	t.Parallel()
	rows := []RegimePointInTimeRow{{
		Date: "2026-05-29",
		VIXTermStructure: RegimePointInTimeVIXTerm{
			VIX:          new(30.0),
			VIX3M:        new(24.0),
			VIXPrevClose: new(20.0),
		},
		HYGSPYDivergence: RegimePointInTimeHYGSPY{
			HYGPrice:     new(78.0),
			HYG50DMA:     new(79.0),
			SPYPrice:     new(95.0),
			SPY52WHigh:   new(110.0),
			SPYPrevClose: new(100.0),
		},
	}}
	observations := buildRegimeBacktestObservations(rows)
	got := observations[0].Regime
	if got.VIXTermStructure.VIXChangePct == nil || *got.VIXTermStructure.VIXChangePct != 50.0 {
		t.Fatalf("vix_change_pct = %v, want 50.0", got.VIXTermStructure.VIXChangePct)
	}
	if got.HYGSPYDivergence.SPYChange == nil || *got.HYGSPYDivergence.SPYChange != -5.0 {
		t.Fatalf("spy_change = %v, want -5.0", got.HYGSPYDivergence.SPYChange)
	}
	if got.HYGSPYDivergence.SPYChangePct == nil || *got.HYGSPYDivergence.SPYChangePct != -5.0 {
		t.Fatalf("spy_change_pct = %v, want -5.0", got.HYGSPYDivergence.SPYChangePct)
	}
}

func TestOpportunityBacktestSampleProducesMarketOutcomeMetrics(t *testing.T) {
	t.Parallel()
	rows := readOpportunityBacktestFixture(t)
	res := runOpportunityBacktest(rows, time.Date(2026, 5, 31, 12, 5, 0, 0, time.UTC))

	if got, want := res.Metrics.Observations, 8; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TargetOpportunity, 5; got != want {
		t.Fatalf("target_opportunity = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalFired, 6; got != want {
		t.Fatalf("signal_fired = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TruePositive, 4; got != want {
		t.Fatalf("true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.FalsePositive, 2; got != want {
		t.Fatalf("false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.Miss, 1; got != want {
		t.Fatalf("miss = %d, want %d", got, want)
	}
	if got, want := res.Metrics.PositiveExcess, 4; got != want {
		t.Fatalf("positive_excess = %d, want %d", got, want)
	}
	if res.Metrics.Recall == nil || *res.Metrics.Recall != 0.8 {
		t.Fatalf("recall = %v, want 0.8", res.Metrics.Recall)
	}
	if res.Metrics.AvgExcessReturnPct == nil || math.Abs(*res.Metrics.AvgExcessReturnPct-64.25) > 0.01 {
		t.Fatalf("avg_excess_return_pct = %v, want about 64.25", res.Metrics.AvgExcessReturnPct)
	}
	findings := strings.Join(res.Findings, "\n")
	for _, want := range []string{
		"missed 1 labelled opportunity",
		"fired on 2 non-opportunity",
		"4/6 fired signal row(s) had positive excess return",
	} {
		if !strings.Contains(findings, want) {
			t.Fatalf("findings missing %q: %+v", want, res.Findings)
		}
	}
}

func TestRunBacktestOpportunityRendersText(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"opportunity", "--input", opportunityBacktestFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Opportunity Backtest",
		"8 observations",
		"Signal       precision 67%",
		"Outcome      hit 67%",
		"AI infrastructure opportunity",
		"AI mega-cap fragility",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("opportunity backtest output missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func readBacktestFixture(t *testing.T) []CanaryBacktestObservation {
	t.Helper()
	f, err := os.Open(backtestFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readCanaryBacktestObservations(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func readRegimeBacktestFixture(t *testing.T) []RegimeBacktestObservation {
	t.Helper()
	f, err := os.Open(regimeBacktestFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readRegimeBacktestObservations(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func readOpportunityBacktestFixture(t *testing.T) []OpportunityBacktestObservation {
	t.Helper()
	f, err := os.Open(opportunityBacktestFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readOpportunityBacktestObservations(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func backtestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "canary_backtest_sample.jsonl")
}

func regimeBacktestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "regime_backtest_sample.jsonl")
}

func regimePointInTimeFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "regime_pit_panel_sample.jsonl")
}

func opportunityBacktestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "opportunity_backtest_sample.jsonl")
}

func clusterHasRebalanceWatch(res CanaryBacktestResult, name string) bool {
	for _, cluster := range res.Clusters {
		if cluster.Name == name && cluster.Metrics.RebalanceWatch > 0 {
			return true
		}
	}
	return false
}
