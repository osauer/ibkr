package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func newRiskPolicyTestServer(t *testing.T, policyTOML string) *Server {
	t.Helper()
	m, _ := newTestRiskPolicyManager(t, policyTOML)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Server{
		now:          time.Now,
		riskPolicies: m,
		riskCapital:  &riskCapitalStore{now: time.Now},
	}
	s.installBriefStateStore()
	return s
}

func rawParams(t *testing.T, v any) *rpc.Request {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return &rpc.Request{Params: raw}
}

// Every risk-policy write is human-only: governance acts, not broker
// writes, and no agent session may perform them in any trading mode.
func TestRiskPolicyWritesRejectAgentOrigin(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyTOML)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(origin string) error
	}{
		{"capital_event", func(origin string) error {
			_, err := s.handleRiskPolicyCapitalEvent(ctx, rawParams(t, rpc.CapitalEventParams{Type: "deposit", AmountBase: 100, Origin: origin}))
			return err
		}},
		{"override", func(origin string) error {
			_, err := s.handleRiskPolicyOverride(ctx, rawParams(t, rpc.OverrideParams{Control: "drawdown.warn_consumed_pct", Reason: "r", Hours: 1, Origin: origin}))
			return err
		}},
		{"reset_drawdown", func(origin string) error {
			_, err := s.handleRiskPolicyResetDrawdown(ctx, rawParams(t, rpc.ResetDrawdownParams{Reason: "r", Origin: origin}))
			return err
		}},
		{"artefact", func(origin string) error {
			_, err := s.handleRiskPolicyArtefact(ctx, rawParams(t, rpc.ArtefactParams{Artefact: "morning", Origin: origin}))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, origin := range []string{rpc.OrderOriginAgent, "", "made-up-origin"} {
				if err := tc.call(origin); err == nil || !strings.Contains(err.Error(), "human-only") {
					t.Fatalf("origin %q: err = %v, want human-only rejection", origin, err)
				}
			}
			if err := tc.call(rpc.OrderOriginHumanTTY); err != nil {
				t.Fatalf("human origin: err = %v, want success", err)
			}
		})
	}
}

func TestRiskPolicySnapshotDisclosesAbsentPolicy(t *testing.T) {
	s := newRiskPolicyTestServer(t, "")
	res, err := s.handleRiskPolicySnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != rpc.RiskPolicyStatusAbsent {
		t.Fatalf("status = %s, want absent", res.Status)
	}
	if res.Capital.Tier != risk.CapitalTierUnapproved {
		t.Fatalf("tier = %s, want unapproved (no file, no numbers)", res.Capital.Tier)
	}
	if len(res.Unapproved) == 0 {
		t.Fatal("absent policy must enumerate every material key as unapproved")
	}
	if len(res.Limits) == 0 {
		t.Fatal("the explain view must render even with no policy file")
	}
	for _, l := range res.Limits {
		if l.Key == "capital.protected_floor" && l.Source != "unapproved" {
			t.Fatalf("floor source = %s, want unapproved", l.Source)
		}
	}
}

// CLI JSON is a straight marshal of this result (printJSON), so the RPC
// struct is the cross-surface parity contract: assert the load-bearing
// fields serialize under their documented names.
func TestRiskPolicySnapshotWireContract(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyTOML)
	res, err := s.handleRiskPolicySnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"as_of", "status", "policy_id", "policy_version", "policy_fingerprint", "capital", "limits", "inventory", "input_health"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("wire payload missing %q", key)
		}
	}
	fp, _ := decoded["policy_fingerprint"].(map[string]any)
	if fp["version"] != rpc.RiskConstitutionFingerprintVersion {
		t.Fatalf("fingerprint version = %v, want %s", fp["version"], rpc.RiskConstitutionFingerprintVersion)
	}
	capital, _ := decoded["capital"].(map[string]any)
	if capital["tier"] != risk.CapitalTierUnknown {
		// no equity observation exists in this test server; unknown, never ok
		t.Fatalf("tier = %v, want unknown without an observation", capital["tier"])
	}
	if capital["flow_source"] != rpc.CapitalFlowSourceDeclared {
		t.Fatalf("v2 flow source = %v", capital["flow_source"])
	}
	if _, ok := capital["declared_cum_flows_base"]; !ok {
		t.Fatal("v2 capital missing declared_cum_flows_base")
	}
	if _, ok := capital["statement_cum_flows_base"]; ok {
		t.Fatal("v2 capital unexpectedly includes statement_cum_flows_base")
	}
}

