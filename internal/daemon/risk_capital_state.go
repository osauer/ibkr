package daemon

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Runtime capital state for the risk constitution (docs/design/risk-policy.md):
// the cash-flow-adjusted equity peak, the drawdown latch, declared capital
// events, statement-authoritative v3 facts, one-shot overrides, and cadence
// artefact completions.
//
// Authority split: risk-policy.toml owns the numbers, internal/risk owns the
// evaluation, this store owns observed/derived runtime facts. Aggregates that
// can be re-derived from the capital-events journal (declared flows, reconcile
// evidence) are re-derived on every load — the journal is the source of truth,
// while statement facts are rebuilt from retained statements. The state file
// caches what those sources cannot rebuild (peak, latch, applied statement
// correction ids, daily equity samples, overrides; regime-latch
// never-trust-stored-bytes precedent).
//
// Nothing in this file may influence submit eligibility, blockers, freeze,
// pins, or tokens: v1 is advisory/shadow end to end.

const (
	riskCapitalStateFile     = "risk-capital-state.json"
	capitalEventsJournalFile = "capital-events.jsonl"
	riskPolicyJournalFile    = "risk-policy-journal.jsonl"
	riskCapitalStateVer      = 1
	// riskCapitalDailySampleKeep bounds the per-day equity sample cache
	// feeding the same-day recon equity check and the backtest replay.
	riskCapitalDailySampleKeep = 45 * 24 * time.Hour
	// riskCapitalPersistEvery throttles equity-cache persistence; latch,
	// peak, and event writes always persist immediately.
	riskCapitalPersistEvery = time.Minute
	riskCapitalAutoOrigin   = "daemon-auto"
)

type riskCapitalStateFileV1 struct {
	Version                           int                  `json:"version"`
	GenesisAt                         time.Time            `json:"genesis_at,omitzero"`
	Seeded                            bool                 `json:"seeded"`
	AdjustedPeakBase                  float64              `json:"adjusted_peak_base"`
	PeakAsOf                          time.Time            `json:"peak_as_of,omitzero"`
	LastEquityBase                    float64              `json:"last_equity_base"`
	LastEquityAsOf                    time.Time            `json:"last_equity_as_of,omitzero"`
	DailyEquity                       map[string]float64   `json:"daily_equity,omitempty"`
	LastTier                          string               `json:"last_tier,omitempty"`
	BlockLatched                      bool                 `json:"block_latched"`
	LatchedAt                         time.Time            `json:"latched_at,omitzero"`
	LatchEpisodeSeq                   uint64               `json:"latch_episode_seq,omitempty"`
	LatchConsumedPct                  float64              `json:"latch_consumed_pct,omitempty"`
	Overrides                         []rpc.OverrideRecord `json:"overrides,omitempty"`
	Artefacts                         []rpc.ArtefactRecord `json:"artefacts,omitempty"`
	StatementFlowsBase                float64              `json:"statement_flows_base,omitempty"`
	StatementCoverageTo               time.Time            `json:"statement_coverage_to,omitzero"`
	StatementAuthorityActive          bool                 `json:"statement_authority_active,omitempty"`
	IncorporatedStatementLineIDs      []string             `json:"incorporated_statement_line_ids,omitempty"`
	AppliedStatementPeakCorrectionIDs []string             `json:"applied_statement_peak_correction_ids,omitempty"`
}

type capitalEventV1 struct {
	Version     int       `json:"version"`
	At          time.Time `json:"at"`
	Type        string    `json:"type"` // deposit | withdrawal | reconcile
	AmountBase  float64   `json:"amount_base,omitempty"`
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	Note        string    `json:"note,omitempty"`
	Origin      string    `json:"origin,omitempty"`
	// ReportID and CoverageTo record which recon report a reconcile signed
	// off: an audit fact previously preserved only in the human message.
	ReportID   string    `json:"report_id,omitempty"`
	CoverageTo time.Time `json:"coverage_to,omitzero"`
}

type capitalReconRef struct {
	ReportID   string
	CoverageTo time.Time
}

type capitalEventReplay struct {
	declaredFlowsBase      float64
	declaredEvents         []capitalEventV1
	lastReconciledAt       time.Time
	lastReconcileReportID  string
	lastReconcileSource    string
	lastAutoExtendedAt     time.Time
	lastAutoExtendReportID string
	reconciledReportIDs    map[string]struct{}
}

