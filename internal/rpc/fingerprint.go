package rpc

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/osauer/ibkr/internal/risk"
)

const (
	RegimeFingerprintVersion = "regime-fp-v1"
	CanaryFingerprintVersion = "canary-fp-v1"
)

// BuildRegimeFingerprint returns the semantic identity of a regime snapshot.
// It hashes classified state only: bands, statuses, composite counts, warning
// codes/scopes/severities, and high-level data quality. It deliberately ignores
// timestamps, raw measurements, and prose.
func BuildRegimeFingerprint(r *RegimeSnapshotResult) Fingerprint {
	if r == nil {
		return semanticFingerprint(RegimeFingerprintVersion, nil)
	}
	projection := regimeFingerprintProjection{
		Composite: regimeCompositeFingerprint{
			Verdict:              cleanString(r.Composite.Verdict),
			GreenCount:           r.Composite.GreenCount,
			YellowCount:          r.Composite.YellowCount,
			RedCount:             r.Composite.RedCount,
			RankedCount:          r.Composite.RankedCount,
			UnrankedCount:        r.Composite.UnrankedCount,
			ClusterGreenCount:    r.Composite.ClusterGreenCount,
			ClusterYellowCount:   r.Composite.ClusterYellowCount,
			ClusterRedCount:      r.Composite.ClusterRedCount,
			ClusterRankedCount:   r.Composite.ClusterRankedCount,
			ClusterUnrankedCount: r.Composite.ClusterUnrankedCount,
		},
		Indicators: []regimeIndicatorFingerprint{
			{Name: "vix_term_structure", Band: cleanString(r.VIXTermStructure.Band), Status: cleanString(r.VIXTermStructure.Status), FieldsMissing: cleanSorted(r.VIXTermStructure.FieldsMissing)},
			{Name: "vol_of_vol", Band: cleanString(r.VolOfVol.Band), Status: cleanString(r.VolOfVol.Status)},
			{Name: "hyg_spy_divergence", Band: cleanString(r.HYGSPYDivergence.Band), Status: cleanString(r.HYGSPYDivergence.Status), FieldsMissing: cleanSorted(r.HYGSPYDivergence.FieldsMissing)},
			{Name: "credit_spreads", Band: cleanString(r.CreditSpreads.Band), Status: cleanString(r.CreditSpreads.Status), FieldsMissing: cleanSorted(r.CreditSpreads.FieldsMissing)},
			{Name: "funding_stress", Band: cleanString(r.FundingStress.Band), Status: cleanString(r.FundingStress.Status), FieldsMissing: cleanSorted(r.FundingStress.FieldsMissing)},
			{Name: "usd_jpy", Band: cleanString(r.USDJPY.Band), Status: cleanString(r.USDJPY.Status), FieldsMissing: cleanSorted(r.USDJPY.FieldsMissing)},
			{Name: "gamma_zero", Band: cleanString(r.GammaZero.Band), Status: cleanString(r.GammaZero.Status), FieldsMissing: cleanSorted(r.GammaZero.FieldsMissing)},
			{Name: "breadth", Band: cleanString(r.Breadth.Band), Status: cleanString(r.Breadth.Status), FieldsMissing: cleanSorted(r.Breadth.FieldsMissing)},
		},
		Gamma:       buildRegimeGammaFingerprint(r.GammaZero),
		Breadth:     buildRegimeBreadthFingerprint(r.Breadth),
		Warnings:    regimeWarningFingerprints(r.WarningDetails),
		DataQuality: dataQualityFingerprints(r.DataQuality),
	}
	return semanticFingerprint(RegimeFingerprintVersion, projection)
}

