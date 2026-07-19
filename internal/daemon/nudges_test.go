package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func validRiskPolicyV4TOML() string {
	v4 := strings.Replace(validRiskPolicyV3TOML(), "policy_version = 3", "policy_version = 4", 1)
	return v4 + `

[cadence.nudges]
timezone = "Europe/Berlin"
reconcile_warning_days = 2

[cadence.monthly]
class = "advisory"
day_of_month = 1
nudge_at_local = "09:00"

[inventory.rulebook]
id = "rulebook-v2"
version = "2"

[inventory.protection]
id = "protection-mvp"
version = "1"

[inventory.canary]
id = "active-v1"
version = "risk-policy-v1"
`
}

func newV4NudgeTestServer(t *testing.T, now time.Time) *Server {
	t.Helper()
	return newNudgeAuthorityTestServer(t, now, validRiskPolicyV4TOML())
}

func newNudgeAuthorityTestServer(t *testing.T, now time.Time, policyTOML string) *Server {
	t.Helper()
	s := newRiskPolicyTestServer(t, policyTOML)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	if s.riskPolicies != nil {
		s.riskPolicies.mu.Lock()
		s.riskPolicies.now = s.now
		s.riskPolicies.loadedAt = now
		s.riskPolicies.lastCheckedAt = now
		s.riskPolicies.mu.Unlock()
	}
	s.installNudgeStateStore()
	pm := newProtectionPolicyManager("", false, time.Second, s.now)
	pm.reload()
	s.protectionPolicies = pm
	return s
}

func primeNudgeBlockEpisode(s *Server, now time.Time, consumedKnown bool) {
	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 260000
	if consumedKnown {
		s.riskCapital.state.LastEquityBase = 240000
		s.riskCapital.state.LastEquityAsOf = now
	} else {
		s.riskCapital.state.LastEquityBase = 0
		s.riskCapital.state.LastEquityAsOf = time.Time{}
	}
	s.riskCapital.state.BlockLatched = true
	s.riskCapital.state.LatchedAt = now.Add(-2 * time.Hour)
	s.riskCapital.state.LatchEpisodeSeq = 1
	s.riskCapital.mu.Unlock()
}

