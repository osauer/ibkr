package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestAlertShadowComposerCanaryNormalStressRecoveryReopenAndMetrics(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	now := base.Add(2 * time.Second)
	composer.now = func() time.Time { return now }
	relevant := true

	nearMiss := alertShadowTestCanary(base, risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "near-miss")
	normal, err := composer.ObserveCanary(t.Context(), scope, nearMiss)
	if err != nil {
		t.Fatal(err)
	}
	if normal.CurrentState != rpc.AlertSnapshotUnknown || len(normal.Candidates) != 0 {
		t.Fatalf("near miss manufactured a global clear or candidate: %+v", normal)
	}
	assertAlertShadowCoverage(t, normal.Coverage, []rpc.AlertSource{rpc.AlertSourceCanary})

	now = base.Add(time.Minute + 2*time.Second)
	stress := alertShadowTestCanary(base.Add(time.Minute), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "stress")
	opened, err := composer.ObserveCanary(t.Context(), scope, stress)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Candidates) != 1 {
		t.Fatalf("stress candidates=%+v", opened.Candidates)
	}
	opening := opened.Candidates[0]
	if opening.Source != rpc.AlertSourceCanary || opening.Kind != rpc.AlertKindPortfolioRisk ||
		opening.State != rpc.AlertEpisodeOpen || opening.Severity != rpc.AlertSeverityWatch ||
		opening.DeliveryPreference != rpc.AlertDeliveryUnapproved || opening.Destination != rpc.AlertDestinationAlerts {
		t.Fatalf("unexpected Canary candidate: %+v", opening)
	}

	duplicate, err := composer.ObserveCanary(t.Context(), scope, stress)
	if err != nil {
		t.Fatal(err)
	}
	if len(duplicate.Candidates) != 1 || duplicate.Candidates[0].OccurrenceKey != opening.OccurrenceKey {
		t.Fatalf("duplicate changed occurrence: %+v", duplicate.Candidates)
	}

	now = base.Add(2*time.Minute + 2*time.Second)
	repeatedStress := alertShadowTestCanary(base.Add(2*time.Minute), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "stress")
	repeated, err := composer.ObserveCanary(t.Context(), scope, repeatedStress)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Candidates[0].OccurrenceKey != opening.OccurrenceKey {
		t.Fatal("semantic replay rotated occurrence")
	}

	now = base.Add(3*time.Minute + 2*time.Second)
	recovery := alertShadowTestCanary(base.Add(3*time.Minute), risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "recovery")
	recovered, err := composer.ObserveCanary(t.Context(), scope, recovery)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Candidates) != 1 || recovered.Candidates[0].State != rpc.AlertEpisodeRecovered || recovered.Candidates[0].Severity != rpc.AlertSeverityObserve ||
		recovered.Candidates[0].EvidenceFingerprint != recovery.Fingerprint.Key ||
		recovered.Candidates[0].OccurrenceKey != opening.OccurrenceKey {
		t.Fatalf("authoritative recovery invalid: %+v", recovered.Candidates)
	}
	if recovered.CurrentState != rpc.AlertSnapshotUnknown {
		t.Fatalf("global partial coverage reported %q, want unknown", recovered.CurrentState)
	}
	restartDuringRecovery := newAlertShadowComposer(registry)
	restartProjection, ok, err := restartDuringRecovery.Snapshot(scope)
	if err != nil || !ok || len(restartProjection.Candidates) != 0 || restartProjection.CurrentState != rpc.AlertSnapshotUnknown ||
		restartProjection.Coverage.State != rpc.AlertCoverageUnavailable || restartProjection.Coverage.Freshness != rpc.AlertCoverageUnknown ||
		len(restartProjection.Coverage.CoveredSources) != 0 {
		t.Fatalf("restart replayed one-shot recovery: %+v ok=%v err=%v", restartProjection, ok, err)
	}

	now = base.Add(4*time.Minute + 2*time.Second)
	reopenInput := alertShadowTestCanary(base.Add(4*time.Minute), risk.SeverityAct, "defend", &relevant, rpc.SourceStatusOK, "reopen")
	reopened, err := composer.ObserveCanary(t.Context(), scope, reopenInput)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Candidates) != 1 || reopened.Candidates[0].State != rpc.AlertEpisodeOpen ||
		reopened.Candidates[0].OccurrenceKey == opening.OccurrenceKey {
		t.Fatalf("reopen did not re-arm occurrence: %+v", reopened.Candidates)
	}

	status := composer.Status(scope)
	if len(status.ExpectedSources) != 9 || status.HumanPrecision != alertShadowHumanLabelUnlabelled || status.HumanRecall != alertShadowHumanLabelUnlabelled {
		t.Fatalf("status contract incomplete: %+v", status)
	}
	canary := alertShadowTestSourceStatus(t, status, rpc.AlertSourceCanary)
	if canary.Measurements.EpisodesOpened != 1 || canary.Measurements.EpisodesRecovered != 1 || canary.Measurements.EpisodesReopened != 1 {
		t.Fatalf("Canary churn metrics=%+v", canary.Measurements)
	}
	if canary.Measurements.DuplicateInputs != 1 || canary.Measurements.DuplicateCandidates != 0 || canary.Measurements.RepeatedActive == 0 {
		t.Fatalf("Canary duplicate metrics=%+v", canary.Measurements)
	}
	if canary.Measurements.ActiveEvaluations == 0 || canary.Measurements.TimeToObserveSamples != 5 ||
		canary.Measurements.TimeToObserveTotal != 10*time.Second || canary.Measurements.TimeToObserveMax != 2*time.Second {
		t.Fatalf("Canary prevalence/latency metrics=%+v", canary.Measurements)
	}
	regime := alertShadowTestSourceStatus(t, status, rpc.AlertSourceRegime)
	if regime.Status != alertShadowStatusProducerNotImplemented || regime.Reason != alertShadowReasonProducerNotImplemented || regime.Measurements.Evaluations != 0 || regime.Measurements.CoverageFailures != 0 {
		t.Fatalf("Regime unavailable status=%+v", regime)
	}
	orderIntegrity := alertShadowTestSourceStatus(t, status, rpc.AlertSourceOrderIntegrity)
	if orderIntegrity.Status != alertShadowStatusPositiveOnlyNotWired || orderIntegrity.Reason != alertShadowReasonPositiveOnlyNotWired {
		t.Fatalf("order-integrity status=%+v", orderIntegrity)
	}
}