// BuildCanaryFingerprint returns the semantic alert identity of a canary
// result. It hashes the classified alert state and source fingerprints, not
// timestamps, exact observed values, evidence strings, or render text.
func BuildCanaryFingerprint(r *CanaryResult) Fingerprint {
	if r == nil {
		return semanticFingerprint(CanaryFingerprintVersion, nil)
	}
	projection := canaryFingerprintProjection{
		Policy:           cleanString(r.Policy),
		Direction:        r.Direction,
		PortfolioPosture: r.PortfolioPosture,
		Severity:         r.Severity,
		PlannerModeHint:  r.PlannerModeHint,
		PlannerReadiness: r.PlannerReadiness,
		DataConfidence:   cleanString(r.DataConfidence),
		SignalConfidence: cleanString(r.SignalConfidence),
		PrimaryDrivers:   signalIDs(r.PrimaryDrivers),
		Signals:          canarySignalFingerprints(r.Signals),
		Rows:             canaryRowFingerprints(r.Rows),
		Market: canaryMarketFingerprint{
			RegimeVerdict:      cleanString(r.Market.RegimeVerdict),
			RedClusters:        r.Market.RedClusters,
			YellowClusters:     r.Market.YellowClusters,
			RankedClusters:     r.Market.RankedClusters,
			UnrankedClusters:   r.Market.UnrankedClusters,
			RedClusterNames:    cleanSorted(r.Market.RedClusterNames),
			YellowClusterNames: cleanSorted(r.Market.YellowClusterNames),
			AmbiguousClusters:  cleanSorted(r.Market.AmbiguousClusters),
			PartialClusters:    cleanSorted(r.Market.PartialClusters),
			ComputingClusters:  cleanSorted(r.Market.ComputingClusters),
			DegradedClusters:   cleanSorted(r.Market.DegradedClusters),
			StaleClusters:      cleanSorted(r.Market.StaleClusters),
		},
	}
	if r.SourceFingerprints.Regime != nil {
		projection.Source.Regime = *r.SourceFingerprints.Regime
	}
	return semanticFingerprint(CanaryFingerprintVersion, projection)
}

type regimeFingerprintProjection struct {
	Composite   regimeCompositeFingerprint   `json:"composite"`
	Indicators  []regimeIndicatorFingerprint `json:"indicators"`
	Gamma       regimeGammaFingerprint       `json:"gamma,omitzero"`
	Breadth     regimeBreadthFingerprint     `json:"breadth,omitzero"`
	Warnings    []regimeWarningFingerprint   `json:"warnings,omitempty"`
	DataQuality []dataQualityFingerprint     `json:"data_quality,omitempty"`
}

type regimeCompositeFingerprint struct {
	Verdict              string `json:"verdict,omitempty"`
	GreenCount           int    `json:"green_count"`
	YellowCount          int    `json:"yellow_count"`
	RedCount             int    `json:"red_count"`
	RankedCount          int    `json:"ranked_count"`
	UnrankedCount        int    `json:"unranked_count"`
	ClusterGreenCount    int    `json:"cluster_green_count"`
	ClusterYellowCount   int    `json:"cluster_yellow_count"`
	ClusterRedCount      int    `json:"cluster_red_count"`
	ClusterRankedCount   int    `json:"cluster_ranked_count"`
	ClusterUnrankedCount int    `json:"cluster_unranked_count"`
}

type regimeIndicatorFingerprint struct {
	Name          string   `json:"name"`
	Band          string   `json:"band,omitempty"`
	Status        string   `json:"status,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
}

type regimeGammaFingerprint struct {
	EnvelopeStatus   string `json:"envelope_status,omitempty"`
	ZeroGammaStatus  string `json:"zero_gamma_status,omitempty"`
	Regime           string `json:"regime,omitempty"`
	Confidence       string `json:"confidence,omitempty"`
	RegimeAgreement  string `json:"regime_agreement,omitempty"`
	HorizonAgreement string `json:"horizon_agreement,omitempty"`
}

type regimeBreadthFingerprint struct {
	State string `json:"state,omitempty"`
}

type regimeWarningFingerprint struct {
	Code     string `json:"code,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Severity string `json:"severity,omitempty"`
}

type dataQualityFingerprint struct {
	Surface          string   `json:"surface,omitempty"`
	Status           string   `json:"status,omitempty"`
	StaleClusters    []string `json:"stale_clusters,omitempty"`
	DegradedClusters []string `json:"degraded_clusters,omitempty"`
}