func TestGovernanceAuthorityGatesCandidatesShadowAndMonthly(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		policyTOML string
		mutate     func(*Server)
		wantStatus string
		wantReason string
		wantActive bool
	}{
		{name: "active approved v4", policyTOML: validRiskPolicyV4TOML(), wantStatus: rpc.NudgeInputStatusOK, wantActive: true},
		{name: "v3", policyTOML: validRiskPolicyV3TOML(), wantStatus: rpc.NudgeInputStatusUnapproved, wantReason: rpc.NudgeHealthReasonPolicyUnapproved},
		{name: "inactive", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.status = rpc.RiskPolicyStatusAbsent }, wantStatus: rpc.NudgeInputStatusUnavailable, wantReason: rpc.NudgeHealthReasonSourceUnavailable},
		{name: "unapproved", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.active.Cadence.Monthly.DayOfMonth = nil }, wantStatus: rpc.NudgeInputStatusUnapproved, wantReason: rpc.NudgeHealthReasonPolicyUnapproved},
		{name: "error", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.status = rpc.RiskPolicyStatusError }, wantStatus: rpc.NudgeInputStatusError, wantReason: rpc.NudgeHealthReasonEvaluationError},
		{name: "unavailable", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.source = "" }, wantStatus: rpc.NudgeInputStatusUnavailable, wantReason: rpc.NudgeHealthReasonSourceUnavailable},
		{name: "stale", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.lastCheckedAt = now.Add(-2 * time.Minute) }, wantStatus: rpc.NudgeInputStatusStale, wantReason: rpc.NudgeHealthReasonEvidenceStale},
		{name: "drifted", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.status = rpc.RiskPolicyStatusDrift }, wantStatus: rpc.NudgeInputStatusUnapproved, wantReason: rpc.NudgeHealthReasonPolicyUnapproved},
		{name: "internally inconsistent", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.active.Kind = "wrong.kind" }, wantStatus: rpc.NudgeInputStatusError, wantReason: rpc.NudgeHealthReasonEvaluationError},
	}
	stockBuy := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "MSFT", SecType: "STK"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newNudgeAuthorityTestServer(t, now, tt.policyTOML)
			primeNudgeBlockEpisode(s, now, true)
			if tt.mutate != nil {
				s.riskPolicies.mu.Lock()
				tt.mutate(s)
				s.riskPolicies.mu.Unlock()
			}
			warnings := s.riskPolicyPreviewWarnings(stockBuy, rpc.OrderPositionImpact{Effect: "open"})
			if tt.wantActive && len(warnings) != 1 {
				t.Fatalf("active v4 warnings=%+v", warnings)
			}
			result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
			if err != nil {
				t.Fatal(err)
			}
			if result.SourceHealth.Policy.Status != tt.wantStatus || result.SourceHealth.Policy.Reason != tt.wantReason {
				t.Fatalf("policy health=%+v, want %s/%s", result.SourceHealth.Policy, tt.wantStatus, tt.wantReason)
			}
			if tt.wantActive {
				for _, kind := range []string{rpc.NudgeKindShadowWouldBlock, rpc.NudgeKindDrawdownLatched, rpc.NudgeKindMonthlyPulse} {
					if !candidateKindPresent(result.Candidates, kind) {
						t.Fatalf("active candidates=%+v, missing %s", result.Candidates, kind)
					}
				}
				if result.Context == nil || result.Context.Shadow == nil || result.Context.Shadow.Count != 1 || result.Context.Drawdown == nil {
					t.Fatalf("active context=%+v", result.Context)
				}
				return
			}
			if len(result.Candidates) != 0 || result.Context != nil || result.IsCleanEmpty() {
				t.Fatalf("blocked result candidates=%+v context=%+v clean=%v", result.Candidates, result.Context, result.IsCleanEmpty())
			}
			s.nudges.mu.Lock()
			count := s.nudges.state.Shadow.Count
			monthly := len(s.nudges.state.MonthlyCompletions)
			s.nudges.mu.Unlock()
			if count != 0 || monthly != 0 {
				t.Fatalf("blocked authority advanced state: shadow=%d monthly=%d", count, monthly)
			}
		})
	}
}

func TestShadowOccurrenceUsesFirstPreviewTimeNotLatchTime(t *testing.T) {
	latchTime := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	previewTime := latchTime.Add(90 * time.Minute)
	s := newV4NudgeTestServer(t, previewTime)
	primeNudgeBlockEpisode(s, latchTime.Add(2*time.Hour), true)
	s.riskCapital.mu.Lock()
	s.riskCapital.state.LatchedAt = latchTime
	s.riskCapital.state.LastEquityAsOf = previewTime
	s.riskCapital.mu.Unlock()
	stockBuy := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "MSFT", SecType: "STK"}}
	if got := s.riskPolicyPreviewWarnings(stockBuy, rpc.OrderPositionImpact{Effect: "open"}); len(got) != 1 {
		t.Fatalf("preview warnings=%+v", got)
	}
	result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range result.Candidates {
		if candidate.Kind == rpc.NudgeKindShadowWouldBlock {
			if !candidate.OccurredAt.Equal(previewTime) {
				t.Fatalf("shadow occurred_at=%s, want first preview %s (latch %s)", candidate.OccurredAt, previewTime, latchTime)
			}
			return
		}
	}
	t.Fatal("missing shadow candidate")
}

