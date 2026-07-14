package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// writeFlexFixture drops one raw statement into the retained-statements
// dir, the same way the fetcher would.
func writeFlexFixture(t *testing.T, name, whenGenerated, from, to, body string) {
	t.Helper()
	dir, err := flexStatementsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`<FlexQueryResponse queryName="recon" type="AF">
 <FlexStatements count="1">
  <FlexStatement accountId="U1234567" fromDate="%s" toDate="%s" whenGenerated="%s">
%s
  </FlexStatement>
 </FlexStatements>
</FlexQueryResponse>`, from, to, whenGenerated, body)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cashLine(id, typ string, amount float64, date string) string {
	return fmt.Sprintf(`   <CashTransactions><CashTransaction transactionID=%q type=%q currency="EUR" fxRateToBase="1" amount="%f" dateTime="%s;120000" settleDate=%q description="FIXTURE" /></CashTransactions>`, id, typ, amount, date, date)
}

// newReconTestServer builds a server with an active policy (recon keys
// approved), an isolated state dir, and no gateway.
func newReconTestServer(t *testing.T) *Server {
	t.Helper()
	return newRiskPolicyTestServer(t, validRiskPolicyTOML)
}

func declare(t *testing.T, s *Server, typ string, amount float64, effectiveAt string) {
	t.Helper()
	var eff time.Time
	if effectiveAt != "" {
		var err error
		if eff, err = time.Parse("2006-01-02", effectiveAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.riskCapital.ApplyCapitalEvent(rpc.CapitalEventParams{Type: typ, AmountBase: amount, EffectiveAt: eff}, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
}

func recentGenerated() string { return time.Now().UTC().Format("20060102") + ";060000" }

func TestReconMatchAndCategories(t *testing.T) {
	s := newReconTestServer(t)
	gen := recentGenerated()

	// Matched deposit; missing withdrawal; amount mismatch; date mismatch;
	// ledger-only event; unknown line type.
	writeFlexFixture(t, "flex-20260710-000001.xml", gen, "20260706", "20260712",
		cashLine("m1", "Deposits/Withdrawals", 20000, "20260707")+"\n"+
			cashLine("w1", "Deposits/Withdrawals", -9250, "20260708")+"\n"+
			cashLine("a1", "Deposits/Withdrawals", 5000, "20260709")+"\n"+
			cashLine("d1", "Deposits/Withdrawals", 3000, "20260701")+"\n"+
			cashLine("u1", "Some Future Line Type", 12.34, "20260710"))
	declare(t, s, "deposit", 20000, "2026-07-07")  // matches m1
	declare(t, s, "deposit", 5300, "2026-07-09")   // amount mismatch vs a1 (300 > tol)
	declare(t, s, "deposit", 3000, "2026-07-08")   // 5 business days from d1 → date mismatch
	declare(t, s, "withdrawal", 700, "2026-06-15") // ledger-only

	rep := s.buildReconReport()
	if rep.Status != rpc.ReconStatusActive {
		t.Fatalf("status = %s (%s)", rep.Status, rep.Message)
	}
	want := map[string]int{
		"matched":                  1,
		rpc.ReconMissingFromLedger: 1, // w1
		rpc.ReconAmountMismatch:    1, // a1
		rpc.ReconDateMismatch:      1, // d1
		rpc.ReconLedgerOnly:        1,
		rpc.ReconUncategorized:     1, // u1
	}
	for cat, n := range want {
		if rep.Counts[cat] != n {
			t.Fatalf("count[%s] = %d, want %d (all: %v)", cat, rep.Counts[cat], n, rep.Counts)
		}
	}
	if rep.Unresolved != 5 {
		t.Fatalf("unresolved = %d, want 5", rep.Unresolved)
	}
	if !strings.HasPrefix(rep.ReportID, "recon-") {
		t.Fatalf("report id = %q", rep.ReportID)
	}
	// Deterministic: same inputs, same id.
	if again := s.buildReconReport(); again.ReportID != rep.ReportID {
		t.Fatalf("report id not deterministic: %s vs %s", again.ReportID, rep.ReportID)
	}
}

// Two declared events qualifying for one statement line is ambiguous —
// never a best-effort pick (never-false-match).
func TestReconAmbiguityNeverAutoResolves(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-20260710-000001.xml", recentGenerated(), "20260706", "20260712",
		cashLine("amb1", "Deposits/Withdrawals", 10000, "20260708"))
	declare(t, s, "deposit", 10000, "2026-07-08")
	declare(t, s, "deposit", 10001, "2026-07-09")

	rep := s.buildReconReport()
	if rep.Counts[rpc.ReconAmbiguous] != 1 {
		t.Fatalf("counts = %v, want one ambiguous", rep.Counts)
	}
	if rep.Counts["matched"] != 0 {
		t.Fatalf("ambiguous line must not also match (counts %v)", rep.Counts)
	}
}

// Restatement: the same line id in a newer file supersedes the older copy.
func TestReconRestatementSupersedesByLineID(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-20260709-000001.xml", recentGenerated(), "20260706", "20260709",
		cashLine("r1", "Deposits/Withdrawals", 9999, "20260708"))
	writeFlexFixture(t, "flex-20260710-000002.xml", recentGenerated(), "20260706", "20260710",
		cashLine("r1", "Deposits/Withdrawals", 10000, "20260708"))
	declare(t, s, "deposit", 10000, "2026-07-08")

	rep := s.buildReconReport()
	if rep.Counts["matched"] != 1 || rep.Unresolved != 0 {
		t.Fatalf("restated line must match once cleanly: counts %v unresolved %d", rep.Counts, rep.Unresolved)
	}
}

func TestReconDismissResolvesAndChangesReportID(t *testing.T) {
	s := newReconTestServer(t)
	ctx := context.Background()
	writeFlexFixture(t, "flex-20260710-000001.xml", recentGenerated(), "20260706", "20260712",
		cashLine("x1", "Deposits/Withdrawals", -500, "20260708"))

	before := s.buildReconReport()
	if before.Unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1", before.Unresolved)
	}

	// Agent origins cannot dismiss.
	if _, err := s.handleReconDismiss(ctx, rawParams(t, rpc.ReconDismissParams{LineID: "cash-x1", Reason: "r", Origin: rpc.OrderOriginAgent})); err == nil {
		t.Fatal("agent dismiss must be rejected")
	}
	// Unknown lines cannot be dismissed.
	if _, err := s.handleReconDismiss(ctx, rawParams(t, rpc.ReconDismissParams{LineID: "cash-nope", Reason: "r", Origin: rpc.OrderOriginHumanTTY})); err == nil {
		t.Fatal("dismissing a non-exception must fail")
	}
	if _, err := s.handleReconDismiss(ctx, rawParams(t, rpc.ReconDismissParams{LineID: "cash-x1", Reason: "bank fee reversal, not a flow", Origin: rpc.OrderOriginHumanTTY})); err != nil {
		t.Fatal(err)
	}

	after := s.buildReconReport()
	if after.Unresolved != 0 {
		t.Fatalf("unresolved after dismiss = %d, want 0", after.Unresolved)
	}
	if len(after.Exceptions) != 1 || !after.Exceptions[0].Dismissed || after.Exceptions[0].DismissReason == "" {
		t.Fatalf("exception = %+v, want dismissed with reason", after.Exceptions[0])
	}
	if after.ReportID == before.ReportID {
		t.Fatal("report id must change when the exception set changes")
	}
	// Double dismissal is refused.
	if _, err := s.handleReconDismiss(ctx, rawParams(t, rpc.ReconDismissParams{LineID: "cash-x1", Reason: "again", Origin: rpc.OrderOriginHumanTTY})); err == nil {
		t.Fatal("double dismiss must fail")
	}
}

