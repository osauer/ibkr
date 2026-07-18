package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// The risk constitution (docs/design/risk-policy.md) is the operator's
// authoritative personal capital policy: protected floor, declared risk
// capital, drawdown response ladder, override governance, cadence artefact
// declarations, and sibling-policy pins. It is loaded from
// ~/.config/ibkr/policies/risk-policy.toml by the daemon manager.
//
// Two properties distinguish it from the sibling policies:
//
//   - No embedded default. A missing file or a missing material key is
//     "unapproved", never a code value — material limits must be written by
//     the operator (interview decisions of 2026-07-12). Material keys are
//     pointers; nil means unapproved.
//   - Non-overridable safety invariants (account/route pins, WhatIf, preview
//     tokens, journaling, agent-origin gating, freeze) deliberately have no
//     keys in this schema, so no revision or override can reach them.

const ConstitutionKind = "ibkr.risk_policy"

// Capital tier vocabulary. Data absence never renders ok: unapproved and
// unknown are load-bearing non-ok states (rulebook never-false-pass
// precedent).
const (
	CapitalTierOK         = "ok"
	CapitalTierWarn       = "warn"
	CapitalTierBlock      = "block"
	CapitalTierUnknown    = "unknown"
	CapitalTierUnapproved = "unapproved"
)

// Enforcement classes a constitution control may declare. "hard" is
// deliberately not accepted by v1 validation: promotion to a real pre-trade
// gate is a later human policy revision made after the shadow period
// (phase-1 decision; docs/design/risk-policy.md).
const (
	EnforcementShadow   = "shadow"
	EnforcementAdvisory = "advisory"
)

// Constitution is the typed schema of risk-policy.toml. Material limits are
// pointers: nil = unapproved, and validation never backfills them.
type Constitution struct {
	Kind          string `toml:"kind" json:"kind"`
	SchemaVersion int    `toml:"schema_version" json:"schema_version"`
	PolicyID      string `toml:"policy_id" json:"policy_id"`
	PolicyVersion int    `toml:"policy_version" json:"policy_version"`

	Capital   ConstitutionCapital   `toml:"capital" json:"capital"`
	Drawdown  ConstitutionDrawdown  `toml:"drawdown" json:"drawdown"`
	Override  ConstitutionOverride  `toml:"override" json:"override"`
	Recon     ConstitutionRecon     `toml:"recon" json:"recon"`
	Cadence   ConstitutionCadence   `toml:"cadence" json:"cadence"`
	Inventory ConstitutionInventory `toml:"inventory" json:"inventory"`
}

// ConstitutionCapital anchors the capital authority: an internal protected
// equity floor and a declared (human-authorized) risk capital, both in the
// account base currency. Effective risk capital =
// min(declared_risk_capital, equity − protected_floor); nothing —
// deposits, profits, live events — raises the declared number without a
// fingerprinted policy revision.
type ConstitutionCapital struct {
	BaseCurrency        string   `toml:"base_currency" json:"base_currency"`
	ProtectedFloor      *float64 `toml:"protected_floor" json:"protected_floor"`
	DeclaredRiskCapital *float64 `toml:"declared_risk_capital" json:"declared_risk_capital"`
	// MaxEquityAgeMinutes bounds trust in the last equity observation;
	// beyond it the capital state is stale (tier unknown while advisory,
	// fail-closed once the block control is promoted to hard).
	MaxEquityAgeMinutes *int `toml:"max_equity_age_minutes" json:"max_equity_age_minutes"`
	// MaxUnreconciledDays bounds trust in the declared capital-event ledger
	// between reconcile attestations; same posture as equity age.
	MaxUnreconciledDays *int `toml:"max_unreconciled_days" json:"max_unreconciled_days"`
}

// ConstitutionDrawdown is the two-tier response ladder. Both thresholds are
// percentages of declared risk capital consumed from the cash-flow-adjusted
// equity peak. Warn is advisory and self-clearing; block latches in daemon
// state and clears only through a journaled human reset that re-bases the
// peak (interview decisions 4 and 5).
type ConstitutionDrawdown struct {
	WarnConsumedPct  *float64 `toml:"warn_consumed_pct" json:"warn_consumed_pct"`
	BlockConsumedPct *float64 `toml:"block_consumed_pct" json:"block_consumed_pct"`
	// BlockEnforcement is shadow (default when empty) or advisory in v1.
	BlockEnforcement string `toml:"block_enforcement" json:"block_enforcement"`
}

