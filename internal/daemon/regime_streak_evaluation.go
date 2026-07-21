package daemon

import (
	"context"
	"fmt"
	"maps"
	"time"
)

// cloneForRegimeEvaluation snapshots the current streak authority into a
// write-isolated in-memory evaluator. The caller may run the normal Tick and
// Latch methods against the clone; no SQLite write occurs until commit below.
func (s *StreakStore) cloneForRegimeEvaluation() *StreakStore {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return &StreakStore{
		entries:  cloneStreakEntries(s.entries),
		loaded:   true,
		volatile: true,
		asOf:     s.asOf,
	}
}

// commitRegimeEvaluation publishes one already-classified evaluator after the
// enclosing regime snapshot has committed as last-good. Regime refresh is
// single-flight, so this is the only production writer in the evaluation
// interval. Persistence is part of the projection barrier: failure withholds
// the committed snapshot until exact recovery succeeds.
func (s *StreakStore) commitRegimeEvaluation(ctx context.Context, evaluated *StreakStore, plan regimeProjectionPlan) error {
	if s == nil || evaluated == nil {
		return nil
	}
	publication := plan.publication
	evaluated.mu.Lock()
	entries := cloneStreakEntries(evaluated.entries)
	evaluated.mu.Unlock()

	s.mu.Lock()
	s.loadLocked()
	position, err := s.regimeProjectionPosition(plan)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if position == regimeProjectionCurrent {
		if !maps.Equal(s.entries, entries) {
			s.mu.Unlock()
			return fmt.Errorf("regime streak projection content mismatch at snapshot revision %d", publication.Revision)
		}
		s.mu.Unlock()
		return nil
	}
	beforeEntries := cloneStreakEntries(s.entries)
	beforeAsOf := s.asOf
	beforePublication := s.publication
	beforeExists := s.stateExists
	s.entries = entries
	s.loaded = true
	err = s.saveLockedContextPublication(ctx, publication)
	if err != nil {
		s.entries = beforeEntries
		s.asOf = beforeAsOf
		s.publication = beforePublication
		s.stateExists = beforeExists
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("commit regime streak projection: %w", err)
	}
	return nil
}

func (s *StreakStore) commitLegacyRegimeEvaluation(ctx context.Context, evaluated *StreakStore, publishedAt time.Time) error {
	if s == nil || evaluated == nil {
		return nil
	}
	evaluated.mu.Lock()
	entries := cloneStreakEntries(evaluated.entries)
	evaluated.mu.Unlock()
	s.mu.Lock()
	beforeEntries := cloneStreakEntries(s.entries)
	beforeAsOf := s.asOf
	s.entries = entries
	s.loaded = true
	err := s.saveLockedContextAt(ctx, publishedAt)
	if err != nil {
		s.entries = beforeEntries
		s.asOf = beforeAsOf
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("commit legacy regime streak projection: %w", err)
	}
	return nil
}

func cloneStreakEntries(in map[string]StreakEntry) map[string]StreakEntry {
	out := make(map[string]StreakEntry, len(in))
	maps.Copy(out, in)
	return out
}