func TestReconUnapprovedAndUnavailable(t *testing.T) {
	// Policy without [recon] keys: unapproved, and reconcile is refused.
	minimal := `
kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 1
`
	s := newRiskPolicyTestServer(t, minimal)
	rep := s.buildReconReport()
	if rep.Status != rpc.ReconStatusUnapproved {
		t.Fatalf("status = %s, want unapproved", rep.Status)
	}
	if err := s.reconcileReportGate("recon-whatever"); err == nil || !strings.Contains(err.Error(), "unapproved") {
		t.Fatalf("gate err = %v, want unapproved refusal", err)
	}

	// Approved keys but no statements: unavailable, reconcile refused.
	s2 := newReconTestServer(t)
	rep2 := s2.buildReconReport()
	if rep2.Status != rpc.ReconStatusUnavailable {
		t.Fatalf("status = %s, want unavailable", rep2.Status)
	}
	if err := s2.reconcileReportGate("recon-whatever"); err == nil || !strings.Contains(err.Error(), "no recon report") {
		t.Fatalf("gate err = %v, want unavailable refusal", err)
	}
}

func TestReconcileReportGate(t *testing.T) {
	s := newReconTestServer(t)
	ctx := context.Background()
	writeFlexFixture(t, "flex-20260710-000001.xml", recentGenerated(), "20260706", "20260712",
		cashLine("g1", "Deposits/Withdrawals", 1000, "20260708"))
	declare(t, s, "deposit", 1000, "2026-07-08")

	rep := s.buildReconReport()
	if rep.Unresolved != 0 {
		t.Fatalf("fixture not clean: %+v", rep.Counts)
	}

	// Bare attestation is retired.
	if err := s.reconcileReportGate(""); err == nil || !strings.Contains(err.Error(), "requires --report") {
		t.Fatalf("err = %v, want report requirement", err)
	}
	// Superseded/wrong ids are refused, and the error names the current one.
	if err := s.reconcileReportGate("recon-stale123"); err == nil || !strings.Contains(err.Error(), rep.ReportID) {
		t.Fatalf("err = %v, want supersession pointing at %s", err, rep.ReportID)
	}
	// The real id passes end to end through the handler.
	if _, err := s.handleRiskPolicyCapitalEvent(ctx, rawParams(t, rpc.CapitalEventParams{
		Type: "reconcile", Report: rep.ReportID, Origin: rpc.OrderOriginHumanTTY,
	})); err != nil {
		t.Fatal(err)
	}

	// Unresolved exceptions block sign-off.
	writeFlexFixture(t, "flex-20260711-000002.xml", recentGenerated(), "20260706", "20260712",
		cashLine("g2", "Deposits/Withdrawals", -400, "20260709"))
	rep2 := s.buildReconReport()
	if err := s.reconcileReportGate(rep2.ReportID); err == nil || !strings.Contains(err.Error(), "unresolved exception") {
		t.Fatalf("err = %v, want unresolved refusal", err)
	}
}

