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
	Stage        string              `json:"stage,omitempty"`
	Scope        string              `json:"scope,omitempty"`
	Severity     string              `json:"severity,omitempty"`
	Readiness    string              `json:"readiness,omitempty"`
	Timing       string              `json:"timing,omitempty"`
	Confidence   string              `json:"confidence,omitempty"`
	Evidence     []LifecycleEvidence `json:"evidence,omitempty"`
	ConfirmedBy  []string            `json:"confirmed_by,omitempty"`
	Unconfirmed  []string            `json:"unconfirmed,omitempty"`
	Suppressed   []string            `json:"suppressed,omitempty"`
	RejectedBy   []string            `json:"rejected_by,omitempty"`
	Fingerprint  Fingerprint         `json:"fingerprint,omitzero"`
	NotExecution string              `json:"not_execution,omitempty"`
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
func BuildRegimeLifecycle(r *RegimeSnapshotResult) LifecycleState {
	if r == nil {
		return LifecycleState{Stage: LifecycleDataQuality, Scope: "market", Severity: "watch", Readiness: "blocked", Timing: LifecycleTimingDataQuality, Confidence: "low"}
	}
	c := r.Composite
	raw, confirmed := regimeLifecycleClusterBands(*r)
	confirmedNames := regimeLifecycleClusterNames(confirmed, "red")
	yellowNames := regimeLifecycleClusterNames(confirmed, "yellow")
	unconfirmed := regimeUnconfirmedRedClusters(raw, confirmed)
	evidence := regimeLifecycleEvidence(raw, confirmed, *r)
	confidence := r.Summary.Confidence
	if strings.TrimSpace(confidence) == "" {
		confidence = regimeLifecycleConfidence(c)
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
	case c.ClusterRankedCount < 3:
		state.Stage = LifecycleDataQuality
		state.Severity = "watch"
		state.Readiness = "blocked"
		state.Timing = LifecycleTimingDataQuality
	case regimeLifecyclePanic(*r, c):
		state.Stage = LifecyclePanic
		state.Severity = "urgent"
		state.Timing = LifecycleTimingContemporary
	case regimeLifecycleConfirmedStress(*r, c):
		state.Stage = LifecycleConfirmedStress
		state.Severity = "act"
		state.Timing = LifecycleTimingContemporary
	case regimeLifecycleEarlyWarning(*r, c, unconfirmed):
		state.Stage = LifecycleEarlyWarning
		state.Severity = "watch"
		state.Timing = LifecycleTimingForwardWarning
	case regimeLifecycleOpportunity(*r, c):
		state.Stage = LifecycleOpportunity
		state.Severity = "watch"
		state.Timing = LifecycleTimingRecovery
	case regimeLifecycleStabilization(*r, c):
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
	if state.Readiness == "ready" && (regimeLifecycleHasDegradedInputs(*r) || regimeLifecycleHasWeakSourceRows(*r)) {
		state.Readiness = "degraded"
		state.Confidence = capLifecycleConfidence(state.Confidence)
	}
	state.Fingerprint = lifecycleFingerprint(state)
	return state
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
	label := regimePostureLabel(r.Composite)
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

func regimePostureLabel(c RegimeComposite) string {
	switch {
	case c.ClusterRankedCount == 0:
		return "No usable signal yet"
	case c.ClusterRankedCount < 3:
		return "Insufficient signal — too few inputs ready"
	case c.ClusterUnrankedCount == 0 && c.ClusterRedCount == c.ClusterRankedCount:
		return "Full risk-off conditions"
	case c.ClusterRedCount >= 2:
		return "Broad stress regime"
	case c.ClusterRedCount >= 1:
		return "Stress signal present"
	case c.ClusterYellowCount >= 3:
		return "Elevated stress watch"
	default:
		return "Normal regime"
	}
}

func regimePostureTone(c RegimeComposite, lifecycle LifecycleState) string {
	label := strings.TrimSpace(c.Verdict)
	if label == "" {
		label = regimePostureLabel(c)
	}
	if label == "Full risk-off conditions" {
		return RegimeToneRiskOff
	}
	if c.ClusterRankedCount < 3 {
		return RegimeToneDataQuality
	}
	switch lifecycle.Stage {
	case LifecycleDataQuality:
		return RegimeToneDataQuality
	case LifecyclePanic:
		return RegimeToneStress
	case LifecycleConfirmedStress, LifecycleForcedDefense:
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
	case "Broad stress regime":
		return RegimeToneStress
	case "Stress signal present", "Elevated stress watch":
		return RegimeToneWatch
	case "No usable signal yet", "Insufficient signal — too few inputs ready", "No ranked indicators — see rows below for state":
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
	_, bands := regimeLifecycleClusterBands(*r)
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
			Confidence:           regimeSourceConfidence(status),
			FingerprintStability: FingerprintStabilitySemanticBuckets,
		})
	}
	return out
}

func BuildLifecycleFingerprint(state LifecycleState) Fingerprint {
	return lifecycleFingerprint(state)
}

func lifecycleFingerprint(state LifecycleState) Fingerprint {
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
	}
	return semanticFingerprint("lifecycle-fp-v1", projection)
}

func regimeLifecyclePanic(r RegimeSnapshotResult, c RegimeComposite) bool {
	return c.ClusterRedCount >= 3 ||
		(pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0) && c.ClusterRedCount >= 1) ||
		pctAtMost(r.HYGSPYDivergence.SPYChangePct, -7.0)
}