// ConstitutionOverride caps the one-shot exception mechanism: human-only,
// single named control, reason required, hard expiry. The mechanism itself
// (origin gating, journaling) is code-owned; only the lifetime cap is
// policy.
type ConstitutionOverride struct {
	MaxDurationHours *int `toml:"max_duration_hours" json:"max_duration_hours"`
}

// ConstitutionRecon sets what counts as a reconciliation exception when
// broker statement flows are matched against the declared capital-event
// ledger (docs/design/post-trade-truth.md). These are policy, not
// plumbing: they decide which differences the operator must look at.
type ConstitutionRecon struct {
	// A statement flow and a declared event match on amount when they
	// differ by at most max(amount_tolerance_pct% of the statement
	// amount, amount_tolerance_min in base currency).
	AmountTolerancePct *float64 `toml:"amount_tolerance_pct" json:"amount_tolerance_pct"`
	AmountToleranceMin *float64 `toml:"amount_tolerance_min" json:"amount_tolerance_min"`
	// DateWindowBusinessDays bounds how far apart the statement value
	// date and the declared effective date may sit (weekday count).
	DateWindowBusinessDays *int `toml:"date_window_business_days" json:"date_window_business_days"`
	// MaxReportAgeDays bounds how old the newest ingested statement may
	// be for a recon report to back a reconcile sign-off.
	MaxReportAgeDays *int `toml:"max_report_age_days" json:"max_report_age_days"`
	// MaxEquityDivergencePct bounds the absolute same-day difference between
	// broker statement equity and the runtime observation before v3 may
	// automatically accept a clean report as reconcile evidence.
	MaxEquityDivergencePct *float64 `toml:"max_equity_divergence_pct" json:"max_equity_divergence_pct"`
}

// ConstitutionCadence declares the operating-cadence artefacts so their
// completion can be journaled (phase-2 adherence data). All artefacts are
// advisory in v1: a missed artefact is recorded, never blocking (interview
// decision 8).
type ConstitutionCadence struct {
	Morning ConstitutionArtefact `toml:"morning" json:"morning"`
	EOD     ConstitutionArtefact `toml:"eod" json:"eod"`
	Weekly  ConstitutionArtefact `toml:"weekly" json:"weekly"`
}

// ConstitutionArtefact declares one cadence artefact. Class "" means the
// artefact is not declared; "advisory" is the only accepted class in v1.
type ConstitutionArtefact struct {
	Class string `toml:"class" json:"class,omitempty"`
}

// ConstitutionInventory pins the sibling policies by identity so the policy
// view can disclose drift between what the constitution was approved
// against and what is live. Pins are identity references, not threshold
// copies: the siblings stay authoritative for their own numbers.
type ConstitutionInventory struct {
	Rulebook   *ConstitutionPolicyPin `toml:"rulebook" json:"rulebook,omitempty"`
	Protection *ConstitutionPolicyPin `toml:"protection" json:"protection,omitempty"`
	Canary     *ConstitutionPolicyPin `toml:"canary" json:"canary,omitempty"`
}

// ConstitutionPolicyPin identifies one sibling policy version. Version is a
// string so integer-versioned (rulebook, protection) and string-versioned
// (canary) policies pin uniformly.
type ConstitutionPolicyPin struct {
	ID      string `toml:"id" json:"id"`
	Version string `toml:"version" json:"version"`
}

