package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Runtime capital state for the risk constitution (docs/design/risk-policy.md):
// the cash-flow-adjusted equity peak, the drawdown latch, declared capital
// events, one-shot overrides, and cadence artefact completions.
//
// Authority split: risk-policy.toml owns the numbers, internal/risk owns the
// evaluation, this store owns observed/derived runtime facts. Aggregates that
// can be re-derived from the capital-events journal (cumulative flows, last
// reconcile attestation) are re-derived on every load — the journal is the
// source of truth, the state file is a cache of what journals cannot rebuild
// (peak, latch, overrides; regime-latch never-trust-stored-bytes precedent).
//
// Nothing in this file may influence submit eligibility, blockers, freeze,
// pins, or tokens: v1 is advisory/shadow end to end.

const (
	riskCapitalStateFile     = "risk-capital-state.json"
	capitalEventsJournalFile = "capital-events.jsonl"
	riskPolicyJournalFile    = "risk-policy-journal.jsonl"
	riskCapitalStateVer      = 1
	// riskCapitalPersistEvery throttles equity-cache persistence; latch,
	// peak, and event writes always persist immediately.
	riskCapitalPersistEvery = time.Minute
)

type riskCapitalStateFileV1 struct {
	Version          int                  `json:"version"`
	GenesisAt        time.Time            `json:"genesis_at,omitzero"`
	Seeded           bool                 `json:"seeded"`
	AdjustedPeakBase float64              `json:"adjusted_peak_base"`
	PeakAsOf         time.Time            `json:"peak_as_of,omitzero"`
	LastEquityBase   float64              `json:"last_equity_base"`
	LastEquityAsOf   time.Time            `json:"last_equity_as_of,omitzero"`
	LastTier         string               `json:"last_tier,omitempty"`
	BlockLatched     bool                 `json:"block_latched"`
	LatchedAt        time.Time            `json:"latched_at,omitzero"`
	LatchConsumedPct float64              `json:"latch_consumed_pct,omitempty"`
	Overrides        []rpc.OverrideRecord `json:"overrides,omitempty"`
	Artefacts        []rpc.ArtefactRecord `json:"artefacts,omitempty"`
}

type capitalEventV1 struct {
	Version     int       `json:"version"`
	At          time.Time `json:"at"`
	Type        string    `json:"type"` // deposit | withdrawal | reconcile
	AmountBase  float64   `json:"amount_base,omitempty"`
	EffectiveAt time.Time `json:"effective_at,omitzero"`
	Note        string    `json:"note,omitempty"`
	Origin      string    `json:"origin,omitempty"`
}

type riskCapitalStore struct {
	mu     sync.Mutex
	now    func() time.Time
	loaded bool
	state  riskCapitalStateFileV1
	// Re-derived from capital-events.jsonl on load, maintained incrementally
	// afterwards; never trusted from the state file.
	cumFlowsBase     float64
	lastReconciledAt time.Time
	lastPersistAt    time.Time
	overrideSeq      int
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
	// Journal replay owns flows and reconciliation recency.
	st.cumFlowsBase, st.lastReconciledAt = replayCapitalEvents()
}

func replayCapitalEvents() (cumFlows float64, lastReconciled time.Time) {
	path, err := defaultTradingStatePath(capitalEventsJournalFile)
	if err != nil {
		return 0, time.Time{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, time.Time{}
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
			cumFlows += ev.AmountBase
		case "withdrawal":
			cumFlows -= ev.AmountBase
		case "reconcile":
			if ev.At.After(lastReconciled) {
				lastReconciled = ev.At
			}
		}
	}
	return cumFlows, lastReconciled
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
func (st *riskCapitalStore) runtimeLocked() risk.CapitalRuntime {
	return risk.CapitalRuntime{
		AdjustedPeakBase:     st.state.AdjustedPeakBase,
		PeakAsOf:             st.state.PeakAsOf,
		CumExternalFlowsBase: st.cumFlowsBase,
		Seeded:               st.state.Seeded,
		BlockLatched:         st.state.BlockLatched,
		LastReconciledAt:     st.lastReconciledAt,
	}
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

	adjusted := equityBase - st.cumFlowsBase
	if !st.state.Seeded || adjusted > st.state.AdjustedPeakBase {
		st.state.Seeded = true
		st.state.AdjustedPeakBase = adjusted
		st.state.PeakAsOf = asOf
		force = true
	}

	obs := risk.CapitalObservation{EquityBase: equityBase, AsOf: asOf}
	v := risk.EvaluateCapital(c, st.runtimeLocked(), &obs, now)
	if v.Tier == risk.CapitalTierBlock && !st.state.BlockLatched && v.ConsumedPct != nil {
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
func (st *riskCapitalStore) ApplyCapitalEvent(p rpc.CapitalEventParams, origin string) (capitalEventV1, error) {
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
	}
	if err := appendCapitalEvent(ev); err != nil {
		return capitalEventV1{}, err
	}
	switch ev.Type {
	case "deposit":
		st.cumFlowsBase += ev.AmountBase
		if st.state.Seeded && !st.state.PeakAsOf.IsZero() && !st.state.PeakAsOf.Before(ev.EffectiveAt) {
			st.state.AdjustedPeakBase -= ev.AmountBase
		}
	case "withdrawal":
		st.cumFlowsBase -= ev.AmountBase
	case "reconcile":
		if ev.At.After(st.lastReconciledAt) {
			st.lastReconciledAt = ev.At
		}
	}
	st.persistLocked(true)
	return ev, nil
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
		st.state.AdjustedPeakBase = st.state.LastEquityBase - st.cumFlowsBase
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
	rec := rpc.ArtefactRecord{Artefact: name, Class: class, CompletedAt: now, Note: strings.TrimSpace(p.Note)}
	kept := st.state.Artefacts[:0:0]
	for _, a := range st.state.Artefacts {
		if a.Artefact != name {
			kept = append(kept, a)
		}
	}
	st.state.Artefacts = append(kept, rec)
	appendRiskPolicyJournal(map[string]any{
		"version": 1, "at": now, "kind": "artefact_completed", "artefact": name,
		"note": rec.Note, "policy_fingerprint": constitutionFingerprint(c),
	})
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
	v := risk.EvaluateCapital(c, st.runtimeLocked(), obs, now)

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
		ReconcileStale:           v.ReconcileStale,
		Reasons:                  v.Reasons,
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
		flows := st.cumFlowsBase
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

// LastEquity returns the persisted last equity observation for the recon
// equity-divergence check; zero when never observed.
func (st *riskCapitalStore) LastEquity() (float64, time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	return st.state.LastEquityBase, st.state.LastEquityAsOf
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
	return risk.EvaluateCapital(c, st.runtimeLocked(), obs, st.now())
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
