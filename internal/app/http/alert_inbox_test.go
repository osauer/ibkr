package apphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/live"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestAlertInboxV2RoutesRequireAuthAndBootstrapMatchesGET(t *testing.T) {
	t.Parallel()
	srv, store, _ := newGovernanceTestHandlerWithoutPoll(t, routeFakeClient{})
	handler := srv.Handler()

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/alert-inbox-v2", nil),
		httptest.NewRequest(http.MethodGet, "/api/alert-inbox-v2/attention", nil),
		httptest.NewRequest(http.MethodPost, "/api/alert-inbox-v2/attention/read", strings.NewReader(`{"through_seq":0}`)),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s %s status=%d", request.Method, request.URL.Path, response.Code)
		}
	}

	privateEpisode, privateOccurrence := alertInboxHTTPObserve(t, store, time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC))
	cookie := routeSessionCookie(t, handler)

	getRequest := httptest.NewRequest(http.MethodGet, "/api/alert-inbox-v2", nil)
	getRequest.AddCookie(cookie)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	var getDTO AlertInboxV2DTO
	if err := json.Unmarshal(getResponse.Body.Bytes(), &getDTO); err != nil {
		t.Fatal(err)
	}
	if getDTO.SchemaVersion != AlertInboxV2SchemaVersion || getDTO.Authority != AlertInboxV2Authority || !getDTO.Initialized || getDTO.Generation != 1 {
		t.Fatalf("GET envelope=%+v", getDTO)
	}
	if len(getDTO.Occurrences) != 1 || getDTO.Occurrences[0].DisplayID == "" || getDTO.Attention.UnreadCount != 1 || len(getDTO.Attention.UnreadRefs) != 1 {
		t.Fatalf("GET occurrences/attention=%+v", getDTO)
	}

	bootstrapRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootstrapRequest.AddCookie(cookie)
	bootstrapResponse := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapResponse, bootstrapRequest)
	if bootstrapResponse.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", bootstrapResponse.Code, bootstrapResponse.Body.String())
	}
	var bootstrap struct {
		AlertInboxV2 json.RawMessage `json:"alert_inbox_v2"`
	}
	if err := json.Unmarshal(bootstrapResponse.Body.Bytes(), &bootstrap); err != nil {
		t.Fatal(err)
	}
	var getValue, bootstrapValue any
	if err := json.Unmarshal(getResponse.Body.Bytes(), &getValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bootstrap.AlertInboxV2, &bootstrapValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(getValue, bootstrapValue) {
		t.Fatalf("bootstrap projection differs from GET\nGET=%s\nbootstrap=%s", getResponse.Body.Bytes(), bootstrap.AlertInboxV2)
	}

	raw := getResponse.Body.String()
	for _, forbidden := range []string{
		`"source_watermarks"`, `"transport_eligible"`, `"attempt_totals"`, `"last_push_service_acceptance_at"`,
		`"episode_key"`, `"occurrence_key"`, `"evidence_fingerprint"`, `"attempt_id"`, `"receipt_key"`,
		`"target_ref"`, `"device_id"`, `"subscription_id"`, privateEpisode, privateOccurrence,
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("public alert inbox leaked %q: %s", forbidden, raw)
		}
	}
	assertAlertInboxV2ExactJSONKeys(t, getResponse.Body.Bytes())
}

func TestAlertInboxV2AttentionIsIndependentAndReadReturnsFullGeneration(t *testing.T) {
	t.Parallel()
	srv, store, _ := newGovernanceTestHandlerWithoutPoll(t, routeFakeClient{})
	handler := srv.Handler()
	if err := store.RecordAlert(state.AlertRecord{ID: "legacy-canary"}); err != nil {
		t.Fatal(err)
	}
	legacyBefore := store.Attention()
	alertInboxHTTPObserve(t, store, time.Date(2026, 7, 20, 18, 5, 0, 0, time.UTC))
	if got := store.Attention(); !reflect.DeepEqual(got, legacyBefore) {
		t.Fatalf("v2 observation changed legacy attention: before=%+v after=%+v", legacyBefore, got)
	}

	cookie := routeSessionCookie(t, handler)
	read := httptest.NewRequest(http.MethodPost, "/api/alert-inbox-v2/attention/read", strings.NewReader(`{"through_seq":1}`))
	read.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, read)
	var dto AlertInboxV2DTO
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &dto) != nil {
		t.Fatalf("read status=%d body=%s", response.Code, response.Body.String())
	}
	if dto.Generation != 2 || dto.Attention.HighWaterSeq != 1 || dto.Attention.ReadThroughSeq != 1 || dto.Attention.UnreadCount != 0 || len(dto.Attention.UnreadRefs) != 0 {
		t.Fatalf("read returned incoherent full generation: %+v", dto)
	}
	if got := store.Attention(); !reflect.DeepEqual(got, legacyBefore) {
		t.Fatalf("v2 read changed legacy attention: before=%+v after=%+v", legacyBefore, got)
	}
	if _, err := store.MarkAttentionRead(legacyBefore.HighWaterSeq); err != nil {
		t.Fatal(err)
	}
	if got := store.AlertDelivery(time.Now().UTC()).Attention; got.ReadThroughSeq != 1 || got.HighWaterSeq != 1 {
		t.Fatalf("legacy read changed v2 attention: %+v", got)
	}
}