// Validate rejects a structurally unusable constitution. It never backfills
// material keys: a file that is valid but incomplete loads with unapproved
// gaps, which is the intended state until the operator writes each number.
func (c Constitution) Validate() error {
	if c.Kind != ConstitutionKind {
		return fmt.Errorf("risk policy kind %q is invalid (want %s)", c.Kind, ConstitutionKind)
	}
	if c.SchemaVersion != 1 {
		return fmt.Errorf("risk policy schema_version %d is unsupported", c.SchemaVersion)
	}
	if strings.TrimSpace(c.PolicyID) == "" {
		return fmt.Errorf("risk policy policy_id is required")
	}
	if c.PolicyVersion <= 0 {
		return fmt.Errorf("risk policy policy_version must be positive")
	}
	if cur := strings.TrimSpace(c.Capital.BaseCurrency); cur != "" && len(cur) != 3 {
		return fmt.Errorf("capital.base_currency %q must be a 3-letter currency code", c.Capital.BaseCurrency)
	}
	if v := c.Capital.ProtectedFloor; v != nil && *v < 0 {
		return fmt.Errorf("capital.protected_floor must not be negative")
	}
	if v := c.Capital.DeclaredRiskCapital; v != nil && *v <= 0 {
		return fmt.Errorf("capital.declared_risk_capital must be positive")
	}
	if v := c.Capital.MaxEquityAgeMinutes; v != nil && *v <= 0 {
		return fmt.Errorf("capital.max_equity_age_minutes must be positive")
	}
	if v := c.Capital.MaxUnreconciledDays; v != nil && *v <= 0 {
		return fmt.Errorf("capital.max_unreconciled_days must be positive")
	}
	warn, block := c.Drawdown.WarnConsumedPct, c.Drawdown.BlockConsumedPct
	if warn != nil && (*warn <= 0 || *warn > 100) {
		return fmt.Errorf("drawdown.warn_consumed_pct must be in (0, 100]")
	}
	if block != nil && (*block <= 0 || *block > 100) {
		return fmt.Errorf("drawdown.block_consumed_pct must be in (0, 100]")
	}
	if warn != nil && block != nil && *warn >= *block {
		return fmt.Errorf("drawdown.warn_consumed_pct must be below block_consumed_pct")
	}
	switch c.Drawdown.BlockEnforcement {
	case "", EnforcementShadow, EnforcementAdvisory:
	case "hard":
		return fmt.Errorf("drawdown.block_enforcement %q is not promotable in schema v1; promotion to a pre-trade gate is a later human policy revision", c.Drawdown.BlockEnforcement)
	default:
		return fmt.Errorf("drawdown.block_enforcement %q is invalid; use shadow or advisory", c.Drawdown.BlockEnforcement)
	}
	if v := c.Override.MaxDurationHours; v != nil && *v <= 0 {
		return fmt.Errorf("override.max_duration_hours must be positive")
	}
	if v := c.Recon.AmountTolerancePct; v != nil && (*v < 0 || *v > 100) {
		return fmt.Errorf("recon.amount_tolerance_pct must be in [0, 100]")
	}
	if v := c.Recon.AmountToleranceMin; v != nil && *v < 0 {
		return fmt.Errorf("recon.amount_tolerance_min must not be negative")
	}
	if v := c.Recon.DateWindowBusinessDays; v != nil && *v <= 0 {
		return fmt.Errorf("recon.date_window_business_days must be positive")
	}
	if v := c.Recon.MaxReportAgeDays; v != nil && *v <= 0 {
		return fmt.Errorf("recon.max_report_age_days must be positive")
	}
	if v := c.Recon.MaxEquityDivergencePct; v != nil {
		if c.PolicyVersion < 3 {
			return fmt.Errorf("recon.max_equity_divergence_pct requires policy_version >= 3")
		}
		if math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
			return fmt.Errorf("recon.max_equity_divergence_pct must be positive and finite")
		}
	}
	for _, a := range []struct {
		key   string
		class string
	}{
		{"cadence.morning", c.Cadence.Morning.Class},
		{"cadence.eod", c.Cadence.EOD.Class},
		{"cadence.weekly", c.Cadence.Weekly.Class},
	} {
		if a.class != "" && a.class != EnforcementAdvisory {
			return fmt.Errorf("%s.class %q is invalid; only advisory is accepted in v1", a.key, a.class)
		}
	}
	for _, p := range []struct {
		key string
		pin *ConstitutionPolicyPin
	}{
		{"inventory.rulebook", c.Inventory.Rulebook},
		{"inventory.protection", c.Inventory.Protection},
		{"inventory.canary", c.Inventory.Canary},
	} {
		if p.pin != nil && (strings.TrimSpace(p.pin.ID) == "" || strings.TrimSpace(p.pin.Version) == "") {
			return fmt.Errorf("%s pin needs both id and version", p.key)
		}
	}
	return nil
}

// EffectiveBlockEnforcement resolves the block tier's enforcement class;
// empty defaults to shadow — the fail-safe direction (observe and journal,
// never gate).
func (c Constitution) EffectiveBlockEnforcement() string {
	if c.Drawdown.BlockEnforcement == "" {
		return EnforcementShadow
	}
	return c.Drawdown.BlockEnforcement
}