func TestNudgeSnapshotContextKnownUnknownAndValidatorCorrelation(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	known := newV4NudgeTestServer(t, now)
	primeNudgeBlockEpisode(known, now, true)
	knownResult := known.composeNudgesSnapshot()
	if knownResult.Context == nil || knownResult.Context.Drawdown == nil || knownResult.Context.Drawdown.ConsumedPct == nil {
		t.Fatalf("known drawdown context=%+v", knownResult.Context)
	}
	if _, err := json.Marshal(knownResult); err != nil {
		t.Fatalf("known context rejected: %v", err)
	}

	unknown := newV4NudgeTestServer(t, now)
	primeNudgeBlockEpisode(unknown, now, false)
	unknownResult := unknown.composeNudgesSnapshot()
	if unknownResult.Context == nil || unknownResult.Context.Drawdown == nil || unknownResult.Context.Drawdown.ConsumedPct != nil {
		t.Fatalf("unknown drawdown context=%+v, want explicit nil magnitude", unknownResult.Context)
	}
	wire, err := json.Marshal(unknownResult)
	if err != nil {
		t.Fatalf("unknown context rejected: %v", err)
	}
	if !strings.Contains(string(wire), `"consumed_pct":null`) {
		t.Fatalf("unknown magnitude was not JSON null: %s", wire)
	}

	inconsistent := knownResult
	inconsistent.Context = nil
	if _, err := json.Marshal(inconsistent); err == nil || !strings.Contains(err.Error(), "drawdown summary") {
		t.Fatalf("inconsistent candidate/context error=%v", err)
	}
}

func TestNudgeSnapshotAuthoritativeCleanEmptyHasNoContext(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, _ := newCutoverReviewTestServer(t, now)
	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 250000
	s.riskCapital.state.LastEquityBase = 250000
	s.riskCapital.state.LastEquityAsOf = now
	s.riskCapital.lastReconciledAt = now
	s.riskCapital.mu.Unlock()
	if _, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: "sha256:clean-empty", Origin: rpc.OrderOriginPairedDevice,
	})); err != nil {
		t.Fatal(err)
	}
	result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 || result.Context != nil || !result.IsCleanEmpty() || result.SourceHealth.Aggregate != rpc.NudgeAggregateReady {
		t.Fatalf("authoritative clean empty candidates=%+v context=%+v health=%+v clean=%v", result.Candidates, result.Context, result.SourceHealth, result.IsCleanEmpty())
	}
}

func TestNudgesSnapshotAssemblesTypedTriggersAndRedactsHostileSources(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := newV4NudgeTestServer(t, now)
	day := now.Format("20060102")
	sentinel := "HOSTILE_U1234567_ORDER_99_TOKEN_https://evil.example"
	body := strings.Replace(cashLine("flow-private", "Deposits/Withdrawals", 250, day), "FIXTURE", sentinel, 1) + "\n" +
		strings.Replace(cashLine("exception-private", "Unknown Broker Instruction", 12, day), "FIXTURE", sentinel, 1)
	writeFlexFixture(t, "flex-nudges.xml", now.Add(-time.Minute).Format("20060102;150405"), day, day, body)
	report := s.buildReconReport()
	if len(report.Confirmed) != 1 || report.Unresolved != 1 {
		t.Fatalf("fixture report confirmed=%d unresolved=%d", len(report.Confirmed), report.Unresolved)
	}

	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.LastEquityBase = 240000
	s.riskCapital.state.LastEquityAsOf = now
	s.riskCapital.state.BlockLatched = true
	s.riskCapital.state.LatchedAt = now.Add(-3 * time.Hour)
	s.riskCapital.lastReconciledAt = now.Add(-6 * 24 * time.Hour)
	s.riskCapital.mu.Unlock()

	policyIdentity := nudgePolicyIdentity(s.riskPolicies.snapshot().policy)
	_, episode, _ := s.riskCapital.NudgeLatch()
	if err := s.nudges.recordShadow(policyIdentity, episode, true, false, true); err != nil {
		t.Fatal(err)
	}
	currentRows := []string{confirmedFlowContentIdentity(report.Confirmed[0])}
	if err := s.nudges.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
		PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "cutover"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.nudges.reviewConfirmedCutover(nudgeCutoverReviewEvidence{
		ReviewedAt: now, PolicyIdentity: policyIdentity, PolicyVersion: 4,
		ReportIdentity: opaqueIdentity("report", "cutover"), ConfirmedRows: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.nudges.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
		PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "current"), ConfirmedRows: currentRows,
	}); err != nil {
		t.Fatal(err)
	}

	before := stateTree(t, os.Getenv("XDG_STATE_HOME"))
	beforeContents := stateContents(t, os.Getenv("XDG_STATE_HOME"))
	result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	after := stateTree(t, os.Getenv("XDG_STATE_HOME"))
	if !slices.Equal(before, after) {
		t.Fatalf("snapshot changed state tree: before=%v after=%v", before, after)
	}
	if afterContents := stateContents(t, os.Getenv("XDG_STATE_HOME")); !maps.Equal(beforeContents, afterContents) {
		t.Fatalf("snapshot changed state or journal contents: before=%v after=%v", beforeContents, afterContents)
	}
	kinds := make(map[string]bool)
	for _, candidate := range result.Candidates {
		kinds[candidate.Kind] = true
	}
	for _, want := range []string{
		rpc.NudgeKindReconcileDue, rpc.NudgeKindReconcileException,
		rpc.NudgeKindShadowWouldBlock, rpc.NudgeKindDrawdownLatched,
		rpc.NudgeKindConfirmedFlow,
	} {
		if !kinds[want] {
			t.Fatalf("candidate kinds=%v, missing %s; health=%+v coverage=%+v", kinds, want, result.SourceHealth, result.ConfirmedFlowCoverage)
		}
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	stateRaw, err := os.ReadFile(s.nudges.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{sentinel, "flow-private", "exception-private", "U1234567", "evil.example"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("nudge wire leaked %q: %s", forbidden, wire)
		}
		if strings.Contains(string(stateRaw), forbidden) {
			t.Fatalf("nudge state leaked %q: %s", forbidden, stateRaw)
		}
	}
}

