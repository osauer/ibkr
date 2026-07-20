package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// The rulebook's regime-conditional thresholds (rules 3/4/12) consume a
// bucketed regime lifecycle stage. The daemon latches the bucket whenever a
// regime snapshot completes and persists it, so a restart mid-stress cannot
// silently reset thresholds to calm. A stale latch is served as "carried" —
// the evaluator then applies worse-of(carried, calm) semantics — and the
// rules path kicks one async regime refresh to re-freshen it (single-flight
// with cooldown; never from the preview path, never synchronously).

const (
	rulesRegimeStageFile     = "rules-regime-stage.json"
	rulesRegimeKickCooldown  = 10 * time.Minute
	rulesRegimeKickTimeout   = 5 * time.Minute
	rulesRegimeStageStateVer = 1
)

type rulesRegimeStageState struct {
	Version int       `json:"version"`
	Bucket  string    `json:"bucket"`
	Stage   string    `json:"stage"`
	AsOf    time.Time `json:"as_of"`
}

func (s *Server) bindRulesRegimeStage(ctx context.Context, core *corestore.Store) error {
	if s == nil || core == nil {
		return fmt.Errorf("rules regime stage SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindRulesRegimeStage)
	if err != nil {
		return fmt.Errorf("load rules regime stage from SQLite: %w", err)
	}
	state := rulesRegimeStageState{Version: rulesRegimeStageStateVer}
	if ok {
		if err := json.Unmarshal(doc.JSON, &state); err != nil || state.Version != rulesRegimeStageStateVer {
			if err == nil {
				err = fmt.Errorf("unsupported version %d", state.Version)
			}
			return fmt.Errorf("decode rules regime stage from SQLite: %w", err)
		}
		if state.Stage != "" {
			state.Bucket = bucketRegimeStage(state.Stage)
			if state.Bucket == "" {
				return fmt.Errorf("decode rules regime stage from SQLite: invalid stage %q", state.Stage)
			}
		}
	} else {
		return fmt.Errorf("rules regime stage is missing from SQLite; cutover bootstrap was not completed")
	}
	s.rulesRegimeStageMu.Lock()
	s.rulesRegimeStage = state
	s.rulesRegimeStageLoaded = true
	s.rulesRegimeStageMu.Unlock()
	return nil
}

// bucketRegimeStage maps a lifecycle stage onto the rulebook's three
// threshold buckets. data_quality (and empty) return "" — hold the previous
// latch, a data-quality stage is an input problem, not a market read. An
// unrecognized future stage maps to the MIDDLE bucket, never silently to
// calm.
func bucketRegimeStage(stage string) string {
	switch stage {
	case rpc.LifecycleQuiet, rpc.LifecycleOpportunity:
		return risk.RegimeBucketCalm
	case rpc.LifecycleEarlyWarning, rpc.LifecycleStabilization:
		return risk.RegimeBucketEarlyWarning
	case rpc.LifecycleConfirmedStress, rpc.LifecyclePanic, rpc.LifecycleForcedDefense:
		return risk.RegimeBucketConfirmed
	case "", rpc.LifecycleDataQuality:
		return ""
	default:
		return risk.RegimeBucketEarlyWarning
	}
}

// latchRulesRegimeStage records the snapshot's lifecycle stage for the
// rulebook and persists it. Called from handleRegimeSnapshot's success path.
func (s *Server) latchRulesRegimeStage(res *rpc.RegimeSnapshotResult) {
	if res == nil {
		return
	}
	if res.TapeSessionState == rpc.TapeSessionClosedDate {
		// Closed-date snapshots hold the previous latch (Oliver, 2026-07-19,
		// closed-date tape-gating pass): with tape terms gated, a
		// weekend/holiday stage is cluster-only and must neither re-freshen
		// nor relax the rulebook bucket. The last trading-date stage instead
		// ages past RegimeStageMaxAgeMinutes into the existing carried
		// worse-of(carried, calm) path — stale regime data can hold or
		// tighten a verdict but never relax it — and the first live snapshot
		// at the next open re-latches fresh.
		return
	}
	bucket := bucketRegimeStage(res.Lifecycle.Stage)
	if bucket == "" {
		return // hold the previous latch
	}
	st := rulesRegimeStageState{Version: rulesRegimeStageStateVer, Bucket: bucket, Stage: res.Lifecycle.Stage, AsOf: time.Now()}
	s.rulesRegimeStageMu.Lock()
	if s.coreStore != nil {
		doc, ok, err := s.coreStore.GetStateDocument(context.Background(), daemonStateScope, stateKindRulesRegimeStage)
		if err != nil || !ok {
			s.rulesRegimeStageMu.Unlock()
			s.warnf("rules regime stage: SQLite authority read failed: %v", err)
			return
		}
		raw, err := json.Marshal(st)
		if err == nil {
			_, err = s.coreStore.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
				ScopeKey: daemonStateScope, Kind: stateKindRulesRegimeStage,
				ExpectedRevision: doc.Revision, JSON: raw,
			})
		}
		if err != nil {
			s.rulesRegimeStageMu.Unlock()
			s.warnf("rules regime stage: SQLite authority write failed: %v", err)
			return
		}
		s.rulesRegimeStage = st
		s.rulesRegimeStageLoaded = true
		s.rulesRegimeStageMu.Unlock()
		return
	}
	// Legacy unit/import helper only.
	s.rulesRegimeStage = st
	s.rulesRegimeStageLoaded = true
	s.rulesRegimeStageMu.Unlock()
	if path, err := defaultTradingStatePath(rulesRegimeStageFile); err == nil {
		if data, err := json.Marshal(st); err == nil {
			_ = writePrivateStateAtomic(path, data)
		}
	}
}

