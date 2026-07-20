package daemon

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/canary"
	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// canaryDecisionJournal appends one typed SQLite event per decision-relevant
// portfolio-canary snapshot. It mirrors regimeDecisionJournal's fingerprint
// dedupe and hourly heartbeat. The path branch and writer lock remain only for
// legacy unit/import oracles.
type canaryDecisionJournal struct {
	path string // legacy unit/import helper only
	core *corestore.Store

	mu              sync.Mutex
	lastFingerprint string
	lastWrite       time.Time
}

func canaryDecisionsDefaultPath() (string, error) {
	return defaultTradingStatePath("canary-decisions.jsonl")
}

const (
	canaryDecisionHeartbeat = time.Hour
	// canaryJournalEvery is the cadence loop interval. Pinned at 5 minutes:
	// each tick includes a buildAccountSummary broker round-trip, so the
	// app's 1-minute poll cadence would add gateway load whenever the app
	// is not attached; 5 minutes plus fingerprint dedupe plus the hourly
	// heartbeat is calibration-sufficient (disclosed in the design doc).
	canaryJournalEvery = 5 * time.Minute
)

// canaryDecisionPolicy is the journal line's policy identity block.
type canaryDecisionPolicy struct {
	Policy      string          `json:"policy,omitempty"`
	Profile     string          `json:"profile,omitempty"`
	Version     string          `json:"version,omitempty"`
	Fingerprint rpc.Fingerprint `json:"fingerprint,omitzero"`
}

// canaryDecisionLine is the v1 journal record: the canary's decision
// output, its market/portfolio evidence, and the classified upstream
// fingerprints — enough to replay an alert decision offline.
type canaryDecisionLine struct {
	V                      int                          `json:"v"`
	TS                     time.Time                    `json:"ts"`
	SessionKey             string                       `json:"session_key"`
	Fingerprint            string                       `json:"fingerprint"`
	Account                string                       `json:"account,omitempty"`
	AccountMode            string                       `json:"account_mode,omitempty"`
	Action                 string                       `json:"action,omitempty"`
	Severity               risk.SignalSeverity          `json:"severity"`
	Direction              risk.SignalDirection         `json:"direction,omitempty"`
	MarketConfirmation     string                       `json:"market_confirmation,omitempty"`
	PortfolioFit           string                       `json:"portfolio_fit,omitempty"`
	PortfolioAlertRelevant *bool                        `json:"portfolio_alert_relevant,omitempty"`
	InputHealth            string                       `json:"input_health,omitempty"`
	PlannerModeHint        risk.PlannerMode             `json:"planner_mode_hint,omitempty"`
	PlannerReadiness       risk.PlannerReadiness        `json:"planner_readiness,omitempty"`
	Summary                string                       `json:"summary"`
	PrimaryDrivers         []risk.SignalID              `json:"primary_drivers,omitempty"`
	Policy                 canaryDecisionPolicy         `json:"policy,omitzero"`
	Market                 rpc.CanaryMarketSummary      `json:"market"`
	HeldStress             []rpc.CanaryHeldStress       `json:"held_stress,omitempty"`
	Rows                   []rpc.CanaryRow              `json:"rows,omitempty"`
	SourceFingerprints     rpc.CanarySourceFingerprints `json:"source_fingerprints,omitzero"`
	SourceAsOf             rpc.CanarySourceAsOf         `json:"source_as_of,omitzero"`
	Warnings               []string                     `json:"warnings,omitempty"`
}

func (s *Server) installCanaryDecisionJournal() {
	path, err := canaryDecisionsDefaultPath()
	if err != nil {
		s.logger.Warnf("canary decisions: resolve state path: %v (journal disabled)", err)
		return
	}
	s.canaryDecisions = &canaryDecisionJournal{path: path}
}

// journalCanaryDecision appends the canary snapshot when its semantic
// fingerprint changed or the heartbeat interval elapsed. Failures degrade
// to warnings — journaling must never fail a snapshot or brief. Disabled
// via `ibkr settings set canary.journal.enabled=false`. Always ends with
// the data-free history-index kick.
func (s *Server) journalCanaryDecision(res *rpc.CanaryResult) {
	if s == nil || s.canaryDecisions == nil || res == nil {
		return
	}
	if s.canaryJournalEnabled() {
		scope := s.currentBrokerStateScope()
		if err := s.canaryDecisions.append(time.Now(), scope.Account, scope.Mode, res); err != nil {
			s.logger.Warnf("canary: decisions journal append failed: %v", err)
		}
	}
	// Wake the history-index ingester unconditionally (not gated on the
	// append outcome): the kick carries no data, only "look at the file".
	s.kickHistoryIndex()
}

func (s *Server) canaryJournalEnabled() bool {
	if s.platformSettings == nil {
		return true
	}
	data := s.platformSettings.snapshot()
	if data.Canary.Journal.Enabled == nil {
		return true
	}
	return *data.Canary.Journal.Enabled
}

