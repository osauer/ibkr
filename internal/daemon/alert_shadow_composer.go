package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	alertShadowStatusNotObserved            = "not_observed"
	alertShadowStatusCurrent                = "current"
	alertShadowStatusPartial                = "partial"
	alertShadowStatusStale                  = "stale"
	alertShadowStatusUnavailable            = "unavailable"
	alertShadowStatusError                  = "error"
	alertShadowStatusProducerNotImplemented = "producer_not_implemented"
	alertShadowStatusPositiveOnlyNotWired   = "positive_only_not_wired"

	alertShadowReasonNotObserved                   = "not_observed"
	alertShadowReasonCurrent                       = "current"
	alertShadowReasonProducerNotImplemented        = "producer_not_implemented"
	alertShadowReasonPositiveOnlyNotWired          = "positive_only_not_wired"
	alertShadowReasonMissingRelevanceStamp         = "missing_relevance_stamp"
	alertShadowReasonInputHealthNotOK              = "input_health_not_ok"
	alertShadowReasonSourceHealthMissing           = "source_health_missing"
	alertShadowReasonSourceHealthIncomplete        = "source_health_incomplete"
	alertShadowReasonSourceHealthStale             = "source_health_stale"
	alertShadowReasonSourceHealthUnavailable       = "source_health_unavailable"
	alertShadowReasonSourceHealthError             = "source_health_error"
	alertShadowReasonSourceTimeInvalid             = "source_time_invalid"
	alertShadowReasonEvidenceFingerprintInvalid    = "evidence_fingerprint_invalid"
	alertShadowReasonPolicyFingerprintInvalid      = "policy_fingerprint_invalid"
	alertShadowReasonCandidateInvalid              = "candidate_invalid"
	alertShadowReasonSnapshotInvalid               = "snapshot_invalid"
	alertShadowReasonHealthUnapproved              = "health_unapproved"
	alertShadowReasonHealthStale                   = "health_stale"
	alertShadowReasonHealthUnavailable             = "health_unavailable"
	alertShadowReasonHealthError                   = "health_error"
	alertShadowReasonHealthTimeInvalid             = "health_time_invalid"
	alertShadowReasonBrokerScopeInvalid            = "broker_scope_invalid"
	alertShadowReasonRegistryApplyFailed           = "registry_apply_failed"
	alertShadowDecisionLegacyGateActive            = "legacy_gate_active"
	alertShadowDecisionNudgeActive                 = "nudge_active"
	alertShadowDecisionClassifiedClear             = "classified_clear"
	alertShadowDecisionClassifiedNegativeUntrusted = "classified_negative_untrusted"

	alertShadowHumanLabelUnlabelled = "unlabelled"
)

// alertShadowExpectedSources is deliberately fixed. Delivery is not a producer
// and therefore never belongs in the measurement universe.
var alertShadowExpectedSources = [...]rpc.AlertSource{
	rpc.AlertSourceCanary,
	rpc.AlertSourceRegime,
	rpc.AlertSourceRulebook,
	rpc.AlertSourceRiskPolicy,
	rpc.AlertSourceProtection,
	rpc.AlertSourceOrderIntegrity,
	rpc.AlertSourceReconciliation,
	rpc.AlertSourceGovernance,
	rpc.AlertSourceDataHealth,
}

var alertShadowNudgeSources = [...]rpc.AlertSource{
	rpc.AlertSourceRiskPolicy,
	rpc.AlertSourceReconciliation,
	rpc.AlertSourceGovernance,
}

const (
	alertShadowCursorCanary         = "canary"
	alertShadowCursorNudges         = "nudges"
	alertShadowCanarySilenceHorizon = 5 * time.Minute
	alertShadowNudgeSilenceHorizon  = time.Minute
	alertShadowReasonProducerSilent = "producer_silent"
)

// alertShadowComposer is a record-only producer boundary. It has no sender,
// pageability, cooldown, dwell, or threshold policy. Producers submit already
// classified typed snapshots; the composer maintains source-scoped coverage
// and lets alertEpisodeRegistry own durable lifecycle identity.
type alertShadowComposer struct {
	mu       sync.Mutex
	registry *alertEpisodeRegistry
	now      func() time.Time
	scopes   map[string]*alertShadowScopeState
}

type alertShadowScopeState struct {
	scope                alertShadowBrokerScope
	sources              map[rpc.AlertSource]alertShadowSourceBatch
	lastCanary           alertShadowInputCursor
	lastNudges           alertShadowInputCursor
	pendingApplyFailures uint64
	applied              bool
}

