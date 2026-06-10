package daemon

import (
	"os"
	"path/filepath"
	"strings"
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

// newTestPaperSmokeSigner builds an orderTokenSigner with a fresh key in a
// temp dir, the evidence-MAC dependency of the readiness store.
func newTestPaperSmokeSigner(t *testing.T) *orderTokenSigner {
	t.Helper()
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), nil)
	if err != nil {
		t.Fatalf("newOrderTokenSigner: %v", err)
	}
	return signer
}

func TestTradingReadinessSavePaperSmokeWritesPrivateState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "trading-readiness.json")
	store := newTradingReadinessStore(path, newTestPaperSmokeSigner(t))
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
	store := newTradingReadinessStore(filepath.Join(t.TempDir(), "trading-readiness.json"), newTestPaperSmokeSigner(t))
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

func TestTradingReadinessRejectsTamperedEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "trading-readiness.json")
	store := newTradingReadinessStore(path, newTestPaperSmokeSigner(t))
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

	// Hand-edit a field: the MAC no longer matches.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	tampered := strings.ReplaceAll(string(raw), "DU1234567", "DU7654321")
	if tampered == string(raw) {
		t.Fatal("tamper substitution did not apply")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write tampered: %v", err)
	}
	check := store.CheckPaperSmoke("U7654321", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusUnsigned {
		t.Fatalf("status = %q, want unsigned for tampered evidence", check.Status)
	}
}

func TestTradingReadinessRejectsHandWrittenEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "trading-readiness.json")
	// The pre-MAC era de-facto live switch: a hand-written file with no MAC.
	handWritten := `{"version":1,"paper_smoke":{"account":"DU1234567","endpoint":"127.0.0.1:4002","endpoint_class":"paper","client_id":31,"version":"test-version","result":"passed","at":"2026-05-28T06:00:00Z"}}`
	if err := os.WriteFile(path, []byte(handWritten), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := newTradingReadinessStore(path, newTestPaperSmokeSigner(t))
	check := store.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusUnsigned {
		t.Fatalf("status = %q, want unsigned for hand-written evidence", check.Status)
	}
	if check.Action == "" || check.Message == "" {
		t.Fatalf("unsigned check must carry message+action: %+v", check)
	}
}

func TestTradingReadinessNilSignerFailsClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "trading-readiness.json")
	signed := newTradingReadinessStore(path, newTestPaperSmokeSigner(t))
	if err := signed.SavePaperSmoke(tradingPaperSmokeEvidence{
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

	unsignedStore := newTradingReadinessStore(path, nil)
	if err := unsignedStore.SavePaperSmoke(tradingPaperSmokeEvidence{}); err == nil {
		t.Fatal("SavePaperSmoke with nil signer must error")
	}
	check := unsignedStore.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusUnsigned {
		t.Fatalf("status = %q, want unsigned with nil signer", check.Status)
	}
}

func TestPaperSmokeMACSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	signer := newTestPaperSmokeSigner(t)
	ev := tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            time.Date(2026, 5, 28, 6, 0, 0, 123456789, time.UTC),
	}
	mac, err := signer.signPaperSmoke(ev)
	if err != nil || mac == "" {
		t.Fatalf("signPaperSmoke: %q, %v", mac, err)
	}
	if !signer.verifyPaperSmoke(ev, mac) {
		t.Fatal("verifyPaperSmoke must accept its own MAC")
	}
	if signer.verifyPaperSmoke(ev, "") {
		t.Fatal("empty MAC must not verify")
	}
	other := ev
	other.Result = tradingPaperSmokeResultFailed
	if signer.verifyPaperSmoke(other, mac) {
		t.Fatal("MAC must not transfer to altered evidence")
	}
	otherKey := newTestPaperSmokeSigner(t)
	if otherKey.verifyPaperSmoke(ev, mac) {
		t.Fatal("MAC must not verify under a different key")
	}
}
