package daemon

import (
	"context"
	"time"
)

// startRegimeRefreshLoop starts the daemon-owned Regime freshness scheduler.
// It waits for gateway readiness without consuming refresh backoff and is
// deliberately independent of the alert registry, Canary journaling, and app
// polling.
func (s *Server) startRegimeRefreshLoop(ctx context.Context) {
	if s == nil || ctx == nil || ctx.Err() != nil || s.regimeSnapshots == nil {
		return
	}
	s.regimeRefreshLoopWG.Add(1)
	go func() {
		defer s.regimeRefreshLoopWG.Done()
		runRegimeRefreshLoop(
			ctx,
			s.regimeSnapshots,
			regimeSnapshotRefreshPoll,
			regimeSnapshotRefreshAhead,
			s.regimeRefreshGatewayReady,
			s.acquireRegimeSnapshot,
		)
	}()
}

// regimeRefreshGatewayReady is a non-triggering readiness read. The scheduler
// must not race cold-start discovery or create a reconnect loop of its own;
// normal connection ownership will make the next poll eligible.
func (s *Server) regimeRefreshGatewayReady() bool {
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	return c != nil && c.IsReady()
}

func runRegimeRefreshLoop(
	ctx context.Context,
	cache *regimeSnapshotCache,
	pollEvery time.Duration,
	refreshAhead time.Duration,
	ready func() bool,
	refresh regimeSnapshotRefreshFunc,
) {
	if ctx == nil || cache == nil || ready == nil || refresh == nil || pollEvery <= 0 || refreshAhead <= 0 {
		return
	}
	kick := func() {
		if ready() {
			cache.startRefreshAhead(refresh, refreshAhead)
		}
	}
	kick()

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			kick()
		}
	}
}