type alertShadowInputCursor struct {
	AsOf        time.Time `json:"as_of,omitzero"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

// alertShadowBrokerScope is a validated, normalized account/mode authority
// value. Raw parts exist only in memory and enter BuildAlertEpisodeKey; only
// the helper's opaque digest is persisted.
type alertShadowBrokerScope struct {
	account   string
	mode      string
	authority string
}

// alertShadowNudgeInput binds candidates, the exact constitution authority,
// and durable nudge-store health captured by one daemon composition call.
// Callers must not assemble these fields via independent later reads.
type alertShadowNudgeInput struct {
	Snapshot          rpc.NudgesSnapshotResult
	PolicyFingerprint rpc.Fingerprint
	StoreHealth       rpc.NudgeInputHealth
	Scope             alertShadowBrokerScope
}

type alertShadowSourceBatch struct {
	Source                      rpc.AlertSource
	Status                      string
	Reason                      string
	InputAsOf                   time.Time
	ObservedAt                  time.Time
	EvidenceAsOf                time.Time
	EvidenceHealth              rpc.AlertEvidenceHealth
	FreshUntil                  time.Time
	PolicyFingerprint           string
	NegativeEvidenceFingerprint string
	NegativeSeverity            rpc.AlertSeverity
	NegativeDestination         rpc.AlertDestination
	Scope                       alertShadowBrokerScope
	Covered                     bool
	NegativeReady               bool
	DuplicateCandidates         uint64
	Observations                []alertEpisodeObservation
}

// alertShadowStatusReport is the non-delivery operational view. Counts are
// descriptive shadow measurements only: human precision and recall stay
// explicitly unlabelled until an operator supplies outcome labels.
type alertShadowStatusReport struct {
	AsOf                  time.Time                 `json:"as_of,omitzero"`
	ExpectedSources       []rpc.AlertSource         `json:"expected_sources"`
	Evaluations           uint64                    `json:"evaluations"`
	RegistryApplyFailures uint64                    `json:"registry_apply_failures"`
	Equivocations         uint64                    `json:"equivocations"`
	LastErrorCode         string                    `json:"last_error_code,omitempty"`
	HumanPrecision        string                    `json:"human_precision"`
	HumanRecall           string                    `json:"human_recall"`
	Sources               []alertShadowSourceStatus `json:"sources"`
}

type alertShadowSourceStatus struct {
	Source       rpc.AlertSource          `json:"source"`
	Status       string                   `json:"status"`
	Reason       string                   `json:"reason"`
	InputAsOf    time.Time                `json:"input_as_of,omitzero"`
	ObservedAt   time.Time                `json:"observed_at,omitzero"`
	Covered      bool                     `json:"covered"`
	Active       int                      `json:"active_candidates"`
	Measurements alertShadowSourceMetrics `json:"measurements"`
}

type alertShadowSourceMetrics struct {
	Evaluations          uint64        `json:"evaluations"`
	CoveredEvaluations   uint64        `json:"covered_evaluations"`
	ActiveEvaluations    uint64        `json:"active_evaluations"`
	ActiveObservations   uint64        `json:"active_observations"`
	EpisodesOpened       uint64        `json:"episodes_opened"`
	EpisodesEscalated    uint64        `json:"episodes_escalated"`
	EpisodesRecovered    uint64        `json:"episodes_recovered"`
	EpisodesReopened     uint64        `json:"episodes_reopened"`
	DuplicateInputs      uint64        `json:"duplicate_inputs"`
	DuplicateCandidates  uint64        `json:"duplicate_candidates"`
	RepeatedActive       uint64        `json:"repeated_active_observations"`
	ActiveEvidenceChurn  uint64        `json:"active_evidence_revisions"`
	Equivocations        uint64        `json:"equivocations"`
	StaleSuppressions    uint64        `json:"stale_suppressions"`
	CoverageFailures     uint64        `json:"coverage_failures"`
	TimeToObserveSamples uint64        `json:"time_to_observe_samples"`
	TimeToObserveTotal   time.Duration `json:"time_to_observe_total"`
	TimeToObserveMax     time.Duration `json:"time_to_observe_max"`
}

func newAlertShadowComposer(registry *alertEpisodeRegistry) *alertShadowComposer {
	return &alertShadowComposer{registry: registry, now: time.Now, scopes: make(map[string]*alertShadowScopeState)}
}

func defaultAlertShadowSources(scope alertShadowBrokerScope) map[rpc.AlertSource]alertShadowSourceBatch {
	out := make(map[rpc.AlertSource]alertShadowSourceBatch, len(alertShadowExpectedSources))
	for _, source := range alertShadowExpectedSources {
		batch := alertShadowSourceBatch{
			Source: source, Status: alertShadowStatusNotObserved, Reason: alertShadowReasonNotObserved,
			EvidenceHealth: rpc.AlertEvidenceUnavailable, Scope: scope, Observations: []alertEpisodeObservation{},
		}
		switch source {
		case rpc.AlertSourceRegime, rpc.AlertSourceRulebook, rpc.AlertSourceProtection, rpc.AlertSourceDataHealth:
			batch.Status = alertShadowStatusProducerNotImplemented
			batch.Reason = alertShadowReasonProducerNotImplemented
		case rpc.AlertSourceOrderIntegrity:
			batch.Status = alertShadowStatusPositiveOnlyNotWired
			batch.Reason = alertShadowReasonPositiveOnlyNotWired
		}
		out[source] = batch
	}
	return out
}

func newAlertShadowBrokerScope(scope brokerStateScope) (alertShadowBrokerScope, error) {
	if !brokerScopeConcrete(scope) {
		return alertShadowBrokerScope{}, errors.New("alert shadow broker scope is not concrete")
	}
	account := strings.ToUpper(strings.TrimSpace(scope.Account))
	mode := strings.ToLower(strings.TrimSpace(scope.Mode))
	authority, err := rpc.BuildAlertAuthorityScope(account, mode)
	if err != nil {
		return alertShadowBrokerScope{}, err
	}
	return alertShadowBrokerScope{account: account, mode: mode, authority: authority}, nil
}

func (scope alertShadowBrokerScope) valid() bool {
	return scope.account != "" && brokerScopeConcrete(brokerStateScope{Account: scope.account, Mode: scope.mode}) &&
		rpc.ValidateAlertAuthorityScope(scope.authority) == nil
}

func (c *alertShadowComposer) scopeStateLocked(scope alertShadowBrokerScope) (*alertShadowScopeState, error) {
	if state := c.scopes[scope.authority]; state != nil {
		return state, nil
	}
	state := &alertShadowScopeState{scope: scope, sources: defaultAlertShadowSources(scope)}
	durable, ok, err := c.registry.scopeState(scope.authority)
	if err != nil {
		return nil, err
	}
	if ok {
		state.lastCanary, state.lastNudges = durable.Cursors.Canary, durable.Cursors.Nudges
		for _, sourceState := range durable.SourceStates {
			batch := state.sources[sourceState.Source]
			batch.Status, batch.Reason = sourceState.Status, sourceState.Reason
			batch.InputAsOf, batch.ObservedAt, batch.EvidenceAsOf = sourceState.InputAsOf, sourceState.ObservedAt, sourceState.EvidenceAsOf
			batch.EvidenceHealth, batch.FreshUntil = sourceState.EvidenceHealth, sourceState.FreshUntil
			// Coverage is current-process knowledge. Durable producer evidence
			// remains available for audit and active-episode retention, but restart
			// cannot resurrect a trustworthy negative or omission boundary before
			// this process observes the source again.
			batch.Covered = false
			if sourceState.Covered {
				batch.Status, batch.Reason = alertShadowStatusNotObserved, alertShadowReasonNotObserved
				batch.EvidenceHealth = rpc.AlertEvidenceUnavailable
			}
			state.sources[sourceState.Source] = batch
		}
	}
	c.scopes[scope.authority] = state
	return state, nil
}

// ObserveCanary consumes the daemon-authored Canary result. Eligibility is the
// existing legacy occurrence gate exactly, including its nil-relevance
// fail-open for positives. A missing relevance stamp can never authorize a
// negative or source coverage.
func (c *alertShadowComposer) ObserveCanary(ctx context.Context, scope alertShadowBrokerScope, result rpc.CanaryResult) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert shadow composer is unavailable")
	}
	if result.AsOf.IsZero() || !scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowCanaryInputFingerprint(scope, result)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(result.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastCanary, result.AsOf, inputFingerprint, []rpc.AlertSource{rpc.AlertSourceCanary}, observedAt); handled {
		return snapshot, err
	}
	previous := state.sources[rpc.AlertSourceCanary]
	state.sources[rpc.AlertSourceCanary] = alertShadowMapCanary(scope, result, observedAt)
	cursor := alertShadowInputCursor{AsOf: result.AsOf.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, []rpc.AlertSource{rpc.AlertSourceCanary}, alertShadowCursorCanary, cursor)
	if err != nil {
		state.sources[rpc.AlertSourceCanary] = previous
		c.recordApplyFailureLocked(ctx, state, observedAt)
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastCanary = cursor
	return snapshot, nil
}

// ObserveNudges splits the canonical Nudge snapshot into three non-overlapping
// producer owners. policyFingerprint must be the current constitution semantic
// fingerprint, not candidate copy or rendered text.
func (c *alertShadowComposer) ObserveNudges(ctx context.Context, input alertShadowNudgeInput) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert shadow composer is unavailable")
	}
	result := input.Snapshot
	if result.AsOf.IsZero() || !input.Scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowNudgeInputFingerprint(input)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(input.Scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(result.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastNudges, result.AsOf, inputFingerprint, alertShadowNudgeSources[:], observedAt); handled {
		return snapshot, err
	}
	batches, duplicateCandidates, mapErr := alertShadowMapNudges(input, observedAt)
	if mapErr != nil {
		if dispositionErr := c.registry.RecordInputDisposition(ctx, input.Scope.authority, observedAt, alertShadowNudgeSources[:], alertShadowDispositionEquivocation); dispositionErr != nil {
			return rpc.AlertCandidateSnapshot{}, errors.Join(mapErr, dispositionErr)
		}
		snapshot, snapshotErr := c.currentSnapshotLocked(input.Scope)
		return snapshot, errors.Join(mapErr, snapshotErr)
	}
	previous := make(map[rpc.AlertSource]alertShadowSourceBatch, len(alertShadowNudgeSources))
	for _, source := range alertShadowNudgeSources {
		previous[source] = state.sources[source]
		batch := batches[source]
		batch.DuplicateCandidates = duplicateCandidates[source]
		state.sources[source] = batch
	}
	cursor := alertShadowInputCursor{AsOf: result.AsOf.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, alertShadowNudgeSources[:], alertShadowCursorNudges, cursor)
	if err != nil {
		for _, source := range alertShadowNudgeSources {
			state.sources[source] = previous[source]
		}
		c.recordApplyFailureLocked(ctx, state, observedAt)
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastNudges = cursor
	return snapshot, nil
}

// Snapshot returns only durable registry state. false means the composer has
// not committed an evaluation yet; callers must not manufacture a clear.
func (c *alertShadowComposer) Snapshot(scope alertShadowBrokerScope) (rpc.AlertCandidateSnapshot, bool, error) {
	if c == nil || c.registry == nil || !scope.valid() {
		return rpc.AlertCandidateSnapshot{}, false, errors.New("alert shadow composer is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, false, err
	}
	now := c.nowLocked()
	snapshot, ok, err := c.registry.Snapshot(scope.authority, now)
	if err != nil || !ok {
		return snapshot, ok, err
	}
	snapshot, err = c.projectProcessSnapshotLocked(snapshot, state, now)
	return snapshot, true, err
}

func (c *alertShadowComposer) Status(scope alertShadowBrokerScope) alertShadowStatusReport {
	report := alertShadowStatusReport{ExpectedSources: alertShadowExpectedSourceSlice(), HumanPrecision: alertShadowHumanLabelUnlabelled,
		HumanRecall: alertShadowHumanLabelUnlabelled, Sources: make([]alertShadowSourceStatus, 0, len(alertShadowExpectedSources))}
	if c == nil || c.registry == nil || !scope.valid() {
		return report
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowLocked()
	state, _ := c.scopeStateLocked(scope)
	durable, ok, err := c.registry.scopeState(scope.authority)
	if err != nil {
		report.LastErrorCode = alertShadowReasonRegistryApplyFailed
		return report
	}
	metricsBySource := make(map[rpc.AlertSource]alertShadowSourceMetrics)
	if ok {
		report.AsOf, report.Evaluations = durable.Metrics.AsOf, durable.Metrics.Evaluations
		report.RegistryApplyFailures, report.Equivocations = durable.Metrics.RegistryApplyFailures, durable.Metrics.Equivocations
		report.LastErrorCode = durable.Metrics.LastErrorCode
		for _, row := range durable.Metrics.Sources {
			metricsBySource[row.Source] = row.Measurements
		}
	}
	clockInvalid := ok && !durable.AsOf.IsZero() && durable.AsOf.After(now)
	if clockInvalid {
		report.LastErrorCode = alertShadowReasonSourceTimeInvalid
	}
	if state != nil && state.pendingApplyFailures > 0 {
		report.RegistryApplyFailures += state.pendingApplyFailures
		report.LastErrorCode = alertShadowReasonRegistryApplyFailed
	}
	activeBySource := make(map[rpc.AlertSource]int)
	if snapshot, snapshotOK, snapshotErr := c.registry.Snapshot(scope.authority, now); snapshotErr == nil && snapshotOK {
		for _, candidate := range snapshot.Candidates {
			if candidate.State != rpc.AlertEpisodeRecovered {
				activeBySource[candidate.Source]++
			}
		}
	}
	defaults := defaultAlertShadowSources(scope)
	for _, source := range alertShadowExpectedSources {
		batch := defaults[source]
		if state != nil {
			batch = state.sources[source]
		}
		covered := batch.Covered
		if covered && !batch.FreshUntil.IsZero() && now.After(batch.FreshUntil) {
			covered = false
			batch.Status, batch.Reason = alertShadowStatusStale, alertShadowReasonProducerSilent
		}
		if clockInvalid || (!batch.InputAsOf.IsZero() && batch.InputAsOf.After(now)) || (!batch.ObservedAt.IsZero() && batch.ObservedAt.After(now)) {
			covered = false
			batch.Status, batch.Reason = alertShadowStatusError, alertShadowReasonSourceTimeInvalid
		}
		report.Sources = append(report.Sources, alertShadowSourceStatus{
			Source: source, Status: batch.Status, Reason: batch.Reason, InputAsOf: batch.InputAsOf,
			ObservedAt: batch.ObservedAt, Covered: covered, Active: activeBySource[source], Measurements: metricsBySource[source],
		})
	}
	return report
}

func (c *alertShadowComposer) handleInputCursorLocked(ctx context.Context, state *alertShadowScopeState, cursor *alertShadowInputCursor, asOf time.Time, fingerprint string, sources []rpc.AlertSource, observedAt time.Time) (rpc.AlertCandidateSnapshot, bool, error) {
	asOf = asOf.UTC()
	disposition := ""
	var resultErr error
	if !cursor.AsOf.IsZero() && asOf.Before(cursor.AsOf) {
		disposition = alertShadowDispositionStale
	} else if asOf.Equal(cursor.AsOf) {
		if fingerprint != cursor.Fingerprint {
			disposition = alertShadowDispositionEquivocation
			resultErr = errors.New("alert shadow input timestamp equivocation")
		} else if alertShadowSourcesNeedObservation(state, sources) {
			// The first identical producer read after restart is a real process
			// observation, not a duplicate. Re-evaluate it once so current-process
			// coverage can be re-established without waiting for semantic churn.
			return rpc.AlertCandidateSnapshot{}, false, nil
		} else {
			disposition = alertShadowDispositionDuplicate
		}
	}
	if disposition == "" {
		return rpc.AlertCandidateSnapshot{}, false, nil
	}
	if err := c.registry.RecordInputDisposition(ctx, state.scope.authority, observedAt, sources, disposition); err != nil {
		return rpc.AlertCandidateSnapshot{}, true, err
	}
	snapshot, snapshotErr := c.currentSnapshotLocked(state.scope)
	return snapshot, true, errors.Join(resultErr, snapshotErr)
}

func alertShadowSourcesNeedObservation(state *alertShadowScopeState, sources []rpc.AlertSource) bool {
	if state == nil {
		return true
	}
	for _, source := range sources {
		batch := state.sources[source]
		if !batch.Covered && batch.Status == alertShadowStatusNotObserved && batch.Reason == alertShadowReasonNotObserved {
			return true
		}
	}
	return false
}

func (c *alertShadowComposer) currentSnapshotLocked(scope alertShadowBrokerScope) (rpc.AlertCandidateSnapshot, error) {
	now := c.nowLocked()
	snapshot, ok, err := c.registry.Snapshot(scope.authority, now)
	if err != nil || !ok {
		return rpc.AlertCandidateSnapshot{}, err
	}
	state := c.scopes[scope.authority]
	if state == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert shadow process scope is unavailable")
	}
	return c.projectProcessSnapshotLocked(snapshot, state, now)
}

func (c *alertShadowComposer) projectProcessSnapshotLocked(snapshot rpc.AlertCandidateSnapshot, state *alertShadowScopeState, now time.Time) (rpc.AlertCandidateSnapshot, error) {
	coverage := alertShadowCoverage(now, state.sources)
	if now.Before(snapshot.AsOf) {
		coverage = rpc.AlertCoverage{
			State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: snapshot.AsOf,
			ExpectedSources: append([]rpc.AlertSource(nil), coverage.ExpectedSources...), CoveredSources: []rpc.AlertSource{},
		}
	}
	covered := make(map[rpc.AlertSource]struct{}, len(coverage.CoveredSources))
	for _, source := range coverage.CoveredSources {
		covered[source] = struct{}{}
	}
	filtered := make([]rpc.AlertCandidate, 0, len(snapshot.Candidates))
	for _, candidate := range snapshot.Candidates {
		_, sourceCovered := covered[candidate.Source]
		if candidate.State == rpc.AlertEpisodeRecovered && (!state.applied || !sourceCovered) {
			continue
		}
		if candidate.EvidenceHealth == rpc.AlertEvidenceCurrent && !sourceCovered {
			health := state.sources[candidate.Source].EvidenceHealth
			if health == "" || health == rpc.AlertEvidenceCurrent {
				health = rpc.AlertEvidenceUnavailable
			}
			candidate.EvidenceHealth = health
		}
		filtered = append(filtered, candidate)
	}
	snapshot.Coverage = coverage
	snapshot.Candidates = filtered
	snapshot.CurrentState = rpc.AlertSnapshotUnknown
	for _, candidate := range filtered {
		if candidate.State == rpc.AlertEpisodeOpen || candidate.State == rpc.AlertEpisodeEscalated {
			snapshot.CurrentState = rpc.AlertSnapshotActive
			break
		}
	}
	if snapshot.CurrentState != rpc.AlertSnapshotActive && coverage.State == rpc.AlertCoverageComplete && coverage.Freshness == rpc.AlertCoverageCurrent {
		snapshot.CurrentState = rpc.AlertSnapshotClear
	}
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	return snapshot, nil
}

func (c *alertShadowComposer) recordApplyFailureLocked(ctx context.Context, state *alertShadowScopeState, at time.Time) {
	// The failed evaluation context may already be cancelled. A bounded,
	// cancellation-detached follow-up may still persist measurement evidence
	// without advancing producer cursors or lifecycle. If SQLite itself remains
	// unavailable, retain an in-memory pending count for the next successful
	// evaluation; there is no alternate file authority.
	persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := c.registry.RecordApplyFailure(persistContext, state.scope.authority, at); err != nil {
		state.pendingApplyFailures++
	}
}

func (c *alertShadowComposer) nowLocked() time.Time {
	now := time.Now().UTC()
	if c.now != nil {
		now = c.now().UTC()
	}
	return now
}

func (c *alertShadowComposer) observationTimeLocked(sourceAsOf time.Time) (time.Time, error) {
	now := c.nowLocked()
	if now.Before(sourceAsOf) {
		return time.Time{}, errors.New("alert shadow producer as_of is in the future")
	}
	return now, nil
}

func (c *alertShadowComposer) applyLocked(ctx context.Context, state *alertShadowScopeState, evaluationAt time.Time, opportunitySources []rpc.AlertSource, cursorKind string, cursor alertShadowInputCursor) (rpc.AlertCandidateSnapshot, error) {
	evaluationAt = evaluationAt.UTC()
	if durable, ok, err := c.registry.scopeState(state.scope.authority); err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	} else if ok && evaluationAt.Before(durable.AsOf) {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert shadow authority clock is behind durable scoped state")
	}
	updated := make(map[rpc.AlertSource]struct{}, len(opportunitySources))
	for _, source := range opportunitySources {
		updated[source] = struct{}{}
		batch := state.sources[source]
		batch.ObservedAt = evaluationAt
		for i := range batch.Observations {
			batch.Observations[i].ObservedAt = evaluationAt
		}
		state.sources[source] = batch
	}
	before, beforeOK, err := c.registry.Snapshot(state.scope.authority, evaluationAt)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observations := make([]alertEpisodeObservation, 0)
	represented := make(map[string]struct{})
	for _, source := range opportunitySources {
		batch := state.sources[source]
		for _, observation := range batch.Observations {
			observations = append(observations, observation)
			if observation.Active {
				represented[observation.EpisodeKey] = struct{}{}
			}
		}
	}
	if beforeOK {
		for _, candidate := range before.Candidates {
			if candidate.State == rpc.AlertEpisodeRecovered {
				continue
			}
			if _, present := represented[candidate.EpisodeKey]; present {
				continue
			}
			if _, isUpdated := updated[candidate.Source]; !isUpdated {
				continue
			}
			batch := state.sources[candidate.Source]
			if !batch.NegativeReady || !alertShadowCandidateMatchesScope(candidate, batch.Scope) {
				continue
			}
			reason := alertShadowDecisionClassifiedNegativeUntrusted
			if batch.Covered && batch.EvidenceHealth == rpc.AlertEvidenceCurrent {
				reason = alertShadowDecisionClassifiedClear
			}
			evidenceFingerprint := batch.NegativeEvidenceFingerprint
			if evidenceFingerprint == "" && !batch.Covered {
				// An untrusted negative may update health/source time, but must
				// retain the producer identity needed to prove broker scope on a
				// later authoritative observation.
				evidenceFingerprint = candidate.EvidenceFingerprint
			}
			if evidenceFingerprint == "" {
				evidenceFingerprint, err = alertShadowFingerprint(struct {
					Decision          string          `json:"decision"`
					Source            rpc.AlertSource `json:"source"`
					EpisodeKey        string          `json:"episode_key"`
					PolicyFingerprint string          `json:"policy_fingerprint"`
				}{"negative", candidate.Source, candidate.EpisodeKey, batch.PolicyFingerprint})
				if err != nil {
					return rpc.AlertCandidateSnapshot{}, err
				}
			}
			severity := candidate.Severity
			if batch.NegativeSeverity != "" {
				severity = batch.NegativeSeverity
			}
			destination := candidate.Destination
			if batch.NegativeDestination != "" {
				destination = batch.NegativeDestination
			}
			observations = append(observations, alertEpisodeObservation{
				EpisodeKey: candidate.EpisodeKey, Source: candidate.Source, Kind: candidate.Kind,
				Active: false, Severity: severity, DeliveryPreference: rpc.AlertDeliveryUnapproved,
				EvidenceFingerprint: evidenceFingerprint, EvidenceHealth: batch.EvidenceHealth,
				Destination: destination, EvidenceAsOf: batch.EvidenceAsOf, ObservedAt: evaluationAt,
				PolicyFingerprint: batch.PolicyFingerprint, ProducerDecisionReason: reason,
			})
		}
	}
	coverage := alertShadowCoverage(evaluationAt, state.sources)
	sourceStates := make([]alertEpisodeRegistrySourceState, 0, len(opportunitySources))
	for _, source := range opportunitySources {
		sourceStates = append(sourceStates, alertShadowRegistrySourceState(state.sources[source]))
	}
	snapshot, err := c.registry.Apply(ctx, alertEpisodeEvaluation{AuthorityScope: state.scope.authority, AsOf: evaluationAt,
		Coverage: coverage, Observations: observations, OpportunitySources: append([]rpc.AlertSource(nil), opportunitySources...),
		SourceStates: sourceStates, CursorKind: cursorKind, Cursor: cursor, PendingApplyFailures: state.pendingApplyFailures})
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.pendingApplyFailures = 0
	state.applied = true
	return snapshot, nil
}

func alertShadowRegistrySourceState(batch alertShadowSourceBatch) alertEpisodeRegistrySourceState {
	return alertEpisodeRegistrySourceState{Source: batch.Source, Status: batch.Status, Reason: batch.Reason,
		InputAsOf: batch.InputAsOf, ObservedAt: batch.ObservedAt, EvidenceAsOf: batch.EvidenceAsOf,
		EvidenceHealth: batch.EvidenceHealth, Covered: batch.Covered, FreshUntil: batch.FreshUntil,
		DuplicateCandidates: batch.DuplicateCandidates}
}

func alertShadowCoverage(asOf time.Time, sources map[rpc.AlertSource]alertShadowSourceBatch) rpc.AlertCoverage {
	covered := make([]rpc.AlertSource, 0, len(alertShadowExpectedSources))
	stale := false
	for _, source := range alertShadowExpectedSources {
		batch := sources[source]
		if batch.Covered {
			covered = append(covered, source)
			if !batch.FreshUntil.IsZero() && asOf.After(batch.FreshUntil) {
				stale = true
			}
		}
	}
	coverage := rpc.AlertCoverage{
		State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: asOf,
		ExpectedSources: alertShadowExpectedSourceSlice(), CoveredSources: covered,
	}
	if len(covered) > 0 {
		coverage.State = rpc.AlertCoveragePartial
		coverage.Freshness = rpc.AlertCoverageCurrent
		if len(covered) == len(coverage.ExpectedSources) {
			coverage.State = rpc.AlertCoverageComplete
		}
		if stale {
			coverage.Freshness = rpc.AlertCoverageStale
		}
	}
	return coverage
}

func alertShadowExpectedSourceSlice() []rpc.AlertSource {
	out := make([]rpc.AlertSource, len(alertShadowExpectedSources))
	copy(out, alertShadowExpectedSources[:])
	return out
}

func alertShadowMapCanary(scope alertShadowBrokerScope, result rpc.CanaryResult, observedAt time.Time) alertShadowSourceBatch {
	batch := alertShadowSourceBatch{
		Source: rpc.AlertSourceCanary, Status: alertShadowStatusUnavailable, Reason: alertShadowReasonSourceHealthUnavailable,
		InputAsOf: result.AsOf.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: result.AsOf.UTC(),
		EvidenceHealth: rpc.AlertEvidenceUnavailable, PolicyFingerprint: result.PolicyFingerprint.Key,
		NegativeEvidenceFingerprint: result.Fingerprint.Key, NegativeDestination: rpc.AlertDestinationAlerts,
		FreshUntil:   alertShadowCanaryFreshUntil(result, observedAt),
		Scope:        scope,
		Observations: []alertEpisodeObservation{},
	}
	covered, health, reason, evidenceAsOf := alertShadowCanaryHealth(result)
	batch.Covered = covered
	batch.EvidenceHealth = health
	batch.Reason = reason
	if !evidenceAsOf.IsZero() {
		batch.EvidenceAsOf = evidenceAsOf
	}
	batch.Status = alertShadowStatusForEvidence(health)
	if covered {
		batch.Status = alertShadowStatusCurrent
		batch.Reason = alertShadowReasonCurrent
	}
	if result.Fingerprint.Version != rpc.CanaryFingerprintVersion || !validAlertRegistryFingerprint(result.Fingerprint.Key) {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status = alertShadowStatusError
		batch.Reason = alertShadowReasonEvidenceFingerprintInvalid
		batch.EvidenceHealth = rpc.AlertEvidenceError
		return batch
	}
	if result.PolicyFingerprint.Version != risk.CanaryPolicyFingerprintVersion || !validAlertRegistryFingerprint(result.PolicyFingerprint.Key) {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status = alertShadowStatusError
		batch.Reason = alertShadowReasonPolicyFingerprintInvalid
		batch.EvidenceHealth = rpc.AlertEvidenceError
		return batch
	}
	severity, ok := alertShadowCanarySeverity(result.Severity)
	if !ok {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status = alertShadowStatusError
		batch.Reason = alertShadowReasonCandidateInvalid
		batch.EvidenceHealth = rpc.AlertEvidenceError
		return batch
	}
	batch.NegativeSeverity = severity
	batch.NegativeReady = !batch.EvidenceAsOf.IsZero()
	if !alertShadowCanaryOccurrenceEligible(result) {
		return batch
	}
	episodeKey, err := rpc.BuildAlertEpisodeKey(
		rpc.AlertSourceCanary, rpc.AlertKindPortfolioRisk,
		scope.account, scope.mode, "portfolio_canary",
	)
	if err != nil {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status = alertShadowStatusError
		batch.Reason = alertShadowReasonCandidateInvalid
		batch.EvidenceHealth = rpc.AlertEvidenceError
		return batch
	}
	batch.Observations = append(batch.Observations, alertEpisodeObservation{
		EpisodeKey: episodeKey, Source: rpc.AlertSourceCanary, Kind: rpc.AlertKindPortfolioRisk,
		Active: true, Severity: severity, DeliveryPreference: rpc.AlertDeliveryUnapproved,
		EvidenceFingerprint: result.Fingerprint.Key, EvidenceHealth: batch.EvidenceHealth,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: batch.EvidenceAsOf, ObservedAt: observedAt.UTC(),
		PolicyFingerprint: result.PolicyFingerprint.Key, ProducerDecisionReason: alertShadowDecisionLegacyGateActive,
	})
	return batch
}

func alertShadowCanaryFreshUntil(result rpc.CanaryResult, observedAt time.Time) time.Time {
	deadline := observedAt.UTC().Add(alertShadowCanarySilenceHorizon)
	for _, source := range result.SourceHealth {
		if source.AsOf.IsZero() || source.MaxAgeSeconds <= 0 {
			continue
		}
		candidate := source.AsOf.UTC().Add(time.Duration(source.MaxAgeSeconds) * time.Second)
		if candidate.Before(deadline) {
			deadline = candidate
		}
	}
	if deadline.Before(observedAt) {
		return observedAt.UTC()
	}
	return deadline
}

func alertShadowCanaryHealth(result rpc.CanaryResult) (bool, rpc.AlertEvidenceHealth, string, time.Time) {
	if result.PortfolioAlertRelevant == nil {
		return false, rpc.AlertEvidencePartial, alertShadowReasonMissingRelevanceStamp, alertShadowOldestCanarySourceTime(result)
	}
	if strings.TrimSpace(result.InputHealth) != "ok" {
		return false, rpc.AlertEvidencePartial, alertShadowReasonInputHealthNotOK, alertShadowOldestCanarySourceTime(result)
	}
	if len(result.SourceHealth) == 0 {
		return false, rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthMissing, result.AsOf.UTC()
	}
	required := map[string]bool{"account": false, "positions": false, "regime": false}
	seen := make(map[string]struct{}, len(result.SourceHealth))
	evidenceAsOf := time.Time{}
	worstHealth := rpc.AlertEvidenceCurrent
	worstReason := alertShadowReasonCurrent
	for _, source := range result.SourceHealth {
		name := strings.TrimSpace(source.Source)
		if _, duplicate := seen[name]; duplicate || name == "" {
			return false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, alertShadowOldestCanarySourceTime(result)
		}
		seen[name] = struct{}{}
		if _, ok := required[name]; ok {
			required[name] = true
		} else if name != "market_events" {
			return false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, alertShadowOldestCanarySourceTime(result)
		}
		if source.AsOf.IsZero() || source.AsOf.After(result.AsOf) {
			return false, rpc.AlertEvidenceError, alertShadowReasonSourceTimeInvalid, alertShadowOldestCanarySourceTime(result)
		}
		if source.Fingerprint == nil || strings.TrimSpace(source.Fingerprint.Version) == "" || !validAlertRegistryFingerprint(source.Fingerprint.Key) {
			return false, rpc.AlertEvidenceError, alertShadowReasonEvidenceFingerprintInvalid, alertShadowOldestCanarySourceTime(result)
		}
		if evidenceAsOf.IsZero() || source.AsOf.Before(evidenceAsOf) {
			evidenceAsOf = source.AsOf.UTC()
		}
		rowHealth, rowReason := alertShadowCanarySourceHealth(source)
		if alertShadowEvidenceRank(rowHealth) > alertShadowEvidenceRank(worstHealth) {
			worstHealth, worstReason = rowHealth, rowReason
		}
	}
	for _, present := range required {
		if !present {
			return false, rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete, evidenceAsOf
		}
	}
	if worstHealth != rpc.AlertEvidenceCurrent {
		return false, worstHealth, worstReason, evidenceAsOf
	}
	return true, rpc.AlertEvidenceCurrent, alertShadowReasonCurrent, evidenceAsOf
}

func alertShadowCanarySourceHealth(source rpc.SourceHealth) (rpc.AlertEvidenceHealth, string) {
	if source.MaxAgeSeconds > 0 && source.AgeSeconds > source.MaxAgeSeconds {
		return rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale
	}
	switch source.Status {
	case rpc.SourceStatusOK:
		return rpc.AlertEvidenceCurrent, alertShadowReasonCurrent
	case rpc.SourceStatusStale:
		return rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale
	case rpc.SourceStatusPartial, rpc.SourceStatusDegraded:
		return rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete
	case rpc.SourceStatusUnknown:
		return rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthUnavailable
	default:
		return rpc.AlertEvidenceError, alertShadowReasonSourceHealthError
	}
}

func alertShadowOldestCanarySourceTime(result rpc.CanaryResult) time.Time {
	oldest := time.Time{}
	for _, source := range result.SourceHealth {
		if source.AsOf.IsZero() {
			continue
		}
		if oldest.IsZero() || source.AsOf.Before(oldest) {
			oldest = source.AsOf.UTC()
		}
	}
	if oldest.IsZero() {
		return result.AsOf.UTC()
	}
	return oldest
}

func alertShadowCanaryOccurrenceEligible(result rpc.CanaryResult) bool {
	// This is intentionally identical to internal/app/alerts' approved legacy
	// occurrence policy. Nil relevance fails open for the positive gate only.
	relevant := result.PortfolioAlertRelevant == nil || *result.PortfolioAlertRelevant
	return relevant && (alertShadowCanarySeverityAtLeast(result.Severity, risk.SeverityWatch) ||
		alertShadowCanarySeverityAtLeast(result.Severity, risk.SeverityAct) ||
		result.Action == "defend" || result.Action == "rebalance" || result.Action == "confirm_inputs")
}

func alertShadowCanarySeverityAtLeast(got, want risk.SignalSeverity) bool {
	rank := map[risk.SignalSeverity]int{
		risk.SeverityObserve: 0, risk.SeverityWatch: 1, risk.SeverityAct: 2, risk.SeverityUrgent: 3,
	}
	return rank[got] >= rank[want]
}

func alertShadowCanarySeverity(severity risk.SignalSeverity) (rpc.AlertSeverity, bool) {
	switch severity {
	case risk.SeverityObserve:
		return rpc.AlertSeverityObserve, true
	case risk.SeverityWatch:
		return rpc.AlertSeverityWatch, true
	case risk.SeverityAct:
		return rpc.AlertSeverityAct, true
	case risk.SeverityUrgent:
		return rpc.AlertSeverityUrgent, true
	default:
		return "", false
	}
}

func alertShadowMapNudges(input alertShadowNudgeInput, observedAt time.Time) (map[rpc.AlertSource]alertShadowSourceBatch, map[rpc.AlertSource]uint64, error) {
	result := input.Snapshot
	policyFingerprint := input.PolicyFingerprint
	batches := make(map[rpc.AlertSource]alertShadowSourceBatch, len(alertShadowNudgeSources))
	duplicates := make(map[rpc.AlertSource]uint64, len(alertShadowNudgeSources))
	health := rpc.NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates))
	validSnapshot := true
	if _, err := json.Marshal(result); err != nil {
		validSnapshot = false
	}
	policyValid := policyFingerprint.Version == rpc.RiskConstitutionFingerprintVersion && validAlertRegistryFingerprint(policyFingerprint.Key)
	storeValid, storeReason := alertShadowNudgeStoreHealthValid(input.StoreHealth, result.AsOf)
	for _, source := range alertShadowNudgeSources {
		covered, evidenceHealth, reason, evidenceAsOf := alertShadowNudgeHealth(source, result.AsOf, health, input.StoreHealth)
		batch := alertShadowSourceBatch{
			Source: source, Status: alertShadowStatusForEvidence(evidenceHealth), Reason: reason,
			InputAsOf: result.AsOf.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: evidenceAsOf,
			EvidenceHealth: evidenceHealth, PolicyFingerprint: policyFingerprint.Key,
			FreshUntil: observedAt.UTC().Add(alertShadowNudgeSilenceHorizon),
			Scope:      input.Scope,
			Covered:    covered, NegativeReady: policyValid && !evidenceAsOf.IsZero(), Observations: []alertEpisodeObservation{},
		}
		if covered {
			batch.Status = alertShadowStatusCurrent
			batch.Reason = alertShadowReasonCurrent
		}
		if !policyValid {
			batch.Covered = false
			batch.NegativeReady = false
			batch.Status = alertShadowStatusError
			batch.Reason = alertShadowReasonPolicyFingerprintInvalid
			batch.EvidenceHealth = rpc.AlertEvidenceError
		}
		if !validSnapshot {
			batch.Covered = false
			batch.Status = alertShadowStatusError
			batch.Reason = alertShadowReasonSnapshotInvalid
			batch.EvidenceHealth = rpc.AlertEvidenceError
		}
		if !storeValid {
			batch.Covered = false
			batch.NegativeReady = false
			batch.Status = alertShadowStatusError
			batch.Reason = storeReason
			batch.EvidenceHealth = rpc.AlertEvidenceError
		}
		batches[source] = batch
	}
	if !validSnapshot || !policyValid || !storeValid {
		return batches, duplicates, nil
	}

	seen := make(map[string]alertEpisodeObservation)
	for _, wireCandidate := range result.Candidates {
		canonical, err := risk.CanonicalizeNudgeCandidate(risk.NudgeCandidate{
			Fingerprint: wireCandidate.Fingerprint, Kind: wireCandidate.Kind, State: wireCandidate.State,
			Severity: wireCandidate.Severity, Title: wireCandidate.Title, Body: wireCandidate.Body,
			OccurredAt: wireCandidate.OccurredAt, DueAt: wireCandidate.DueAt,
			ExpiresAt: wireCandidate.ExpiresAt, Destination: wireCandidate.Destination,
		})
		if err != nil || canonical.OccurredAt.After(result.AsOf) {
			for _, source := range alertShadowNudgeSources {
				batch := batches[source]
				batch.Covered = false
				batch.Status = alertShadowStatusError
				batch.Reason = alertShadowReasonCandidateInvalid
				batch.EvidenceHealth = rpc.AlertEvidenceError
				batches[source] = batch
			}
			continue
		}
		source, kind, ok := alertShadowNudgeOwner(canonical.Kind)
		if !ok {
			for _, owner := range alertShadowNudgeSources {
				batch := batches[owner]
				batch.Covered = false
				batch.Status = alertShadowStatusError
				batch.Reason = alertShadowReasonCandidateInvalid
				batch.EvidenceHealth = rpc.AlertEvidenceError
				batches[owner] = batch
			}
			continue
		}
		severity, severityOK := alertShadowNudgeSeverity(canonical.Severity)
		destination, destinationOK := alertShadowNudgeDestination(canonical.Destination)
		if !severityOK || !destinationOK || !validAlertRegistryFingerprint(canonical.Fingerprint) {
			batch := batches[source]
			batch.Covered = false
			batch.Status = alertShadowStatusError
			batch.Reason = alertShadowReasonCandidateInvalid
			batch.EvidenceHealth = rpc.AlertEvidenceError
			batches[source] = batch
			continue
		}
		episodeKey, err := rpc.BuildAlertEpisodeKey(
			source, kind, input.Scope.account, input.Scope.mode, canonical.Fingerprint,
		)
		if err != nil {
			return nil, nil, err
		}
		batch := batches[source]
		observation := alertEpisodeObservation{
			EpisodeKey: episodeKey, Source: source, Kind: kind, Active: true, Severity: severity,
			DeliveryPreference: rpc.AlertDeliveryUnapproved, EvidenceFingerprint: canonical.Fingerprint,
			EvidenceHealth: batch.EvidenceHealth, Destination: destination, EvidenceAsOf: canonical.OccurredAt.UTC(),
			ObservedAt: observedAt.UTC(), PolicyFingerprint: policyFingerprint.Key,
			ProducerDecisionReason: alertShadowDecisionNudgeActive,
		}
		if previous, duplicate := seen[episodeKey]; duplicate {
			if !alertShadowObservationsEquivalent(previous, observation) {
				return nil, nil, errors.New("alert shadow nudge candidate equivocation")
			}
			duplicates[source]++
			continue
		}
		seen[episodeKey] = observation
		batch.Observations = append(batch.Observations, observation)
		batches[source] = batch
	}
	for _, source := range alertShadowNudgeSources {
		batch := batches[source]
		if batch.Status == alertShadowStatusError {
			batch.Covered = false
			for i := range batch.Observations {
				batch.Observations[i].EvidenceHealth = rpc.AlertEvidenceError
			}
		}
		sort.Slice(batch.Observations, func(i, j int) bool {
			return batch.Observations[i].EpisodeKey < batch.Observations[j].EpisodeKey
		})
		batches[source] = batch
	}
	return batches, duplicates, nil
}

func alertShadowNudgeHealth(source rpc.AlertSource, asOf time.Time, health rpc.NudgeSourceHealth, storeHealth rpc.NudgeInputHealth) (bool, rpc.AlertEvidenceHealth, string, time.Time) {
	inputs := alertShadowNudgeRequiredHealth(source, health, storeHealth)
	oldest := time.Time{}
	worst := rpc.AlertEvidenceCurrent
	reason := alertShadowReasonCurrent
	for _, input := range inputs {
		if input.AsOf.IsZero() || input.AsOf.After(asOf) {
			return false, rpc.AlertEvidenceError, alertShadowReasonHealthTimeInvalid, oldest
		}
		if oldest.IsZero() || input.AsOf.Before(oldest) {
			oldest = input.AsOf.UTC()
		}
		candidateHealth, candidateReason := alertShadowNudgeInputEvidence(input)
		if alertShadowEvidenceRank(candidateHealth) > alertShadowEvidenceRank(worst) {
			worst, reason = candidateHealth, candidateReason
		}
	}
	if worst != rpc.AlertEvidenceCurrent {
		return false, worst, reason, oldest
	}
	return true, rpc.AlertEvidenceCurrent, alertShadowReasonCurrent, oldest
}

func alertShadowNudgeRequiredHealth(source rpc.AlertSource, health rpc.NudgeSourceHealth, storeHealth rpc.NudgeInputHealth) []rpc.NudgeInputHealth {
	switch source {
	case rpc.AlertSourceRiskPolicy:
		return []rpc.NudgeInputHealth{health.Policy, health.Capital, health.Pins, storeHealth}
	case rpc.AlertSourceReconciliation:
		return []rpc.NudgeInputHealth{health.Policy, health.Reconciliation, health.Capital, health.Cadence, health.ConfirmedFlow, storeHealth}
	case rpc.AlertSourceGovernance:
		return []rpc.NudgeInputHealth{health.Policy, health.Pins, health.Cadence, storeHealth}
	default:
		return []rpc.NudgeInputHealth{{Status: rpc.NudgeInputStatusError, Reason: rpc.NudgeHealthReasonInvalid}}
	}
}

func alertShadowNudgeInputEvidence(input rpc.NudgeInputHealth) (rpc.AlertEvidenceHealth, string) {
	switch input.Status {
	case rpc.NudgeInputStatusOK:
		return rpc.AlertEvidenceCurrent, alertShadowReasonCurrent
	case rpc.NudgeInputStatusUnapproved:
		return rpc.AlertEvidencePartial, alertShadowReasonHealthUnapproved
	case rpc.NudgeInputStatusStale:
		return rpc.AlertEvidenceStale, alertShadowReasonHealthStale
	case rpc.NudgeInputStatusUnavailable:
		return rpc.AlertEvidenceUnavailable, alertShadowReasonHealthUnavailable
	case rpc.NudgeInputStatusError:
		return rpc.AlertEvidenceError, alertShadowReasonHealthError
	default:
		return rpc.AlertEvidenceError, alertShadowReasonHealthError
	}
}

func alertShadowNudgeStoreHealthValid(health rpc.NudgeInputHealth, asOf time.Time) (bool, string) {
	if health.AsOf.IsZero() || !health.AsOf.Equal(asOf) {
		return false, alertShadowReasonHealthTimeInvalid
	}
	switch health.Status {
	case rpc.NudgeInputStatusOK:
		return health.Reason == rpc.NudgeHealthReasonNone, alertShadowReasonHealthError
	case rpc.NudgeInputStatusStale:
		return health.Reason == rpc.NudgeHealthReasonEvidenceStale, alertShadowReasonHealthError
	case rpc.NudgeInputStatusUnavailable:
		return health.Reason == rpc.NudgeHealthReasonSourceUnavailable, alertShadowReasonHealthError
	case rpc.NudgeInputStatusError:
		return health.Reason == rpc.NudgeHealthReasonEvaluationError || health.Reason == rpc.NudgeHealthReasonInvalid, alertShadowReasonHealthError
	default:
		return false, alertShadowReasonHealthError
	}
}

func alertShadowNudgeOwner(kind string) (rpc.AlertSource, rpc.AlertKind, bool) {
	switch kind {
	case rpc.NudgeKindShadowWouldBlock:
		return rpc.AlertSourceRiskPolicy, rpc.AlertKindPortfolioRisk, true
	case rpc.NudgeKindDrawdownLatched:
		return rpc.AlertSourceRiskPolicy, rpc.AlertKindDrawdown, true
	case rpc.NudgeKindPolicyDrift:
		return rpc.AlertSourceRiskPolicy, rpc.AlertKindPolicyDrift, true
	case rpc.NudgeKindReconcileDue:
		return rpc.AlertSourceReconciliation, rpc.AlertKindGovernance, true
	case rpc.NudgeKindReconcileException:
		return rpc.AlertSourceReconciliation, rpc.AlertKindReconciliationException, true
	case rpc.NudgeKindConfirmedFlow:
		return rpc.AlertSourceReconciliation, rpc.AlertKindGovernance, true
	case rpc.NudgeKindMonthlyPulse:
		return rpc.AlertSourceGovernance, rpc.AlertKindGovernance, true
	default:
		return "", "", false
	}
}

func alertShadowCandidateMatchesScope(candidate rpc.AlertCandidate, scope alertShadowBrokerScope) bool {
	if !scope.valid() {
		return false
	}
	identity := candidate.EvidenceFingerprint
	if candidate.Source == rpc.AlertSourceCanary {
		identity = "portfolio_canary"
	}
	key, err := rpc.BuildAlertEpisodeKey(
		candidate.Source, candidate.Kind, scope.account, scope.mode, identity,
	)
	return err == nil && key == candidate.EpisodeKey
}

func alertShadowNudgeSeverity(severity string) (rpc.AlertSeverity, bool) {
	switch severity {
	case rpc.NudgeSeverityWatch:
		return rpc.AlertSeverityWatch, true
	case rpc.NudgeSeverityAct:
		return rpc.AlertSeverityAct, true
	default:
		return "", false
	}
}

func alertShadowNudgeDestination(destination string) (rpc.AlertDestination, bool) {
	switch destination {
	case rpc.NudgeDestinationMonitor:
		return rpc.AlertDestinationMonitor, true
	case rpc.NudgeDestinationAlerts:
		return rpc.AlertDestinationAlerts, true
	case rpc.NudgeDestinationBrief:
		return rpc.AlertDestinationBrief, true
	default:
		return "", false
	}
}

func alertShadowStatusForEvidence(health rpc.AlertEvidenceHealth) string {
	switch health {
	case rpc.AlertEvidenceCurrent:
		return alertShadowStatusCurrent
	case rpc.AlertEvidencePartial:
		return alertShadowStatusPartial
	case rpc.AlertEvidenceStale:
		return alertShadowStatusStale
	case rpc.AlertEvidenceUnavailable:
		return alertShadowStatusUnavailable
	default:
		return alertShadowStatusError
	}
}

func alertShadowEvidenceRank(health rpc.AlertEvidenceHealth) int {
	switch health {
	case rpc.AlertEvidenceCurrent:
		return 0
	case rpc.AlertEvidencePartial:
		return 1
	case rpc.AlertEvidenceUnavailable:
		return 2
	case rpc.AlertEvidenceStale:
		return 3
	case rpc.AlertEvidenceError:
		return 4
	default:
		return 5
	}
}

func alertShadowCanaryInputFingerprint(scope alertShadowBrokerScope, result rpc.CanaryResult) (string, error) {
	type sourceHealth struct {
		Source      string           `json:"source"`
		Status      string           `json:"status"`
		AsOf        time.Time        `json:"as_of"`
		Age         int64            `json:"age"`
		MaxAge      int64            `json:"max_age"`
		Fingerprint *rpc.Fingerprint `json:"fingerprint,omitempty"`
	}
	health := make([]sourceHealth, 0, len(result.SourceHealth))
	for _, source := range result.SourceHealth {
		health = append(health, sourceHealth{
			Source: source.Source, Status: source.Status, AsOf: source.AsOf, Age: source.AgeSeconds,
			MaxAge: source.MaxAgeSeconds, Fingerprint: source.Fingerprint,
		})
	}
	sort.Slice(health, func(i, j int) bool { return health[i].Source < health[j].Source })
	return alertShadowFingerprint(struct {
		Account           string              `json:"account"`
		Mode              string              `json:"mode"`
		AsOf              time.Time           `json:"as_of"`
		Fingerprint       rpc.Fingerprint     `json:"fingerprint"`
		PolicyFingerprint rpc.Fingerprint     `json:"policy_fingerprint"`
		Action            string              `json:"action"`
		Severity          risk.SignalSeverity `json:"severity"`
		Relevance         *bool               `json:"relevance,omitempty"`
		InputHealth       string              `json:"input_health"`
		SourceHealth      []sourceHealth      `json:"source_health"`
	}{
		Account: scope.account, Mode: scope.mode, AsOf: result.AsOf.UTC(),
		Fingerprint: result.Fingerprint, PolicyFingerprint: result.PolicyFingerprint,
		Action: result.Action, Severity: result.Severity, Relevance: result.PortfolioAlertRelevant,
		InputHealth: result.InputHealth, SourceHealth: health,
	})
}

func alertShadowNudgeInputFingerprint(input alertShadowNudgeInput) (string, error) {
	result := input.Snapshot
	type candidate struct {
		Fingerprint string    `json:"fingerprint"`
		Kind        string    `json:"kind"`
		State       string    `json:"state"`
		OccurredAt  time.Time `json:"occurred_at"`
		DueAt       time.Time `json:"due_at"`
	}
	candidates := make([]candidate, 0, len(result.Candidates))
	for _, item := range result.Candidates {
		candidates = append(candidates, candidate{
			Fingerprint: item.Fingerprint, Kind: item.Kind, State: item.State,
			OccurredAt: item.OccurredAt, DueAt: item.DueAt,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Kind != candidates[j].Kind {
			return candidates[i].Kind < candidates[j].Kind
		}
		return candidates[i].Fingerprint < candidates[j].Fingerprint
	})
	return alertShadowFingerprint(struct {
		Account               string                          `json:"account"`
		Mode                  string                          `json:"mode"`
		AsOf                  time.Time                       `json:"as_of"`
		PolicyFingerprint     rpc.Fingerprint                 `json:"policy_fingerprint"`
		Candidates            []candidate                     `json:"candidates"`
		SourceHealth          rpc.NudgeSourceHealth           `json:"source_health"`
		StoreHealth           rpc.NudgeInputHealth            `json:"store_health"`
		ConfirmedFlowCoverage *rpc.NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
	}{
		input.Scope.account, input.Scope.mode, result.AsOf.UTC(), input.PolicyFingerprint, candidates,
		rpc.NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates)), input.StoreHealth, result.ConfirmedFlowCoverage,
	})
}

func alertShadowObservationsEquivalent(left, right alertEpisodeObservation) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func alertShadowFingerprint(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint alert shadow value: %w", err)
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
