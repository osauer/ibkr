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
	if got, want := res.Metrics.WatchTruePositive, 5; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchMiss, 0; got != want {
		t.Fatalf("watch_miss = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchFalsePositive, 2; got != want {
		t.Fatalf("watch_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.ActTruePositive, 3; got != want {
		t.Fatalf("act_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.DataQualityWatch, 1; got != want {
		t.Fatalf("data_quality_watch = %d, want %d", got, want)
	}
	if res.Metrics.WatchRecall == nil || *res.Metrics.WatchRecall != 1 {
		t.Fatalf("watch_recall = %v, want 1", res.Metrics.WatchRecall)
	}
	if !strings.Contains(strings.Join(res.Findings, "\n"), "Watch-level defensive alerts caught every labelled stress row") {
		t.Fatalf("findings did not record watch-level coverage: %+v", res.Findings)
	}
	if !clusterHasWatchFalsePositive(res, "2023-2026 AI mega-cap concentration") {
		t.Fatalf("AI concentration cluster should expose a watch false positive: %+v", res.Clusters)
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
		"2024 yen carry unwind",
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

func backtestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "canary_backtest_sample.jsonl")
}

func clusterHasWatchFalsePositive(res CanaryBacktestResult, name string) bool {
	for _, cluster := range res.Clusters {
		if cluster.Name == name && cluster.Metrics.WatchFalsePositive > 0 {
			return true
		}
	}
	return false
}
