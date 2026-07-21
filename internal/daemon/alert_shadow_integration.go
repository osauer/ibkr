package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

const alertShadowAuthority = "shadow"

func (s *Server) attachAlertShadowAuthority(ctx context.Context) error {
	if s == nil || s.coreStore == nil {
		return errors.New("alert shadow SQLite authority is unavailable")
	}
	registry, err := newAlertEpisodeRegistry(ctx, s.coreStore)
	if err != nil {
		return fmt.Errorf("load alert episode registry: %w", err)
	}
	s.alertEpisodes = registry
	s.alertShadow = newAlertShadowComposer(registry)
	return nil
}

func (s *Server) handleAlertCandidates(ctx context.Context, req *rpc.Request) (*rpc.AlertCandidateSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Params) > 0 {
		var params rpc.AlertCandidatesParams
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, err
		}
	}
	if s == nil || s.alertShadow == nil {
		return nil, errors.New("alert shadow authority is unavailable")
	}
	scope, err := newAlertShadowBrokerScope(s.currentBrokerStateScope())
	if err != nil {
		return nil, errors.New("alert shadow authority scope is unavailable")
	}
	snapshot, ok, err := s.alertShadow.Snapshot(scope)
	if err != nil {
		return nil, err
	}
	if !ok {
		now := time.Now().UTC()
		if s.now != nil {
			now = s.now().UTC()
		}
		snapshot = unavailableAlertCandidateSnapshot(now, scope.authority)
	}
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		return nil, fmt.Errorf("validate alert candidate snapshot: %w", err)
	}
	return &snapshot, nil
}

func unavailableAlertCandidateSnapshot(asOf time.Time, authorityScope string) rpc.AlertCandidateSnapshot {
	asOf = asOf.UTC()
	return rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AuthorityScope: authorityScope,
		AsOf: asOf, CurrentState: rpc.AlertSnapshotUnknown,
		Coverage: rpc.AlertCoverage{
			State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: asOf,
			ExpectedSources: alertShadowExpectedSourceSlice(), CoveredSources: []rpc.AlertSource{},
		},
		Candidates: []rpc.AlertCandidate{},
	}
}

func (s *Server) handleAlertShadowStatus(ctx context.Context, req *rpc.Request) (*rpc.AlertShadowStatusResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Params) > 0 {
		var params rpc.AlertShadowStatusParams
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, err
		}
	}
	if s == nil || s.alertShadow == nil {
		return nil, errors.New("alert shadow authority is unavailable")
	}
	scope, err := newAlertShadowBrokerScope(s.currentBrokerStateScope())
	if err != nil {
		return nil, errors.New("alert shadow authority scope is unavailable")
	}
	status := s.alertShadow.Status(scope)
	result := &rpc.AlertShadowStatusResult{
		AsOf: status.AsOf, Authority: alertShadowAuthority, DeliveryActive: false,
		ExpectedSources: append([]rpc.AlertSource(nil), status.ExpectedSources...),
		Evaluations:     status.Evaluations, RegistryApplyFailures: status.RegistryApplyFailures,
		Equivocations: status.Equivocations, LastErrorCode: status.LastErrorCode,
		HumanPrecision: status.HumanPrecision, HumanRecall: status.HumanRecall,
		Sources: make([]rpc.AlertShadowSourceStatus, 0, len(status.Sources)),
	}
	for _, source := range status.Sources {
		m := source.Measurements
		result.Sources = append(result.Sources, rpc.AlertShadowSourceStatus{
			Source: source.Source, Status: source.Status, Reason: source.Reason,
			InputAsOf: source.InputAsOf, ObservedAt: source.ObservedAt, Covered: source.Covered, Active: source.Active,
			Measurements: rpc.AlertShadowMeasurements{
				Evaluations: m.Evaluations, CoveredEvaluations: m.CoveredEvaluations,
				ActiveEvaluations: m.ActiveEvaluations, ActiveObservations: m.ActiveObservations,
				EpisodesOpened: m.EpisodesOpened, EpisodesEscalated: m.EpisodesEscalated,
				EpisodesRecovered: m.EpisodesRecovered, EpisodesReopened: m.EpisodesReopened,
				DuplicateInputs: m.DuplicateInputs, DuplicateCandidates: m.DuplicateCandidates,
				RepeatedActive: m.RepeatedActive, ActiveEvidenceChurn: m.ActiveEvidenceChurn,
				Equivocations: m.Equivocations, StaleSuppressions: m.StaleSuppressions,
				CoverageFailures: m.CoverageFailures, TimeToObserveSamples: m.TimeToObserveSamples,
				TimeToObserveTotalSecond: m.TimeToObserveTotal.Seconds(),
				TimeToObserveMaxSecond:   m.TimeToObserveMax.Seconds(),
			},
		})
	}
	return result, nil
}

func (s *Server) observeCanaryAlertShadow(result *rpc.CanaryResult, brokerScope brokerStateScope) {
	if s == nil || s.alertShadow == nil || result == nil {
		return
	}
	scope, err := newAlertShadowBrokerScope(brokerScope)
	if err != nil {
		s.warnf("alert shadow: Canary observation skipped: %v", err)
		return
	}
	ctx := context.Background()
	s.mu.Lock()
	if s.serverCtx != nil {
		ctx = s.serverCtx
	}
	s.mu.Unlock()
	if _, err := s.alertShadow.ObserveCanary(ctx, scope, *result); err != nil {
		s.warnf("alert shadow: Canary observation failed: %v", err)
	}
}

func (s *Server) observeNudgesAlertShadow(ctx context.Context, input alertShadowNudgeInput) {
	if s == nil || s.alertShadow == nil {
		return
	}
	if _, err := s.alertShadow.ObserveNudges(ctx, input); err != nil {
		s.warnf("alert shadow: Nudge observation failed: %v", err)
	}
}