// UnapprovedKeys lists the material keys the operator has not chosen yet.
// Order matches the explain view.
func (c Constitution) UnapprovedKeys() []string {
	var out []string
	if strings.TrimSpace(c.Capital.BaseCurrency) == "" {
		out = append(out, "capital.base_currency")
	}
	if c.Capital.ProtectedFloor == nil {
		out = append(out, "capital.protected_floor")
	}
	if c.Capital.DeclaredRiskCapital == nil {
		out = append(out, "capital.declared_risk_capital")
	}
	if c.Capital.MaxEquityAgeMinutes == nil {
		out = append(out, "capital.max_equity_age_minutes")
	}
	if c.Capital.MaxUnreconciledDays == nil {
		out = append(out, "capital.max_unreconciled_days")
	}
	if c.Drawdown.WarnConsumedPct == nil {
		out = append(out, "drawdown.warn_consumed_pct")
	}
	if c.Drawdown.BlockConsumedPct == nil {
		out = append(out, "drawdown.block_consumed_pct")
	}
	if c.Override.MaxDurationHours == nil {
		out = append(out, "override.max_duration_hours")
	}
	if c.Recon.AmountTolerancePct == nil {
		out = append(out, "recon.amount_tolerance_pct")
	}
	if c.Recon.AmountToleranceMin == nil {
		out = append(out, "recon.amount_tolerance_min")
	}
	if c.Recon.DateWindowBusinessDays == nil {
		out = append(out, "recon.date_window_business_days")
	}
	if c.Recon.MaxReportAgeDays == nil {
		out = append(out, "recon.max_report_age_days")
	}
	if (c.PolicyVersion == 0 || c.PolicyVersion >= 3) && c.Recon.MaxEquityDivergencePct == nil {
		out = append(out, "recon.max_equity_divergence_pct")
	}
	return out
}

