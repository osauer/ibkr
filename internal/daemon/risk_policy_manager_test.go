package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

const validRiskPolicyTOML = `
kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 1

[capital]
base_currency = "EUR"
protected_floor = 200000.0
declared_risk_capital = 50000.0
max_equity_age_minutes = 240
max_unreconciled_days = 7

[drawdown]
warn_consumed_pct = 15.0
block_consumed_pct = 30.0
block_enforcement = "shadow"

[override]
max_duration_hours = 24

[cadence.morning]
class = "advisory"
`

func newTestRiskPolicyManager(t *testing.T, contents string) (*riskPolicyManager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "risk-policy.toml")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := newRiskPolicyManager(path, time.Second, time.Now)
	m.reload()
	return m, path
}

// The constitution has no embedded default: a missing file is a disclosed
// absent state with a nil policy, never silent code values.
func TestRiskPolicyManagerAbsentFile(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, "")
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusAbsent {
		t.Fatalf("status = %s, want absent", snap.status)
	}
	if snap.policy != nil {
		t.Fatal("absent file must yield a nil policy, not a default")
	}
}

func TestRiskPolicyManagerLoadsValidFile(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, validRiskPolicyTOML)
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusActive {
		t.Fatalf("status = %s (%s), want active", snap.status, snap.message)
	}
	if snap.policy == nil || snap.policy.PolicyID != "risk-constitution" {
		t.Fatalf("policy = %+v, want risk-constitution", snap.policy)
	}
	if got := snap.policy.UnapprovedKeys(); len(got) != 0 {
		t.Fatalf("fully specified file reports unapproved keys: %v", got)
	}
}

func TestRiskPolicyManagerRejectsUnknownKeys(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, validRiskPolicyTOML+"\nsurprise_key = true\n")
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusError {
		t.Fatalf("status = %s, want error", snap.status)
	}
	if !strings.Contains(snap.message, "unknown risk policy key") {
		t.Fatalf("message = %q, want unknown-key error", snap.message)
	}
}

func TestRiskPolicyManagerRejectsHardEnforcement(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, strings.Replace(validRiskPolicyTOML, `"shadow"`, `"hard"`, 1))
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusError || !strings.Contains(snap.message, "not promotable") {
		t.Fatalf("status = %s message = %q, want error/not promotable", snap.status, snap.message)
	}
}

// Editing the file without bumping policy_version is drift: the last good
// policy stays active and the change is refused until a version bump.
func TestRiskPolicyManagerDriftWithoutVersionBump(t *testing.T) {
	m, path := newTestRiskPolicyManager(t, validRiskPolicyTOML)
	edited := strings.Replace(validRiskPolicyTOML, "declared_risk_capital = 50000.0", "declared_risk_capital = 90000.0", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	m.reload()
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusDrift {
		t.Fatalf("status = %s, want drift", snap.status)
	}
	if got := *snap.policy.Capital.DeclaredRiskCapital; got != 50000 {
		t.Fatalf("active declared = %v, drifted file must not take effect", got)
	}

	bumped := strings.Replace(edited, "policy_version = 1", "policy_version = 2", 1)
	if err := os.WriteFile(path, []byte(bumped), 0o600); err != nil {
		t.Fatal(err)
	}
	m.reload()
	snap = m.snapshot()
	if snap.status != rpc.RiskPolicyStatusActive || *snap.policy.Capital.DeclaredRiskCapital != 90000 {
		t.Fatalf("after version bump: status = %s declared = %v, want active/90000", snap.status, *snap.policy.Capital.DeclaredRiskCapital)
	}
}

// A parse error keeps the last good policy active and discloses the error.
func TestRiskPolicyManagerKeepsLastGoodOnError(t *testing.T) {
	m, path := newTestRiskPolicyManager(t, validRiskPolicyTOML)
	if err := os.WriteFile(path, []byte("kind = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	m.reload()
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusError {
		t.Fatalf("status = %s, want error", snap.status)
	}
	if snap.policy == nil || snap.policy.PolicyID != "risk-constitution" {
		t.Fatal("last good policy must stay active through a parse error")
	}
	if !strings.Contains(snap.message, "last good policy stays active") {
		t.Fatalf("message %q must disclose last-good retention", snap.message)
	}
}