func TestAlertShadowComposerRetriesFailedApplyExpiresCrossSourceCoverageAndIsolatesScope(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	base := time.Date(2026, 7, 21, 8, 30, 0, 0, time.UTC)
	now := base
	composer.now = func() time.Time { return now }
	scope := alertShadowTestBrokerScope(t)
	relevant := true
	stress := alertShadowTestCanary(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "retry-open")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := composer.ObserveCanary(cancelled, scope, stress); err == nil {
		t.Fatal("cancelled registry apply unexpectedly succeeded")
	}
	failed := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceCanary)
	if composer.Status(scope).RegistryApplyFailures != 1 || failed.Measurements.DuplicateInputs != 0 {
		t.Fatalf("failed apply advanced cursor or missed failure: %+v", composer.Status(scope))
	}
	restartedRegistry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	restartedAfterFailure := newAlertShadowComposer(restartedRegistry)
	restartedAfterFailure.now = func() time.Time { return base }
	if got := restartedAfterFailure.Status(scope); got.RegistryApplyFailures != 1 || got.LastErrorCode != alertShadowReasonRegistryApplyFailed {
		t.Fatalf("restart lost durable apply failure: %+v", got)
	}
	opened, err := composer.ObserveCanary(t.Context(), scope, stress)
	if err != nil || len(opened.Candidates) != 1 || opened.Candidates[0].State != rpc.AlertEpisodeOpen {
		t.Fatalf("exact retry did not persist: %+v err=%v", opened, err)
	}
	oldScopeEpisode := opened.Candidates[0].EpisodeKey

	now = base.Add(time.Minute)
	nudgeInput := alertShadowTestNudges(scope, now)
	afterNudges, err := composer.ObserveNudges(t.Context(), nudgeInput)
	if err != nil {
		t.Fatal(err)
	}
	assertAlertShadowCoverage(t, afterNudges.Coverage, []rpc.AlertSource{
		rpc.AlertSourceCanary, rpc.AlertSourceRiskPolicy, rpc.AlertSourceReconciliation, rpc.AlertSourceGovernance,
	})
	if len(afterNudges.Candidates) != 1 || afterNudges.Candidates[0].State != rpc.AlertEpisodeOpen ||
		afterNudges.Candidates[0].EvidenceHealth != rpc.AlertEvidenceCurrent {
		t.Fatalf("cross-source poll reasserted or recovered cached Canary: %+v", afterNudges.Candidates)
	}
	if !alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceCanary).Covered {
		t.Fatal("fresh Canary coverage expired on unrelated Nudge evaluation")
	}
	statusAfterNudges := composer.Status(scope)
	if got := alertShadowTestSourceStatus(t, statusAfterNudges, rpc.AlertSourceCanary).Measurements.Evaluations; got != 1 {
		t.Fatalf("unrelated Nudge poll incremented Canary evaluation opportunities: %d", got)
	}
	for _, source := range alertShadowNudgeSources {
		if got := alertShadowTestSourceStatus(t, statusAfterNudges, source).Measurements.Evaluations; got != 1 {
			t.Fatalf("Nudge source %s evaluations=%d want 1", source, got)
		}
	}

	otherScope, err := newAlertShadowBrokerScope(brokerStateScope{Account: "DU-OTHER", Mode: rpc.AccountModeLive})
	if err != nil {
		t.Fatal(err)
	}
	now = base.Add(2 * time.Minute)
	otherStress := alertShadowTestCanary(base, risk.SeverityAct, "defend", &relevant, rpc.SourceStatusOK, "other-scope")
	isolated, err := composer.ObserveCanary(t.Context(), otherScope, otherStress)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated.Candidates) != 1 || isolated.Candidates[0].EpisodeKey == oldScopeEpisode {
		t.Fatalf("scope change leaked prior authority: %+v", isolated.Candidates)
	}
	oldScopeSnapshot, ok, err := composer.Snapshot(scope)
	if err != nil || !ok || len(oldScopeSnapshot.Candidates) != 1 || oldScopeSnapshot.Candidates[0].EpisodeKey != oldScopeEpisode {
		t.Fatalf("prior scope audit/current state lost: %+v ok=%v err=%v", oldScopeSnapshot, ok, err)
	}
	document, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, alertEpisodeRegistryStateKind)
	if err != nil || !ok {
		t.Fatalf("load registry document ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(document.JSON), "DU-SHADOW") || strings.Contains(string(document.JSON), "DU-OTHER") {
		t.Fatalf("registry persisted raw broker scope: %s", document.JSON)
	}

	future := alertShadowTestCanary(now.Add(time.Minute), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "future")
	if _, err := composer.ObserveCanary(t.Context(), otherScope, future); err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("future producer time error=%v", err)
	}
}