// append journals one deduped canary decision. The mutex is held across
// marshal, directory ensure, open, write, and close — the writer-quiescence
// contract rotation relies on (a live-file rename is invisible to an
// open-per-append writer only while no append is in flight).
func (j *canaryDecisionJournal) append(now time.Time, account, accountMode string, res *rpc.CanaryResult) error {
	if j == nil || res == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	fp := res.Fingerprint.Key
	if fp != "" && fp == j.lastFingerprint && now.Sub(j.lastWrite) < canaryDecisionHeartbeat {
		return nil
	}
	line := canaryDecisionLine{
		V:                      1,
		TS:                     now,
		SessionKey:             nyTradingSessionKey(nyTime(now)),
		Fingerprint:            fp,
		Account:                account,
		AccountMode:            accountMode,
		Action:                 res.Action,
		Severity:               res.Severity,
		Direction:              res.Direction,
		MarketConfirmation:     res.MarketConfirmation,
		PortfolioFit:           res.PortfolioFit,
		PortfolioAlertRelevant: res.PortfolioAlertRelevant,
		InputHealth:            res.InputHealth,
		PlannerModeHint:        res.PlannerModeHint,
		PlannerReadiness:       res.PlannerReadiness,
		Summary:                res.Summary,
		PrimaryDrivers:         res.PrimaryDrivers,
		Policy: canaryDecisionPolicy{
			Policy:      res.Policy,
			Profile:     res.PolicyProfile,
			Version:     res.PolicyVersion,
			Fingerprint: res.PolicyFingerprint,
		},
		Market:             res.Market,
		HeldStress:         res.Portfolio.HeldStress,
		Rows:               res.Rows,
		SourceFingerprints: res.SourceFingerprints,
		SourceAsOf:         res.SourceAsOf,
		Warnings:           res.Warnings,
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	if j.core != nil {
		key, err := coreStoreEventKey(context.Background(), j.core, coreEventCanaryDecision, now, b, 0)
		if err != nil {
			return err
		}
		_, err = j.core.AppendEvents(context.Background(), []corestore.EventInput{{
			ScopeKey: daemonStateScope, EventKey: key, Type: coreEventCanaryDecision,
			Action: coreEventActionRecord, Origin: coreEventOriginDaemon,
			OccurredAt: now, PayloadJSON: b,
			Projection: corestore.EventProjection{CanaryTransition: &corestore.CanaryTransitionProjection{
				Action: line.Action, Severity: string(line.Severity), Direction: string(line.Direction),
				MarketStage: line.Market.RegimePosture.Stage, InputHealth: line.InputHealth,
				PortfolioAlertRelevant: line.PortfolioAlertRelevant,
			}},
		}})
		if err != nil {
			return err
		}
		j.lastFingerprint, j.lastWrite = fp, now
		return nil
	}
	j.lastFingerprint, j.lastWrite = fp, now
	b = append(b, '\n')
	if err := ensurePrivateStateDir(j.path); err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// runCanaryJournalLoop is the evidence cadence: every canaryJournalEvery
// it composes the canary exactly as composeBrief does (account snapshot
// without capital observation, positions, the cached regime snapshot,
// held-symbol market events) and journals the result. Skipped while the
// gateway is disconnected, while the journal is disabled, and until the
// first daemon regime poll completes.
func (s *Server) runCanaryJournalLoop(ctx context.Context) {
	if s == nil || s.canaryDecisions == nil {
		return
	}
	t := time.NewTicker(canaryJournalEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.canaryJournalTick(ctx)
	}
}

// canaryJournalTick composes and journals one canary decision. Split from
// the loop for tests.
func (s *Server) canaryJournalTick(ctx context.Context) {
	if s == nil || s.canaryDecisions == nil || !s.canaryJournalEnabled() {
		return
	}
	if s.gatewayConnector() == nil {
		return
	}
	regime, err := s.briefRegimeSnapshot()
	if err != nil || regime == nil {
		return // cached snapshot is nil until the first regime poll
	}
	acct, _ := s.buildAccountSummary(ctx, false)
	pos, _ := s.handlePositionsList(ctx, &rpc.Request{})
	var events *rpc.MarketEventsResult
	if pos != nil {
		events, _ = s.handleMarketEventsSnapshot(ctx, &rpc.Request{Params: briefJSON(rpc.MarketEventsParams{Symbols: marketEventSymbolsFromPositions(pos)})})
	}
	in := rpc.CanaryInput{Now: s.briefNow()}
	if acct != nil {
		in.Account = *acct
	}
	if pos != nil {
		in.Positions = *pos
	}
	in.Regime = *regime
	if events != nil {
		in.MarketEvents = *events
	}
	can := canary.ComputeCanary(in)
	s.journalCanaryDecision(&can)
}
