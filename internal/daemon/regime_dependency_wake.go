package daemon

import "time"

// canaryEvaluationWakeChannel returns the daemon-lifetime, capacity-one wake
// channel. A buffered wake survives startup ordering and naturally coalesces
// repeated signals while Canary is already evaluating.
func (s *Server) canaryEvaluationWakeChannel() <-chan struct{} {
	return s.canaryEvaluationWakeSender()
}

func (s *Server) canaryEvaluationWakeSender() chan struct{} {
	if s == nil {
		return nil
	}
	s.regimeConsumerWakeMu.Lock()
	if s.canaryEvaluationWake == nil {
		s.canaryEvaluationWake = make(chan struct{}, 1)
	}
	wake := s.canaryEvaluationWake
	s.regimeConsumerWakeMu.Unlock()
	return wake
}

func (s *Server) wakeCanaryEvaluation() {
	wake := s.canaryEvaluationWakeSender()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// publishRulesRegimeStageState makes the new Regime stage and Rulebook cache
// boundary atomic with respect to a complete Rulebook evaluation. An
// evaluation already in flight finishes first and is then invalidated; one
// starting later sees the new stage. Interactive reads therefore cannot serve
// an older cached Rulebook result after the Regime stage becomes visible.
func (s *Server) publishRulesRegimeStageState(state rulesRegimeStageState, publication regimeSnapshotPublication) {
	if s == nil {
		return
	}
	s.rulesEvaluationMu.Lock()
	s.rulesRegimeStageMu.Lock()
	s.rulesRegimeStage = state
	s.rulesRegimeStageLoaded = true
	s.rulesRegimeStageMu.Unlock()

	notify := s.claimRegimeConsumerPublication(publication)
	var rulebookWake chan struct{}
	if notify {
		s.rulesMu.Lock()
		// Retain the prior result as the transition-journal baseline, but make it
		// ineligible for every read and immediately due for replacement.
		s.lastRulesAt = time.Time{}
		if s.rulesRefreshWake == nil {
			s.rulesRefreshWake = make(chan struct{}, 1)
		}
		rulebookWake = s.rulesRefreshWake
		s.rulesMu.Unlock()
	}
	s.rulesEvaluationMu.Unlock()

	if !notify {
		return
	}
	select {
	case rulebookWake <- struct{}{}:
	default:
	}
	s.wakeCanaryEvaluation()
}

// claimRegimeConsumerPublication admits each monotonic publication revision at
// most once. Consumers always read the latest immutable snapshot, so a burst
// of newer publications may share one buffered wake without losing state.
func (s *Server) claimRegimeConsumerPublication(publication regimeSnapshotPublication) bool {
	if s == nil || publication.Revision <= 0 {
		return false
	}
	s.regimeConsumerWakeMu.Lock()
	defer s.regimeConsumerWakeMu.Unlock()
	if publication.Revision <= s.regimeConsumerRevision {
		return false
	}
	s.regimeConsumerRevision = publication.Revision
	return true
}