func TestAlertShadowComposerCanaryOutageAndUnstampedNegativeNeverRecover(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	now := base
	composer.now = func() time.Time { return now }
	relevant := true

	stress := alertShadowTestCanary(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "outage-open")
	opened, err := composer.ObserveCanary(t.Context(), scope, stress)
	if err != nil {
		t.Fatal(err)
	}
	openingOccurrence := opened.Candidates[0].OccurrenceKey

	now = base.Add(time.Minute)
	staleNegative := alertShadowTestCanary(now, risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusStale, "stale-negative")
	held, err := composer.ObserveCanary(t.Context(), scope, staleNegative)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen || held.Candidates[0].EvidenceHealth != rpc.AlertEvidenceStale || held.Candidates[0].OccurrenceKey != openingOccurrence {
		t.Fatalf("stale negative recovered or rewrote episode: %+v", held.Candidates)
	}
	if held.Coverage.State != rpc.AlertCoverageUnavailable || held.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("outage coverage=%+v state=%s", held.Coverage, held.CurrentState)
	}

	now = base.Add(2 * time.Minute)
	unstamped := alertShadowTestCanary(now, risk.SeverityObserve, "observe", nil, rpc.SourceStatusOK, "unstamped-negative")
	held, err = composer.ObserveCanary(t.Context(), scope, unstamped)
	if err != nil {
		t.Fatal(err)
	}
	if held.Candidates[0].State != rpc.AlertEpisodeOpen || held.Candidates[0].EvidenceHealth != rpc.AlertEvidencePartial {
		t.Fatalf("unstamped negative recovered episode: %+v", held.Candidates)
	}

	older := alertShadowTestCanary(base.Add(90*time.Second), risk.SeverityObserve, "observe", &relevant, rpc.SourceStatusOK, "older")
	if _, err := composer.ObserveCanary(t.Context(), scope, older); err != nil {
		t.Fatal(err)
	}
	canary := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceCanary)
	if canary.Reason != alertShadowReasonMissingRelevanceStamp || canary.Covered || canary.Measurements.StaleSuppressions < 2 || canary.Measurements.CoverageFailures < 2 {
		t.Fatalf("outage metrics/status=%+v", canary)
	}
}

