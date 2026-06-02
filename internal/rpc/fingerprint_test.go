package rpc

import (
	"slices"
	"testing"
	"time"
)

func TestRegimeFingerprintIgnoresTimestampsRawValuesAndProse(t *testing.T) {
	t.Parallel()
	vix := 18.0
	base := RegimeSnapshotResult{
		AsOf: time.Date(2026, 5, 31, 8, 30, 0, 0, time.UTC),
		Summary: RegimeSummary{
			Label:     "Normal regime",
			Evidence:  "some rendered prose",
			PunchLine: "do not hash this",
		},
		Composite: RegimeComposite{Verdict: "Normal regime", GreenCount: 1, RankedCount: 1, ClusterGreenCount: 1, ClusterRankedCount: 1},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "green"},
			Status:              RegimeStatusOK,
			VIX:                 &vix,
			Notes:               "long methodology prose",
		},
	}
	first := BuildRegimeFingerprint(&base)

	changed := base
	changed.AsOf = changed.AsOf.Add(time.Hour)
	changed.Summary.Evidence = "different rendered prose"
	changed.Summary.PunchLine = "different punch line"
	changed.VIXTermStructure.Notes = "different notes"
	changedVIX := 19.25
	changed.VIXTermStructure.VIX = &changedVIX
	second := BuildRegimeFingerprint(&changed)

	if first != second {
		t.Fatalf("fingerprint changed on timestamp/raw/prose-only mutation: %v != %v", first, second)
	}
}

func TestRegimeFingerprintTracksClassifiedStateAndCanonicalOrdering(t *testing.T) {
	t.Parallel()
	base := RegimeSnapshotResult{
		Composite: RegimeComposite{Verdict: "Stress signal present", RedCount: 1, RankedCount: 1, ClusterRedCount: 1, ClusterRankedCount: 1},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "red"},
			Status:              RegimeStatusOK,
			FieldsMissing:       []string{"ratio", "vix3m"},
		},
		WarningDetails: []RegimeWarning{
			{Code: "b", Scope: "breadth", Severity: "warning", Message: "ignored"},
			{Code: "a", Scope: "gamma", Severity: "info", Impact: "ignored"},
		},
		DataQuality: []DataQualityHealth{
			{Surface: "regime", Status: "partial", StaleClusters: []string{"vol", "credit"}, PartialClusters: []string{"breadth", "gamma"}},
			{Surface: "gamma", Status: "degraded", DegradedClusters: []string{"gamma"}},
		},
	}
	reordered := base
	reordered.VIXTermStructure.FieldsMissing = []string{"vix3m", "ratio"}
	reordered.WarningDetails = []RegimeWarning{base.WarningDetails[1], base.WarningDetails[0]}
	reordered.DataQuality = []DataQualityHealth{
		{Surface: "gamma", Status: "degraded", DegradedClusters: []string{"gamma"}},
		{Surface: "regime", Status: "partial", StaleClusters: []string{"credit", "vol"}, PartialClusters: []string{"gamma", "breadth"}},
	}
	if first, second := BuildRegimeFingerprint(&base), BuildRegimeFingerprint(&reordered); first != second {
		t.Fatalf("fingerprint should canonicalize slice ordering: %v != %v", first, second)
	}

	changedBand := base
	changedBand.VIXTermStructure.Band = "yellow"
	if BuildRegimeFingerprint(&base) == BuildRegimeFingerprint(&changedBand) {
		t.Fatal("fingerprint did not change when indicator band changed")
	}

	changedWarning := base
	changedWarning.WarningDetails = slices.Clone(base.WarningDetails)
	changedWarning.WarningDetails[0].Severity = "error"
	if BuildRegimeFingerprint(&base) == BuildRegimeFingerprint(&changedWarning) {
		t.Fatal("fingerprint did not change when warning semantic severity changed")
	}

	changedPartial := base
	changedPartial.DataQuality = slices.Clone(base.DataQuality)
	changedPartial.DataQuality[0].PartialClusters = []string{"gamma"}
	if BuildRegimeFingerprint(&base) == BuildRegimeFingerprint(&changedPartial) {
		t.Fatal("fingerprint did not change when partial cluster set changed")
	}
}

