package rpc

import (
	"strings"
	"time"
)

const (
	LifecycleQuiet           = "quiet"
	LifecycleEarlyWarning    = "early_warning"
	LifecycleConfirmedStress = "confirmed_stress"
	LifecyclePanic           = "panic"
	LifecycleForcedDefense   = "forced_defense"
	LifecycleStabilization   = "stabilization"
	LifecycleOpportunity     = "opportunity"
	LifecycleDataQuality     = "data_quality"

	LifecycleTimingForwardWarning = "forward_warning"
	LifecycleTimingContemporary   = "contemporaneous"
	LifecycleTimingRecovery       = "recovery"
	LifecycleTimingDataQuality    = "data_quality"

	FingerprintStabilitySemanticBuckets = "semantic_buckets_only"
)

const (
	SourceStatusOK       = "ok"
	SourceStatusPartial  = "partial"
	SourceStatusStale    = "stale"
	SourceStatusUnknown  = "unknown"
	SourceStatusDegraded = "degraded"
)

const (
	RegimeToneNormal      = "normal"
	RegimeToneWatch       = "watch"
	RegimeToneStress      = "stress"
	RegimeToneRiskOff     = "risk_off"
	RegimeToneDataQuality = "data_quality"
)

// LifecycleState is the stable monitor/orchestration surface for regime and
// canary payloads. Stage is intentionally a small state machine, while
// Evidence preserves weak/unconfirmed inputs without letting them dominate the
// trigger.
type LifecycleState struct {
	Stage       string              `json:"stage,omitempty"`
	Scope       string              `json:"scope,omitempty"`
	Severity    string              `json:"severity,omitempty"`
	Readiness   string              `json:"readiness,omitempty"`
	Timing      string              `json:"timing,omitempty"`
	Confidence  string              `json:"confidence,omitempty"`
	Evidence    []LifecycleEvidence `json:"evidence,omitempty"`
	ConfirmedBy []string            `json:"confirmed_by,omitempty"`
	Unconfirmed []string            `json:"unconfirmed,omitempty"`
	Suppressed  []string            `json:"suppressed,omitempty"`
	RejectedBy  []string            `json:"rejected_by,omitempty"`
	// Governors discloses every policy downgrade applied after stage
	// selection (provenance gate, evidence-keyed quality cap). Nothing is
	// silently weakened: when severity reads lower than the stage suggests,
	// this is where the reason lives.
	Governors    []GovernorAction `json:"governors,omitempty"`
	Fingerprint  Fingerprint      `json:"fingerprint,omitzero"`
	NotExecution string           `json:"not_execution,omitempty"`
}

// GovernorAction is one disclosed policy downgrade. Reasons are stable
// tokens: "pending_backtest_no_tape_cosign" (heuristic threshold sets
// confirmed the stage but no fresh tape co-signature was present) and
// "confirming_cluster_quality" (a confirming cluster's data quality is
// stale/partial/degraded).
type GovernorAction struct {
	Action   string   `json:"action"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Clusters []string `json:"clusters,omitempty"`
}

type LifecycleEvidence struct {
	Source    string `json:"source,omitempty"`
	Signal    string `json:"signal,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Timing    string `json:"timing,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Confirmed bool   `json:"confirmed,omitempty"`
}

// SourceHealth is the source-level freshness and confidence contract for
// scheduled monitors. FingerprintStability says why downstream dedupe should
// key off Fingerprint rather than wall-clock AsOf churn.
type SourceHealth struct {
	Source               string       `json:"source"`
	Status               string       `json:"status"`
	AsOf                 time.Time    `json:"as_of,omitzero"`
	AgeSeconds           int64        `json:"age_seconds,omitempty"`
	MaxAgeSeconds        int64        `json:"max_age_seconds,omitempty"`
	Confidence           string       `json:"confidence,omitempty"`
	Fingerprint          *Fingerprint `json:"fingerprint,omitempty"`
	FingerprintStability string       `json:"fingerprint_stability,omitempty"`
	Notes                []string     `json:"notes,omitempty"`
}