func TestAlertShadowComposerNudgeOwnershipRecoveryAndNoDelivery(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	base := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	now := base.Add(time.Second)
	composer.now = func() time.Time { return now }
	scope := alertShadowTestBrokerScope(t)

	input := alertShadowTestNudges(scope, base,
		alertShadowTestPolicyDrift(base.Add(-time.Minute)),
		alertShadowTestReconcileException(base.Add(-time.Minute)),
		alertShadowTestMonthlyPulse(base.Add(-time.Minute)),
	)
	opened, err := composer.ObserveNudges(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	assertAlertShadowCoverage(t, opened.Coverage, []rpc.AlertSource{
		rpc.AlertSourceRiskPolicy, rpc.AlertSourceReconciliation, rpc.AlertSourceGovernance,
	})
	if len(opened.Candidates) != 3 || opened.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("Nudge candidates=%+v state=%s", opened.Candidates, opened.CurrentState)
	}
	wantKind := map[rpc.AlertSource]rpc.AlertKind{
		rpc.AlertSourceRiskPolicy:     rpc.AlertKindPolicyDrift,
		rpc.AlertSourceReconciliation: rpc.AlertKindReconciliationException,
		rpc.AlertSourceGovernance:     rpc.AlertKindGovernance,
	}
	occurrences := make(map[rpc.AlertSource]string)
	for _, candidate := range opened.Candidates {
		if candidate.Kind != wantKind[candidate.Source] || candidate.DeliveryPreference != rpc.AlertDeliveryUnapproved {
			t.Fatalf("Nudge ownership/delivery mismatch: %+v", candidate)
		}
		occurrences[candidate.Source] = candidate.OccurrenceKey
	}

	now = base.Add(time.Minute + time.Second)
	empty := alertShadowTestNudges(scope, base.Add(time.Minute))
	recovered, err := composer.ObserveNudges(t.Context(), empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.Candidates) != 3 || recovered.CurrentState != rpc.AlertSnapshotUnknown {
		t.Fatalf("Nudge recovery snapshot=%+v", recovered)
	}
	for _, candidate := range recovered.Candidates {
		if candidate.State != rpc.AlertEpisodeRecovered || candidate.OccurrenceKey != occurrences[candidate.Source] || candidate.DeliveryPreference != rpc.AlertDeliveryUnapproved {
			t.Fatalf("Nudge recovery invalid: %+v", candidate)
		}
	}
	status := composer.Status(scope)
	for _, source := range alertShadowNudgeSources {
		item := alertShadowTestSourceStatus(t, status, source)
		if !item.Covered || item.Status != alertShadowStatusCurrent || item.Measurements.EpisodesRecovered != 1 {
			t.Fatalf("source %s status=%+v", source, item)
		}
	}
}

