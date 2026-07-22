package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

type earningsRoundTripFunc func(*http.Request) (*http.Response, error)

func (f earningsRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestParseNasdaqEarningsTypedOutcomes(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		body   string
		status string
		code   string
		stage  string
	}{
		{"announcement absent", `{"data":{},"status":{"rCode":200}}`, rpc.EarningsStatusNoDatePublished, "", ""},
		{"announcement null", `{"data":{"announcement":null},"status":{"rCode":200}}`, rpc.EarningsStatusNoDatePublished, "", ""},
		{"announcement empty", `{"data":{"announcement":""},"status":{"rCode":200}}`, rpc.EarningsStatusNoDatePublished, "", ""},
		{"changed announcement", `{"data":{"announcement":"No scheduled event currently"},"status":{"rCode":200}}`, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"malformed JSON", `{`, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqDecode},
		{"missing data", `{"status":{"rCode":200}}`, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageNasdaqSchema},
		{"unsupported", `{"data":null,"status":{"rCode":404}}`, rpc.EarningsStatusUnsupportedSecurity, "", ""},
		{"past date", `{"data":{"announcement":"Earnings announcement for OLD: Jul 20, 2026"},"status":{"rCode":200}}`, rpc.EarningsStatusNoDatePublished, "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseNasdaqEarnings([]byte(test.body), now); err == nil {
				t.Fatal("typed unresolved payload returned nil error")
			} else {
				var outcome *earningsProviderError
				if !errors.As(err, &outcome) {
					t.Fatalf("error type = %T, want *earningsProviderError", err)
				}
				if outcome.status != test.status {
					t.Fatalf("status = %q, want %q", outcome.status, test.status)
				}
				if test.code == "" {
					if outcome.failure != nil {
						t.Fatalf("semantic outcome leaked failure: %+v", outcome.failure)
					}
					return
				}
				if outcome.failure == nil || outcome.failure.Code != test.code || outcome.failure.Stage != test.stage {
					t.Fatalf("failure = %+v, want %s/%s", outcome.failure, test.code, test.stage)
				}
			}
		})
	}
}

func TestResolveEarningsProvidersAgreementAndLastGood(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	entry := func(date, session string) earningsEntry {
		return earningsEntry{Date: date, TimeOfDay: session, ObservedAt: now}
	}
	state := func(status string, current, lastGood *earningsEntry) earningsProviderState {
		next := now.Add(earningsFreshWindow)
		return earningsProviderState{
			LastAttempt: earningsProviderAttempt{
				Status: status, Entry: cloneEarningsEntry(current), AttemptedAt: now,
				CompletedAt: now, NextAttempt: &next,
			},
			LastGood: cloneEarningsEntry(lastGood),
		}
	}
	aapl := entry("2026-07-30", "")
	aaplAMC := entry("2026-07-30", "amc")
	aaplBMO := entry("2026-07-30", "bmo")
	different := entry("2026-07-31", "amc")

	tests := []struct {
		name      string
		providers map[string]earningsProviderState
		status    string
		reason    string
		date      string
		stale     bool
	}{
		{
			name: "matching dates compatible sessions",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusDate, &aapl, &aapl),
				earningsWSHProvider:    state(rpc.EarningsStatusDate, &aaplAMC, &aaplAMC),
			}, status: rpc.EarningsStatusDate, reason: earningsReasonConsensus, date: aapl.Date,
		},
		{
			name: "different dates conflict",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusDate, &aaplAMC, &aaplAMC),
				earningsWSHProvider:    state(rpc.EarningsStatusDate, &different, &different),
			}, status: rpc.EarningsStatusConflictingSources, reason: earningsReasonConflicting,
		},
		{
			name: "explicit sessions conflict",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusDate, &aaplAMC, &aaplAMC),
				earningsWSHProvider:    state(rpc.EarningsStatusDate, &aaplBMO, &aaplBMO),
			}, status: rpc.EarningsStatusConflictingSources, reason: earningsReasonConflicting,
		},
		{
			name: "one date plus explicit no date",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusDate, &aaplAMC, &aaplAMC),
				earningsWSHProvider:    state(rpc.EarningsStatusNoDatePublished, nil, nil),
			}, status: rpc.EarningsStatusDate, reason: earningsReasonSingleSource, date: aapl.Date,
		},
		{
			name: "transport retains last good",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusTransportFailure, nil, &aaplAMC),
			}, status: rpc.EarningsStatusDate, reason: earningsReasonRetainedLastGood, date: aapl.Date, stale: true,
		},
		{
			name: "no date does not hide behind last good",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusNoDatePublished, nil, &aaplAMC),
			}, status: rpc.EarningsStatusNoDatePublished, reason: rpc.EarningsStatusNoDatePublished,
		},
		{
			name: "format change does not hide behind last good",
			providers: map[string]earningsProviderState{
				earningsNasdaqProvider: state(rpc.EarningsStatusFormatChange, nil, &aaplAMC),
			}, status: rpc.EarningsStatusFormatChange, reason: rpc.EarningsStatusFormatChange,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := resolveEarningsProviders(test.providers, now)
			if got.Status != test.status || got.Reason != test.reason || got.Stale != test.stale {
				t.Fatalf("resolution = %+v, want status=%s reason=%s stale=%v", got, test.status, test.reason, test.stale)
			}
			if test.date == "" {
				if got.Entry != nil {
					t.Fatalf("unknown/conflict exposed date %+v", got.Entry)
				}
			} else if got.Entry == nil || got.Entry.Date != test.date {
				t.Fatalf("date = %+v, want %s", got.Entry, test.date)
			}
		})
	}
}

