package daemon

import (
	"context"
	"encoding/json"
	"os"
	"time"

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
	bucket := bucketRegimeStage(res.Lifecycle.Stage)
	if bucket == "" {
		return // hold the previous latch
	}
	st := rulesRegimeStageState{Version: rulesRegimeStageStateVer, Bucket: bucket, Stage: res.Lifecycle.Stage, AsOf: time.Now()}
	s.rulesRegimeStageMu.Lock()
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