// FingerprintKey hashes an explicit JSON projection of the full policy
// (risk.Policy / protection-policy discipline — the rulebook's %.4f
// variant is the outlier, not the model). Absent material keys marshal as
// null and are part of the identity: an unapproved gap is policy state.
func (c Constitution) FingerprintKey() string {
	type fingerprintBase struct {
		Kind          string                `json:"kind"`
		SchemaVersion int                   `json:"schema_version"`
		PolicyID      string                `json:"policy_id"`
		PolicyVersion int                   `json:"policy_version"`
		Capital       ConstitutionCapital   `json:"capital"`
		Drawdown      ConstitutionDrawdown  `json:"drawdown"`
		Override      ConstitutionOverride  `json:"override"`
		Cadence       ConstitutionCadence   `json:"cadence"`
		Inventory     ConstitutionInventory `json:"inventory"`
	}
	base := fingerprintBase{
		Kind: strings.TrimSpace(c.Kind), SchemaVersion: c.SchemaVersion,
		PolicyID: strings.TrimSpace(c.PolicyID), PolicyVersion: c.PolicyVersion,
		Capital: c.Capital, Drawdown: c.Drawdown, Override: c.Override,
		Cadence: c.Cadence, Inventory: c.Inventory,
	}
	var raw []byte
	if c.PolicyVersion < 3 {
		// Preserve the pre-v3 projection byte-for-byte: adding a nil v3-only
		// field must not change an existing policy fingerprint.
		recon := struct {
			AmountTolerancePct     *float64 `json:"amount_tolerance_pct"`
			AmountToleranceMin     *float64 `json:"amount_tolerance_min"`
			DateWindowBusinessDays *int     `json:"date_window_business_days"`
			MaxReportAgeDays       *int     `json:"max_report_age_days"`
		}{c.Recon.AmountTolerancePct, c.Recon.AmountToleranceMin, c.Recon.DateWindowBusinessDays, c.Recon.MaxReportAgeDays}
		normalized := struct {
			Kind          string                `json:"kind"`
			SchemaVersion int                   `json:"schema_version"`
			PolicyID      string                `json:"policy_id"`
			PolicyVersion int                   `json:"policy_version"`
			Capital       ConstitutionCapital   `json:"capital"`
			Drawdown      ConstitutionDrawdown  `json:"drawdown"`
			Override      ConstitutionOverride  `json:"override"`
			Recon         any                   `json:"recon"`
			Cadence       ConstitutionCadence   `json:"cadence"`
			Inventory     ConstitutionInventory `json:"inventory"`
		}{
			Kind: base.Kind, SchemaVersion: base.SchemaVersion, PolicyID: base.PolicyID, PolicyVersion: base.PolicyVersion,
			Capital: base.Capital, Drawdown: base.Drawdown, Override: base.Override, Recon: recon,
			Cadence: base.Cadence, Inventory: base.Inventory,
		}
		raw, _ = json.Marshal(normalized)
	} else {
		normalized := struct {
			Kind          string                `json:"kind"`
			SchemaVersion int                   `json:"schema_version"`
			PolicyID      string                `json:"policy_id"`
			PolicyVersion int                   `json:"policy_version"`
			Capital       ConstitutionCapital   `json:"capital"`
			Drawdown      ConstitutionDrawdown  `json:"drawdown"`
			Override      ConstitutionOverride  `json:"override"`
			Recon         ConstitutionRecon     `json:"recon"`
			Cadence       ConstitutionCadence   `json:"cadence"`
			Inventory     ConstitutionInventory `json:"inventory"`
		}{
			Kind:          strings.TrimSpace(c.Kind),
			SchemaVersion: c.SchemaVersion,
			PolicyID:      strings.TrimSpace(c.PolicyID),
			PolicyVersion: c.PolicyVersion,
			Capital:       c.Capital,
			Drawdown:      c.Drawdown,
			Override:      c.Override,
			Recon:         c.Recon,
			Cadence:       c.Cadence,
			Inventory:     c.Inventory,
		}
		raw, _ = json.Marshal(normalized)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CapitalObservation is one equity reading in base currency.
type CapitalObservation struct {
	EquityBase float64
	AsOf       time.Time
}

// CapitalRuntime is the daemon-owned runtime state the evaluator consumes:
// the cash-flow-adjusted peak, effective cumulative external flows, the
// drawdown latch, and reconciliation recency. The daemon owns mutation; the
// evaluator only reads.
type CapitalRuntime struct {
	// AdjustedPeakBase is the peak of (equity − cumulative external flows).
	AdjustedPeakBase float64
	PeakAsOf         time.Time
	// CumExternalFlowsBase is the policy-version-selected cumulative flow
	// input: declared events through v2, statement truth plus bridges in v3.
	CumExternalFlowsBase float64
	// Seeded is false until the first equity observation establishes the
	// peak; an unseeded state evaluates unknown, never ok.
	Seeded bool
	// BlockLatched persists across restarts and mark recovery; only a
	// journaled human reset clears it.
	BlockLatched bool
	// LastReconciledAt is the last human or automatic reconcile evidence;
	// zero means never reconciled.
	LastReconciledAt time.Time
	// UnreconciledOverrideUntil is populated only from an active, unexpired
	// one-shot override on capital.max_unreconciled_days. No other override
	// control reaches evaluation.
	UnreconciledOverrideUntil time.Time
}

// CapitalVerdict is the pure evaluation result.
type CapitalVerdict struct {
	Tier string
	// EffectiveRiskCapitalBase = min(declared, equity − floor); nil when
	// unapproved inputs or no usable equity observation.
	EffectiveRiskCapitalBase *float64
	// DrawdownBase and ConsumedPct measure from the cash-flow-adjusted
	// peak; ConsumedPct is drawdown / declared risk capital × 100.
	DrawdownBase *float64
	ConsumedPct  *float64
	EquityStale  bool
	// ReconcileStale means the declared-events ledger is older than
	// capital.max_unreconciled_days (or never attested).
	ReconcileStale bool
	Unapproved     []string
	Reasons        []string
}

// UnreconciledClock is the shared pure projection of the constitution's
// unreconciled horizon. Approved is false when the operator has not declared
// capital.max_unreconciled_days. A zero LastReconciledAt is stale with no
// fabricated year-one deadline.
type UnreconciledClock struct {
	Approved      bool
	Deadline      time.Time
	DaysRemaining *int
	Stale         bool
}

// EvaluateUnreconciledClock computes the deadline used by both capital
// evaluation and reporting. The one-shot outage override may only extend the
// ordinary deadline; it can never shorten it.
func EvaluateUnreconciledClock(maxDays *int, lastReconciledAt, overrideUntil, now time.Time) UnreconciledClock {
	if maxDays == nil {
		return UnreconciledClock{}
	}
	out := UnreconciledClock{Approved: true}
	if lastReconciledAt.IsZero() {
		out.Stale = true
		return out
	}
	out.Deadline = lastReconciledAt.Add(time.Duration(*maxDays) * 24 * time.Hour)
	if overrideUntil.After(out.Deadline) {
		out.Deadline = overrideUntil
	}
	remaining := int(math.Ceil(out.Deadline.Sub(now).Hours() / 24))
	out.DaysRemaining = &remaining
	out.Stale = now.After(out.Deadline)
	return out
}

// EvaluateCapital applies the constitution to the runtime state and the
// latest observation. Invariants: absence of data or of approved numbers
// never yields ok; the latch dominates everything except unapproved
// disclosure; risk-reducing exemptions are the caller's concern (order
// classification lives on the preview path, not here).
func EvaluateCapital(c *Constitution, rt CapitalRuntime, obs *CapitalObservation, now time.Time) CapitalVerdict {
	v := CapitalVerdict{Tier: CapitalTierUnknown}
	if c == nil {
		v.Tier = CapitalTierUnapproved
		v.Reasons = append(v.Reasons, "no risk policy file loaded; every capital control is unapproved")
		return v
	}
	v.Unapproved = c.UnapprovedKeys()

	// Reconciliation recency is reportable whenever its horizon exists.
	if clock := EvaluateUnreconciledClock(c.Capital.MaxUnreconciledDays, rt.LastReconciledAt, rt.UnreconciledOverrideUntil, now); clock.Approved {
		if clock.Stale {
			v.ReconcileStale = true
			reason := "capital ledger is past its reconcile horizon; declared events are unattested"
			if c.PolicyVersion >= 3 {
				reason = "reconcile evidence is past capital.max_unreconciled_days; no current automatic clean-report extension or human sign-off"
			}
			v.Reasons = append(v.Reasons, reason)
		}
	}

	usableObs := obs != nil && !obs.AsOf.IsZero() && obs.EquityBase > 0
	if usableObs && c.Capital.MaxEquityAgeMinutes != nil {
		if now.Sub(obs.AsOf) > time.Duration(*c.Capital.MaxEquityAgeMinutes)*time.Minute {
			v.EquityStale = true
			v.Reasons = append(v.Reasons, "equity observation is older than capital.max_equity_age_minutes")
		}
	}

	floor, declared := c.Capital.ProtectedFloor, c.Capital.DeclaredRiskCapital
	if usableObs && floor != nil && declared != nil {
		eff := min(*declared, obs.EquityBase-*floor)
		v.EffectiveRiskCapitalBase = &eff
	}
	if usableObs && rt.Seeded && declared != nil {
		adjusted := obs.EquityBase - rt.CumExternalFlowsBase
		dd := max(rt.AdjustedPeakBase-adjusted, 0)
		pct := dd / *declared * 100
		v.DrawdownBase = &dd
		v.ConsumedPct = &pct
	}

	// The latch dominates: a breached block stays block until a human
	// reset, regardless of recovery, staleness, or later policy edits.
	if rt.BlockLatched {
		v.Tier = CapitalTierBlock
		v.Reasons = append(v.Reasons, "drawdown block is latched; a journaled human reset (with re-based peak) is required to resume risk")
		return v
	}
	if len(v.Unapproved) > 0 {
		v.Tier = CapitalTierUnapproved
		return v
	}
	if !usableObs || !rt.Seeded {
		v.Reasons = append(v.Reasons, "no usable equity observation; capital tier is unknown, never ok")
		return v
	}
	if v.EquityStale || v.ReconcileStale {
		return v // tier stays unknown: stale inputs never pass (decision 7)
	}
	switch {
	case v.ConsumedPct != nil && *v.ConsumedPct >= *c.Drawdown.BlockConsumedPct:
		v.Tier = CapitalTierBlock
	case v.ConsumedPct != nil && *v.ConsumedPct >= *c.Drawdown.WarnConsumedPct:
		v.Tier = CapitalTierWarn
	default:
		v.Tier = CapitalTierOK
	}
	return v
}
