package cli

import (
	"bytes"
	"context"
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

	if got, want := res.Metrics.Observations, 10; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TargetStress, 5; got != want {
		t.Fatalf("target_stress = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalTruePositive, 5; got != want {
		t.Fatalf("signal_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalFalsePositive, 2; got != want {
		t.Fatalf("signal_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchTruePositive, 4; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchMiss, 1; got != want {
		t.Fatalf("watch_miss = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchFalsePositive, 0; got != want {
		t.Fatalf("watch_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.ActTruePositive, 4; got != want {
		t.Fatalf("act_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.RebalanceWatch, 3; got != want {
		t.Fatalf("rebalance_watch = %d, want %d", got, want)
	}
	if got, want := res.Metrics.DataQualityWatch, 1; got != want {
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
		"10 observations",
		"precision 71%",
		"Defensive    precision 100%",
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

	if got, want := res.Metrics.Observations, 10; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.ScoredObservations, 9; got != want {
		t.Fatalf("scored_observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.OutOfScope, 1; got != want {
		t.Fatalf("out_of_scope = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TargetStress, 5; got != want {
		t.Fatalf("target_stress = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchTruePositive, 5; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchFalsePositive, 1; got != want {
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
	findings := strings.Join(res.Findings, "\n")
	for _, want := range []string{
		"out-of-scope row(s) were excluded",
		"Regime watch caught every scored market-stress row",
		"Regime watch fired on 1 scored non-stress row",
	} {
		if !strings.Contains(findings, want) {
			t.Fatalf("findings missing %q: %+v", want, res.Findings)
		}
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
		"10 observations",
		"Watch        precision 83%",
		"Stress       precision 100%",
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

func backtestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "canary_backtest_sample.jsonl")
}

func regimeBacktestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "regime_backtest_sample.jsonl")
}

func clusterHasRebalanceWatch(res CanaryBacktestResult, name string) bool {
	for _, cluster := range res.Clusters {
		if cluster.Name == name && cluster.Metrics.RebalanceWatch > 0 {
			return true
		}
	}
	return false
}