func TestEarningsProviderOutcomesPersistAndRecoverWithoutRawError(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := openMarketTestCoreStore(t)
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":{"announcement":null},"status":{"rCode":200}}`))}, nil
	})}
	if err := cache.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
		return transportFailureResult(rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false, now), errors.New("SECRET provider prose")
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	cache.refreshOne(context.Background(), "AAPL")

	view, ok := cache.resolution("AAPL")
	if !ok || view.Status != rpc.EarningsStatusTransportFailure || len(view.Providers) != 2 {
		t.Fatalf("resolution = %+v ok=%v", view, ok)
	}
	doc, ok, err := store.GetStateDocument(context.Background(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok {
		t.Fatalf("state read: ok=%v err=%v", ok, err)
	}
	if bytesContain(doc.JSON, "SECRET") {
		t.Fatal("raw provider error entered state authority")
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(doc.JSON, &header); err != nil || header.Version != earningsPersistVersion {
		t.Fatalf("state version: header=%+v err=%v", header, err)
	}
	observations, err := store.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsProviderObservationKind,
	})
	if err != nil || len(observations) != 2 {
		t.Fatalf("provider observations=%d err=%v", len(observations), err)
	}
	for _, observation := range observations {
		if bytesContain(observation.Payload, "SECRET") {
			t.Fatal("raw provider error entered immutable observation")
		}
	}

	restarted := newEarningsCacheMemory(nil)
	restarted.clock = func() time.Time { return now.Add(time.Minute) }
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatalf("restart attach: %v", err)
	}
	recovered, ok := restarted.resolution("AAPL")
	if !ok || recovered.Status != rpc.EarningsStatusTransportFailure || len(recovered.Providers) != 2 {
		t.Fatalf("recovered = %+v ok=%v", recovered, ok)
	}
	for _, provider := range recovered.Providers {
		if provider.NextAttempt == nil || !provider.NextAttempt.After(now) {
			t.Fatalf("provider retry state not recovered: %+v", provider)
		}
	}
}

