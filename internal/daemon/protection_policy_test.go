package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestProtectionPolicyDefaultDriftAndHigherVersion(t *testing.T) {
	now := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	pm := newProtectionPolicyManager("", true, time.Second, func() time.Time { return now })
	pm.reload()
	_, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusDefault {
		t.Fatalf("default status=%q, want default", st.Status)
	}

	path := filepath.Join(t.TempDir(), "policy.toml")
	pm.path = path
	writePolicy(t, path, 1, 6)
	pm.reload()
	_, st = pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusDrift {
		t.Fatalf("same-version changed policy status=%q, want drift", st.Status)
	}

	writePolicy(t, path, 2, 6)
	pm.reload()
	p, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusActive || p.PolicyVersion != 2 {
		t.Fatalf("higher version status=%q version=%d, want active v2", st.Status, p.PolicyVersion)
	}
}

func TestProtectionPolicyInvalidHigherVersionBlocksWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.toml")
	writePolicy(t, path, 2, -1)
	pm := newProtectionPolicyManager(path, true, time.Second, time.Now)
	pm.reload()
	_, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusError {
		t.Fatalf("invalid policy status=%q, want error", st.Status)
	}
	if len(st.Blockers) == 0 {
		t.Fatal("invalid policy should expose blockers")
	}
}

func writePolicy(t *testing.T, path string, version int, theta float64) {
	t.Helper()
	body := []byte(`kind = "ibkr.protection_policy"
schema_version = 1
policy_id = "protection-mvp"
policy_version = ` + strconv.Itoa(version) + `
profile = "theta-priority-mvp"

[authority]
paper_only = true
close_reduce_only = true
auto_submit = false

[buckets.theta_hygiene]
enabled = true
max_dte = 21
min_abs_theta_per_day = ` + strconv.FormatFloat(theta, 'f', 1, 64) + `
max_spread_pct_of_mid = 25.0

[buckets.risk_reduction]
enabled = true
single_name_target_pct_nlv = 25.0
max_order_notional = 10000.0
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