type riskCapitalStore struct {
	mu     sync.Mutex
	now    func() time.Time
	nudges *nudgeStateStore
	loaded bool
	state  riskCapitalStateFileV1
	// Re-derived from capital-events.jsonl on load, maintained incrementally
	// afterwards; never trusted from the state file.
	cumFlowsBase           float64
	declaredEvents         []capitalEventV1
	lastReconciledAt       time.Time
	lastReconcileReportID  string
	lastReconcileSource    string
	lastAutoExtendedAt     time.Time
	lastAutoExtendReportID string
	reconciledReportIDs    map[string]struct{}
	lastPersistAt          time.Time
	overrideSeq            int
}

func (s *Server) installRiskCapitalStore() {
	if s == nil {
		return
	}
	s.riskCapital = &riskCapitalStore{now: s.now}
}

func (st *riskCapitalStore) loadLocked() {
	if st.loaded {
		return
	}
	st.loaded = true
	if path, err := defaultTradingStatePath(riskCapitalStateFile); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			var f riskCapitalStateFileV1
			if json.Unmarshal(data, &f) == nil && f.Version == riskCapitalStateVer {
				st.state = f
			}
		}
	}
	st.state.Version = riskCapitalStateVer
	if st.state.BlockLatched && st.state.LatchEpisodeSeq == 0 {
		// Backward-compatible replay for a latch created before the opaque
		// episode counter existed. Snapshot reads do not persist this upgrade.
		st.state.LatchEpisodeSeq = 1
	}
	// Journal replay owns flows and reconciliation recency.
	replayed := replayCapitalEvents()
	st.cumFlowsBase = replayed.declaredFlowsBase
	st.declaredEvents = replayed.declaredEvents
	st.lastReconciledAt = replayed.lastReconciledAt
	st.lastReconcileReportID = replayed.lastReconcileReportID
	st.lastReconcileSource = replayed.lastReconcileSource
	st.lastAutoExtendedAt = replayed.lastAutoExtendedAt
	st.lastAutoExtendReportID = replayed.lastAutoExtendReportID
	st.reconciledReportIDs = replayed.reconciledReportIDs
}

func replayCapitalEvents() capitalEventReplay {
	out := capitalEventReplay{reconciledReportIDs: make(map[string]struct{})}
	path, err := defaultTradingStatePath(capitalEventsJournalFile)
	if err != nil {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev capitalEventV1
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Version != 1 {
			continue
		}
		switch ev.Type {
		case "deposit":
			out.declaredFlowsBase += ev.AmountBase
			out.declaredEvents = append(out.declaredEvents, ev)
		case "withdrawal":
			out.declaredFlowsBase -= ev.AmountBase
			out.declaredEvents = append(out.declaredEvents, ev)
		case "reconcile":
			if ev.ReportID != "" {
				out.reconciledReportIDs[ev.ReportID] = struct{}{}
			}
			if ev.At.After(out.lastReconciledAt) {
				out.lastReconciledAt = ev.At
				out.lastReconcileReportID = ev.ReportID
				out.lastReconcileSource = rpc.ReconcileSourceHuman
				if ev.Origin == riskCapitalAutoOrigin {
					out.lastReconcileSource = rpc.ReconcileSourceAutomatic
				}
			}
			if ev.Origin == riskCapitalAutoOrigin && ev.At.After(out.lastAutoExtendedAt) {
				out.lastAutoExtendedAt = ev.At
				out.lastAutoExtendReportID = ev.ReportID
			}
		}
	}
	return out
}

func (st *riskCapitalStore) persistLocked(force bool) {
	now := st.now()
	if !force && now.Sub(st.lastPersistAt) < riskCapitalPersistEvery {
		return
	}
	st.lastPersistAt = now
	if path, err := defaultTradingStatePath(riskCapitalStateFile); err == nil {
		if data, err := json.Marshal(st.state); err == nil {
			_ = writePrivateStateAtomic(path, data) // best-effort, never fails the hot path
		}
	}
}