// BuildRegimeLifecycle classifies broad-market-only regime evidence into the
// stress lifecycle used by downstream orchestration. It does not look at
// account, position, margin, or execution state.
//
// Confirmation policy (docs/design/regime-calibration.md): only ELIGIBLE
// reds — deep, persistent, cadence-fresh evidence per the shared gates —
// count toward confirmed_stress/panic and confirmed_by. Provisional reds
// stay visible, land in unconfirmed, and drive early_warning only. A
// severity governor then applies the provenance gate (heuristic
// pending_backtest evidence without a tape co-signature is capped one rung
// down) and the evidence-keyed quality cap, disclosing every downgrade in
// Governors.
func BuildRegimeLifecycle(r *RegimeSnapshotResult) LifecycleState {
	if r == nil {
		return LifecycleState{Stage: LifecycleDataQuality, Scope: "market", Severity: "watch", Readiness: "blocked", Timing: LifecycleTimingDataQuality, Confidence: "low"}
	}
	cb := BuildRegimeClusterBands(r)
	tally := tallyRegimeClusters(cb)
	confirmedNames := regimeEligibleRedNames(cb)
	yellowNames := regimeClusterNamesWithBand(cb.Confirmed, "yellow")
	unconfirmed := regimeProvisionalRedNames(cb)
	evidence := regimeLifecycleEvidence(cb, *r)
	confidence := r.Summary.Confidence
	if strings.TrimSpace(confidence) == "" {
		confidence = regimeLifecycleConfidence(tally)
	}
	state := LifecycleState{
		Scope:        "market",
		Severity:     "observe",
		Readiness:    "ready",
		Timing:       "",
		Confidence:   confidence,
		Evidence:     evidence,
		Unconfirmed:  unconfirmed,
		NotExecution: "Regime read only; no orders are placed by ibkr.",
	}
	switch {
	case tally.ranked < RegimeVerdictFloor:
		state.Stage = LifecycleDataQuality
		state.Severity = "watch"
		state.Readiness = "blocked"
		state.Timing = LifecycleTimingDataQuality
	case regimeLifecyclePanic(*r, tally):
		state.Stage = LifecyclePanic
		state.Severity = "urgent"
		state.Timing = LifecycleTimingContemporary
	case regimeLifecycleConfirmedStress(*r, tally):
		state.Stage = LifecycleConfirmedStress
		state.Severity = "act"
		state.Timing = LifecycleTimingContemporary
	case regimeLifecycleEarlyWarning(*r, tally, unconfirmed):
		state.Stage = LifecycleEarlyWarning
		state.Severity = "watch"
		state.Timing = LifecycleTimingForwardWarning
	case regimeLifecycleOpportunity(*r, tally):
		state.Stage = LifecycleOpportunity
		state.Severity = "watch"
		state.Timing = LifecycleTimingRecovery
	case regimeLifecycleStabilization(*r, tally):
		state.Stage = LifecycleStabilization
		state.Severity = "observe"
		state.Timing = LifecycleTimingRecovery
	default:
		state.Stage = LifecycleQuiet
		state.Timing = "current"
	}
	if len(unconfirmed) > 0 && (state.Stage == LifecycleQuiet || state.Stage == LifecycleOpportunity || state.Stage == LifecycleStabilization) {
		state.Suppressed = append(state.Suppressed, unconfirmed...)
	}
	if len(yellowNames) > 0 && state.Stage == LifecycleQuiet {
		state.RejectedBy = append(state.RejectedBy, yellowNames...)
	}
	if state.Stage == LifecycleConfirmedStress || state.Stage == LifecyclePanic {
		state.ConfirmedBy = confirmedNames
	}
	if state.Readiness == "ready" && (regimeLifecycleHasDegradedInputs(*r, cb) || regimeLifecycleHasWeakSourceRows(*r, cb)) {
		state.Readiness = "degraded"
		state.Confidence = capLifecycleConfidence(state.Confidence)
	}
	applyRegimeSeverityGovernor(&state, r, tally)
	state.Fingerprint = lifecycleFingerprint(state)
	return state
}

// regimeClusterTally is the lifecycle's working view of the combined cluster
// bands. red/yellow/green count CONFIRMED bands; eligibleRed and
// provisionalRed split the raw reds by confirmation eligibility.
type regimeClusterTally struct {
	ranked         int
	green          int
	yellow         int
	red            int
	unranked       int
	eligibleRed    int
	provisionalRed int
}

func tallyRegimeClusters(cb RegimeClusterBands) regimeClusterTally {
	t := regimeClusterTally{
		eligibleRed:    cb.EligibleRedCount(),
		provisionalRed: cb.ProvisionalRedCount(),
	}
	for _, band := range cb.Confirmed {
		switch band {
		case "green":
			t.green++
			t.ranked++
		case "yellow":
			t.yellow++
			t.ranked++
		case "red":
			t.red++
			t.ranked++
		default:
			t.unranked++
		}
	}
	return t
}

// applyRegimeSeverityGovernor applies, in order, the provenance gate and the
// evidence-keyed quality cap. Pure-tape panic (SPY ≤ −4% / −7%) is exempt
// from both — the tape is its own co-signature and its own evidence quality.
func applyRegimeSeverityGovernor(state *LifecycleState, r *RegimeSnapshotResult, tally regimeClusterTally) {
	if state.Stage != LifecycleConfirmedStress && state.Stage != LifecyclePanic {
		return
	}
	tapePanic := state.Stage == LifecyclePanic && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0)
	pending := regimePendingBacktestClusters(r, state.ConfirmedBy)
	if len(pending) > 0 && !tapePanic && !regimeTapeCosign(r) {
		from := state.Severity
		to := from
		switch state.Stage {
		case LifecycleConfirmedStress:
			to = "watch"
		case LifecyclePanic:
			to = "act"
		}
		if to != from {
			state.Severity = to
			state.Governors = append(state.Governors, GovernorAction{
				Action:   "severity_capped",
				From:     from,
				To:       to,
				Reason:   "pending_backtest_no_tape_cosign",
				Clusters: pending,
			})
		}
	}
	impaired := regimeImpairedConfirmingClusters(r, state.ConfirmedBy)
	if len(impaired) > 0 && !tapePanic && severityRank(state.Severity) > severityRank("watch") {
		from := state.Severity
		state.Severity = "watch"
		state.Governors = append(state.Governors, GovernorAction{
			Action:   "severity_capped",
			From:     from,
			To:       "watch",
			Reason:   "confirming_cluster_quality",
			Clusters: impaired,
		})
	}
}