func TestAlertInboxV2AttentionReadRejectsBodiesAndCursors(t *testing.T) {
	t.Parallel()
	srv, store, _ := newGovernanceTestHandlerWithoutPoll(t, routeFakeClient{})
	handler := srv.Handler()
	alertInboxHTTPObserve(t, store, time.Date(2026, 7, 20, 18, 10, 0, 0, time.UTC))
	cookie := routeSessionCookie(t, handler)

	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"through_seq":null}`, `{"other":1}`, `{"through_seq":1,"other":2}`,
		`{"through_seq":-1}`, `{"through_seq":1.5}`, `{"through_seq":"1"}`, `{"through_seq":1,"through_seq":1}`,
		`{"through_seq":1}{"through_seq":1}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/alert-inbox-v2/attention/read", strings.NewReader(body))
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body=%q status=%d response=%s", body, response.Code, response.Body.String())
		}
		if got := store.AlertDelivery(time.Now().UTC()).Attention; got.ReadThroughSeq != 0 || got.UnreadCount != 1 {
			t.Fatalf("invalid body=%q changed attention=%+v", body, got)
		}
	}

	invalidCursor := httptest.NewRequest(http.MethodPost, "/api/alert-inbox-v2/attention/read", strings.NewReader(`{"through_seq":2}`))
	invalidCursor.AddCookie(cookie)
	invalidCursorResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidCursorResponse, invalidCursor)
	if invalidCursorResponse.Code != http.StatusConflict {
		t.Fatalf("future cursor status=%d body=%s", invalidCursorResponse.Code, invalidCursorResponse.Body.String())
	}

	valid := httptest.NewRequest(http.MethodPost, "/api/alert-inbox-v2/attention/read", strings.NewReader(`{"through_seq":1}`))
	valid.AddCookie(cookie)
	validResponse := httptest.NewRecorder()
	handler.ServeHTTP(validResponse, valid)
	if validResponse.Code != http.StatusOK {
		t.Fatalf("valid cursor status=%d body=%s", validResponse.Code, validResponse.Body.String())
	}
	regression := httptest.NewRequest(http.MethodPost, "/api/alert-inbox-v2/attention/read", strings.NewReader(`{"through_seq":0}`))
	regression.AddCookie(cookie)
	regressionResponse := httptest.NewRecorder()
	handler.ServeHTTP(regressionResponse, regression)
	if regressionResponse.Code != http.StatusConflict {
		t.Fatalf("regression status=%d body=%s", regressionResponse.Code, regressionResponse.Body.String())
	}
}

func TestAlertInboxV2AttentionPersistenceFailureIs500AndRollsBack(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	alertInboxHTTPObserve(t, store, time.Date(2026, 7, 20, 18, 15, 0, 0, time.UTC))
	before := store.AlertDelivery(time.Now().UTC()).Attention
	backup := dir + "-persisted"
	if err := os.Rename(dir, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("block app state directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		_ = os.Remove(dir)
		_ = os.Rename(backup, dir)
		restored = true
	}
	t.Cleanup(restore)

	h := &handler{deps: Dependencies{Store: store}}
	request := httptest.NewRequest(http.MethodPost, "/api/alert-inbox-v2/attention/read", strings.NewReader(`{"through_seq":1}`))
	response := httptest.NewRecorder()
	h.handleAlertInboxV2AttentionRead(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("persistence status=%d body=%s", response.Code, response.Body.String())
	}
	after := store.AlertDelivery(time.Now().UTC()).Attention
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed persistence changed in-memory cursor: before=%+v after=%+v", before, after)
	}
	restore()
	reopened, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if durable := reopened.AlertDelivery(time.Now().UTC()).Attention; !reflect.DeepEqual(durable, before) {
		t.Fatalf("failed persistence changed durable cursor: before=%+v durable=%+v", before, durable)
	}
}