func TestAlertShadowComposerNudgeOutageDuplicateAndEquivocation(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	now := base
	composer.now = func() time.Time { return now }
	scope := alertShadowTestBrokerScope(t)
	active := alertShadowTestPolicyDrift(base.Add(-time.Minute))
	input := alertShadowTestNudges(scope, base, active, active)
	opened, err := composer.ObserveNudges(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Candidates) != 1 {
		t.Fatalf("duplicate Nudge was not suppressed: %+v", opened.Candidates)
	}
	riskPolicy := alertShadowTestSourceStatus(t, composer.Status(scope), rpc.AlertSourceRiskPolicy)
	if riskPolicy.Measurements.DuplicateCandidates != 1 {
		t.Fatalf("duplicate Nudge metric=%+v", riskPolicy.Measurements)
	}

	now = base.Add(time.Minute)
	outage := alertShadowTestNudges(scope, now)
	outage.Snapshot.SourceHealth.Policy = rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusStale, Reason: rpc.NudgeHealthReasonEvidenceStale, AsOf: now}
	held, err := composer.ObserveNudges(t.Context(), outage)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Candidates) != 1 || held.Candidates[0].State != rpc.AlertEpisodeOpen || held.Candidates[0].EvidenceHealth != rpc.AlertEvidenceStale {
		t.Fatalf("Nudge outage recovered episode: %+v", held.Candidates)
	}

	equivocal := outage
	equivocal.Snapshot.Candidates = []rpc.NudgeCandidate{alertShadowTestReconcileException(now.Add(-time.Second))}
	if _, err := composer.ObserveNudges(t.Context(), equivocal); err == nil || !strings.Contains(err.Error(), "equivocation") {
		t.Fatalf("same-time Nudge equivocation error=%v", err)
	}
	status := composer.Status(scope)
	if status.Equivocations != 1 {
		t.Fatalf("equivocations=%d", status.Equivocations)
	}
	for _, source := range alertShadowNudgeSources {
		if alertShadowTestSourceStatus(t, status, source).Measurements.Equivocations != 1 {
			t.Fatalf("source %s missed equivocation", source)
		}
	}
}