// runtimeLocked builds the evaluator's view of the state.
func (st *riskCapitalStore) runtimeLocked(c *risk.Constitution, now time.Time) risk.CapitalRuntime {
	flows, _, _ := st.effectiveFlowsLocked(c)
	var overrideUntil time.Time
	for _, o := range st.state.Overrides {
		if o.Active && o.Control == "capital.max_unreconciled_days" && !now.After(o.ExpiresAt) && o.ExpiresAt.After(overrideUntil) {
			overrideUntil = o.ExpiresAt
		}
	}
	return risk.CapitalRuntime{
		AdjustedPeakBase:          st.state.AdjustedPeakBase,
		PeakAsOf:                  st.state.PeakAsOf,
		CumExternalFlowsBase:      flows,
		Seeded:                    st.state.Seeded,
		BlockLatched:              st.state.BlockLatched,
		LastReconciledAt:          st.lastReconciledAt,
		UnreconciledOverrideUntil: overrideUntil,
	}
}

func (st *riskCapitalStore) effectiveFlowsLocked(c *risk.Constitution) (effective, statement float64, source string) {
	if c == nil || c.PolicyVersion < 3 {
		return st.cumFlowsBase, 0, rpc.CapitalFlowSourceDeclared
	}
	statement = st.state.StatementFlowsBase
	for _, ev := range st.declaredEvents {
		effectiveAt := ev.EffectiveAt
		if effectiveAt.IsZero() {
			effectiveAt = ev.At
		}
		if st.state.Seeded && !st.state.GenesisAt.IsZero() && utcDateBefore(effectiveAt, st.state.GenesisAt) {
			continue
		}
		if !st.state.StatementCoverageTo.IsZero() && !utcDateAfter(effectiveAt, st.state.StatementCoverageTo) {
			continue
		}
		if ev.Type == "deposit" {
			statement += ev.AmountBase
		} else if ev.Type == "withdrawal" {
			statement -= ev.AmountBase
		}
	}
	return statement, statement, rpc.CapitalFlowSourceStatement
}

// Observe folds one equity reading into the state: seeds or raises the
// cash-flow-adjusted peak, evaluates the tier under the active constitution,
// engages the latch on a block breach, and journals tier transitions.
// Called from the account-summary success path — observation cadence is
// usage cadence; there is deliberately no new scheduler in v1.
func (st *riskCapitalStore) Observe(equityBase float64, asOf time.Time, c *risk.Constitution) {
	if st == nil || equityBase <= 0 || asOf.IsZero() {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()

	now := st.now()
	force := false
	if st.state.GenesisAt.IsZero() {
		st.state.GenesisAt = now.UTC()
		force = true
	}
	st.state.LastEquityBase = equityBase
	st.state.LastEquityAsOf = asOf
	if st.state.DailyEquity == nil {
		st.state.DailyEquity = make(map[string]float64)
	}
	st.state.DailyEquity[asOf.UTC().Format("2006-01-02")] = equityBase
	cutoff := now.UTC().Add(-riskCapitalDailySampleKeep)
	cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	for day := range st.state.DailyEquity {
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil || parsed.Before(cutoff) {
			delete(st.state.DailyEquity, day)
		}
	}

	flows, _, _ := st.effectiveFlowsLocked(c)
	adjusted := equityBase - flows
	if !st.state.Seeded || adjusted > st.state.AdjustedPeakBase {
		st.state.Seeded = true
		st.state.AdjustedPeakBase = adjusted
		st.state.PeakAsOf = asOf
		force = true
	}

	obs := risk.CapitalObservation{EquityBase: equityBase, AsOf: asOf}
	v := risk.EvaluateCapital(c, st.runtimeLocked(c, now), &obs, now)
	if v.Tier == risk.CapitalTierBlock && !st.state.BlockLatched && v.ConsumedPct != nil {
		st.state.LatchEpisodeSeq++
		st.state.BlockLatched = true
		st.state.LatchedAt = now.UTC()
		st.state.LatchConsumedPct = *v.ConsumedPct
		force = true
		appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "drawdown_block_latched",
			"consumed_pct": *v.ConsumedPct, "enforcement": constitutionEnforcement(c),
			"policy_fingerprint": constitutionFingerprint(c),
		})
	}
	if prev := st.state.LastTier; prev != v.Tier {
		appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "capital_tier", "from": prev, "to": v.Tier,
			"policy_fingerprint": constitutionFingerprint(c),
		})
		st.state.LastTier = v.Tier
		force = true
	}
	st.persistLocked(force)
}

// ApplyCapitalEvent journals a declared capital fact and folds it into the
// derived aggregates. A deposit whose effective time precedes the recorded
// peak corrects the peak downward: the peak was observed with the deposit
// already in equity, and an inflated peak would overstate drawdown against
// money that was never earned (never-inflate discipline; the symmetric
// withdrawal case cannot inflate the peak, so no correction is needed).
func (st *riskCapitalStore) ApplyCapitalEvent(p rpc.CapitalEventParams, origin string, refs ...*capitalReconRef) (capitalEventV1, error) {
	return st.ApplyCapitalEventForPolicy(p, origin, nil, refs...)
}