func TestNudgesSnapshotFailClosedHealthAndNoReadWrites(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyV3TOML())
	s.installNudgeStateStore()
	root := os.Getenv("XDG_STATE_HOME")
	before := stateTree(t, root)
	beforeContents := stateContents(t, root)
	for range 3 {
		result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
		if err != nil {
			t.Fatal(err)
		}
		if result.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed || result.IsCleanEmpty() {
			t.Fatalf("health=%+v clean=%v, want suppressed non-clean empty", result.SourceHealth, result.IsCleanEmpty())
		}
		if result.SourceHealth.ConfirmedFlow.Status == rpc.NudgeInputStatusOK {
			t.Fatal("v3 without cutover coverage reported confirmed-flow ready")
		}
	}
	after := stateTree(t, root)
	if !slices.Equal(before, after) {
		t.Fatalf("repeated snapshot created state: before=%v after=%v", before, after)
	}
	if afterContents := stateContents(t, root); !maps.Equal(beforeContents, afterContents) {
		t.Fatalf("repeated snapshot changed state contents: before=%v after=%v", beforeContents, afterContents)
	}
}

func stateContents(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read state %s: %v", path, readErr)
		}
		out[rel] = string(raw)
		return nil
	})
	return out
}

func TestNudgesMonthlyRequiresMatchingPinsWhileReadableDriftStillTriggers(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	matching := s.composeNudgesSnapshot()
	if !candidateKindPresent(matching.Candidates, rpc.NudgeKindMonthlyPulse) {
		t.Fatalf("matching pins did not produce due monthly pulse: %+v", matching.Candidates)
	}

	s.protectionPolicies.mu.Lock()
	s.protectionPolicies.status.PolicyVersion++
	s.protectionPolicies.mu.Unlock()
	drifted := s.composeNudgesSnapshot()
	if !candidateKindPresent(drifted.Candidates, rpc.NudgeKindPolicyDrift) {
		t.Fatalf("readable drift did not produce policy candidate: %+v", drifted.Candidates)
	}
	if candidateKindPresent(drifted.Candidates, rpc.NudgeKindMonthlyPulse) {
		t.Fatalf("monthly pulse remained eligible under drift: %+v", drifted.Candidates)
	}
	if drifted.SourceHealth.Pins.Status != rpc.NudgeInputStatusOK {
		t.Fatalf("readable drift pin health=%+v, want ok evidence", drifted.SourceHealth.Pins)
	}
}

