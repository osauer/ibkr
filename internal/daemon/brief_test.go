package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func dailyBriefPolicyTOML() string {
	return strings.Replace(validRiskPolicyTOML, "[cadence.morning]\nclass = \"advisory\"",
		"[cadence.morning]\nclass = \"advisory\"\n\n[cadence.eod]\nclass = \"advisory\"\n\n[cadence.weekly]\nclass = \"advisory\"", 1)
}

func TestBriefAckOriginIdempotenceAndAuditFields(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	now := time.Date(2026, 7, 18, 8, 30, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now

	statePath, _ := defaultTradingStatePath(briefStateFile)
	journalPath, _ := defaultTradingStatePath(riskPolicyJournalFile)
	for _, origin := range []string{"", rpc.OrderOriginAgent, "unknown"} {
		_, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
			Kind: rpc.BriefKindMorning, BriefFingerprint: "sha256:rendered", Origin: origin,
		}))
		if err == nil || !strings.Contains(err.Error(), "human-only") {
			t.Fatalf("origin %q: err=%v, want human-only refusal", origin, err)
		}
	}
	for _, path := range []string{statePath, journalPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refused ack wrote %s: %v", path, err)
		}
	}

	ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMorning, BriefFingerprint: "sha256:rendered", Origin: rpc.OrderOriginHumanTTY,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.AlreadyStamped || ack.Kind != rpc.BriefKindMorning || ack.Day != "2026-07-18" {
		t.Fatalf("ack=%+v", ack)
	}
	records := s.riskCapital.Artefacts()
	if len(records) != 1 || records[0].Origin != rpc.OrderOriginHumanTTY || records[0].BriefFingerprint != "sha256:rendered" {
		t.Fatalf("artefact records=%+v", records)
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(data, &line); err != nil {
		t.Fatal(err)
	}
	if line["kind"] != "artefact_completed" || line["origin"] != rpc.OrderOriginHumanTTY || line["brief_fingerprint"] != "sha256:rendered" {
		t.Fatalf("journal=%v", line)
	}

	before := string(data)
	repeat, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMorning, BriefFingerprint: "sha256:different", Origin: rpc.OrderOriginHumanTTY,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.AlreadyStamped || repeat.At != ack.At {
		t.Fatalf("repeat=%+v want idempotent receipt at %s", repeat, ack.At)
	}
	after, _ := os.ReadFile(journalPath)
	if string(after) != before {
		t.Fatal("repeat ack appended another journal entry")
	}
}

func TestBriefArtefactExtensionPreservesLegacyPolicyPathAndJSON(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	res, err := s.handleRiskPolicyArtefact(context.Background(), rawParams(t, rpc.ArtefactParams{
		Artefact: rpc.BriefKindMorning,
		Note:     "ordinary policy artefact",
		Origin:   rpc.OrderOriginHumanTTY,
	}))
	if err != nil || !res.OK {
		t.Fatalf("existing policy artefact path: result=%+v err=%v", res, err)
	}
	records := s.riskCapital.Artefacts()
	if len(records) != 1 || records[0].BriefFingerprint != "" || records[0].Origin != rpc.OrderOriginHumanTTY {
		t.Fatalf("existing policy artefact record=%+v", records)
	}

	// Older persisted records and journal-shaped JSON omit both extension
	// fields. Go's typed decoder must continue accepting those lines.
	var legacy rpc.ArtefactRecord
	if err := json.Unmarshal([]byte(`{"artefact":"morning","class":"advisory","completed_at":"2026-07-18T08:00:00Z"}`), &legacy); err != nil {
		t.Fatalf("legacy artefact JSON: %v", err)
	}
	if legacy.Artefact != rpc.BriefKindMorning || legacy.Origin != "" || legacy.BriefFingerprint != "" {
		t.Fatalf("legacy artefact decoded=%+v", legacy)
	}
}

func TestBriefFirstIncompleteAndExplicitKind(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.Local)
	c := &risk.Constitution{Cadence: risk.ConstitutionCadence{
		Morning: risk.ConstitutionArtefact{Class: risk.EnforcementAdvisory},
		EOD:     risk.ConstitutionArtefact{Class: risk.EnforcementAdvisory},
	}}
	policy := &rpc.RiskPolicyResult{}
	if kind, reason := briefStampTarget(policy, c, now); kind != rpc.BriefKindMorning || reason != "" {
		t.Fatalf("initial target=%q reason=%q", kind, reason)
	}
	policy.Cadence = []rpc.ArtefactRecord{{Artefact: rpc.BriefKindMorning, Class: risk.EnforcementAdvisory, CompletedAt: now.Add(-time.Hour)}}
	if kind, reason := briefStampTarget(policy, c, now); kind != rpc.BriefKindEOD || reason != "" {
		t.Fatalf("after morning target=%q reason=%q", kind, reason)
	}
	policy.Cadence = append(policy.Cadence, rpc.ArtefactRecord{Artefact: rpc.BriefKindEOD, Class: risk.EnforcementAdvisory, CompletedAt: now})
	if kind, reason := briefStampTarget(policy, c, now); kind != "" || reason != "both daily artefacts complete" {
		t.Fatalf("complete target=%q reason=%q", kind, reason)
	}

	// The explicit kind is honored even while the default target is morning.
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindEOD, BriefFingerprint: "sha256:eod-override", Origin: rpc.OrderOriginHumanTTY,
	}))
	if err != nil || ack.Kind != rpc.BriefKindEOD || ack.AlreadyStamped {
		t.Fatalf("explicit eod ack=%+v err=%v", ack, err)
	}
}