func TestAlertInboxV2SSESendsInitialAndGenerationUpdate(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	liveService := live.New(routeFakeClient{}, time.Minute, time.Minute)
	h := &handler{deps: Dependencies{Store: store, Live: liveService}}
	recorder := newAlertInboxSSERecorder()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handleEvents(recorder, request)
	}()

	alertInboxSSEWait(t, recorder, func(body string) bool {
		return strings.Count(body, "event: alert_inbox_v2") >= 1 && strings.Contains(body, `"initialized":false`) && strings.Contains(body, `"generation":0`)
	})
	alertInboxHTTPObserve(t, store, time.Date(2026, 7, 20, 18, 20, 0, 0, time.UTC))
	alertInboxSSEWait(t, recorder, func(body string) bool {
		return strings.Count(body, "event: alert_inbox_v2") >= 2 && strings.Contains(body, `"initialized":true`) && strings.Contains(body, `"generation":1`)
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not stop after cancellation")
	}
}

func TestAlertInboxV2ColdWriteFailureIsPublicAndAdvancesSSECursor(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 7, 20, 18, 19, 0, 0, time.UTC)
	baseline := newAlertInboxV2DTO(state.AlertDeliveryView{})
	failure := newAlertInboxV2DTO(state.AlertDeliveryView{
		Generation: 1,
		DeliveryHealth: state.AlertDeliveryHealth{
			State: state.AlertDeliveryHealthUnavailable, Class: state.AlertDeliveryHealthClassStateWrite, UpdatedAt: at,
		},
	})
	if failure.Initialized || failure.Generation != 1 || failure.AsOf != nil || failure.CurrentState != nil || failure.Coverage != nil || len(failure.Occurrences) != 0 {
		t.Fatalf("cold failure invented initialized alert data: %+v", failure)
	}
	if failure.DeliveryHealth.State != state.AlertDeliveryHealthUnavailable || failure.DeliveryHealth.Class != state.AlertDeliveryHealthClassStateWrite || failure.DeliveryHealth.UpdatedAt == nil || !failure.DeliveryHealth.UpdatedAt.Equal(at) {
		t.Fatalf("cold persistence failure was hidden: %+v", failure.DeliveryHealth)
	}
	if newAlertInboxV2StreamCursor(baseline) == newAlertInboxV2StreamCursor(failure) {
		t.Fatalf("cold persistence failure did not advance the SSE cursor: baseline=%+v failure=%+v", baseline, failure)
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"initialized":false`) || !strings.Contains(string(raw), `"class":"state_write_failure"`) {
		t.Fatalf("cold failure JSON=%s", raw)
	}
}

func TestAlertInboxV2InvalidPersistedStateIsRedactedUninitializedAndNotClearable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	privateValue := "private-quarantined-ledger-value"
	persisted := `{"alert_settings":{"mode":"watch_and_act"},"alert_history":[{"id":"legacy-still-usable"}],"alert_delivery":{"version":17,"generation":9,"private_marker":"` + privateValue + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(persisted), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("Open quarantined state: %v", err)
	}
	h := &handler{deps: Dependencies{Store: store}}
	dto := h.alertInboxV2DTO()
	if dto.Initialized || dto.Generation == 0 || dto.AsOf != nil || dto.CurrentState != nil || dto.Coverage != nil ||
		len(dto.Occurrences) != 0 || dto.Attention.UnreadCount != 0 || dto.Attention.HighWaterSeq != 0 || dto.Attention.ReadThroughSeq != 0 || len(dto.Attention.UnreadRefs) != 0 {
		t.Fatalf("quarantine invented initialized/public state: %+v", dto)
	}
	if dto.DeliveryHealth.State != state.AlertDeliveryHealthUnavailable || dto.DeliveryHealth.Class != state.AlertDeliveryHealthClassInvalidPersistedState || dto.DeliveryHealth.UpdatedAt == nil {
		t.Fatalf("quarantine health=%+v", dto.DeliveryHealth)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{privateValue, "private_marker", "unsupported-private-version", "alert-delivery-quarantine-sha256-", "artifact", "path"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("public quarantine DTO leaked %q: %s", forbidden, raw)
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, top, "schema_version", "authority", "initialized", "generation", "as_of", "current_state", "coverage", "occurrences", "attention", "delivery_health")

	request := httptest.NewRequest(http.MethodPost, "/api/alert-inbox-v2/attention/read", strings.NewReader(`{"through_seq":0}`))
	response := httptest.NewRecorder()
	h.handleAlertInboxV2AttentionRead(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "state is unavailable") {
		t.Fatalf("quarantine attention read status=%d body=%s", response.Code, response.Body.String())
	}
	after := h.alertInboxV2DTO()
	if after.Initialized || after.DeliveryHealth.Class != state.AlertDeliveryHealthClassInvalidPersistedState || after.Generation != dto.Generation {
		t.Fatalf("attention read altered quarantine: before=%+v after=%+v", dto, after)
	}
}

func alertInboxHTTPObserve(t *testing.T, store *state.Store, at time.Time) (string, string) {
	t.Helper()
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceCanary, rpc.AlertKindPortfolioRisk, "private-account-and-symbol")
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := rpc.BuildAlertOccurrenceKey(episode, "private-opening")
	if err != nil {
		t.Fatal(err)
	}
	candidate := rpc.AlertCandidate{
		EpisodeKey: episode, OccurrenceKey: occurrence,
		EvidenceFingerprint: "sha256:" + strings.Repeat("a", 64),
		Source:              rpc.AlertSourceCanary, Kind: rpc.AlertKindPortfolioRisk, State: rpc.AlertEpisodeOpen,
		Severity: rpc.AlertSeverityWatch, DeliveryPreference: rpc.AlertDeliveryUnapproved,
		EvidenceHealth: rpc.AlertEvidenceCurrent, Destination: rpc.AlertDestinationAlerts,
		EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at,
	}
	snapshot := rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AsOf: at, CurrentState: rpc.AlertSnapshotActive,
		Coverage: rpc.AlertCoverage{
			State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: at,
			ExpectedSources: []rpc.AlertSource{rpc.AlertSourceCanary}, CoveredSources: []rpc.AlertSource{rpc.AlertSourceCanary},
		},
		Candidates: []rpc.AlertCandidate{candidate},
	}
	if _, err := store.ObserveAlertSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	return episode, occurrence
}