func (st *riskCapitalStore) ApplyCapitalEventForPolicy(p rpc.CapitalEventParams, origin string, c *risk.Constitution, refs ...*capitalReconRef) (capitalEventV1, error) {
	typ := strings.ToLower(strings.TrimSpace(p.Type))
	switch typ {
	case "deposit", "withdrawal":
		if p.AmountBase <= 0 {
			return capitalEventV1{}, fmt.Errorf("capital event amount_base must be positive")
		}
	case "reconcile":
	default:
		return capitalEventV1{}, fmt.Errorf("capital event type %q is invalid; use deposit, withdrawal, or reconcile", p.Type)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()

	now := st.now().UTC()
	ev := capitalEventV1{
		Version: 1, At: now, Type: typ, AmountBase: p.AmountBase,
		EffectiveAt: p.EffectiveAt, Note: strings.TrimSpace(p.Note), Origin: origin,
	}
	if ev.EffectiveAt.IsZero() {
		ev.EffectiveAt = now
	}
	if ev.Type == "reconcile" {
		ev.AmountBase = 0
		if len(refs) > 0 && refs[0] != nil {
			ev.ReportID = strings.TrimSpace(refs[0].ReportID)
			ev.CoverageTo = refs[0].CoverageTo
		}
	}
	if err := appendCapitalEvent(ev); err != nil {
		return capitalEventV1{}, err
	}
	switch ev.Type {
	case "deposit":
		st.cumFlowsBase += ev.AmountBase
		st.declaredEvents = append(st.declaredEvents, ev)
		if (c == nil || c.PolicyVersion < 3) && st.state.Seeded && !st.state.PeakAsOf.IsZero() && !st.state.PeakAsOf.Before(ev.EffectiveAt) {
			st.state.AdjustedPeakBase -= ev.AmountBase
		}
	case "withdrawal":
		st.cumFlowsBase -= ev.AmountBase
		st.declaredEvents = append(st.declaredEvents, ev)
	case "reconcile":
		if ev.ReportID != "" {
			if st.reconciledReportIDs == nil {
				st.reconciledReportIDs = make(map[string]struct{})
			}
			st.reconciledReportIDs[ev.ReportID] = struct{}{}
		}
		if ev.At.After(st.lastReconciledAt) {
			st.lastReconciledAt = ev.At
			st.lastReconcileReportID = ev.ReportID
			st.lastReconcileSource = rpc.ReconcileSourceHuman
			if ev.Origin == riskCapitalAutoOrigin {
				st.lastReconcileSource = rpc.ReconcileSourceAutomatic
			}
		}
		if ev.Origin == riskCapitalAutoOrigin && ev.At.After(st.lastAutoExtendedAt) {
			st.lastAutoExtendedAt = ev.At
			st.lastAutoExtendReportID = ev.ReportID
		}
	}
	st.persistLocked(true)
	return ev, nil
}

// ApplyAutomaticReconcile appends daemon-owned evidence while holding the
// same serialization lock as human capital events. The report id is checked
// and recorded atomically, so concurrent startup/fetch evaluations cannot
// append the same pinned report twice.
func (st *riskCapitalStore) ApplyAutomaticReconcile(reportID string, coverageTo time.Time) (capitalEventV1, bool, error) {
	reportID = strings.TrimSpace(reportID)
	if reportID == "" {
		return capitalEventV1{}, false, fmt.Errorf("automatic reconcile requires a report id")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if _, exists := st.reconciledReportIDs[reportID]; exists {
		return capitalEventV1{}, false, nil
	}
	now := st.now().UTC()
	ev := capitalEventV1{
		Version: 1, At: now, Type: "reconcile", Origin: riskCapitalAutoOrigin,
		ReportID: reportID, CoverageTo: coverageTo,
	}
	if err := appendCapitalEvent(ev); err != nil {
		return capitalEventV1{}, false, err
	}
	if st.reconciledReportIDs == nil {
		st.reconciledReportIDs = make(map[string]struct{})
	}
	st.reconciledReportIDs[reportID] = struct{}{}
	st.lastReconciledAt = ev.At
	st.lastReconcileReportID = reportID
	st.lastReconcileSource = rpc.ReconcileSourceAutomatic
	st.lastAutoExtendedAt = ev.At
	st.lastAutoExtendReportID = reportID
	st.persistLocked(true)
	return ev, true, nil
}

type statementCapitalSnapshot struct {
	FlowsBase           float64
	CoverageTo          time.Time
	Flows               []reconFlow
	NudgeConfirmedFlows nudgeConfirmedFlowSnapshot
}

// IncorporateStatementSnapshot installs one fully healthy reconstruction.
// The first v3 incorporation is an activation baseline: existing lines are
// marked seen without changing peak/latch state. Later new deposits use their
// broker value dates for the one-time R4 peak correction.
func (st *riskCapitalStore) IncorporateStatementSnapshot(snap statementCapitalSnapshot) {
	st.mu.Lock()
	st.loadLocked()
	incorporated := make(map[string]struct{}, len(st.state.IncorporatedStatementLineIDs))
	for _, id := range st.state.IncorporatedStatementLineIDs {
		incorporated[id] = struct{}{}
	}
	applied := make(map[string]struct{}, len(st.state.AppliedStatementPeakCorrectionIDs))
	for _, id := range st.state.AppliedStatementPeakCorrectionIDs {
		applied[id] = struct{}{}
	}
	activation := !st.state.StatementAuthorityActive
	for _, flow := range snap.Flows {
		if _, seen := incorporated[flow.id]; seen {
			continue
		}
		incorporated[flow.id] = struct{}{}
		st.state.IncorporatedStatementLineIDs = append(st.state.IncorporatedStatementLineIDs, flow.id)
		if activation || flow.amountBase <= 0 || !st.state.Seeded || st.state.PeakAsOf.IsZero() || utcDateAfter(flow.valueDate, st.state.PeakAsOf) {
			continue
		}
		if _, corrected := applied[flow.id]; corrected {
			continue
		}
		st.state.AdjustedPeakBase -= flow.amountBase
		applied[flow.id] = struct{}{}
		st.state.AppliedStatementPeakCorrectionIDs = append(st.state.AppliedStatementPeakCorrectionIDs, flow.id)
	}
	st.state.StatementAuthorityActive = true
	st.state.StatementFlowsBase = snap.FlowsBase
	st.state.StatementCoverageTo = snap.CoverageTo
	st.persistLocked(true)
	st.mu.Unlock()
	// The nudge store has its own lock and persistence boundary. Observe only
	// after capital state is installed so neither store is held while writing
	// the other, and never let advisory nudge persistence alter capital truth.
	if st.nudges != nil {
		_ = st.nudges.observeConfirmedFlows(snap.NudgeConfirmedFlows)
	}
}

func (st *riskCapitalStore) ActivateStatementAuthorityWithoutStatements() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.state.StatementAuthorityActive {
		return
	}
	st.state.StatementAuthorityActive = true
	st.persistLocked(true)
}

// GrantOverride records a one-shot, expiring exception against one named
// control. The caller has already verified a human origin; this layer
// enforces the policy's lifetime cap and journals the grant.
func (st *riskCapitalStore) GrantOverride(p rpc.OverrideParams, c *risk.Constitution) (rpc.OverrideRecord, error) {
	control := strings.TrimSpace(p.Control)
	reason := strings.TrimSpace(p.Reason)
	if control == "" || reason == "" {
		return rpc.OverrideRecord{}, fmt.Errorf("override needs both control and reason")
	}
	if p.Hours <= 0 {
		return rpc.OverrideRecord{}, fmt.Errorf("override hours must be positive")
	}
	if c == nil || c.Override.MaxDurationHours == nil {
		return rpc.OverrideRecord{}, fmt.Errorf("override.max_duration_hours is unapproved; overrides are unavailable until the policy declares the cap")
	}
	if p.Hours > *c.Override.MaxDurationHours {
		return rpc.OverrideRecord{}, fmt.Errorf("override hours %d exceed override.max_duration_hours %d", p.Hours, *c.Override.MaxDurationHours)
	}
	known := false
	for _, l := range risk.ConstitutionLimits(c) {
		if l.Key == control {
			known = true
			break
		}
	}
	if !known {
		return rpc.OverrideRecord{}, fmt.Errorf("override control %q is not a constitution key; safety invariants have no keys and cannot be overridden", control)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now().UTC()
	st.overrideSeq++
	rec := rpc.OverrideRecord{
		ID:                fmt.Sprintf("ov-%s-%d", now.Format("20060102-150405"), st.overrideSeq),
		Control:           control,
		Reason:            reason,
		GrantedAt:         now,
		ExpiresAt:         now.Add(time.Duration(p.Hours) * time.Hour),
		PolicyFingerprint: constitutionFingerprint(c),
		Active:            true,
	}
	st.state.Overrides = append(st.state.Overrides, rec)
	appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": now, "kind": "override_granted", "id": rec.ID,
		"control": rec.Control, "reason": rec.Reason, "expires_at": rec.ExpiresAt,
		"policy_fingerprint": rec.PolicyFingerprint,
	})
	st.persistLocked(true)
	return rec, nil
}