type canaryFingerprintProjection struct {
	Policy           string                    `json:"policy,omitempty"`
	Direction        risk.SignalDirection      `json:"direction,omitempty"`
	PortfolioPosture risk.PortfolioPosture     `json:"portfolio_posture,omitempty"`
	Severity         risk.SignalSeverity       `json:"severity,omitempty"`
	PlannerModeHint  risk.PlannerMode          `json:"planner_mode_hint,omitempty"`
	PlannerReadiness risk.PlannerReadiness     `json:"planner_readiness,omitempty"`
	DataConfidence   string                    `json:"data_confidence,omitempty"`
	SignalConfidence string                    `json:"signal_confidence,omitempty"`
	PrimaryDrivers   []string                  `json:"primary_drivers,omitempty"`
	Signals          []canarySignalFingerprint `json:"signals,omitempty"`
	Rows             []canaryRowFingerprint    `json:"rows,omitempty"`
	Market           canaryMarketFingerprint   `json:"market"`
	Source           canarySourceFingerprint   `json:"source,omitzero"`
}

type canarySourceFingerprint struct {
	Regime Fingerprint `json:"regime,omitzero"`
}

type canaryMarketFingerprint struct {
	RegimeVerdict      string   `json:"regime_verdict,omitempty"`
	RedClusters        int      `json:"red_clusters"`
	YellowClusters     int      `json:"yellow_clusters"`
	RankedClusters     int      `json:"ranked_clusters"`
	UnrankedClusters   int      `json:"unranked_clusters"`
	RedClusterNames    []string `json:"red_cluster_names,omitempty"`
	YellowClusterNames []string `json:"yellow_cluster_names,omitempty"`
	AmbiguousClusters  []string `json:"ambiguous_clusters,omitempty"`
	PartialClusters    []string `json:"partial_clusters,omitempty"`
	ComputingClusters  []string `json:"computing_clusters,omitempty"`
	DegradedClusters   []string `json:"degraded_clusters,omitempty"`
	StaleClusters      []string `json:"stale_clusters,omitempty"`
}

type canarySignalFingerprint struct {
	ID               string   `json:"id"`
	Direction        string   `json:"direction,omitempty"`
	Posture          string   `json:"posture,omitempty"`
	Severity         string   `json:"severity,omitempty"`
	Subject          string   `json:"subject,omitempty"`
	Metric           string   `json:"metric,omitempty"`
	Threshold        string   `json:"threshold,omitempty"`
	Target           string   `json:"target,omitempty"`
	Unit             string   `json:"unit,omitempty"`
	Confidence       string   `json:"confidence,omitempty"`
	BlockedBy        []string `json:"blocked_by,omitempty"`
	ConfidenceImpact string   `json:"confidence_impact,omitempty"`
}

type canaryRowFingerprint struct {
	Title     string `json:"title,omitempty"`
	Direction string `json:"direction,omitempty"`
	Severity  string `json:"severity,omitempty"`
}

func semanticFingerprint(version string, projection any) Fingerprint {
	b, _ := json.Marshal(projection)
	sum := sha256.Sum256(b)
	return Fingerprint{
		Version: version,
		Key:     "sha256:" + hex.EncodeToString(sum[:]),
	}
}

func buildRegimeGammaFingerprint(g RegimeGammaZero) regimeGammaFingerprint {
	fp := regimeGammaFingerprint{
		EnvelopeStatus:   cleanString(g.Envelope.Status),
		HorizonAgreement: cleanString(g.HorizonAgreement),
	}
	if g.Envelope.Result == nil {
		return fp
	}
	fp.RegimeAgreement = cleanString(g.Envelope.Result.RegimeAgreement)
	if g.Envelope.Result.Summary != nil {
		fp.ZeroGammaStatus = cleanString(g.Envelope.Result.Summary.ZeroGammaStatus)
		fp.Regime = cleanString(g.Envelope.Result.Summary.Regime)
		fp.Confidence = cleanString(g.Envelope.Result.Summary.Confidence)
	}
	return fp
}

func buildRegimeBreadthFingerprint(b RegimeBreadth) regimeBreadthFingerprint {
	return regimeBreadthFingerprint{State: cleanString(string(b.Envelope.State))}
}