func assertAlertInboxV2ExactJSONKeys(t *testing.T, raw []byte) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, top, "schema_version", "authority", "initialized", "generation", "as_of", "current_state", "coverage", "occurrences", "attention", "delivery_health")
	var health map[string]json.RawMessage
	if err := json.Unmarshal(top["delivery_health"], &health); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, health, "state", "class", "updated_at")
	var coverage map[string]json.RawMessage
	if err := json.Unmarshal(top["coverage"], &coverage); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, coverage, "state", "freshness", "as_of", "expected_sources", "covered_sources")
	var attention map[string]json.RawMessage
	if err := json.Unmarshal(top["attention"], &attention); err != nil {
		t.Fatal(err)
	}
	assertJSONKeys(t, attention, "unread_count", "high_water_seq", "read_through_seq", "unread_refs")
	var unreadRefs []map[string]json.RawMessage
	if err := json.Unmarshal(attention["unread_refs"], &unreadRefs); err != nil || len(unreadRefs) != 1 {
		t.Fatalf("unread_refs decode=%v len=%d", err, len(unreadRefs))
	}
	assertJSONKeys(t, unreadRefs[0], "display_id", "source", "kind")
	var occurrences []map[string]json.RawMessage
	if err := json.Unmarshal(top["occurrences"], &occurrences); err != nil || len(occurrences) != 1 {
		t.Fatalf("occurrences decode=%v len=%d", err, len(occurrences))
	}
	assertJSONKeys(t, occurrences[0], "display_id", "source", "kind", "state", "severity", "delivery_preference", "evidence_health", "destination", "evidence_as_of", "state_changed_at", "first_seen_at", "last_seen_at", "ended_at", "end_reason", "attention_seq")
}

func assertJSONKeys(t *testing.T, value map[string]json.RawMessage, expected ...string) {
	t.Helper()
	if len(value) != len(expected) {
		t.Fatalf("JSON keys=%v want=%v", reflect.ValueOf(value).MapKeys(), expected)
	}
	for _, key := range expected {
		if _, ok := value[key]; !ok {
			t.Fatalf("JSON missing key %q in %v", key, reflect.ValueOf(value).MapKeys())
		}
	}
}

type alertInboxSSERecorder struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	status int
}

func newAlertInboxSSERecorder() *alertInboxSSERecorder {
	return &alertInboxSSERecorder{header: make(http.Header)}
}

func (r *alertInboxSSERecorder) Header() http.Header { return r.header }

func (r *alertInboxSSERecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *alertInboxSSERecorder) Write(value []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(value)
}

func (r *alertInboxSSERecorder) Flush() {}

func (r *alertInboxSSERecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func alertInboxSSEWait(t *testing.T, recorder *alertInboxSSERecorder, ready func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready(recorder.String()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for SSE state: %s", recorder.String())
}

var _ http.Flusher = (*alertInboxSSERecorder)(nil)
var _ io.Writer = (*alertInboxSSERecorder)(nil)