func TestBriefSnapshotPurityAndDegradedRows(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	root := os.Getenv("XDG_STATE_HOME")
	before := stateTree(t, root)
	for range 3 {
		res, _ := s.composeBrief(context.Background())
		if res.Market.Regime.Status != rpc.BriefStatusUnavailable || res.Portfolio.Account.Status != rpc.BriefStatusUnavailable {
			t.Fatalf("gateway rows not unavailable: market=%+v account=%+v", res.Market.Regime, res.Portfolio.Account)
		}
		if res.RiskLimits.Capital.Status == "" || res.Process.Reconcile.Status == "" || res.BriefFingerprint == "" {
			t.Fatalf("policy/process rows did not render: %+v", res)
		}
	}
	after := stateTree(t, root)
	if !slices.Equal(before, after) {
		t.Fatalf("brief.snapshot mutated state tree: before=%v after=%v", before, after)
	}
}

func stateTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != root {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	slices.Sort(out)
	return out
}

func TestBriefRulesDeltaAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, briefStateFile)
	baseline := &rpc.RulesResult{
		PolicyFingerprint: &rpc.Fingerprint{Key: "sha256:old"},
		Rules:             []risk.RuleRow{{ID: "kept", Status: risk.RuleStatusPass}, {ID: "removed", Status: risk.RuleStatusWatch}},
	}
	store := &briefStateStore{path: path}
	at := time.Date(2026, 7, 17, 17, 0, 0, 0, time.UTC)
	if err := store.stamp(rpc.BriefKindEOD, "sha256:brief", at, baseline); err != nil {
		t.Fatal(err)
	}
	s := &Server{briefState: &briefStateStore{path: path}}
	current := &rpc.RulesResult{
		PolicyFingerprint: &rpc.Fingerprint{Key: "sha256:new"},
		Rules:             []risk.RuleRow{{ID: "kept", Status: risk.RuleStatusAct}, {ID: "added", Status: risk.RuleStatusPass}},
	}
	delta := s.briefRulesDelta(current)
	if !delta.RulebookFingerprintChanged || len(delta.Transitions) != 1 || delta.Transitions[0].RuleID != "kept" ||
		!slices.Equal(delta.Added, []string{"added"}) || !slices.Equal(delta.Removed, []string{"removed"}) || !delta.BaselineAt.Equal(at) {
		t.Fatalf("delta=%+v", delta)
	}
	if got := (&Server{briefState: &briefStateStore{path: filepath.Join(dir, "missing.json")}}).briefRulesDelta(current); got.Detail != "no delta baseline yet" {
		t.Fatalf("no-baseline detail=%q", got.Detail)
	}
}

func TestBriefNilMoneyAndGreeksDegradeWithoutZeroFill(t *testing.T) {
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		{Symbol: "AAPL", SecType: "OPT", Right: "C", Quantity: 1},
		{Symbol: "SPY", SecType: "OPT", Right: "P", Quantity: 1, Multiplier: 100},
	}}
	premium := briefPremiumAtRisk(pos, "EUR")
	if premium.Status != rpc.BriefStatusDegraded || premium.AmountBase != nil || premium.ExcludedLegs != 2 {
		t.Fatalf("premium=%+v", premium)
	}
	hedge := briefHedgeCost(pos, "EUR")
	if hedge.Status != rpc.BriefStatusDegraded || hedge.AmountBase != nil || hedge.ExcludedLegs != 1 {
		t.Fatalf("hedge=%+v", hedge)
	}
}

func TestBriefResultContainsNoPrivateIdentityOrTokenFields(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	res, _ := s.composeBrief(context.Background())
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"account_id", "order_id", "order_ref", "preview_token", "submit_eligible"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("brief result contains forbidden field %q: %s", forbidden, text)
		}
	}
}

func TestUnreconciledClockSharedProjection(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	last := now.Add(-5 * 24 * time.Hour)
	maxDays := 7
	clock := risk.EvaluateUnreconciledClock(&maxDays, last, time.Time{}, now)
	if !clock.Approved || clock.Stale || !clock.Deadline.Equal(last.Add(7*24*time.Hour)) || clock.DaysRemaining == nil || *clock.DaysRemaining != 2 {
		t.Fatalf("clock=%+v", clock)
	}
	override := now.Add(4 * 24 * time.Hour)
	clock = risk.EvaluateUnreconciledClock(&maxDays, last, override, now)
	if !clock.Deadline.Equal(override) || clock.DaysRemaining == nil || *clock.DaysRemaining != 4 {
		t.Fatalf("override clock=%+v", clock)
	}
	never := risk.EvaluateUnreconciledClock(&maxDays, time.Time{}, time.Time{}, now)
	if !never.Stale || !never.Deadline.IsZero() || never.DaysRemaining != nil {
		t.Fatalf("never clock=%+v", never)
	}
}