func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "urgent":
		return 3
	case "act":
		return 2
	case "watch":
		return 1
	default:
		return 0
	}
}

// regimeTapeCosign reports whether the tape itself corroborates stress in
// this snapshot: SPY down 1.5%+, VIX up 10%+, or a fresh same-session
// VIX-term inversion. While threshold sets are pending_backtest, "act"
// requires one of these as the second witness.
func regimeTapeCosign(r *RegimeSnapshotResult) bool {
	if pctAtMost(r.HYGSPYDivergence.SPYChangePct, -1.5) || pctAtLeast(r.VIXTermStructure.VIXChangePct, 10.0) {
		return true
	}
	return r.VIXTermStructure.Status == RegimeStatusOK &&
		r.VIXTermStructure.Ratio != nil && *r.VIXTermStructure.Ratio >= 1.0
}

// regimePendingBacktestClusters returns the confirming clusters whose red
// rows classify on threshold sets still flagged pending_backtest. The flag
// finally becomes load-bearing here: per-set promotion (version-label bump
// via the backtest plan) relaxes the gate without policy edits.
func regimePendingBacktestClusters(r *RegimeSnapshotResult, confirmedBy []string) []string {
	var out []string
	for _, name := range confirmedBy {
		for _, meta := range regimeClusterRowMetas(r, name) {
			if meta.Band == "red" && meta.Thresholds != nil && meta.Thresholds.PendingBacktest {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// regimeImpairedConfirmingClusters returns confirming clusters whose source
// health is stale/partial/degraded — the evidence-keyed readiness cap.
// Deliberately scoped to the CONFIRMING clusters: one dead unrelated feed
// must not mute a fresh multi-cluster confirmation.
func regimeImpairedConfirmingClusters(r *RegimeSnapshotResult, confirmedBy []string) []string {
	var out []string
	for _, name := range confirmedBy {
		for _, src := range r.SourceHealth {
			if !strings.EqualFold(src.Source, name) {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(src.Status)) {
			case SourceStatusStale, SourceStatusDegraded, SourceStatusPartial:
				out = append(out, name)
			}
		}
	}
	return out
}

// regimeClusterRowMetas maps a cluster wire name to its row metadata.
func regimeClusterRowMetas(r *RegimeSnapshotResult, cluster string) []RegimeIndicatorMeta {
	switch cluster {
	case "vol":
		return []RegimeIndicatorMeta{r.VIXTermStructure.RegimeIndicatorMeta, r.VolOfVol.RegimeIndicatorMeta}
	case "credit":
		return []RegimeIndicatorMeta{r.HYGSPYDivergence.RegimeIndicatorMeta, r.CreditSpreads.RegimeIndicatorMeta}
	case "funding":
		return []RegimeIndicatorMeta{r.FundingStress.RegimeIndicatorMeta}
	case "fx":
		return []RegimeIndicatorMeta{r.USDJPY.RegimeIndicatorMeta}
	case "gamma":
		return []RegimeIndicatorMeta{r.GammaZero.RegimeIndicatorMeta}
	case "breadth":
		return []RegimeIndicatorMeta{r.Breadth.RegimeIndicatorMeta}
	default:
		return nil
	}
}

func regimeEligibleRedNames(cb RegimeClusterBands) []string {
	var out []string
	for i, band := range cb.Confirmed {
		if band == "red" && i < len(cb.Eligible) && cb.Eligible[i] && i < len(RegimeClusterNames) {
			out = append(out, RegimeClusterNames[i])
		}
	}
	return out
}

func regimeProvisionalRedNames(cb RegimeClusterBands) []string {
	var out []string
	for i, band := range cb.Raw {
		if band != "red" || i >= len(RegimeClusterNames) {
			continue
		}
		if i < len(cb.Confirmed) && cb.Confirmed[i] == "red" && i < len(cb.Eligible) && cb.Eligible[i] {
			continue
		}
		out = append(out, RegimeClusterNames[i])
	}
	return out
}

func regimeClusterNamesWithBand(bands []string, want string) []string {
	var out []string
	for i, band := range bands {
		if band == want && i < len(RegimeClusterNames) {
			out = append(out, RegimeClusterNames[i])
		}
	}
	return out
}

func BuildRegimePosture(r *RegimeSnapshotResult) RegimePosture {
	if r == nil {
		return RegimePosture{
			Label:      "No usable signal yet",
			Tone:       RegimeToneDataQuality,
			Stage:      LifecycleDataQuality,
			Severity:   "watch",
			Readiness:  "blocked",
			Confidence: "low",
		}
	}
	label := RegimeHeadline(r.Composite, r.Lifecycle.Stage)
	confidence := strings.TrimSpace(r.Lifecycle.Confidence)
	if confidence == "" {
		confidence = strings.TrimSpace(r.Summary.Confidence)
	}
	tone := regimePostureTone(r.Composite, r.Lifecycle)
	return RegimePosture{
		Label:      label,
		Tone:       tone,
		Stage:      strings.TrimSpace(r.Lifecycle.Stage),
		Severity:   regimePostureSeverity(r.Lifecycle, tone),
		Readiness:  strings.TrimSpace(r.Lifecycle.Readiness),
		Confidence: confidence,
		Evidence:   strings.TrimSpace(r.Summary.Evidence),
	}
}

// regimePostureTone maps the unified headline + lifecycle state to the display
// tone. Stage keeps the condition label honest, while severity owns urgency:
// governed confirmed stress with severity watch remains an amber watch so red
// stays available for act-grade stress and true risk-off conditions.
func regimePostureTone(c RegimeComposite, lifecycle LifecycleState) string {
	label := RegimeHeadline(c, lifecycle.Stage)
	if label == "Full risk-off conditions" {
		return RegimeToneRiskOff
	}
	if c.ClusterRankedCount < RegimeVerdictFloor {
		return RegimeToneDataQuality
	}
	switch lifecycle.Stage {
	case LifecycleDataQuality:
		return RegimeToneDataQuality
	case LifecycleConfirmedStress, LifecycleForcedDefense:
		if severityRank(lifecycle.Severity) <= severityRank("watch") {
			return RegimeToneWatch
		}
		return RegimeToneStress
	case LifecyclePanic:
		if severityRank(lifecycle.Severity) <= severityRank("watch") {
			return RegimeToneWatch
		}
		return RegimeToneStress
	}
	if regimeLifecycleReadinessDegraded(lifecycle.Readiness) {
		return RegimeToneDataQuality
	}
	switch lifecycle.Stage {
	case LifecycleEarlyWarning, LifecycleStabilization:
		return RegimeToneWatch
	}
	if c.ClusterYellowCount > 0 {
		return RegimeToneWatch
	}
	if lifecycle.Stage == LifecycleOpportunity {
		return RegimeToneNormal
	}
	switch label {
	case "Broad stress regime", "Confirmed stress regime":
		return RegimeToneStress
	case "Stress signal present", "Elevated stress watch":
		return RegimeToneWatch
	case "No usable signal yet", "Insufficient signal — too few inputs ready":
		return RegimeToneDataQuality
	default:
		return RegimeToneNormal
	}
}

func regimePostureSeverity(lifecycle LifecycleState, tone string) string {
	severity := strings.TrimSpace(lifecycle.Severity)
	if severity == "" {
		severity = "observe"
	}
	if severity == "observe" {
		switch tone {
		case RegimeToneWatch, RegimeToneDataQuality:
			return "watch"
		}
	}
	return severity
}

func regimeLifecycleReadinessDegraded(readiness string) bool {
	switch strings.ToLower(strings.TrimSpace(readiness)) {
	case "blocked", "degraded", "failed", "partial", "warming":
		return true
	default:
		return false
	}
}

func BuildRegimeSourceHealth(r *RegimeSnapshotResult, now time.Time) []SourceHealth {
	if r == nil {
		return nil
	}
	if now.IsZero() {
		now = r.AsOf
	}
	bands := BuildRegimeClusterBands(r).Confirmed
	volPartial := regimeSourceMissingRequiredFields(r.VIXTermStructure.Status, r.VIXTermStructure.Band, r.VIXTermStructure.FieldsMissing)
	creditPartial := regimeSourceMissingRequiredFields(r.HYGSPYDivergence.Status, r.HYGSPYDivergence.Band, r.HYGSPYDivergence.FieldsMissing) ||
		regimeSourceMissingRequiredFields(r.CreditSpreads.Status, r.CreditSpreads.Band, r.CreditSpreads.FieldsMissing)
	rows := []struct {
		name          string
		band          string
		statuses      []string
		asOf          []RegimeAsOfSummary
		qualityStatus string
		partial       bool
	}{
		{"vol", bands[0], []string{r.VIXTermStructure.Status, r.VolOfVol.Status}, []RegimeAsOfSummary{metaAsOf(r.VIXTermStructure.RegimeIndicatorMeta), metaAsOf(r.VolOfVol.RegimeIndicatorMeta)}, "", volPartial},
		{"credit", bands[1], []string{r.HYGSPYDivergence.Status, r.CreditSpreads.Status}, []RegimeAsOfSummary{metaAsOf(r.HYGSPYDivergence.RegimeIndicatorMeta), metaAsOf(r.CreditSpreads.RegimeIndicatorMeta)}, "", creditPartial},
		{"funding", bands[2], []string{r.FundingStress.Status}, []RegimeAsOfSummary{metaAsOf(r.FundingStress.RegimeIndicatorMeta)}, "", regimeSourceMissingRequiredFields(r.FundingStress.Status, r.FundingStress.Band, r.FundingStress.FieldsMissing)},
		{"fx", bands[3], []string{r.USDJPY.Status}, []RegimeAsOfSummary{metaAsOf(r.USDJPY.RegimeIndicatorMeta)}, "", regimeSourceMissingRequiredFields(r.USDJPY.Status, r.USDJPY.Band, r.USDJPY.FieldsMissing)},
		{"gamma", bands[4], []string{r.GammaZero.Status}, []RegimeAsOfSummary{metaAsOf(r.GammaZero.RegimeIndicatorMeta)}, regimeSourceQualityStatus(r.DataQuality, "gamma"), regimeSourceMissingRequiredFields(r.GammaZero.Status, r.GammaZero.Band, r.GammaZero.FieldsMissing)},
		{"breadth", bands[5], []string{r.Breadth.Status}, []RegimeAsOfSummary{metaAsOf(r.Breadth.RegimeIndicatorMeta)}, regimeSourceQualityStatus(r.DataQuality, "breadth"), regimeSourceMissingRequiredFields(r.Breadth.Status, r.Breadth.Band, r.Breadth.FieldsMissing)},
	}
	out := make([]SourceHealth, 0, len(rows))
	for _, row := range rows {
		asOf := weakestRegimeAsOf(row.asOf)
		status := regimeSourceStatus(row.statuses, row.band, row.qualityStatus, row.partial)
		out = append(out, SourceHealth{
			Source:               row.name,
			Status:               status,
			AsOf:                 asOf,
			AgeSeconds:           sourceAgeSeconds(now, asOf),
			MaxAgeSeconds:        RegimeSourceMaxAgeSeconds(row.name),
			Confidence:           regimeSourceConfidence(status),
			FingerprintStability: FingerprintStabilitySemanticBuckets,
		})
	}
	return out
}

// RegimeSourceMaxAgeSeconds is the served per-cluster staleness policy for
// the cluster's weakest-leg as_of: older than this is unambiguously overdue.
// Wall-clock documentation values sized to each cluster's slowest native
// cadence (VVIX daily close over a weekend, FRED publication lag, a Friday
// gamma compute read on Monday pre-open); the binding eligibility gate uses
// trading-date logic daemon-side. Served so renderers derive their stale
// badges from the wire instead of hardcoding twins.
func RegimeSourceMaxAgeSeconds(source string) int64 {
	const day = int64(24 * 60 * 60)
	switch source {
	case "vol":
		return 4 * day
	case "credit":
		return 7 * day
	case "funding":
		return 7 * day
	case "fx":
		return 4 * day
	case "gamma":
		return 3 * day
	case "breadth":
		return 4 * day
	default:
		return 0
	}
}

func BuildLifecycleFingerprint(state LifecycleState) Fingerprint {
	return lifecycleFingerprint(state)
}

func lifecycleFingerprint(state LifecycleState) Fingerprint {
	// Governors enter the projection as action:reason tokens only — never
	// ages, depths, or other continuous values, which would churn the
	// fingerprint (and downstream alert dedupe) without a semantic change.
	governors := make([]string, 0, len(state.Governors))
	for _, g := range state.Governors {
		governors = append(governors, strings.ToLower(strings.TrimSpace(g.Action))+":"+strings.ToLower(strings.TrimSpace(g.Reason)))
	}
	projection := struct {
		Scope       string              `json:"scope,omitempty"`
		Stage       string              `json:"stage,omitempty"`
		Severity    string              `json:"severity,omitempty"`
		Readiness   string              `json:"readiness,omitempty"`
		Timing      string              `json:"timing,omitempty"`
		Confidence  string              `json:"confidence,omitempty"`
		Evidence    []LifecycleEvidence `json:"evidence,omitempty"`
		ConfirmedBy []string            `json:"confirmed_by,omitempty"`
		Unconfirmed []string            `json:"unconfirmed,omitempty"`
		Suppressed  []string            `json:"suppressed,omitempty"`
		RejectedBy  []string            `json:"rejected_by,omitempty"`
		Governors   []string            `json:"governors,omitempty"`
	}{
		Scope:       strings.ToLower(strings.TrimSpace(state.Scope)),
		Stage:       strings.ToLower(strings.TrimSpace(state.Stage)),
		Severity:    strings.ToLower(strings.TrimSpace(state.Severity)),
		Readiness:   strings.ToLower(strings.TrimSpace(state.Readiness)),
		Timing:      strings.ToLower(strings.TrimSpace(state.Timing)),
		Confidence:  strings.ToLower(strings.TrimSpace(state.Confidence)),
		Evidence:    state.Evidence,
		ConfirmedBy: cleanSorted(state.ConfirmedBy),
		Unconfirmed: cleanSorted(state.Unconfirmed),
		Suppressed:  cleanSorted(state.Suppressed),
		RejectedBy:  cleanSorted(state.RejectedBy),
		Governors:   cleanSorted(governors),
	}
	// lifecycle-fp-v2: eligibility-aware evidence (Confirmed now means
	// eligible-confirmed) + governors. The version bump re-fires active
	// alerts once on upgrade — accepted and changelog-noted.
	return semanticFingerprint("lifecycle-fp-v2", projection)
}

// regimeLifecyclePanic: three deep/fresh/persistent independent reds, or
// tape-grade crashes. Eligible reds only — provisional evidence never
// reaches the panic tally.
func regimeLifecyclePanic(r RegimeSnapshotResult, t regimeClusterTally) bool {
	return t.eligibleRed >= 3 ||
		(pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0) && t.eligibleRed >= 1) ||
		pctAtMost(r.HYGSPYDivergence.SPYChangePct, -7.0)
}

func regimeLifecycleConfirmedStress(r RegimeSnapshotResult, t regimeClusterTally) bool {
	return t.eligibleRed >= 2 ||
		(t.eligibleRed >= 1 && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -2.5)) ||
		(pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0) && t.yellow >= 2) ||
		(t.eligibleRed >= 1 && pctAtLeast(r.VIXTermStructure.VIXChangePct, 20.0))
}

// regimeLifecycleEarlyWarning is the home of provisional reds: any raw red
// (eligible or not) warns immediately, even though only eligible reds may
// confirm.
func regimeLifecycleEarlyWarning(r RegimeSnapshotResult, t regimeClusterTally, unconfirmed []string) bool {
	return t.eligibleRed+t.provisionalRed >= 1 ||
		t.red >= 1 ||
		t.yellow >= 3 ||
		len(unconfirmed) > 0 ||
		pctAtMost(r.HYGSPYDivergence.SPYChangePct, -1.5) ||
		pctAtLeast(r.VIXTermStructure.VIXChangePct, 10.0)
}

func regimeLifecycleOpportunity(r RegimeSnapshotResult, t regimeClusterTally) bool {
	return t.red == 0 &&
		t.yellow <= 1 &&
		t.ranked >= 3 &&
		pctAtLeast(r.HYGSPYDivergence.SPYChangePct, 1.5) &&
		pctAtMost(r.VIXTermStructure.VIXChangePct, -10.0)
}

func regimeLifecycleStabilization(r RegimeSnapshotResult, t regimeClusterTally) bool {
	return t.red == 0 &&
		t.yellow > 0 &&
		t.yellow <= 2 &&
		(pctAtLeast(r.HYGSPYDivergence.SPYChangePct, 1.0) || pctAtMost(r.VIXTermStructure.VIXChangePct, -10.0))
}

func regimeLifecycleConfidence(t regimeClusterTally) string {
	switch {
	case t.ranked < RegimeVerdictFloor:
		return "low"
	case t.unranked > 0:
		return "medium"
	default:
		return "high"
	}
}

// regimeLifecycleHasDegradedInputs reports whether the served data-quality
// disclosures flag a readiness-relevant problem. Partial/degraded clusters
// (computing, unavailable, error, missing required fields, or gamma's own
// compute-quality degradation) always count, on any cluster — those are
// active data problems, not routine cadence. Stale clusters are exempted
// only when named cluster is UNRANKED (cb scopes this the same way
// regimeLifecycleHasWeakSourceRows does): gamma's documented off-hours
// prior-NY-trading-date stale cache (gammaNotes in internal/daemon/regime.go;
// docs/design/regime-calibration.md Part 3) is real, disclosed evidence (it
// still rides on r.DataQuality for the --explain/status reader) but must not
// by itself flip lifecycle readiness once the cluster is also context-only.
// An item naming no clusters at all is never a stale-only item (see
// regimeStatusQuality/gammaStatusQuality) and always degrades.
func regimeLifecycleHasDegradedInputs(r RegimeSnapshotResult, cb RegimeClusterBands) bool {
	for _, item := range r.DataQuality {
		if len(item.PartialClusters) > 0 || len(item.DegradedClusters) > 0 {
			return true
		}
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "degraded", "partial":
			return true
		case RegimeStatusStale:
			if len(item.StaleClusters) == 0 {
				return true
			}
			for _, name := range item.StaleClusters {
				if regimeClusterNameIsRanked(cb, name) {
					return true
				}
			}
		}
	}
	return false
}