func TestAlertShadowComposerRestartFacingEmptyAndConcurrentReplay(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	composer := newAlertShadowComposer(registry)
	if snapshot, ok, err := composer.Snapshot(alertShadowTestBrokerScope(t)); err != nil || ok || !snapshot.AsOf.IsZero() {
		t.Fatalf("fresh composer snapshot=%+v ok=%v err=%v", snapshot, ok, err)
	}
	initial := composer.Status(alertShadowTestBrokerScope(t))
	if len(initial.Sources) != 9 || initial.HumanPrecision != alertShadowHumanLabelUnlabelled || initial.HumanRecall != alertShadowHumanLabelUnlabelled {
		t.Fatalf("fresh status=%+v", initial)
	}

	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	relevant := true
	scope := alertShadowTestBrokerScope(t)
	composer.now = func() time.Time { return base }
	if _, err := composer.ObserveCanary(t.Context(), scope, alertShadowTestCanary(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "restart-open")); err != nil {
		t.Fatal(err)
	}

	restarted := newAlertShadowComposer(registry)
	restarted.now = func() time.Time { return base.Add(time.Minute) }
	durable, ok, err := restarted.Snapshot(scope)
	if err != nil || !ok || len(durable.Candidates) != 1 || durable.CurrentState != rpc.AlertSnapshotActive ||
		durable.Coverage.State != rpc.AlertCoverageUnavailable || durable.Coverage.Freshness != rpc.AlertCoverageUnknown ||
		len(durable.Coverage.CoveredSources) != 0 || durable.Candidates[0].EvidenceHealth != rpc.AlertEvidenceUnavailable {
		t.Fatalf("restart durable snapshot=%+v ok=%v err=%v", durable, ok, err)
	}
	if got := alertShadowTestSourceStatus(t, restarted.Status(scope), rpc.AlertSourceCanary); got.Status != alertShadowStatusNotObserved || got.Covered {
		t.Fatalf("restart reconstructed Canary coverage: %+v", got)
	}
	restartStatus := restarted.Status(scope)
	restartCanary := alertShadowTestSourceStatus(t, restartStatus, rpc.AlertSourceCanary)
	if restartStatus.Evaluations != 1 || restartCanary.Measurements.Evaluations != 1 || restartCanary.Measurements.EpisodesOpened != 1 || restartCanary.Measurements.TimeToObserveSamples != 1 {
		t.Fatalf("restart lost durable commissioning metrics: %+v", restartStatus)
	}

	staleReplay := alertShadowTestCanary(base.Add(-time.Second), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "restart-open")
	staleProjection, err := restarted.ObserveCanary(t.Context(), scope, staleReplay)
	if err != nil || staleProjection.Coverage.State != rpc.AlertCoverageUnavailable || staleProjection.Coverage.Freshness != rpc.AlertCoverageUnknown ||
		len(staleProjection.Coverage.CoveredSources) != 0 || len(staleProjection.Candidates) != 1 || staleProjection.Candidates[0].EvidenceHealth != rpc.AlertEvidenceUnavailable {
		t.Fatalf("stale restart replay resurrected coverage: %+v err=%v", staleProjection, err)
	}
	if got := alertShadowTestSourceStatus(t, restarted.Status(scope), rpc.AlertSourceCanary); got.Status != alertShadowStatusNotObserved || got.Covered {
		t.Fatalf("stale restart replay changed process coverage: %+v", got)
	}
	exactReplay := alertShadowTestCanary(base, risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "restart-open")
	reobserved, err := restarted.ObserveCanary(t.Context(), scope, exactReplay)
	if err != nil || reobserved.Coverage.State != rpc.AlertCoveragePartial || reobserved.Coverage.Freshness != rpc.AlertCoverageCurrent ||
		len(reobserved.Coverage.CoveredSources) != 1 || reobserved.Coverage.CoveredSources[0] != rpc.AlertSourceCanary {
		t.Fatalf("exact restart reobservation did not restore only Canary coverage: %+v err=%v", reobserved, err)
	}

	restarted.now = func() time.Time { return base.Add(time.Minute + time.Second) }
	replay := alertShadowTestCanary(base.Add(time.Minute), risk.SeverityWatch, "monitor", &relevant, rpc.SourceStatusOK, "restart-open")
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			_, observeErr := restarted.ObserveCanary(context.Background(), scope, replay)
			errs <- observeErr
		})
	}
	wg.Wait()
	close(errs)
	for observeErr := range errs {
		if observeErr != nil {
			t.Fatal(observeErr)
		}
	}
	canary := alertShadowTestSourceStatus(t, restarted.Status(scope), rpc.AlertSourceCanary)
	if canary.Measurements.DuplicateInputs != workers-1 {
		t.Fatalf("concurrent duplicate inputs=%d want %d", canary.Measurements.DuplicateInputs, workers-1)
	}
	final, ok, err := restarted.Snapshot(scope)
	if err != nil || !ok || len(final.Candidates) != 1 || final.Candidates[0].DeliveryPreference != rpc.AlertDeliveryUnapproved {
		t.Fatalf("concurrent replay snapshot=%+v ok=%v err=%v", final, ok, err)
	}
}

func alertShadowTestCanary(at time.Time, severity risk.SignalSeverity, action string, relevant *bool, sourceStatus, seed string) rpc.CanaryResult {
	source := func(name string) rpc.SourceHealth {
		fingerprint := rpc.Fingerprint{Version: name + "-fp-v1", Key: alertShadowTestFingerprint(seed + "-" + name)}
		return rpc.SourceHealth{
			Source: name, Status: sourceStatus, AsOf: at, MaxAgeSeconds: 300,
			Fingerprint: &fingerprint, FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
		}
	}
	return rpc.CanaryResult{
		AsOf: at, Fingerprint: rpc.Fingerprint{Version: rpc.CanaryFingerprintVersion, Key: alertShadowTestFingerprint(seed)},
		PolicyFingerprint: rpc.Fingerprint{Version: "canary-policy-fp-v1", Key: alertShadowTestFingerprint("canary-policy")},
		Action:            action, Severity: severity, PortfolioAlertRelevant: relevant, InputHealth: "ok",
		SourceHealth: []rpc.SourceHealth{source("account"), source("positions"), source("regime")},
	}
}