func TestCapitalStateReportV3DualComputeWire(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyV3TOML())
	now := time.Now()
	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 250000
	s.riskCapital.state.LastEquityBase = 250000
	s.riskCapital.state.LastEquityAsOf = now
	s.riskCapital.state.StatementFlowsBase = 900
	s.riskCapital.state.StatementCoverageTo = now
	s.riskCapital.cumFlowsBase = 1000
	s.riskCapital.lastReconciledAt = now
	s.riskCapital.mu.Unlock()
	rep := s.riskCapital.Report(s.riskPolicies.snapshot().policy, nil)
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["flow_source"] != rpc.CapitalFlowSourceStatement || decoded["declared_cum_flows_base"] != 1000.0 || decoded["statement_cum_flows_base"] != 900.0 {
		t.Fatalf("v3 wire = %v", decoded)
	}
}

func TestRiskPolicyPreviewWarnings(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyTOML)
	c := s.riskPolicies.snapshot().policy
	now := time.Now()
	if _, err := s.riskCapital.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
	s.riskCapital.Observe(260000, now.Add(-2*time.Minute), c)
	s.riskCapital.Observe(250000, now.Add(-time.Minute), c) // 20% consumed → warn

	stockBuy := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "MSFT", SecType: "STK"}}
	open := rpc.OrderPositionImpact{Effect: "open"}

	warns := s.riskPolicyPreviewWarnings(stockBuy, open)
	if len(warns) != 1 || warns[0].Code != "capital_drawdown" || warns[0].Severity != "watch" {
		t.Fatalf("warnings = %+v, want one capital_drawdown/watch", warns)
	}
	if warns[0].Scope != "risk_policy" {
		t.Fatalf("scope = %s, want risk_policy", warns[0].Scope)
	}

	// Risk-reducing intents never warn (decision 4 exemptions).
	for _, effect := range []string{"reduce", "close"} {
		if got := s.riskPolicyPreviewWarnings(stockBuy, rpc.OrderPositionImpact{Effect: effect}); len(got) != 0 {
			t.Fatalf("effect %s: warnings = %+v, want none", effect, got)
		}
	}
	// Hedge entries stay available: long put on a rulebook hedge index.
	hedge := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "SPY", SecType: "OPT", Right: "P"}}
	if got := s.riskPolicyPreviewWarnings(hedge, open); len(got) != 0 {
		t.Fatalf("hedge entry: warnings = %+v, want none", got)
	}

	// Block tier escalates the severity word to act — no ninth word.
	s.riskCapital.Observe(240000, now, c) // 40% consumed → block + latch
	warns = s.riskPolicyPreviewWarnings(stockBuy, open)
	if len(warns) != 1 || warns[0].Severity != "act" {
		t.Fatalf("warnings = %+v, want capital_drawdown/act", warns)
	}
	if !strings.Contains(warns[0].Impact, "submit eligibility is unaffected") {
		t.Fatalf("impact %q must state submit eligibility is untouched", warns[0].Impact)
	}

	// No policy file → no preview noise; policy show owns the disclosure.
	absent := newRiskPolicyTestServer(t, "")
	if got := absent.riskPolicyPreviewWarnings(stockBuy, open); len(got) != 0 {
		t.Fatalf("absent policy: warnings = %+v, want none", got)
	}
}