// regimeClusterNameIsRanked reports whether the named cluster (lowercase
// wire name, e.g. "gamma", or the "FX" title-case used by
// staleRegimeClusters/partialRegimeClusters) currently carries a ranked
// (green/yellow/red) confirmed band rather than being unranked/context-only.
func regimeClusterNameIsRanked(cb RegimeClusterBands, name string) bool {
	for i, clusterName := range RegimeClusterNames {
		if strings.EqualFold(clusterName, name) && i < len(cb.Confirmed) {
			return cb.Confirmed[i] != ""
		}
	}
	// Unknown name (e.g. a non-cluster-scoped surface) — treat as ranked so
	// it still degrades readiness rather than being silently dropped.
	return true
}

// regimeLifecycleHasWeakSourceRows reports whether any source row carries a
// non-ok status that should degrade readiness. computing/unavailable/error
// always count, on any cluster — those are active data problems. stale is
// exempted only when the row's cluster is UNRANKED (cb.Confirmed == ""):
// gamma going status=stale off-hours from an expected prior-NY-trading-date
// cache is the documented case (docs/design/regime-calibration.md Part 3;
// internal/daemon/regime.go's gammaNotes: "a prior-date cache serves
// status=stale, stays visible, and warns only") where the cluster is also
// marked context_only/unranked (rankableLifecycleGammaBand), so it never
// entered the ranked tally in the first place. A RANKED cluster's source
// (vol/credit/funding/fx/breadth, or gamma once it is rankable) going stale
// is still a real data-quality problem and keeps degrading, same as before.
func regimeLifecycleHasWeakSourceRows(r RegimeSnapshotResult, cb RegimeClusterBands) bool {
	rows := []struct {
		cluster  int
		statuses []string
	}{
		{RegimeClusterEquityVol, []string{r.VIXTermStructure.Status, r.VolOfVol.Status}},
		{RegimeClusterCredit, []string{r.HYGSPYDivergence.Status, r.CreditSpreads.Status}},
		{RegimeClusterFunding, []string{r.FundingStress.Status}},
		{RegimeClusterFX, []string{r.USDJPY.Status}},
		{RegimeClusterGamma, []string{r.GammaZero.Status}},
		{RegimeClusterBreadth, []string{r.Breadth.Status}},
	}
	for _, row := range rows {
		ranked := row.cluster < len(cb.Confirmed) && cb.Confirmed[row.cluster] != ""
		for _, status := range row.statuses {
			switch strings.ToLower(strings.TrimSpace(status)) {
			case "", RegimeStatusOK:
			case RegimeStatusStale:
				if ranked {
					return true
				}
				// Unranked cluster's cache went stale (expected off-hours
				// behavior) — context-only evidence, not a readiness input.
			default:
				return true
			}
		}
	}
	return false
}

