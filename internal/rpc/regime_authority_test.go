package rpc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRegimeAuthorityHealthValidStatesRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 8, 15, 0, 0, time.UTC)
	zeroAge := int64(0)
	staleAge := int64(901)

	tests := []RegimeAuthorityHealth{
		{
			Status:     RegimeAuthorityUnavailable,
			Refreshing: true,
		},
		{
			Status:      RegimeAuthorityUnavailable,
			Refreshing:  false,
			FailureCode: RegimeAuthorityFailureNoLastGood,
		},
		{
			Status:                RegimeAuthorityFresh,
			Refreshing:            false,
			LastSuccessAt:         &now,
			LastSuccessAgeSeconds: &zeroAge,
		},
		{
			Status:                RegimeAuthorityStale,
			Refreshing:            true,
			LastSuccessAt:         &now,
			LastSuccessAgeSeconds: &staleAge,
			FailureCode:           RegimeAuthorityFailureRefreshIncomplete,
		},
		{
			Status:                RegimeAuthorityStale,
			Refreshing:            false,
			LastSuccessAt:         &now,
			LastSuccessAgeSeconds: &zeroAge,
			FailureCode:           RegimeAuthorityFailureClockInvalid,
		},
	}

	for _, health := range tests {
		t.Run(string(health.Status)+"/"+string(health.FailureCode), func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(health)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded RegimeAuthorityHealth
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal %s: %v", encoded, err)
			}
			if decoded.Status != health.Status || decoded.Refreshing != health.Refreshing || decoded.FailureCode != health.FailureCode {
				t.Fatalf("round trip changed classified health: got %+v want %+v", decoded, health)
			}
			if (decoded.LastSuccessAt == nil) != (health.LastSuccessAt == nil) || (decoded.LastSuccessAgeSeconds == nil) != (health.LastSuccessAgeSeconds == nil) {
				t.Fatalf("round trip changed last-success presence: got %+v want %+v", decoded, health)
			}
			if decoded.LastSuccessAt != nil && !decoded.LastSuccessAt.Equal(*health.LastSuccessAt) {
				t.Fatalf("last_success_at = %v, want %v", decoded.LastSuccessAt, health.LastSuccessAt)
			}
			if decoded.LastSuccessAgeSeconds != nil && *decoded.LastSuccessAgeSeconds != *health.LastSuccessAgeSeconds {
				t.Fatalf("last_success_age_seconds = %d, want %d", *decoded.LastSuccessAgeSeconds, *health.LastSuccessAgeSeconds)
			}
		})
	}
}

func TestRegimeAuthorityHealthRejectsInconsistentStates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 8, 15, 0, 0, time.UTC)
	zero := time.Time{}
	age := int64(15)
	negativeAge := int64(-1)

	tests := []struct {
		name   string
		health RegimeAuthorityHealth
	}{
		{name: "unknown status", health: RegimeAuthorityHealth{Status: "warming", Refreshing: true}},
		{name: "unknown failure", health: RegimeAuthorityHealth{Status: RegimeAuthorityUnavailable, FailureCode: "raw_broker_error"}},
		{name: "idle unavailable without reason", health: RegimeAuthorityHealth{Status: RegimeAuthorityUnavailable}},
		{name: "unavailable with last good", health: RegimeAuthorityHealth{Status: RegimeAuthorityUnavailable, LastSuccessAt: &now, LastSuccessAgeSeconds: &age, FailureCode: RegimeAuthorityFailureRefreshFailed}},
		{name: "fresh without last good", health: RegimeAuthorityHealth{Status: RegimeAuthorityFresh}},
		{name: "stale without last good", health: RegimeAuthorityHealth{Status: RegimeAuthorityStale, Refreshing: true}},
		{name: "time without age", health: RegimeAuthorityHealth{Status: RegimeAuthorityFresh, LastSuccessAt: &now}},
		{name: "age without time", health: RegimeAuthorityHealth{Status: RegimeAuthorityFresh, LastSuccessAgeSeconds: &age}},
		{name: "zero last success", health: RegimeAuthorityHealth{Status: RegimeAuthorityFresh, LastSuccessAt: &zero, LastSuccessAgeSeconds: &age}},
		{name: "negative age", health: RegimeAuthorityHealth{Status: RegimeAuthorityFresh, LastSuccessAt: &now, LastSuccessAgeSeconds: &negativeAge}},
		{name: "available no-last-good reason", health: RegimeAuthorityHealth{Status: RegimeAuthorityStale, LastSuccessAt: &now, LastSuccessAgeSeconds: &age, FailureCode: RegimeAuthorityFailureNoLastGood}},
		{name: "available invalid-state reason", health: RegimeAuthorityHealth{Status: RegimeAuthorityFresh, LastSuccessAt: &now, LastSuccessAgeSeconds: &age, FailureCode: RegimeAuthorityFailureInvalidPersistedState}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateRegimeAuthorityHealth(tc.health); err == nil {
				t.Fatal("validation unexpectedly succeeded")
			}
			if _, err := json.Marshal(tc.health); err == nil {
				t.Fatal("marshal unexpectedly succeeded")
			}
		})
	}
}

func TestRegimeAuthorityHealthJSONIsExact(t *testing.T) {
	t.Parallel()
	valid := `{"status":"unavailable","refreshing":false,"failure_code":"no_last_good"}`
	tests := []struct {
		name string
		data string
	}{
		{name: "not object", data: `[]`},
		{name: "missing status", data: `{"refreshing":true}`},
		{name: "missing refreshing", data: `{"status":"unavailable","failure_code":"no_last_good"}`},
		{name: "unknown key", data: strings.TrimSuffix(valid, "}") + `,"error":"private upstream text"}`},
		{name: "duplicate key", data: `{"status":"unavailable","status":"stale","refreshing":true}`},
		{name: "null field", data: `{"status":"unavailable","refreshing":false,"failure_code":null}`},
		{name: "trailing json", data: valid + `{}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var health RegimeAuthorityHealth
			if err := json.Unmarshal([]byte(tc.data), &health); err == nil {
				t.Fatalf("unmarshal unexpectedly accepted %s", tc.data)
			}
		})
	}
}

func TestRegimeFingerprintIgnoresAuthorityHealth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 8, 15, 0, 0, time.UTC)
	freshAge := int64(2)
	staleAge := int64(1_802)
	base := RegimeSnapshotResult{
		Composite: RegimeComposite{
			Verdict:            "Normal regime",
			GreenCount:         1,
			RankedCount:        1,
			ClusterGreenCount:  1,
			ClusterRankedCount: 1,
		},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "green"},
			Status:              RegimeStatusOK,
		},
		AuthorityHealth: &RegimeAuthorityHealth{
			Status:                RegimeAuthorityFresh,
			LastSuccessAt:         &now,
			LastSuccessAgeSeconds: &freshAge,
		},
	}

	changed := base
	changed.AuthorityHealth = &RegimeAuthorityHealth{
		Status:                RegimeAuthorityStale,
		Refreshing:            true,
		LastSuccessAt:         &now,
		LastSuccessAgeSeconds: &staleAge,
		FailureCode:           RegimeAuthorityFailureRefreshTimeout,
	}
	if first, second := BuildRegimeFingerprint(&base), BuildRegimeFingerprint(&changed); first != second {
		t.Fatalf("fingerprint changed on authority/cache metadata: %v != %v", first, second)
	}
}

func TestRegimeUnavailableCodeIsStable(t *testing.T) {
	t.Parallel()
	if CodeRegimeUnavailable != "regime_unavailable" {
		t.Fatalf("CodeRegimeUnavailable = %q", CodeRegimeUnavailable)
	}
}
