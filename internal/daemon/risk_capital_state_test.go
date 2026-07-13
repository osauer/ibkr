package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func testConstitution() *risk.Constitution {
	return &risk.Constitution{
		Kind:          risk.ConstitutionKind,
		SchemaVersion: 1,
		PolicyID:      "risk-constitution",
		PolicyVersion: 1,
		Capital: risk.ConstitutionCapital{
			BaseCurrency:        "EUR",
			ProtectedFloor:      new(200000.0),
			DeclaredRiskCapital: new(50000.0),
			MaxEquityAgeMinutes: new(240),
			MaxUnreconciledDays: new(7),
		},
		Drawdown: risk.ConstitutionDrawdown{
			WarnConsumedPct:  new(15.0),
			BlockConsumedPct: new(30.0),
		},
		Override: risk.ConstitutionOverride{MaxDurationHours: new(24)},
		Cadence: risk.ConstitutionCadence{
			Morning: risk.ConstitutionArtefact{Class: risk.EnforcementAdvisory},
		},
	}
}

func newTestRiskCapitalStore(t *testing.T) *riskCapitalStore {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return &riskCapitalStore{now: time.Now}
}

func reconcileNow(t *testing.T, st *riskCapitalStore) {
	t.Helper()
	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
}

func TestRiskCapitalObserveSeedsAndTracksPeak(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-2*time.Minute), c)
	rep := st.Report(c, nil)
	if rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier = %s (%v), want ok", rep.Tier, rep.Reasons)
	}
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 260000 {
		t.Fatalf("peak = %v, want 260000", rep.AdjustedPeakBase)
	}

	st.Observe(252000, now.Add(-time.Minute), c) // −8k = 16% of 50k declared
	rep = st.Report(c, nil)
	if rep.Tier != risk.CapitalTierWarn {
		t.Fatalf("tier = %s, want warn", rep.Tier)
	}
	if rep.BlockLatched {
		t.Fatal("warn tier must not latch")
	}

	st.Observe(258000, now, c) // recovery: warn self-clears (decision 5)
	if rep = st.Report(c, nil); rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier after recovery = %s, want ok (warn is self-clearing)", rep.Tier)
	}
}

// A declared deposit must not read as a new peak, and a declared withdrawal
// must not read as drawdown (cash-flow-adjusted HWM, decision 4).
func TestRiskCapitalExternalFlowsDoNotMoveDrawdown(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-3*time.Minute), c)
	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "deposit", AmountBase: 20000}, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
	st.Observe(280000, now.Add(-2*time.Minute), c)
	rep := st.Report(c, nil)
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 260000 {
		t.Fatalf("peak after deposit = %v, want unchanged 260000", rep.AdjustedPeakBase)
	}
	if rep.Tier != risk.CapitalTierOK || (rep.ConsumedPct != nil && *rep.ConsumedPct != 0) {
		t.Fatalf("tier = %s consumed = %v, deposit must be flow-neutral", rep.Tier, rep.ConsumedPct)
	}

	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "withdrawal", AmountBase: 30000}, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
	st.Observe(250000, now.Add(-time.Minute), c)
	rep = st.Report(c, nil)
	if rep.Tier != risk.CapitalTierOK {
		t.Fatalf("tier after withdrawal = %s (consumed %v), want ok — a withdrawal is not a loss", rep.Tier, rep.ConsumedPct)
	}
}