func candidateKindPresent(candidates []rpc.NudgeCandidate, kind string) bool {
	for _, candidate := range candidates {
		if candidate.Kind == kind {
			return true
		}
	}
	return false
}

func TestConfirmedFlowCutoverRevalidationRestatementAndRestart(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
	st := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	oldID := opaqueIdentity("flow", "old")
	newID := opaqueIdentity("flow", "new")
	restatedID := opaqueIdentity("flow", "restated")

	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "cutover"), ConfirmedRows: []string{oldID}}); err != nil {
		t.Fatal(err)
	}
	coverage, events, ok := st.confirmedSnapshot([]string{oldID})
	if !ok || coverage == nil || !coverage.PreCutoverFlowsUnreviewed || len(events) != 0 {
		t.Fatalf("cutover coverage=%+v events=%+v ok=%v", coverage, events, ok)
	}
	evidence := nudgeCutoverReviewEvidence{
		ReviewedAt: now.Add(time.Minute), PolicyIdentity: opaqueIdentity("policy", "v4"), PolicyVersion: 4,
		ReportIdentity: opaqueIdentity("report", "cutover"), ConfirmedRows: 1,
	}
	if _, reviewed, err := st.reviewConfirmedCutover(evidence); err == nil || reviewed {
		t.Fatalf("future cutover review reviewed=%v err=%v", reviewed, err)
	}
	now = now.Add(time.Minute)
	evidence.ReviewedAt = now
	if _, already, err := st.reviewConfirmedCutover(evidence); err != nil || already {
		t.Fatalf("already=%v err=%v", already, err)
	}
	now = now.Add(2 * time.Minute)
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "next"), ConfirmedRows: []string{oldID, newID}}); err != nil {
		t.Fatal(err)
	}
	coverage, events, _ = st.confirmedSnapshot([]string{oldID, newID})
	if coverage.PreCutoverFlowsUnreviewed || len(events) != 1 || events[0].ContentIdentity != newID {
		t.Fatalf("post-cutover coverage=%+v events=%+v", coverage, events)
	}
	st.mu.Lock()
	anchoredReport := st.state.ConfirmedCoverage.ReportIdentity
	st.mu.Unlock()
	if anchoredReport != opaqueIdentity("report", "cutover") {
		t.Fatalf("cutover report anchor changed to %s", anchoredReport)
	}
	// A snapshot revalidation suppresses a row missing from current broker
	// truth without writing or resolving the durable event.
	if _, hidden, _ := st.confirmedSnapshot([]string{oldID}); len(hidden) != 0 {
		t.Fatalf("removed current row remained eligible: %+v", hidden)
	}
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "removed"), ConfirmedRows: []string{oldID}}); err != nil {
		t.Fatal(err)
	}

	reloaded := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	if _, got, ok := reloaded.confirmedSnapshot([]string{oldID, newID}); !ok || len(got) != 0 {
		t.Fatalf("restart replay resurrected superseded event: ok=%v events=%+v", ok, got)
	}
	now = now.Add(time.Minute)
	if err := reloaded.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "restated"), ConfirmedRows: []string{oldID, restatedID}}); err != nil {
		t.Fatal(err)
	}
	if _, got, _ := reloaded.confirmedSnapshot([]string{oldID, restatedID}); len(got) != 1 || got[0].ContentIdentity != restatedID {
		t.Fatalf("material restatement did not rearm: %+v", got)
	}
}

func newCutoverReviewTestServer(t *testing.T, now time.Time) (*Server, string) {
	t.Helper()
	s := newV4NudgeTestServer(t, now)
	day := now.Format("20060102")
	name := "flex-cutover-review.xml"
	writeFlexFixture(t, name, now.Format("20060102")+";090000", day, day,
		cashLine("cutover-flow", "Deposits/Withdrawals", 250, day))
	report, snapshot := s.buildReconReportWithSnapshot()
	if report == nil || report.Status != rpc.ReconStatusActive || snapshot == nil || report.ReportID == "" {
		t.Fatalf("cutover fixture report=%+v snapshot=%+v", report, snapshot)
	}
	s.riskCapital.IncorporateStatementSnapshot(*snapshot)
	return s, name
}