// rulesRegimeStageSnapshot returns the latched bucket, lazily restoring the
// persisted state on first use. Zero state means "never observed".
func (s *Server) rulesRegimeStageSnapshot() rulesRegimeStageState {
	s.rulesRegimeStageMu.Lock()
	defer s.rulesRegimeStageMu.Unlock()
	if !s.rulesRegimeStageLoaded {
		s.rulesRegimeStageLoaded = true
		// A started daemon binds this latch eagerly from SQLite. Reaching the
		// lazy path is confined to legacy unit/import helpers.
		if path, err := defaultTradingStatePath(rulesRegimeStageFile); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				var st rulesRegimeStageState
				if json.Unmarshal(data, &st) == nil && st.Version == rulesRegimeStageStateVer && bucketRegimeStage(st.Stage) != "" {
					// Never trust the stored bucket: a skewed or hand-edited
					// file must not serve calm for a panic stage.
					st.Bucket = bucketRegimeStage(st.Stage)
					s.rulesRegimeStage = st
				}
			}
		}
	}
	return s.rulesRegimeStage
}

// kickRulesRegimeStageRefresh schedules one background regime snapshot so a
// cold or stale stage latch self-heals after one rules evaluation.
// Single-flight with a cooldown: rules calls must never stampede the
// expensive regime fanout.
func (s *Server) kickRulesRegimeStageRefresh(ctx context.Context) {
	s.rulesRegimeStageMu.Lock()
	tooSoon := time.Since(s.rulesRegimeKickAt) < rulesRegimeKickCooldown
	if !tooSoon {
		s.rulesRegimeKickAt = time.Now()
	}
	s.rulesRegimeStageMu.Unlock()
	if tooSoon || !s.rulesRegimeKickBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.rulesRegimeKickBusy.Store(false)
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rulesRegimeKickTimeout)
		defer cancel()
		// The latch update happens inside handleRegimeSnapshot on success.
		_, _ = s.handleRegimeSnapshot(kctx, &rpc.Request{})
	}()
}