// ResetDrawdown clears the latch and re-bases the adjusted peak to the
// current equity reading. Human-only (caller-verified); reason mandatory.
func (st *riskCapitalStore) ResetDrawdown(reason string, c *risk.Constitution) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("drawdown reset requires a reason")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now().UTC()
	wasLatched := st.state.BlockLatched
	st.state.BlockLatched = false
	st.state.LatchedAt = time.Time{}
	st.state.LatchConsumedPct = 0
	if st.state.LastEquityBase > 0 {
		flows, _, _ := st.effectiveFlowsLocked(c)
		st.state.AdjustedPeakBase = st.state.LastEquityBase - flows
		st.state.PeakAsOf = st.state.LastEquityAsOf
		st.state.Seeded = true
	}
	appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": now, "kind": "drawdown_reset", "reason": reason,
		"was_latched": wasLatched, "policy_fingerprint": constitutionFingerprint(c),
	})
	st.persistLocked(true)
	return nil
}

// RecordArtefact journals completion of one declared cadence artefact.
func (st *riskCapitalStore) RecordArtefact(p rpc.ArtefactParams, c *risk.Constitution) (rpc.ArtefactRecord, error) {
	name := strings.ToLower(strings.TrimSpace(p.Artefact))
	var class string
	switch name {
	case "morning":
		class = artefactDeclaredClass(c, func(cc *risk.Constitution) string { return cc.Cadence.Morning.Class })
	case "eod":
		class = artefactDeclaredClass(c, func(cc *risk.Constitution) string { return cc.Cadence.EOD.Class })
	case "weekly":
		class = artefactDeclaredClass(c, func(cc *risk.Constitution) string { return cc.Cadence.Weekly.Class })
	default:
		return rpc.ArtefactRecord{}, fmt.Errorf("artefact %q is invalid; use morning, eod, or weekly", p.Artefact)
	}
	if class == "" {
		return rpc.ArtefactRecord{}, fmt.Errorf("artefact %q is not declared in the risk policy cadence section", name)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now().UTC()
	rec := rpc.ArtefactRecord{
		Artefact: name, Class: class, CompletedAt: now, Note: strings.TrimSpace(p.Note),
		Origin: strings.TrimSpace(p.Origin), BriefFingerprint: strings.TrimSpace(p.BriefFingerprint),
	}
	kept := st.state.Artefacts[:0:0]
	for _, a := range st.state.Artefacts {
		if a.Artefact != name {
			kept = append(kept, a)
		}
	}
	st.state.Artefacts = append(kept, rec)
	journal := map[string]any{
		"version": 1, "at": now, "kind": "artefact_completed", "artefact": name,
		"note": rec.Note, "policy_fingerprint": constitutionFingerprint(c),
	}
	if rec.Origin != "" {
		journal["origin"] = rec.Origin
	}
	if rec.BriefFingerprint != "" {
		journal["brief_fingerprint"] = rec.BriefFingerprint
	}
	appendRiskPolicyJournal(journal)
	st.persistLocked(true)
	return rec, nil
}

// Report evaluates the current state under the active constitution for the
// snapshot surface. obs may be nil (no fresh reading): the persisted last
// equity serves with its own timestamp so staleness is honest.
func (st *riskCapitalStore) Report(c *risk.Constitution, obs *risk.CapitalObservation) rpc.CapitalStateReport {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now()

	if obs == nil && st.state.LastEquityBase > 0 {
		obs = &risk.CapitalObservation{EquityBase: st.state.LastEquityBase, AsOf: st.state.LastEquityAsOf}
	}
	_, statementFlows, flowSource := st.effectiveFlowsLocked(c)
	v := risk.EvaluateCapital(c, st.runtimeLocked(c, now), obs, now)

	rep := rpc.CapitalStateReport{
		Tier:                     v.Tier,
		Enforcement:              constitutionEnforcement(c),
		EquityStale:              v.EquityStale,
		EffectiveRiskCapitalBase: v.EffectiveRiskCapitalBase,
		DrawdownBase:             v.DrawdownBase,
		ConsumedPct:              v.ConsumedPct,
		BlockLatched:             st.state.BlockLatched,
		LatchedAt:                st.state.LatchedAt,
		LastReconciledAt:         st.lastReconciledAt,
		LastReconcileReportID:    st.lastReconcileReportID,
		LastReconcileSource:      st.lastReconcileSource,
		ReconcileStale:           v.ReconcileStale,
		Reasons:                  v.Reasons,
	}
	declared := st.cumFlowsBase
	rep.DeclaredCumFlowsBase = &declared
	rep.FlowSource = flowSource
	if c != nil && c.PolicyVersion >= 3 {
		rep.StatementCumFlowsBase = &statementFlows
	}
	if c != nil {
		rep.BaseCurrency = c.Capital.BaseCurrency
	}
	if obs != nil {
		rep.EquityBase = &obs.EquityBase
		rep.EquityAsOf = obs.AsOf
	}
	if st.state.Seeded {
		peak := st.state.AdjustedPeakBase
		// Preserve the existing wire field's declared-ledger meaning. The
		// evaluator receives effectiveFlows; the additive dual-compute fields
		// disclose both inputs and FlowSource identifies the selected one.
		flows := declared
		rep.AdjustedPeakBase = &peak
		rep.PeakAsOf = st.state.PeakAsOf
		rep.CumExternalFlowsBase = &flows
	}
	return rep
}

// ActiveOverrides prunes expired overrides and returns the full record list
// (active first ordering is the caller's concern; expiry is journaled once).
func (st *riskCapitalStore) ActiveOverrides() []rpc.OverrideRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	now := st.now()
	changed := false
	for i := range st.state.Overrides {
		o := &st.state.Overrides[i]
		if o.Active && now.After(o.ExpiresAt) {
			o.Active = false
			changed = true
			appendRiskPolicyJournal(map[string]any{
				"version": 1, "at": now.UTC(), "kind": "override_expired", "id": o.ID, "control": o.Control,
			})
		}
	}
	if changed {
		st.persistLocked(true)
	}
	out := make([]rpc.OverrideRecord, len(st.state.Overrides))
	copy(out, st.state.Overrides)
	return out
}