// A deposit declared late — after the peak already reflected the money —
// corrects the peak downward rather than overstating drawdown forever.
func TestRiskCapitalLateDepositCorrectsPeak(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(280000, now.Add(-time.Hour), c) // peak includes an undeclared 20k deposit
	if _, err := st.ApplyCapitalEvent(rpc.CapitalEventParams{
		Type: "deposit", AmountBase: 20000, EffectiveAt: now.Add(-2 * time.Hour),
	}, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
	rep := st.Report(c, nil)
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 260000 {
		t.Fatalf("peak = %v, want corrected to 260000", rep.AdjustedPeakBase)
	}
}

func TestRiskCapitalBlockLatchPersistsAndResets(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()

	st.Observe(260000, now.Add(-3*time.Minute), c)
	st.Observe(240000, now.Add(-2*time.Minute), c) // −20k = 40% ≥ block 30%
	rep := st.Report(c, nil)
	if rep.Tier != risk.CapitalTierBlock || !rep.BlockLatched {
		t.Fatalf("tier = %s latched = %v, want block/true", rep.Tier, rep.BlockLatched)
	}

	// Mark recovery does not clear the latch (decision 5)…
	st.Observe(262000, now.Add(-time.Minute), c)
	if rep = st.Report(c, nil); rep.Tier != risk.CapitalTierBlock {
		t.Fatalf("tier after recovery = %s, want block (latched)", rep.Tier)
	}

	// …and neither does a daemon restart: a fresh store reads it back.
	st2 := &riskCapitalStore{now: time.Now}
	if rep = st2.Report(c, nil); !rep.BlockLatched {
		t.Fatal("latch must survive a restart via risk-capital-state.json")
	}

	// Reset requires a reason, clears the latch, re-bases the peak.
	if err := st2.ResetDrawdown("", c); err == nil {
		t.Fatal("reset without a reason must fail")
	}
	if err := st2.ResetDrawdown("weekly review 2026-07-12: de-risked, resuming at reduced size", c); err != nil {
		t.Fatal(err)
	}
	rep = st2.Report(c, nil)
	if rep.BlockLatched || rep.Tier == risk.CapitalTierBlock {
		t.Fatalf("after reset: tier = %s latched = %v, want unlatched", rep.Tier, rep.BlockLatched)
	}
	if rep.AdjustedPeakBase == nil || *rep.AdjustedPeakBase != 262000 {
		t.Fatalf("peak after reset = %v, want re-based to last equity 262000", rep.AdjustedPeakBase)
	}
}

// The governance journal must carry the policy fingerprint key — the gap
// rules-decisions.jsonl has — so calibration replay can prove which exact
// policy produced a transition.
func TestRiskCapitalJournalCarriesFingerprint(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()
	reconcileNow(t, st)
	now := time.Now()
	st.Observe(260000, now.Add(-2*time.Minute), c)
	st.Observe(240000, now.Add(-time.Minute), c)

	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sawLatch bool
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("journal line is not JSON: %q", line)
		}
		if entry["kind"] == "drawdown_block_latched" {
			sawLatch = true
			fp, _ := entry["policy_fingerprint"].(string)
			if !strings.HasPrefix(fp, "sha256:") {
				t.Fatalf("latch entry fingerprint = %q, want sha256 key", fp)
			}
		}
	}
	if !sawLatch {
		t.Fatal("no drawdown_block_latched journal entry written")
	}
}

func TestRiskCapitalOverrides(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution()

	if _, err := st.GrantOverride(rpc.OverrideParams{Control: "drawdown.warn_consumed_pct", Reason: "r", Hours: 48}, c); err == nil ||
		!strings.Contains(err.Error(), "exceed override.max_duration_hours") {
		t.Fatalf("over-cap override: err = %v, want duration-cap rejection", err)
	}
	if _, err := st.GrantOverride(rpc.OverrideParams{Control: "trading.freeze", Reason: "r", Hours: 1}, c); err == nil ||
		!strings.Contains(err.Error(), "not a constitution key") {
		t.Fatalf("non-constitution control: err = %v, want rejection (invariants have no keys)", err)
	}
	if _, err := st.GrantOverride(rpc.OverrideParams{Control: "drawdown.warn_consumed_pct", Reason: "r", Hours: 1}, nil); err == nil ||
		!strings.Contains(err.Error(), "unapproved") {
		t.Fatalf("nil policy: err = %v, want unapproved-cap rejection", err)
	}

	rec, err := st.GrantOverride(rpc.OverrideParams{Control: "drawdown.warn_consumed_pct", Reason: "earnings week, accepted elevated warn", Hours: 4}, c)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Active || rec.PolicyFingerprint == "" {
		t.Fatalf("override = %+v, want active with fingerprint", rec)
	}

	// Expiry is automatic: a store whose clock has moved past ExpiresAt
	// prunes and journals the expiry.
	st.now = func() time.Time { return rec.ExpiresAt.Add(time.Minute) }
	list := st.ActiveOverrides()
	if len(list) != 1 || list[0].Active {
		t.Fatalf("overrides = %+v, want one expired record", list)
	}
}

func TestRiskCapitalArtefacts(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	c := testConstitution() // declares morning only

	if _, err := st.RecordArtefact(rpc.ArtefactParams{Artefact: "weekly"}, c); err == nil ||
		!strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared artefact: err = %v, want rejection", err)
	}
	rec, err := st.RecordArtefact(rpc.ArtefactParams{Artefact: "morning", Note: "checked ladder"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Class != risk.EnforcementAdvisory {
		t.Fatalf("class = %s, want advisory", rec.Class)
	}
	if got := st.Artefacts(); len(got) != 1 || got[0].Artefact != "morning" {
		t.Fatalf("artefacts = %+v", got)
	}
}