func alertShadowTestNudges(scope alertShadowBrokerScope, at time.Time, candidates ...rpc.NudgeCandidate) alertShadowNudgeInput {
	ok := func() rpc.NudgeInputHealth {
		return rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: at}
	}
	return alertShadowNudgeInput{
		Scope: scope, PolicyFingerprint: alertShadowTestPolicyFingerprint(), StoreHealth: ok(),
		Snapshot: rpc.NudgesSnapshotResult{
			AsOf: at, Candidates: append([]rpc.NudgeCandidate(nil), candidates...),
			SourceHealth: rpc.NudgeSourceHealth{
				Policy: ok(), Reconciliation: ok(), Capital: ok(), Pins: ok(), Cadence: ok(), ConfirmedFlow: ok(),
			},
			ConfirmedFlowCoverage: &rpc.NudgeConfirmedFlowCoverage{CoverageFrom: at.Add(-24 * time.Hour)},
		},
	}
}

func alertShadowTestBrokerScope(t *testing.T) alertShadowBrokerScope {
	t.Helper()
	scope, err := newAlertShadowBrokerScope(brokerStateScope{Account: "DU-SHADOW", Mode: rpc.AccountModePaper})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func alertShadowTestPolicyDrift(at time.Time) rpc.NudgeCandidate {
	return rpcNudgeCandidate(risk.EvaluatePolicyDrift([]risk.NudgePinMismatch{{
		Policy: "risk", PinnedID: "pinned", PinnedVersion: "1", LiveID: "live", LiveVersion: "2",
	}}, at))
}

func alertShadowTestReconcileException(at time.Time) rpc.NudgeCandidate {
	return rpcNudgeCandidate(risk.EvaluateReconcileException([]risk.ReconcileExceptionIdentity{{
		Kind: "amount", Identity: "opaque-row", Material: []string{"classified-material"},
	}}, at))
}

func alertShadowTestMonthlyPulse(at time.Time) rpc.NudgeCandidate {
	return rpc.NudgeCandidate{
		Fingerprint: alertShadowTestFingerprint("monthly-pulse"), Kind: rpc.NudgeKindMonthlyPulse,
		State: rpc.NudgeStateDue, Severity: rpc.NudgeSeverityWatch, OccurredAt: at, DueAt: at,
		Destination: rpc.NudgeDestinationBrief,
	}
}

func alertShadowTestPolicyFingerprint() rpc.Fingerprint {
	return rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: alertShadowTestFingerprint("risk-policy")}
}

func alertShadowTestFingerprint(seed string) string {
	fingerprint, err := alertShadowFingerprint(struct {
		Seed string `json:"seed"`
	}{seed})
	if err != nil {
		panic(err)
	}
	return fingerprint
}

func alertShadowTestSourceStatus(t *testing.T, status alertShadowStatusReport, source rpc.AlertSource) alertShadowSourceStatus {
	t.Helper()
	for _, item := range status.Sources {
		if item.Source == source {
			return item
		}
	}
	t.Fatalf("missing source status %s", source)
	return alertShadowSourceStatus{}
}

func assertAlertShadowCoverage(t *testing.T, coverage rpc.AlertCoverage, covered []rpc.AlertSource) {
	t.Helper()
	if coverage.State != rpc.AlertCoveragePartial || coverage.Freshness != rpc.AlertCoverageCurrent || len(coverage.ExpectedSources) != 9 {
		t.Fatalf("coverage=%+v", coverage)
	}
	if len(coverage.CoveredSources) != len(covered) {
		t.Fatalf("covered=%v want %v", coverage.CoveredSources, covered)
	}
	for i := range covered {
		if coverage.CoveredSources[i] != covered[i] {
			t.Fatalf("covered=%v want %v", coverage.CoveredSources, covered)
		}
	}
}