func regimeLifecycleConfirmedStress(r RegimeSnapshotResult, c RegimeComposite) bool {
	return c.ClusterRedCount >= 2 ||
		(c.ClusterRedCount >= 1 && pctAtMost(r.HYGSPYDivergence.SPYChangePct, -2.5)) ||
		(pctAtMost(r.HYGSPYDivergence.SPYChangePct, -4.0) && c.ClusterYellowCount >= 2) ||
		(c.ClusterRedCount >= 1 && pctAtLeast(r.VIXTermStructure.VIXChangePct, 20.0))
}

func regimeLifecycleEarlyWarning(r RegimeSnapshotResult, c RegimeComposite, unconfirmed []string) bool {
	return c.ClusterRedCount >= 1 ||
		c.ClusterYellowCount >= 3 ||
		len(unconfirmed) > 0 ||
		pctAtMost(r.HYGSPYDivergence.SPYChangePct, -1.5) ||
		pctAtLeast(r.VIXTermStructure.VIXChangePct, 10.0)
}

func regimeLifecycleOpportunity(r RegimeSnapshotResult, c RegimeComposite) bool {
	return c.ClusterRedCount == 0 &&
		c.ClusterYellowCount <= 1 &&
		c.ClusterRankedCount >= 3 &&
		pctAtLeast(r.HYGSPYDivergence.SPYChangePct, 1.5) &&
		pctAtMost(r.VIXTermStructure.VIXChangePct, -10.0)
}

func regimeLifecycleStabilization(r RegimeSnapshotResult, c RegimeComposite) bool {
	return c.ClusterRedCount == 0 &&
		c.ClusterYellowCount > 0 &&
		c.ClusterYellowCount <= 2 &&
		(pctAtLeast(r.HYGSPYDivergence.SPYChangePct, 1.0) || pctAtMost(r.VIXTermStructure.VIXChangePct, -10.0))
}

func regimeLifecycleConfidence(c RegimeComposite) string {
	switch {
	case c.ClusterRankedCount < 3:
		return "low"
	case c.ClusterUnrankedCount > 0:
		return "medium"
	default:
		return "high"
	}
}

func regimeLifecycleHasDegradedInputs(r RegimeSnapshotResult) bool {
	for _, item := range r.DataQuality {
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case RegimeStatusStale, "degraded", "partial":
			return true
		}
	}
	return false
}

func regimeLifecycleHasWeakSourceRows(r RegimeSnapshotResult) bool {
	statuses := []string{
		r.VIXTermStructure.Status,
		r.VolOfVol.Status,
		r.HYGSPYDivergence.Status,
		r.CreditSpreads.Status,
		r.FundingStress.Status,
		r.USDJPY.Status,
		r.GammaZero.Status,
		r.Breadth.Status,
	}
	for _, status := range statuses {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "", RegimeStatusOK:
		default:
			return true
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

func regimeLifecycleClusterBands(r RegimeSnapshotResult) ([]string, []string) {
	raw := []string{
		strongestLifecycleBand(r.VIXTermStructure.Band, r.VolOfVol.Band),
		strongestLifecycleBand(r.HYGSPYDivergence.Band, r.CreditSpreads.Band),
		strongestLifecycleBand(r.FundingStress.Band),
		strongestLifecycleBand(r.USDJPY.Band),
		strongestLifecycleBand(rankableLifecycleGammaBand(r.GammaZero)),
		strongestLifecycleBand(r.Breadth.Band),
	}
	out := append([]string(nil), raw...)
	if r.HYGSPYDivergence.Band == "red" && r.CreditSpreads.Band != "red" && !hasIndependentLifecycleRed(raw, 1) {
		out[1] = "yellow"
	}
	if r.USDJPY.Band == "red" && !hasIndependentLifecycleRed(raw, 3) {
		out[3] = "yellow"
	}
	if out[0] == "red" && !hasIndependentLifecycleRed(out, 0) && !isolatedLifecycleEquityVolConfirmed(r) {
		out[0] = "yellow"
	}
	return raw, out
}

func rankableLifecycleGammaBand(g RegimeGammaZero) string {
	if g.Envelope.Result == nil || g.Envelope.Result.Quality == nil ||
		g.Envelope.Result.Quality.Rankability != GammaRankabilityRankable {
		return ""
	}
	return g.Band
}

func regimeLifecycleEvidence(raw, confirmed []string, r RegimeSnapshotResult) []LifecycleEvidence {
	names := []string{"vol", "credit", "funding", "fx", "gamma", "breadth"}
	out := make([]LifecycleEvidence, 0, len(names)+2)
	for i, name := range names {
		if i >= len(raw) || raw[i] == "" {
			continue
		}
		band := raw[i]
		confirmedSignal := i < len(confirmed) && confirmed[i] == band && band == "red"
		timing := LifecycleTimingForwardWarning
		severity := "watch"
		if band == "red" && confirmedSignal {
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

func regimeLifecycleClusterNames(bands []string, want string) []string {
	names := []string{"vol", "credit", "funding", "fx", "gamma", "breadth"}
	var out []string
	for i, band := range bands {
		if band == want && i < len(names) {
			out = append(out, names[i])
		}
	}
	return out
}

func regimeUnconfirmedRedClusters(raw, confirmed []string) []string {
	names := []string{"vol", "credit", "funding", "fx", "gamma", "breadth"}
	var out []string
	for i, band := range raw {
		if band == "red" && i < len(confirmed) && confirmed[i] != "red" && i < len(names) {
			out = append(out, names[i])
		}
	}
	return out
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

func hasIndependentLifecycleRed(bands []string, self int) bool {
	for i, band := range bands {
		if i != self && band == "red" {
			return true
		}
	}
	return false
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