func regimeWarningFingerprints(warnings []RegimeWarning) []regimeWarningFingerprint {
	out := make([]regimeWarningFingerprint, 0, len(warnings))
	for _, w := range warnings {
		fp := regimeWarningFingerprint{
			Code:     cleanString(w.Code),
			Scope:    cleanString(w.Scope),
			Severity: cleanString(w.Severity),
		}
		if fp.Code == "" && fp.Scope == "" && fp.Severity == "" {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b regimeWarningFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(a.Scope, b.Scope),
			cmp.Compare(a.Severity, b.Severity),
		)
	})
	return out
}

func dataQualityFingerprints(values []DataQualityHealth) []dataQualityFingerprint {
	out := make([]dataQualityFingerprint, 0, len(values))
	for _, q := range values {
		fp := dataQualityFingerprint{
			Surface:          cleanString(q.Surface),
			Status:           cleanString(q.Status),
			StaleClusters:    cleanSorted(q.StaleClusters),
			DegradedClusters: cleanSorted(q.DegradedClusters),
		}
		if fp.Surface == "" && fp.Status == "" && len(fp.StaleClusters) == 0 && len(fp.DegradedClusters) == 0 {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b dataQualityFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.Surface, b.Surface),
			cmp.Compare(a.Status, b.Status),
			cmp.Compare(strings.Join(a.StaleClusters, ","), strings.Join(b.StaleClusters, ",")),
			cmp.Compare(strings.Join(a.DegradedClusters, ","), strings.Join(b.DegradedClusters, ",")),
		)
	})
	return out
}

func canarySignalFingerprints(signals []risk.Signal) []canarySignalFingerprint {
	out := make([]canarySignalFingerprint, 0, len(signals))
	for _, s := range signals {
		fp := canarySignalFingerprint{
			ID:               cleanString(string(s.ID)),
			Direction:        cleanString(string(s.Direction)),
			Posture:          cleanString(string(s.Posture)),
			Severity:         cleanString(string(s.Severity)),
			Subject:          cleanString(s.Subject),
			Metric:           cleanString(s.Metric),
			Threshold:        fingerprintFloat(s.Threshold),
			Target:           fingerprintFloat(s.Target),
			Unit:             cleanString(s.Unit),
			Confidence:       cleanString(s.Confidence),
			BlockedBy:        cleanSorted(s.BlockedBy),
			ConfidenceImpact: cleanString(s.ConfidenceImpact),
		}
		if fp.ID == "" {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b canarySignalFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.ID, b.ID),
			cmp.Compare(a.Direction, b.Direction),
			cmp.Compare(a.Posture, b.Posture),
			cmp.Compare(a.Severity, b.Severity),
			cmp.Compare(a.Subject, b.Subject),
			cmp.Compare(a.Metric, b.Metric),
			cmp.Compare(a.Threshold, b.Threshold),
			cmp.Compare(a.Target, b.Target),
			cmp.Compare(a.Unit, b.Unit),
			cmp.Compare(a.Confidence, b.Confidence),
			cmp.Compare(strings.Join(a.BlockedBy, ","), strings.Join(b.BlockedBy, ",")),
			cmp.Compare(a.ConfidenceImpact, b.ConfidenceImpact),
		)
	})
	return out
}

func canaryRowFingerprints(rows []CanaryRow) []canaryRowFingerprint {
	out := make([]canaryRowFingerprint, 0, len(rows))
	for _, row := range rows {
		fp := canaryRowFingerprint{
			Title:     cleanString(row.Title),
			Direction: cleanString(string(row.Direction)),
			Severity:  cleanString(string(row.Severity)),
		}
		if fp.Title == "" && fp.Direction == "" && fp.Severity == "" {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b canaryRowFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.Title, b.Title),
			cmp.Compare(a.Direction, b.Direction),
			cmp.Compare(a.Severity, b.Severity),
		)
	})
	return out
}

func signalIDs(ids []risk.SignalID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if s := cleanString(string(id)); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func cleanSorted(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s := cleanString(v); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func cleanString(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

func fingerprintFloat(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}
