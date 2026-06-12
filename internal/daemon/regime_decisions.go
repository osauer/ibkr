package daemon

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// regimeDecisionJournal appends one JSON line per decision-relevant regime
// snapshot to a private state file — the forward-collection corpus that
// makes the pending_backtest thresholds calibratable
// (docs/design/regime-calibration.md Part 4). Before it existed nothing
// persisted indicator values, bands, or lifecycle decisions, so the
// 2026-06-12 false positive could not be reconstructed from disk.
//
// Contract mirrors gamma-skew-diagnostics.jsonl: append-only, never read at
// runtime, safe to delete at any time. Lines are deduped on the snapshot's
// semantic fingerprint with an hourly heartbeat for time-in-state
// statistics (the app polls every minute; without dedupe the journal would
// be poll noise).
type regimeDecisionJournal struct {
	path string

	mu              sync.Mutex
	lastFingerprint string
	lastWrite       time.Time
}

func regimeDecisionsDefaultPath() (string, error) {
	return defaultTradingStatePath("regime-decisions.jsonl")
}

const regimeDecisionHeartbeat = time.Hour

// regimeDecisionLine is the v1 journal record: enough raw measurement,
// gate evidence, and decision output to measure false-alarm and recall
// rates offline and to replay incidents.
type regimeDecisionLine struct {
	V           int                                `json:"v"`
	TS          time.Time                          `json:"ts"`
	SessionKey  string                             `json:"session_key"`
	Fingerprint string                             `json:"fingerprint"`
	Stage       string                             `json:"stage"`
	Severity    string                             `json:"severity"`
	Readiness   string                             `json:"readiness"`
	Confidence  string                             `json:"confidence"`
	Verdict     string                             `json:"verdict"`
	ConfirmedBy []string                           `json:"confirmed_by,omitempty"`
	Unconfirmed []string                           `json:"unconfirmed,omitempty"`
	Governors   []rpc.GovernorAction               `json:"governors,omitempty"`
	Composite   rpc.RegimeComposite                `json:"composite"`
	Indicators  map[string]regimeDecisionIndicator `json:"indicators"`
	DataQuality []rpc.DataQualityHealth            `json:"data_quality,omitempty"`
}

type regimeDecisionIndicator struct {
	Status          string   `json:"status,omitempty"`
	Band            string   `json:"band,omitempty"`
	Value           *float64 `json:"value,omitempty"`
	Depth           *float64 `json:"depth,omitempty"`
	StreakSessions  int      `json:"streak_sessions,omitempty"`
	Freshness       string   `json:"freshness,omitempty"`
	Eligible        *bool    `json:"eligible,omitempty"`
	Latched         bool     `json:"latched,omitempty"`
	ThresholdsLabel string   `json:"thresholds_label,omitempty"`
}

// journalRegimeDecision appends the snapshot when its semantic fingerprint
// changed or the heartbeat interval elapsed. Failures degrade to warnings —
// journaling must never fail a snapshot. Disabled via
// `ibkr settings set regime.journal.enabled=false`.
func (s *Server) journalRegimeDecision(res *rpc.RegimeSnapshotResult) {
	if s == nil || s.regimeDecisions == nil || res == nil {
		return
	}
	if !s.regimeJournalEnabled() {
		return
	}
	if err := s.regimeDecisions.append(time.Now(), res); err != nil {
		s.logger.Warnf("regime: decisions journal append failed: %v", err)
	}
}

func (s *Server) regimeJournalEnabled() bool {
	if s.platformSettings == nil {
		return true
	}
	data := s.platformSettings.snapshot()
	if data.Regime.Journal.Enabled == nil {
		return true
	}
	return *data.Regime.Journal.Enabled
}

func (j *regimeDecisionJournal) append(now time.Time, res *rpc.RegimeSnapshotResult) error {
	if j == nil || res == nil {
		return nil
	}
	fp := res.Fingerprint.Key
	j.mu.Lock()
	if fp != "" && fp == j.lastFingerprint && now.Sub(j.lastWrite) < regimeDecisionHeartbeat {
		j.mu.Unlock()
		return nil
	}
	j.lastFingerprint = fp
	j.lastWrite = now
	j.mu.Unlock()

	line := regimeDecisionLine{
		V:           1,
		TS:          now,
		SessionKey:  nyTradingSessionKey(nyTime(now)),
		Fingerprint: fp,
		Stage:       res.Lifecycle.Stage,
		Severity:    res.Lifecycle.Severity,
		Readiness:   res.Lifecycle.Readiness,
		Confidence:  res.Lifecycle.Confidence,
		Verdict:     res.Composite.Verdict,
		ConfirmedBy: res.Lifecycle.ConfirmedBy,
		Unconfirmed: res.Lifecycle.Unconfirmed,
		Governors:   res.Lifecycle.Governors,
		Composite:   res.Composite,
		Indicators:  regimeDecisionIndicators(res),
		DataQuality: res.DataQuality,
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
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

func regimeDecisionIndicators(res *rpc.RegimeSnapshotResult) map[string]regimeDecisionIndicator {
	out := make(map[string]regimeDecisionIndicator, len(streakIndicators))
	for _, ind := range streakIndicators {
		key := ind.key()
		_, value := ind.bandAndValue(res)
		status, meta, streak := regimeDecisionRowView(res, key)
		entry := regimeDecisionIndicator{
			Status: status,
			Band:   meta.Band,
			Depth:  ind.depth(res),
		}
		if meta.Band != "" && meta.Band != "unranked" {
			v := value
			entry.Value = &v
		}
		if streak != nil {
			entry.StreakSessions = streak.Sessions
		}
		if meta.Freshness != nil {
			entry.Freshness = meta.Freshness.Class
		}
		if meta.Eligibility != nil {
			e := meta.Eligibility.Eligible
			entry.Eligible = &e
			entry.Latched = meta.Eligibility.Latched
		}
		if meta.Thresholds != nil {
			entry.ThresholdsLabel = meta.Thresholds.Label
		}
		out[key] = entry
	}
	return out
}

func regimeDecisionRowView(res *rpc.RegimeSnapshotResult, key string) (string, rpc.RegimeIndicatorMeta, *rpc.StreakInfo) {
	switch key {
	case StreakKeyVIXTerm:
		return res.VIXTermStructure.Status, res.VIXTermStructure.RegimeIndicatorMeta, res.VIXTermStructure.Streak
	case StreakKeyVolOfVol:
		return res.VolOfVol.Status, res.VolOfVol.RegimeIndicatorMeta, res.VolOfVol.Streak
	case StreakKeyHYGSPY:
		return res.HYGSPYDivergence.Status, res.HYGSPYDivergence.RegimeIndicatorMeta, res.HYGSPYDivergence.Streak
	case StreakKeyCredit:
		return res.CreditSpreads.Status, res.CreditSpreads.RegimeIndicatorMeta, res.CreditSpreads.Streak
	case StreakKeyFunding:
		return res.FundingStress.Status, res.FundingStress.RegimeIndicatorMeta, res.FundingStress.Streak
	case StreakKeyUSDJPY:
		return res.USDJPY.Status, res.USDJPY.RegimeIndicatorMeta, res.USDJPY.Streak
	case StreakKeyGammaZero:
		return res.GammaZero.Status, res.GammaZero.RegimeIndicatorMeta, res.GammaZero.Streak
	case StreakKeyBreadth:
		return res.Breadth.Status, res.Breadth.RegimeIndicatorMeta, res.Breadth.Streak
	default:
		return "", rpc.RegimeIndicatorMeta{}, nil
	}
}