func capLifecycleConfidence(confidence string) string {
	switch strings.ToLower(strings.TrimSpace(confidence)) {
	case "", "high":
		return "medium"
	default:
		return confidence
	}
}

func rankableLifecycleGammaBand(g RegimeGammaZero) string {
	if g.Envelope.Result == nil || g.Envelope.Result.Quality == nil ||
		g.Envelope.Result.Quality.Rankability != GammaRankabilityRankable {
		return ""
	}
	return g.Band
}

func regimeLifecycleEvidence(cb RegimeClusterBands, r RegimeSnapshotResult) []LifecycleEvidence {
	out := make([]LifecycleEvidence, 0, len(RegimeClusterNames)+2)
	for i, name := range RegimeClusterNames {
		if i >= len(cb.Raw) || cb.Raw[i] == "" {
			continue
		}
		band := cb.Raw[i]
		// Timing honesty: only an ELIGIBLE confirmed red is contemporaneous
		// act-grade evidence. Provisional reds (marginal depth, short
		// streak, or overdue data) are forward warnings, never confirmation.
		confirmedSignal := band == "red" &&
			i < len(cb.Confirmed) && cb.Confirmed[i] == "red" &&
			i < len(cb.Eligible) && cb.Eligible[i]
		timing := LifecycleTimingForwardWarning
		severity := "watch"
		if confirmedSignal {
			timing = LifecycleTimingContemporary
			severity = "act"
		}
		out = append(out, LifecycleEvidence{
			Source:    name,
			Signal:    "cluster",
			Bucket:    band,
			Timing:    timing,
			Severity:  severity,
			Confirmed: confirmedSignal,
		})
	}
	if r.HYGSPYDivergence.SPYChangePct != nil {
		out = append(out, tapeLifecycleEvidence("spy", *r.HYGSPYDivergence.SPYChangePct, -1.5, -2.5))
	}
	if r.VIXTermStructure.VIXChangePct != nil {
		out = append(out, tapeLifecycleEvidence("vix", *r.VIXTermStructure.VIXChangePct, 10, 20))
	}
	return out
}

