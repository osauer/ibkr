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
			{Surface: "regime", Status: "stale", StaleClusters: []string{"vol", "credit"}},
			{Surface: "gamma", Status: "degraded", DegradedClusters: []string{"gamma"}},
		},
	}
	reordered := base
	reordered.VIXTermStructure.FieldsMissing = []string{"vix3m", "ratio"}
	reordered.WarningDetails = []RegimeWarning{base.WarningDetails[1], base.WarningDetails[0]}
	reordered.DataQuality = []DataQualityHealth{
		{Surface: "gamma", Status: "degraded", DegradedClusters: []string{"gamma"}},
		{Surface: "regime", Status: "stale", StaleClusters: []string{"credit", "vol"}},
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
}