func TestRegimeLifecycleDistinguishesEarlyWarningFromConfirmedStress(t *testing.T) {
	t.Parallel()
	spyDrop := -2.0
	base := RegimeSnapshotResult{
		Composite: RegimeComposite{Verdict: "Stress signal present", ClusterRedCount: 1, ClusterYellowCount: 1, ClusterRankedCount: 4, ClusterUnrankedCount: 2},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "red"},
		},
		HYGSPYDivergence: RegimeHYGSPYDivergence{
			RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "green"},
			SPYChangePct:        &spyDrop,
		},
	}
	early := BuildRegimeLifecycle(&base)
	if early.Stage != LifecycleEarlyWarning || early.Severity != "watch" {
		t.Fatalf("early lifecycle = %+v, want early_warning/watch", early)
	}
	if len(early.ConfirmedBy) != 0 {
		t.Fatalf("early confirmed_by = %+v, want none until broad stress is confirmed", early.ConfirmedBy)
	}

	confirmed := base
	confirmed.Composite.ClusterRedCount = 2
	confirmed.CreditSpreads.RegimeIndicatorMeta.Band = "red"
	got := BuildRegimeLifecycle(&confirmed)
	if got.Stage != LifecycleConfirmedStress || got.Severity != "act" {
		t.Fatalf("confirmed lifecycle = %+v, want confirmed_stress/act", got)
	}
	if len(got.ConfirmedBy) == 0 {
		t.Fatalf("confirmed_by empty for confirmed stress: %+v", got)
	}
	if early.Fingerprint == got.Fingerprint {
		t.Fatal("lifecycle fingerprint did not change across semantic stage transition")
	}
}

func TestRegimeLifecycleDegradesReadinessForDataQuality(t *testing.T) {
	t.Parallel()
	base := RegimeSnapshotResult{
		Summary:   RegimeSummary{Confidence: "high"},
		Composite: RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		DataQuality: []DataQualityHealth{
			{Surface: "regime", Status: "stale", StaleClusters: []string{"breadth"}},
		},
	}
	got := BuildRegimeLifecycle(&base)
	if got.Stage != LifecycleQuiet {
		t.Fatalf("stage: want quiet, got %+v", got)
	}
	if got.Readiness != "degraded" {
		t.Fatalf("readiness: want degraded, got %+v", got)
	}
	if got.Confidence != "medium" {
		t.Fatalf("confidence: want medium cap, got %+v", got)
	}
}

func TestRegimeLifecycleDegradesReadinessForWeakRows(t *testing.T) {
	t.Parallel()
	base := RegimeSnapshotResult{
		Summary:   RegimeSummary{Confidence: "high"},
		Composite: RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		GammaZero: RegimeGammaZero{Status: RegimeStatusComputing},
	}
	got := BuildRegimeLifecycle(&base)
	if got.Stage != LifecycleQuiet {
		t.Fatalf("stage: want quiet, got %+v", got)
	}
	if got.Readiness != "degraded" {
		t.Fatalf("readiness: want degraded for computing gamma, got %+v", got)
	}
	if got.Confidence != "medium" {
		t.Fatalf("confidence: want medium cap, got %+v", got)
	}
}

func TestRegimeSourceHealthUsesOldestClusterAsOf(t *testing.T) {
	t.Parallel()
	old := time.Date(2026, time.May, 29, 21, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, time.June, 1, 21, 0, 0, 0, time.UTC)
	res := &RegimeSnapshotResult{
		AsOf:      fresh,
		Composite: RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band: "green",
				AsOf: &RegimeAsOfSummary{Time: fresh},
			},
			Status: RegimeStatusOK,
		},
		VolOfVol: RegimeVolOfVol{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band: "green",
				AsOf: &RegimeAsOfSummary{Time: old},
			},
			Status: RegimeStatusOK,
		},
	}
	got := BuildRegimeSourceHealth(res, fresh)
	var vol *SourceHealth
	for i := range got {
		if got[i].Source == "vol" {
			vol = &got[i]
			break
		}
	}
	if vol == nil {
		t.Fatalf("missing vol source health: %+v", got)
	}
	if !vol.AsOf.Equal(old) {
		t.Fatalf("vol as_of = %s, want oldest member timestamp %s", vol.AsOf, old)
	}
	if want := int64(72 * 60 * 60); vol.AgeSeconds != want {
		t.Fatalf("vol age_seconds = %d, want %d", vol.AgeSeconds, want)
	}
}

func TestRegimeSourceHealthUsesPartialDataQuality(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 21, 0, 0, 0, time.UTC)
	res := &RegimeSnapshotResult{
		AsOf:      now,
		Composite: RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		GammaZero: RegimeGammaZero{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band: "green",
				AsOf: &RegimeAsOfSummary{Time: now},
			},
			Status: RegimeStatusOK,
		},
		DataQuality: []DataQualityHealth{{
			Surface:         "gamma",
			Status:          "partial",
			PartialClusters: []string{"gamma"},
			AsOf:            now,
		}},
	}
	got := BuildRegimeSourceHealth(res, now)
	var gamma *SourceHealth
	for i := range got {
		if got[i].Source == "gamma" {
			gamma = &got[i]
			break
		}
	}
	if gamma == nil {
		t.Fatalf("missing gamma source health: %+v", got)
	}
	if gamma.Status != "partial" || gamma.Confidence != "medium" {
		t.Fatalf("gamma source health = %+v, want partial/medium", gamma)
	}
}