func TestEarningsProviderBackoffPersistsAcrossRestart(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		result   earningsProviderFetchResult
		expected time.Duration
	}{
		{
			name:     "provider-confirmed unsupported security",
			result:   earningsProviderFetchResult{Status: rpc.EarningsStatusUnsupportedSecurity},
			expected: earningsTTL,
		},
		{
			name: "provider format",
			result: earningsProviderFetchResult{Status: rpc.EarningsStatusFormatChange, Failure: &rpc.SourceFailure{
				Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStageWSHDecode, Retryable: false,
			}},
			expected: earningsNonRetryableFailureRetry,
		},
		{
			name:     "non-retryable provider failure",
			result:   transportFailureResult(rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false, base),
			expected: earningsNonRetryableFailureRetry,
		},
		{
			name:     "temporary connector inactive",
			result:   transportFailureResult(rpc.SourceFailureContractUnavailable, rpc.SourceFailureStageWSHContractResolve, true, base),
			expected: earningsContractResolutionRetry,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := openMarketTestCoreStore(t)
			var providerLogs []string
			initial := newEarningsCacheMemory(func(format string, args ...any) {
				providerLogs = append(providerLogs, fmt.Sprintf(format, args...))
			})
			initial.clock = func() time.Time { return base }
			initial.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":{"announcement":null},"status":{"rCode":200}}`))}, nil
			})}
			providerCalls := 0
			if err := initial.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
				providerCalls++
				return tc.result, errors.New("typed provider failure")
			}); err != nil {
				t.Fatal(err)
			}
			if err := initial.UseCoreStore(store); err != nil {
				t.Fatal(err)
			}
			initial.refreshOne(context.Background(), "TESTQ")
			if providerCalls != 1 {
				t.Fatalf("initial provider calls = %d, want 1", providerCalls)
			}
			for _, line := range providerLogs {
				if strings.Contains(line, "TESTQ") {
					t.Fatalf("provider log exposed the requested name: %q", line)
				}
			}

			view, ok := initial.resolution("TESTQ")
			if !ok {
				t.Fatal("missing committed provider outcome")
			}
			var nextAttempt *time.Time
			for _, provider := range view.Providers {
				if provider.Provider == earningsWSHProvider {
					nextAttempt = provider.NextAttempt
				}
			}
			wantNext := base.Add(tc.expected)
			if nextAttempt == nil || !nextAttempt.Equal(wantNext) {
				t.Fatalf("persisted next attempt = %v, want %v", nextAttempt, wantNext)
			}

			restarted := newEarningsCacheMemory(nil)
			restartNow := wantNext.Add(-time.Minute)
			restarted.clock = func() time.Time { return restartNow }
			restarted.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":{"announcement":null},"status":{"rCode":200}}`))}, nil
			})}
			providerCalls = 0
			if err := restarted.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
				providerCalls++
				return earningsProviderFetchResult{}, errors.New("unexpected early request")
			}); err != nil {
				t.Fatal(err)
			}
			if err := restarted.UseCoreStore(store); err != nil {
				t.Fatalf("restart attach: %v", err)
			}
			recovered, ok := restarted.resolution("TESTQ")
			if !ok {
				t.Fatal("restart lost the failed provider outcome")
			}
			recoveredStatus := ""
			for _, provider := range recovered.Providers {
				if provider.Provider == earningsWSHProvider {
					recoveredStatus = provider.Status
				}
			}
			if recoveredStatus != tc.result.Status {
				t.Fatalf("restarted provider status = %q, want visible failure %q", recoveredStatus, tc.result.Status)
			}
			restarted.refreshOne(context.Background(), "TESTQ")
			if providerCalls != 0 {
				t.Fatalf("provider calls before persisted retry = %d, want 0", providerCalls)
			}

			restartNow = wantNext
			restarted.refreshOne(context.Background(), "TESTQ")
			if providerCalls != 1 {
				t.Fatalf("provider calls at persisted retry = %d, want 1", providerCalls)
			}
		})
	}
}