// A report whose newest statement is older than recon.max_report_age_days
// cannot back a sign-off — stale truth is not truth.
func TestReconcileReportGateRejectsStaleStatements(t *testing.T) {
	s := newReconTestServer(t)
	old := time.Now().UTC().AddDate(0, 0, -10).Format("20060102") + ";060000"
	writeFlexFixture(t, "flex-old.xml", old, "20260620", "20260626",
		cashLine("s1", "Deposits/Withdrawals", 1000, "20260624"))
	declare(t, s, "deposit", 1000, "2026-06-24")

	rep := s.buildReconReport()
	if rep.Unresolved != 0 {
		t.Fatalf("fixture not clean: %+v", rep.Counts)
	}
	if err := s.reconcileReportGate(rep.ReportID); err == nil || !strings.Contains(err.Error(), "max_report_age_days") {
		t.Fatalf("err = %v, want staleness refusal", err)
	}
}

func TestBusinessDaysApart(t *testing.T) {
	d := func(s string) time.Time {
		v, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2026-07-08", "2026-07-08", 0}, // same day
		{"2026-07-08", "2026-07-09", 1}, // Wed→Thu
		{"2026-07-10", "2026-07-13", 1}, // Fri→Mon skips the weekend
		{"2026-07-08", "2026-07-15", 5}, // Wed→Wed
		{"2026-07-01", "2026-07-08", 5}, // symmetric order
	} {
		if got := businessDaysApart(d(tc.a), d(tc.b)); got != tc.want {
			t.Fatalf("businessDaysApart(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// The sanitized transport error can never leak a query string (the token
// travels in POST bodies, but belt and suspenders for proxy-style errors).
func TestSanitizeFlexTransportError(t *testing.T) {
	err := fmt.Errorf(`Post "https://gdcdyn.interactivebrokers.com/Universal/servlet/FlexStatementService.SendRequest?t=SECRET&q=1": dial tcp: timeout`)
	got := sanitizeFlexTransportError(err, flexSendRequestURL)
	if strings.Contains(got, "SECRET") {
		t.Fatalf("sanitized error leaks credentials: %q", got)
	}
}