func tapeLifecycleEvidence(source string, observed, watch, act float64) LifecycleEvidence {
	ev := LifecycleEvidence{Source: source, Signal: "tape", Timing: "current"}
	switch source {
	case "spy":
		switch {
		case observed <= act:
			ev.Bucket = "red"
			ev.Timing = LifecycleTimingContemporary
			ev.Severity = "act"
			ev.Confirmed = true
		case observed <= watch:
			ev.Bucket = "yellow"
			ev.Timing = LifecycleTimingForwardWarning
			ev.Severity = "watch"
		default:
			ev.Bucket = "green"
			ev.Severity = "observe"
		}
	default:
		switch {
		case observed >= act:
			ev.Bucket = "red"
			ev.Timing = LifecycleTimingContemporary
			ev.Severity = "act"
			ev.Confirmed = true
		case observed >= watch:
			ev.Bucket = "yellow"
			ev.Timing = LifecycleTimingForwardWarning
			ev.Severity = "watch"
		default:
			ev.Bucket = "green"
			ev.Severity = "observe"
		}
	}
	return ev
}

func strongestLifecycleBand(bands ...string) string {
	seenYellow, seenGreen := false, false
	for _, band := range bands {
		switch strings.ToLower(strings.TrimSpace(band)) {
		case "red":
			return "red"
		case "yellow":
			seenYellow = true
		case "green":
			seenGreen = true
		}
	}
	if seenYellow {
		return "yellow"
	}
	if seenGreen {
		return "green"
	}
	return ""
}