// Artefacts returns the latest completion per declared artefact.
func (st *riskCapitalStore) Artefacts() []rpc.ArtefactRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	out := make([]rpc.ArtefactRecord, len(st.state.Artefacts))
	copy(out, st.state.Artefacts)
	return out
}

// OverridesSnapshot returns the persisted override rows without expiring or
// journaling them. Read-only compositions use this instead of ActiveOverrides,
// whose expiry maintenance is intentionally write-bearing.
func (st *riskCapitalStore) OverridesSnapshot() []rpc.OverrideRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	out := make([]rpc.OverrideRecord, len(st.state.Overrides))
	copy(out, st.state.Overrides)
	return out
}

// UnreconciledClock returns the evaluator's exact deadline projection without
// mutating runtime state.
func (st *riskCapitalStore) UnreconciledClock(c *risk.Constitution, now time.Time) risk.UnreconciledClock {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	var maxDays *int
	if c != nil {
		maxDays = c.Capital.MaxUnreconciledDays
	}
	rt := st.runtimeLocked(c, now)
	return risk.EvaluateUnreconciledClock(maxDays, rt.LastReconciledAt, rt.UnreconciledOverrideUntil, now)
}

// NudgeLatch returns only an opaque episode identity plus the authoritative
// open/occurred facts. It never exposes capital values or changes latch state.
func (st *riskCapitalStore) NudgeLatch() (open bool, episode string, occurredAt time.Time) {
	if st == nil {
		return false, "", time.Time{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if !st.state.BlockLatched || st.state.LatchedAt.IsZero() {
		return false, "", time.Time{}
	}
	sequence := st.state.LatchEpisodeSeq
	if sequence == 0 {
		sequence = 1
	}
	return true, opaqueIdentity("drawdown-latch", st.state.LatchedAt.UTC().Format(time.RFC3339Nano), fmt.Sprintf("%d", sequence)), st.state.LatchedAt
}

// LastEquity returns the persisted last equity observation for the recon
// equity-divergence check; zero when never observed.
func (st *riskCapitalStore) LastEquity() (float64, time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.state.LastEquityBase, st.state.LastEquityAsOf
}

// DailySample returns the runtime equity sample for one UTC day key.
func (st *riskCapitalStore) DailySample(day string) (float64, bool) {
	if st == nil {
		return 0, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	equity, ok := st.state.DailyEquity[day]
	return equity, ok
}

// capitalReplayContext is the read-only snapshot the backtest replay
// consumes.
type capitalReplayContext struct {
	GenesisAt        time.Time
	Seeded           bool
	AdjustedPeakBase float64
	PeakAsOf         time.Time
	LatchedAt        time.Time
	CumFlowsBase     float64
	DailyEquity      map[string]float64 // copy
}

// ReplayContext returns an isolated copy of the runtime facts used by the
// capital-ladder backtest.
func (st *riskCapitalStore) ReplayContext() capitalReplayContext {
	if st == nil {
		return capitalReplayContext{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	ctx := capitalReplayContext{
		GenesisAt:        st.state.GenesisAt,
		Seeded:           st.state.Seeded,
		AdjustedPeakBase: st.state.AdjustedPeakBase,
		PeakAsOf:         st.state.PeakAsOf,
		LatchedAt:        st.state.LatchedAt,
		CumFlowsBase:     st.cumFlowsBase,
	}
	if len(st.state.DailyEquity) > 0 {
		ctx.DailyEquity = make(map[string]float64, len(st.state.DailyEquity))
		maps.Copy(ctx.DailyEquity, st.state.DailyEquity)
	}
	return ctx
}

func (st *riskCapitalStore) LastAutoExtend() (string, time.Time) {
	if st == nil {
		return "", time.Time{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.lastAutoExtendReportID, st.lastAutoExtendedAt
}

func (st *riskCapitalStore) EnsureLoaded() {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
}

// PreviewVerdict is the cheap in-memory evaluation for advisory preview
// causes: persisted last equity only, never an account fetch (a preview
// must stay a preview-priced call; rulebook precedent).
func (st *riskCapitalStore) PreviewVerdict(c *risk.Constitution) risk.CapitalVerdict {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	var obs *risk.CapitalObservation
	if st.state.LastEquityBase > 0 {
		obs = &risk.CapitalObservation{EquityBase: st.state.LastEquityBase, AsOf: st.state.LastEquityAsOf}
	}
	now := st.now()
	return risk.EvaluateCapital(c, st.runtimeLocked(c, now), obs, now)
}

func artefactDeclaredClass(c *risk.Constitution, pick func(*risk.Constitution) string) string {
	if c == nil {
		return ""
	}
	return pick(c)
}

func constitutionFingerprint(c *risk.Constitution) string {
	if c == nil {
		return ""
	}
	return c.FingerprintKey()
}

func constitutionEnforcement(c *risk.Constitution) string {
	if c == nil {
		return risk.EnforcementShadow
	}
	return c.EffectiveBlockEnforcement()
}

func appendCapitalEvent(ev capitalEventV1) error {
	path, err := defaultTradingStatePath(capitalEventsJournalFile)
	if err != nil {
		return err
	}
	if err := ensurePrivateStateDir(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(ev)
}

// appendRiskPolicyJournal records one governance event. Unlike
// rules-decisions.jsonl this journal always carries the policy fingerprint
// key, so calibration replay can prove which exact policy produced a
// transition. Best-effort: journaling never fails the caller.
func appendRiskPolicyJournal(entry map[string]any) {
	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		return
	}
	if err := ensurePrivateStateDir(path); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(entry)
}

// journalRiskPolicyTransition records manager status transitions
// (active/absent/drift/error) with full policy identity.
func (s *Server) journalRiskPolicyTransition(prev, next string, c *risk.Constitution) {
	entry := map[string]any{
		"version": 1, "at": time.Now().UTC(), "kind": "policy_status", "from": prev, "to": next,
	}
	if c != nil {
		entry["policy_id"] = c.PolicyID
		entry["policy_version"] = c.PolicyVersion
		entry["fingerprint_version"] = rpc.RiskConstitutionFingerprintVersion
		entry["policy_fingerprint"] = c.FingerprintKey()
	}
	appendRiskPolicyJournal(entry)
}
