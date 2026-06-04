package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTradingReadinessDefaultPathUsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/ibkr-state")

	got, err := defaultTradingReadinessPath()
	if err != nil {
		t.Fatalf("defaultTradingReadinessPath: %v", err)
	}
	want := filepath.Join("/tmp/ibkr-state", "ibkr", "trading-readiness.json")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestTradingReadinessSavePaperSmokeWritesPrivateState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "trading-readiness.json")
	store := newTradingReadinessStore(path)
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)

	err := store.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            now,
	})
	if err != nil {
		t.Fatalf("SavePaperSmoke: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat readiness file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PaperSmoke == nil || got.PaperSmoke.Account != "DU1234567" || !got.PaperSmoke.At.Equal(now) {
		t.Fatalf("loaded paper smoke = %+v", got.PaperSmoke)
	}
}

func TestTradingReadinessCheckPaperSmoke(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	store := newTradingReadinessStore(filepath.Join(t.TempDir(), "trading-readiness.json"))
	if err := store.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SavePaperSmoke: %v", err)
	}

	check := store.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusValid {
		t.Fatalf("status = %q, want valid: %+v", check.Status, check)
	}

	check = store.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 32, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusMismatch {
		t.Fatalf("status = %q, want mismatch", check.Status)
	}

	check = store.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", time.Minute, now)
	if check.Status != tradingPaperSmokeStatusStale {
		t.Fatalf("status = %q, want stale", check.Status)
	}
}