func isolatedLifecycleEquityVolConfirmed(r RegimeSnapshotResult) bool {
	if r.VIXTermStructure.Band == "red" {
		return true
	}
	if r.VolOfVol.Last != nil && *r.VolOfVol.Last >= 120 {
		return true
	}
	if r.VIXTermStructure.VIXChangePct != nil && *r.VIXTermStructure.VIXChangePct >= 20 {
		return true
	}
	return r.HYGSPYDivergence.SPYChangePct != nil && *r.HYGSPYDivergence.SPYChangePct <= -1
}

func pctAtMost(v *float64, threshold float64) bool {
	return v != nil && *v <= threshold
}

func pctAtLeast(v *float64, threshold float64) bool {
	return v != nil && *v >= threshold
}

func metaAsOf(meta RegimeIndicatorMeta) RegimeAsOfSummary {
	if meta.AsOf == nil {
		return RegimeAsOfSummary{}
	}
	return *meta.AsOf
}

func latestRegimeAsOf(values []RegimeAsOfSummary) time.Time {
	var latest time.Time
	for _, v := range values {
		if !v.Time.IsZero() && v.Time.After(latest) {
			latest = v.Time
		}
	}
	return latest
}

func weakestRegimeAsOf(values []RegimeAsOfSummary) time.Time {
	var oldest time.Time
	for _, v := range values {
		if v.Time.IsZero() {
			continue
		}
		if oldest.IsZero() || v.Time.Before(oldest) {
			oldest = v.Time
		}
	}
	if oldest.IsZero() {
		return latestRegimeAsOf(values)
	}
	return oldest
}