func cutoverReviewRequest(t *testing.T) *rpc.Request {
	t.Helper()
	return rawParams(t, rpc.NudgesCutoverReviewParams{
		Origin:   rpc.NudgeCutoverReviewOriginPairedDevice,
		Evidence: rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	})
}

func TestNudgesCutoverReviewProductionAuthorizationRevalidationAndPersistence(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, fixtureName := newCutoverReviewTestServer(t, now)

	for _, raw := range []string{
		`{"origin":"agent","evidence":"paired_device_foreground_render_review"}`,
		`{"origin":"human-cli","evidence":"paired_device_foreground_render_review"}`,
		`{"origin":"unpaired","evidence":"paired_device_foreground_render_review"}`,
		`{"origin":"paired_device","evidence":"monthly_completion"}`,
	} {
		if _, err := s.handleNudgesCutoverReview(context.Background(), &rpc.Request{Params: json.RawMessage(raw)}); err == nil {
			t.Fatalf("unauthorized cutover request accepted: %s", raw)
		}
	}
	coverage, _, ok := s.nudges.confirmedSnapshot(nil)
	if !ok || coverage == nil || !coverage.PreCutoverFlowsUnreviewed {
		t.Fatalf("refused requests changed coverage=%+v ok=%v", coverage, ok)
	}

	first, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !first.OK || first.AlreadyReviewed || !first.ReviewedAt.Equal(now) || first.CoverageFrom.IsZero() || first.Evidence != rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender {
		t.Fatalf("first cutover result=%+v", first)
	}
	repeat, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil || !repeat.AlreadyReviewed || !repeat.ReviewedAt.Equal(first.ReviewedAt) || !repeat.CoverageFrom.Equal(first.CoverageFrom) {
		t.Fatalf("idempotent retry result=%+v err=%v", repeat, err)
	}

	s.nudges.mu.Lock()
	pinned := *s.nudges.state.ConfirmedCoverage
	statePath := s.nudges.path
	s.nudges.mu.Unlock()
	if pinned.ReviewPolicyIdentity == "" || pinned.ReviewPolicyVersion != 4 || pinned.ReviewReportIdentity == "" || pinned.ReviewedRowCount != 1 {
		t.Fatalf("cutover evidence not pinned to current authority/report: %+v", pinned)
	}

	// Reopening the durable store preserves the exact idempotent receipt.
	s.nudges = &nudgeStateStore{path: statePath, now: s.now}
	s.riskCapital.nudges = s.nudges
	restarted, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil || !restarted.AlreadyReviewed || !restarted.ReviewedAt.Equal(first.ReviewedAt) {
		t.Fatalf("restart retry result=%+v err=%v", restarted, err)
	}

	// A materially changed current broker report conflicts with the pinned
	// review; an identical request may not bless the new truth implicitly.
	day := now.Format("20060102")
	writeFlexFixture(t, fixtureName, now.Format("20060102")+";090000", day, day,
		cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+
			cashLine("later-flow", "Deposits/Withdrawals", 100, day))
	if _, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("changed-report retry error=%v, want conflict", err)
	}
}

func TestNudgesCutoverReviewRejectsStaleCurrentReportAndDispatches(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, _ := newCutoverReviewTestServer(t, now)
	now = now.Add(5 * 24 * time.Hour)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	s.nudges.now = s.now
	s.riskPolicies.mu.Lock()
	s.riskPolicies.now = s.now
	s.riskPolicies.loadedAt = now
	s.riskPolicies.lastCheckedAt = now
	s.riskPolicies.mu.Unlock()
	if _, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err == nil || !strings.Contains(err.Error(), "current active broker-backed reconciliation report") {
		t.Fatalf("stale report error=%v", err)
	}

	// Restore current time and prove the production dispatcher routes the
	// fixed-shape write through the typed result boundary.
	now = time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s.riskPolicies.mu.Lock()
	s.riskPolicies.loadedAt = now
	s.riskPolicies.lastCheckedAt = now
	s.riskPolicies.mu.Unlock()
	s.logger = NewLogger(&bytes.Buffer{}, "error")
	request := cutoverReviewRequest(t)
	request.ID = "cutover-1"
	request.Method = rpc.MethodNudgesCutoverReview
	var output bytes.Buffer
	if terminal := s.dispatch(context.Background(), request, json.NewEncoder(&output), bufio.NewReader(strings.NewReader(""))); terminal {
		t.Fatal("nudges.cutover_review unexpectedly marked streaming")
	}
	var response rpc.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Error != nil {
		t.Fatalf("dispatch response error=%v response=%+v wire=%s", err, response, output.String())
	}
	var result rpc.NudgesCutoverReviewResult
	if err := json.Unmarshal(response.Result, &result); err != nil || !result.OK {
		t.Fatalf("typed cutover result=%+v err=%v", result, err)
	}
}