func TestEarningsConflictPersistsAcrossRestart(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dir, "daemon.db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"data":{"announcement":"Earnings announcement for AAPL: Jul 30, 2026"},"status":{"rCode":200}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if err := cache.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
		return earningsProviderFetchResult{Status: rpc.EarningsStatusDate, Entry: earningsEntry{Date: "2026-07-31", ObservedAt: now}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	cache.refreshOne(context.Background(), "AAPL")
	if _, _, ok := cache.get("AAPL"); ok {
		t.Fatal("conflicting providers exposed a usable earnings date")
	}
	view, ok := cache.resolution("AAPL")
	if !ok || view.Status != rpc.EarningsStatusConflictingSources {
		t.Fatalf("conflict = %+v ok=%v", view, ok)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = corestore.Open(context.Background(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatalf("reopen authority: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close reopened authority: %v", err)
		}
	}()

	restarted := newEarningsCacheMemory(nil)
	restarted.clock = func() time.Time { return now.Add(time.Minute) }
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	view, ok = restarted.resolution("AAPL")
	if !ok || view.Status != rpc.EarningsStatusConflictingSources {
		t.Fatalf("restarted conflict = %+v ok=%v", view, ok)
	}
}

func TestEarningsFailedAuthorityCommitDoesNotPublishMemory(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := openMarketTestCoreStore(t)
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.client = &http.Client{Transport: earningsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"data":{"announcement":"Earnings announcement for AAPL: Jul 30, 2026"},"status":{"rCode":200}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cache.refreshOne(context.Background(), "AAPL")
	if _, ok := cache.resolution("AAPL"); ok {
		t.Fatal("failed SQLite commit published provider result into memory")
	}
}

func TestEarningsV1AuthorityMigratesInPlace(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store := openMarketTestCoreStore(t)
	legacy := earningsPersistEnvelopeV1{Version: earningsLegacyVersion, Entries: map[string]earningsEntry{
		"AAPL": {Date: "2026-07-30", TimeOfDay: "amc", ObservedAt: now},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
		ScopeKey: earningsAuthorityScope, Kind: earningsStateKind, JSON: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now.Add(time.Minute) }
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := store.GetStateDocument(context.Background(), earningsAuthorityScope, earningsStateKind)
	if err != nil || !ok || doc.Revision != created.Revision+1 {
		t.Fatalf("migrated doc revision=%d ok=%v err=%v", doc.Revision, ok, err)
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(doc.JSON, &header); err != nil || header.Version != earningsPersistVersion {
		t.Fatalf("migrated header=%+v err=%v", header, err)
	}
	entry, _, ok := cache.get("AAPL")
	if !ok || entry.Date != "2026-07-30" {
		t.Fatalf("migrated entry=%+v ok=%v", entry, ok)
	}
	observations, err := store.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: earningsAuthorityScope, Kind: earningsProviderObservationKind,
	})
	if err != nil || len(observations) != 0 {
		t.Fatalf("migration invented observations: count=%d err=%v", len(observations), err)
	}
}

func TestEarningsAuthorityRejectsUnknownFields(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if _, err := decodeEarningsEnvelopeV1([]byte(`{"version":1,"entries":{},"unexpected":true}`), now, true); err == nil {
		t.Fatal("strict v1 authority accepted an unknown field")
	}

	next := now.Add(earningsFreshWindow)
	entry := earningsEntry{Date: "2026-07-30", ObservedAt: now}
	providers := map[string]earningsProviderState{earningsNasdaqProvider: {
		LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusDate, Entry: &entry, AttemptedAt: now, CompletedAt: now, NextAttempt: &next},
		LastGood:    &entry,
	}}
	state := earningsSymbolState{Resolution: resolveEarningsProviders(providers, now), Providers: providers, UpdatedAt: now}
	raw, err := json.Marshal(earningsPersistEnvelope{Version: earningsPersistVersion, Symbols: map[string]earningsSymbolState{"AAPL": state}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	symbols := envelope["symbols"].(map[string]any)
	symbols["AAPL"].(map[string]any)["unexpected"] = true
	raw, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEarningsEnvelopeV2(raw, now); err == nil {
		t.Fatal("v2 authority accepted an unknown nested field")
	}
}

func bytesContain(raw []byte, value string) bool {
	return strings.Contains(string(raw), value)
}