func sourceAgeSeconds(now, asOf time.Time) int64 {
	if now.IsZero() || asOf.IsZero() {
		return 0
	}
	age := now.Sub(asOf)
	if age < 0 {
		return 0
	}
	return int64(age.Seconds())
}

func regimeSourceStatus(statuses []string, band string, qualityStatus string, partial bool) string {
	switch qualityStatus {
	case "degraded":
		return "degraded"
	case "partial":
		return "partial"
	case RegimeStatusStale:
		return RegimeStatusStale
	}
	if partial {
		return "partial"
	}
	sawBad, sawStale, sawComputing, sawError, sawUnavailable := false, false, false, false, false
	for _, status := range statuses {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case RegimeStatusStale:
			sawStale = true
		case RegimeStatusComputing:
			sawBad = true
			sawComputing = true
		case RegimeStatusError:
			sawBad = true
			sawError = true
		case RegimeStatusUnavailable:
			sawBad = true
			sawUnavailable = true
		case "", RegimeStatusOK:
		default:
			sawBad = true
		}
	}
	if band != "" && sawBad {
		return "partial"
	}
	if sawError {
		return RegimeStatusError
	}
	if sawUnavailable {
		return RegimeStatusUnavailable
	}
	if sawComputing {
		return RegimeStatusComputing
	}
	if sawStale {
		return RegimeStatusStale
	}
	return RegimeStatusOK
}

func regimeSourceMissingRequiredFields(status string, band string, fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case RegimeStatusOK, RegimeStatusStale:
	default:
		return false
	}
	switch strings.ToLower(strings.TrimSpace(band)) {
	case "", "unranked":
		return true
	default:
		return false
	}
}

func regimeSourceConfidence(status string) string {
	switch status {
	case RegimeStatusOK:
		return "high"
	case RegimeStatusStale, "partial", "degraded":
		return "medium"
	default:
		return "low"
	}
}

func regimeSourceQualityStatus(items []DataQualityHealth, source string) string {
	out := ""
	for _, item := range items {
		status := strings.ToLower(strings.TrimSpace(item.Status))
		if sourceInDataQualityClusters(item.DegradedClusters, source) || sourceInDataQualityClusters(item.PartialClusters, source) || sourceInDataQualityClusters(item.StaleClusters, source) {
			switch status {
			case "degraded":
				return "degraded"
			case "partial":
				if out != "degraded" {
					out = "partial"
				}
			case RegimeStatusStale:
				if out == "" {
					out = RegimeStatusStale
				}
			}
		}
	}
	return out
}

func sourceInDataQualityClusters(clusters []string, source string) bool {
	for _, cluster := range clusters {
		if strings.EqualFold(cluster, source) {
			return true
		}
	}
	return false
}