func TestShadowEpisodeCountRestartResolutionAndRearm(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
	st := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	policyA, policyB := opaqueIdentity("policy", "a"), opaqueIdentity("policy", "b")
	episode := opaqueIdentity("latch", "one")
	for range 2 {
		if err := st.recordShadow(policyA, episode, true, false, true); err != nil {
			t.Fatal(err)
		}
	}
	st.mu.Lock()
	count := st.state.Shadow.Count
	st.mu.Unlock()
	if count != 2 {
		t.Fatalf("episode count=%d, want 2", count)
	}
	reloaded := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	first := reloaded.shadowCandidate(policyA, episode, true)
	if first == nil || reloaded.shadowCandidate(policyA, episode, false) != nil {
		t.Fatalf("restart/open resolution candidate=%+v", first)
	}
	if reloaded.shadowCandidate(policyB, episode, true) != nil {
		t.Fatal("old policy episode survived a policy revision")
	}
	now = now.Add(time.Minute)
	if err := reloaded.recordShadow(policyB, episode, true, false, true); err != nil {
		t.Fatal(err)
	}
	second := reloaded.shadowCandidate(policyB, episode, true)
	if second == nil || second.Fingerprint == first.Fingerprint {
		t.Fatalf("policy revision did not rearm: first=%+v second=%+v", first, second)
	}
}

func TestLatchResetRearmsOpaqueEpisodeEvenAtSameTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	s := newRiskPolicyTestServer(t, validRiskPolicyTOML)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	policy := s.riskPolicies.snapshot().policy
	if _, err := s.riskCapital.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
	s.riskCapital.Observe(260000, now, policy)
	s.riskCapital.Observe(240000, now, policy)
	open, first, _ := s.riskCapital.NudgeLatch()
	if !open || first == "" {
		t.Fatalf("first latch open=%v episode=%q", open, first)
	}
	if err := s.riskCapital.ResetDrawdown("test reset", policy); err != nil {
		t.Fatal(err)
	}
	s.riskCapital.Observe(220000, now, policy)
	open, second, _ := s.riskCapital.NudgeLatch()
	if !open || second == "" || second == first {
		t.Fatalf("reset did not rearm at same time: first=%q second=%q open=%v", first, second, open)
	}
}

func TestNudgesSnapshotDispatchUsesTypedMarshalBoundary(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyV3TOML())
	s.installNudgeStateStore()
	s.logger = NewLogger(&bytes.Buffer{}, "error")
	request := &rpc.Request{ID: "nudge-1", Method: rpc.MethodNudgesSnapshot}
	var output bytes.Buffer
	terminal := s.dispatch(context.Background(), request, json.NewEncoder(&output), bufio.NewReader(strings.NewReader("")))
	if terminal {
		t.Fatal("nudges.snapshot unexpectedly marked streaming")
	}
	var response rpc.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("dispatch output: %v: %s", err, output.String())
	}
	if response.Error != nil || len(response.Result) == 0 {
		t.Fatalf("dispatch response=%+v", response)
	}
	var result rpc.NudgesSnapshotResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed {
		t.Fatalf("aggregate=%q, mandatory marshal normalization did not run", result.SourceHealth.Aggregate)
	}
}
