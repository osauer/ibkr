package apphttp

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hyperserve "github.com/osauer/hyperserve/pkg/server"

	"github.com/osauer/ibkr/internal/app/auth"
	"github.com/osauer/ibkr/internal/app/daemonclient"
	"github.com/osauer/ibkr/internal/app/live"
	"github.com/osauer/ibkr/internal/app/orderreview"
	"github.com/osauer/ibkr/internal/app/relay"
	"github.com/osauer/ibkr/internal/app/state"
	"github.com/osauer/ibkr/internal/rpc"
)

func TestBootstrapRequiresAuth(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", res.Code, res.Body.String())
	}
}

func TestPairingBootstrapAndSnapshotTool(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	pairReq := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", bytes.NewReader([]byte("{}")))
	pairReq.RemoteAddr = "127.0.0.1:12345"
	pairRes := httptest.NewRecorder()
	handler.ServeHTTP(pairRes, pairReq)
	if pairRes.Code != http.StatusOK {
		t.Fatalf("pair status=%d, want 200; body=%s", pairRes.Code, pairRes.Body.String())
	}
	var pairing auth.PairingSession
	if err := json.NewDecoder(pairRes.Body).Decode(&pairing); err != nil {
		t.Fatalf("decode pairing: %v", err)
	}
	key := newRouteTestKey(t)
	completeBody, err := json.Marshal(auth.CompletePairingRequest{
		PairingID:    pairing.ID,
		Nonce:        pairing.Nonce,
		DeviceName:   "iPhone",
		PublicKeyJWK: routeTestJWK(t, key),
		Signature:    routeTestSignature(t, key, pairing.Nonce),
	})
	if err != nil {
		t.Fatalf("marshal complete body: %v", err)
	}
	completeReq := httptest.NewRequest(http.MethodPost, "/api/pairing/complete", bytes.NewReader(completeBody))
	completeRes := httptest.NewRecorder()
	handler.ServeHTTP(completeRes, completeReq)
	if completeRes.Code != http.StatusOK {
		t.Fatalf("complete status=%d, want 200; body=%s", completeRes.Code, completeRes.Body.String())
	}
	cookies := completeRes.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("pairing response did not set a session cookie")
	}

	bootReq := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootReq.AddCookie(cookies[0])
	bootRes := httptest.NewRecorder()
	handler.ServeHTTP(bootRes, bootReq)
	if bootRes.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d, want 200; body=%s", bootRes.Code, bootRes.Body.String())
	}
	var boot map[string]any
	if err := json.NewDecoder(bootRes.Body).Decode(&boot); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if boot["version"] != "test-version" {
		t.Fatalf("version=%v, want test-version", boot["version"])
	}
	snapshot, ok := boot["snapshot"].(map[string]any)
	if !ok || snapshot["market_calendar"] == nil {
		t.Fatalf("bootstrap snapshot missing market_calendar: %#v", boot["snapshot"])
	}

	toolReq := httptest.NewRequest(http.MethodPost, "/api/tools/snapshot", nil)
	toolReq.AddCookie(cookies[0])
	toolRes := httptest.NewRecorder()
	handler.ServeHTTP(toolRes, toolReq)
	if toolRes.Code != http.StatusOK {
		t.Fatalf("snapshot tool status=%d, want 200; body=%s", toolRes.Code, toolRes.Body.String())
	}
}

func TestClearAlertHistory(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	cookie := routeSessionCookie(t, handler)

	clearReq := httptest.NewRequest(http.MethodDelete, "/api/alerts", nil)
	clearReq.AddCookie(cookie)
	clearRes := httptest.NewRecorder()
	handler.ServeHTTP(clearRes, clearReq)
	if clearRes.Code != http.StatusOK {
		t.Fatalf("clear status=%d, want 200; body=%s", clearRes.Code, clearRes.Body.String())
	}

	alertsReq := httptest.NewRequest(http.MethodGet, "/api/alerts", nil)
	alertsReq.AddCookie(cookie)
	alertsRes := httptest.NewRecorder()
	handler.ServeHTTP(alertsRes, alertsReq)
	if alertsRes.Code != http.StatusOK {
		t.Fatalf("alerts status=%d, want 200; body=%s", alertsRes.Code, alertsRes.Body.String())
	}
	var alerts []state.AlertRecord
	if err := json.NewDecoder(alertsRes.Body).Decode(&alerts); err != nil {
		t.Fatalf("decode alerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("alerts len=%d, want 0", len(alerts))
	}
}

func TestOrderReviewSetCreatePreviewAndReadOnlyOrders(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	cookie := routeSessionCookie(t, handler)

	createReq := httptest.NewRequest(http.MethodPost, "/api/order-review-sets", nil)
	createReq.AddCookie(cookie)
	createRes := httptest.NewRecorder()
	handler.ServeHTTP(createRes, createReq)
	if createRes.Code != http.StatusOK {
		t.Fatalf("create status=%d, want 200; body=%s", createRes.Code, createRes.Body.String())
	}
	var set struct {
		ID           string `json:"id"`
		Revision     string `json:"revision"`
		SourceKind   string `json:"source_kind"`
		Intent       string `json:"intent"`
		Capabilities struct {
			CanPreview  bool `json:"can_preview"`
			CanTransmit bool `json:"can_transmit"`
		} `json:"capabilities"`
		Rows []struct {
			RowID            string   `json:"row_id"`
			ProposedQuantity int      `json:"proposed_quantity"`
			EditableQuantity int      `json:"editable_quantity"`
			MaxQuantity      int      `json:"max_quantity"`
			Included         bool     `json:"included"`
			Action           string   `json:"action"`
			Blockers         []string `json:"blockers"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(createRes.Body).Decode(&set); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if set.SourceKind != "risk_plan" || set.Intent != "mitigate_risk" {
		t.Fatalf("review set source/intent = %s/%s", set.SourceKind, set.Intent)
	}
	if !set.Capabilities.CanPreview || set.Capabilities.CanTransmit {
		t.Fatalf("unexpected capabilities: %#v", set.Capabilities)
	}
	if len(set.Rows) != 1 || set.Rows[0].Action != rpc.OrderActionSell || !set.Rows[0].Included {
		t.Fatalf("unexpected rows: %#v", set.Rows)
	}

	body, err := json.Marshal(map[string]any{
		"revision": set.Revision,
		"rows": []map[string]any{{
			"row_id":   set.Rows[0].RowID,
			"included": true,
			"quantity": 2,
		}},
	})
	if err != nil {
		t.Fatalf("marshal preview: %v", err)
	}
	previewReq := httptest.NewRequest(http.MethodPost, "/api/order-review-sets/"+set.ID+"/preview", bytes.NewReader(body))
	previewReq.AddCookie(cookie)
	previewRes := httptest.NewRecorder()
	handler.ServeHTTP(previewRes, previewReq)
	if previewRes.Code != http.StatusOK {
		t.Fatalf("preview status=%d, want 200; body=%s", previewRes.Code, previewRes.Body.String())
	}
	var preview struct {
		Preview struct {
			SubmitReady bool `json:"submit_ready"`
			Rows        []struct {
				RowID          string `json:"row_id"`
				TokenMinted    bool   `json:"token_minted"`
				SubmitEligible bool   `json:"submit_eligible"`
				WhatIfStatus   string `json:"what_if_status"`
			} `json:"rows"`
		} `json:"preview"`
	}
	if err := json.NewDecoder(previewRes.Body).Decode(&preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if !preview.Preview.SubmitReady || len(preview.Preview.Rows) != 1 || !preview.Preview.Rows[0].TokenMinted {
		t.Fatalf("unexpected preview: %#v", preview.Preview)
	}

	openReq := httptest.NewRequest(http.MethodGet, "/api/orders/open", nil)
	openReq.AddCookie(cookie)
	openRes := httptest.NewRecorder()
	handler.ServeHTTP(openRes, openReq)
	if openRes.Code != http.StatusOK {
		t.Fatalf("orders open status=%d, want 200; body=%s", openRes.Code, openRes.Body.String())
	}
}

func TestOrderReviewSetPreviewRejectsStaleRevision(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	authMgr := auth.NewManager(store, time.Minute)
	fakeClient := routeFakeClient{}
	liveSvc := live.New(fakeClient, time.Minute, time.Minute)
	srv, err := hyperserve.NewServer(hyperserve.WithAddr("127.0.0.1:0"), hyperserve.WithSuppressBanner(true))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	Register(Dependencies{
		Server:    srv,
		Store:     store,
		Auth:      authMgr,
		Daemon:    fakeClient,
		Live:      liveSvc,
		Relay:     relay.Noop{PublicURL: "https://relay.example"},
		PublicURL: "https://relay.example",
		Version:   "test-version",
	})
	handler := srv.Handler()
	cookie := routeSessionCookie(t, handler)
	stale := buildRiskPlanReviewSet(rpc.RiskPlanResult{
		PlanID:                     "plan-1",
		RefreshedCanaryFingerprint: rpc.Fingerprint{Key: "fp-1"},
		Candidates: []rpc.RiskPlanCandidate{{
			ID:     "candidate-1",
			Status: rpc.RiskPlanCandidatePreviewable,
			Legs: []rpc.RiskPlanCandidateLeg{{
				Action:         "SELL",
				Contract:       rpc.ContractParams{Symbol: "SPY", SecType: "STK"},
				Quantity:       1,
				HeldQuantity:   10,
				PositionEffect: rpc.OrderPositionEffectReduce,
				OrderType:      rpc.OrderTypeLMT,
				TIF:            rpc.OrderTIFDay,
			}},
		}},
	}, rpc.TradingStatus{CanPreview: true, PreviewRequired: true}, time.Now().UTC())
	stale.Revision = "rev_stale"
	if err := store.RecordOrderReviewSet(stale); err != nil {
		t.Fatalf("RecordOrderReviewSet: %v", err)
	}
	body := bytes.NewReader([]byte(`{"revision":"rev_stale","rows":[{"row_id":"candidate-1:1","included":true,"quantity":1}]}`))
	req := httptest.NewRequest(http.MethodPost, "/api/order-review-sets/"+stale.ID+"/preview", body)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", res.Code, res.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode rebase: %v", err)
	}
	if got["code"] != "rebase_required" || got["current_set"] == nil {
		t.Fatalf("unexpected rebase response: %#v", got)
	}
}

func TestOrderReviewSetMatchesCurrentSnapshotOnly(t *testing.T) {
	t.Parallel()
	snap := live.Snapshot{
		Canary:  &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		Trading: &rpc.TradingStatus{Account: "DU123", Mode: "paper"},
	}
	current := orderreview.Set{
		CanaryFingerprint: "fp-1",
		Capabilities:      rpc.TradingStatus{Account: "DU123", Mode: "paper"},
	}
	if !orderReviewSetMatchesSnapshot(current, snap) {
		t.Fatalf("current set should match snapshot")
	}
	for name, set := range map[string]orderreview.Set{
		"fingerprint": {CanaryFingerprint: "fp-old", Capabilities: current.Capabilities},
		"account":     {CanaryFingerprint: "fp-1", Capabilities: rpc.TradingStatus{Account: "DU999", Mode: "paper"}},
		"mode":        {CanaryFingerprint: "fp-1", Capabilities: rpc.TradingStatus{Account: "DU123", Mode: "live"}},
	} {
		if orderReviewSetMatchesSnapshot(set, snap) {
			t.Fatalf("%s-stale set should not match snapshot", name)
		}
	}
}

func TestOrderReviewSetTransmitRequiresTradingCapability(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t).Handler()
	cookie := routeSessionCookie(t, handler)
	set := createRouteReviewSet(t, handler, cookie)
	previewRouteReviewSet(t, handler, cookie, set.ID, set.Revision, set.RowID)

	req := httptest.NewRequest(http.MethodPost, "/api/order-review-sets/"+set.ID+"/transmit", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("transmit status=%d, want 400; body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "can_transmit=false") {
		t.Fatalf("transmit response missing capability reason: %s", res.Body.String())
	}
}

func TestOrderWriteHTTPAdapters(t *testing.T) {
	t.Parallel()
	handler := newTestHandlerWithClient(t, routeWriteFakeClient{}).Handler()
	cookie := routeSessionCookie(t, handler)
	set := createRouteReviewSet(t, handler, cookie)
	previewRouteReviewSet(t, handler, cookie, set.ID, set.Revision, set.RowID)

	transmitReq := httptest.NewRequest(http.MethodPost, "/api/order-review-sets/"+set.ID+"/transmit", nil)
	transmitReq.AddCookie(cookie)
	transmitRes := httptest.NewRecorder()
	handler.ServeHTTP(transmitRes, transmitReq)
	if transmitRes.Code != http.StatusOK {
		t.Fatalf("transmit status=%d, want 200; body=%s", transmitRes.Code, transmitRes.Body.String())
	}
	var transmit struct {
		Rows []struct {
			RowID  string                `json:"row_id"`
			Result *rpc.OrderPlaceResult `json:"result"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(transmitRes.Body).Decode(&transmit); err != nil {
		t.Fatalf("decode transmit: %v", err)
	}
	if len(transmit.Rows) != 1 || transmit.Rows[0].RowID != set.RowID || transmit.Rows[0].Result == nil || !transmit.Rows[0].Result.Accepted {
		t.Fatalf("unexpected transmit response: %#v", transmit)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/cancel", nil)
	cancelReq.AddCookie(cookie)
	cancelRes := httptest.NewRecorder()
	handler.ServeHTTP(cancelRes, cancelReq)
	if cancelRes.Code != http.StatusOK {
		t.Fatalf("cancel status=%d, want 200; body=%s", cancelRes.Code, cancelRes.Body.String())
	}

	modPreviewBody := bytes.NewReader([]byte(`{"action":"SELL","quantity":1,"limit_price":449.5,"tif":"DAY"}`))
	modPreviewReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/preview-modify", modPreviewBody)
	modPreviewReq.AddCookie(cookie)
	modPreviewRes := httptest.NewRecorder()
	handler.ServeHTTP(modPreviewRes, modPreviewReq)
	if modPreviewRes.Code != http.StatusOK {
		t.Fatalf("preview-modify status=%d, want 200; body=%s", modPreviewRes.Code, modPreviewRes.Body.String())
	}
	var modPreview rpc.OrderPreviewResult
	if err := json.NewDecoder(modPreviewRes.Body).Decode(&modPreview); err != nil {
		t.Fatalf("decode preview-modify: %v", err)
	}
	if modPreview.Draft.Quantity != 1 || modPreview.Draft.OrderRef == "" {
		t.Fatalf("unexpected modify preview draft: %#v", modPreview.Draft)
	}

	modifyBody := bytes.NewReader([]byte(`{"preview_token":"modify-token"}`))
	modifyReq := httptest.NewRequest(http.MethodPost, "/api/orders/ord-1/modify", modifyBody)
	modifyReq.AddCookie(cookie)
	modifyRes := httptest.NewRecorder()
	handler.ServeHTTP(modifyRes, modifyReq)
	if modifyRes.Code != http.StatusOK {
		t.Fatalf("modify status=%d, want 200; body=%s", modifyRes.Code, modifyRes.Body.String())
	}
}

func TestPairingSessionAcceptsLocalPublicURLOverride(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t).Handler()
	body := bytes.NewReader([]byte(`{"public_url":"http://192.168.1.42:8765"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", body)
	req.RemoteAddr = "127.0.0.1:12345"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", res.Code, res.Body.String())
	}
	var pairing auth.PairingSession
	if err := json.NewDecoder(res.Body).Decode(&pairing); err != nil {
		t.Fatalf("decode pairing: %v", err)
	}
	if !strings.HasPrefix(pairing.URL, "http://192.168.1.42:8765/pair.html?") {
		t.Fatalf("pairing URL = %q, want LAN public URL", pairing.URL)
	}
}

func TestPairingSessionRejectsInvalidPublicURLOverride(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t).Handler()
	body := bytes.NewReader([]byte(`{"public_url":"ftp://192.168.1.42:8765"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", body)
	req.RemoteAddr = "127.0.0.1:12345"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", res.Code, res.Body.String())
	}
}

func TestPairingSessionStillRequiresLocalMac(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t).Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", bytes.NewReader([]byte(`{"public_url":"http://192.168.1.42:8765"}`)))
	req.RemoteAddr = "203.0.113.99:12345"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", res.Code, res.Body.String())
	}
}

func TestIsLocalMacAcceptsOwnInterfaceAddress(t *testing.T) {
	t.Parallel()

	got := isLocalMacWithAddrs("192.168.1.42:54321", func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.42"), Mask: net.CIDRMask(24, 32)}}, nil
	})
	if !got {
		t.Fatalf("isLocalMacWithAddrs should accept the Mac's own LAN interface address")
	}
	if isLocalMacWithAddrs("203.0.113.99:54321", func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.42"), Mask: net.CIDRMask(24, 32)}}, nil
	}) {
		t.Fatalf("isLocalMacWithAddrs accepted a non-local remote address")
	}
}

func newTestHandler(t *testing.T) *hyperserve.Server {
	t.Helper()
	return newTestHandlerWithClient(t, routeFakeClient{})
}

func newTestHandlerWithClient(t *testing.T, fakeClient daemonclient.Client) *hyperserve.Server {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.EnsureVAPID(time.Now().UTC(), func() (string, string, error) {
		return "private", "public", nil
	}); err != nil {
		t.Fatalf("EnsureVAPID: %v", err)
	}
	authMgr := auth.NewManager(store, time.Minute)
	liveSvc := live.New(fakeClient, time.Minute, time.Minute)
	liveSvc.PollOnce(t.Context())
	srv, err := hyperserve.NewServer(
		hyperserve.WithAddr("127.0.0.1:0"),
		hyperserve.WithSuppressBanner(true),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	Register(Dependencies{
		Server:    srv,
		Store:     store,
		Auth:      authMgr,
		Daemon:    fakeClient,
		Live:      liveSvc,
		Relay:     relay.Noop{PublicURL: "https://relay.example"},
		PublicURL: "https://relay.example",
		Version:   "test-version",
	})
	return srv
}

type routeReviewSetRef struct {
	ID       string
	Revision string
	RowID    string
}

func createRouteReviewSet(t *testing.T, handler http.Handler, cookie *http.Cookie) routeReviewSetRef {
	t.Helper()
	createReq := httptest.NewRequest(http.MethodPost, "/api/order-review-sets", nil)
	createReq.AddCookie(cookie)
	createRes := httptest.NewRecorder()
	handler.ServeHTTP(createRes, createReq)
	if createRes.Code != http.StatusOK {
		t.Fatalf("create status=%d, want 200; body=%s", createRes.Code, createRes.Body.String())
	}
	var set struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
		Rows     []struct {
			RowID string `json:"row_id"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(createRes.Body).Decode(&set); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if set.ID == "" || set.Revision == "" || len(set.Rows) == 0 {
		t.Fatalf("unexpected created set: %#v", set)
	}
	return routeReviewSetRef{ID: set.ID, Revision: set.Revision, RowID: set.Rows[0].RowID}
}

func previewRouteReviewSet(t *testing.T, handler http.Handler, cookie *http.Cookie, setID, revision, rowID string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"revision": revision,
		"rows": []map[string]any{{
			"row_id":   rowID,
			"included": true,
			"quantity": 2,
		}},
	})
	if err != nil {
		t.Fatalf("marshal preview: %v", err)
	}
	previewReq := httptest.NewRequest(http.MethodPost, "/api/order-review-sets/"+setID+"/preview", bytes.NewReader(body))
	previewReq.AddCookie(cookie)
	previewRes := httptest.NewRecorder()
	handler.ServeHTTP(previewRes, previewReq)
	if previewRes.Code != http.StatusOK {
		t.Fatalf("preview status=%d, want 200; body=%s", previewRes.Code, previewRes.Body.String())
	}
}

func routeSessionCookie(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	pairReq := httptest.NewRequest(http.MethodPost, "/api/pairing/sessions", bytes.NewReader([]byte("{}")))
	pairReq.RemoteAddr = "127.0.0.1:12345"
	pairRes := httptest.NewRecorder()
	handler.ServeHTTP(pairRes, pairReq)
	if pairRes.Code != http.StatusOK {
		t.Fatalf("pair status=%d, want 200; body=%s", pairRes.Code, pairRes.Body.String())
	}
	var pairing auth.PairingSession
	if err := json.NewDecoder(pairRes.Body).Decode(&pairing); err != nil {
		t.Fatalf("decode pairing: %v", err)
	}
	key := newRouteTestKey(t)
	completeBody, err := json.Marshal(auth.CompletePairingRequest{
		PairingID:    pairing.ID,
		Nonce:        pairing.Nonce,
		DeviceName:   "iPhone",
		PublicKeyJWK: routeTestJWK(t, key),
		Signature:    routeTestSignature(t, key, pairing.Nonce),
	})
	if err != nil {
		t.Fatalf("marshal complete body: %v", err)
	}
	completeReq := httptest.NewRequest(http.MethodPost, "/api/pairing/complete", bytes.NewReader(completeBody))
	completeRes := httptest.NewRecorder()
	handler.ServeHTTP(completeRes, completeReq)
	if completeRes.Code != http.StatusOK {
		t.Fatalf("complete status=%d, want 200; body=%s", completeRes.Code, completeRes.Body.String())
	}
	cookies := completeRes.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("pairing response did not set a session cookie")
	}
	return cookies[0]
}

type routeFakeClient struct{}

func (routeFakeClient) Status(context.Context) (*rpc.HealthResult, error) {
	return &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497}, nil
}

func (routeFakeClient) MarketCalendar(context.Context) (*rpc.MarketCalendarResult, error) {
	return &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}}, nil
}

func (routeFakeClient) MarketCalendarFor(_ context.Context, market string) (*rpc.MarketCalendarResult, error) {
	return &rpc.MarketCalendarResult{Market: market, Label: market, Session: rpc.MarketSession{Market: market, State: "regular", IsOpen: true}}, nil
}

func (routeFakeClient) Account(context.Context) (*rpc.AccountResult, error) {
	return &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000}, nil
}

func (routeFakeClient) Positions(context.Context) (*rpc.PositionsResult, error) {
	return &rpc.PositionsResult{}, nil
}

func (routeFakeClient) Quote(_ context.Context, contract rpc.ContractParams) (*rpc.Quote, error) {
	return &rpc.Quote{Symbol: contract.Symbol, Price: new(500.0), ChangePct: new(0.4), DataType: rpc.MarketDataLive}, nil
}

func (routeFakeClient) StreamQuote(context.Context, rpc.ContractParams, func(rpc.Frame) error) error {
	return nil
}

func (routeFakeClient) Canary(context.Context) (*rpc.CanaryResult, error) {
	return &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}}, nil
}

func (routeFakeClient) CanaryWithRegime(context.Context) (*rpc.CanaryResult, *rpc.RegimeMonitorResult, error) {
	return &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		&rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}},
		nil
}

func (routeFakeClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	return &rpc.TradingStatus{
		Enabled:         true,
		Mode:            "paper",
		Account:         "DU123",
		Endpoint:        "127.0.0.1:7497",
		ClientID:        7,
		PreviewRequired: true,
		CanPreview:      true,
		CanTransmit:     false,
		CanModify:       false,
		CanCancel:       false,
	}, nil
}

func (routeFakeClient) RiskPlan(context.Context, string, *rpc.CanaryResult) (*rpc.RiskPlanResult, error) {
	limit := 450.25
	return &rpc.RiskPlanResult{
		PlanID:                     "plan-1",
		RefreshedCanaryFingerprint: rpc.Fingerprint{Key: "fp-1"},
		SourceFingerprints:         rpc.CanarySourceFingerprints{Account: &rpc.Fingerprint{Key: "acct-1"}},
		Candidates: []rpc.RiskPlanCandidate{{
			ID:      "candidate-1",
			Status:  rpc.RiskPlanCandidatePreviewable,
			Subject: "Trim concentration",
			Reason:  "reduce single-name exposure",
			Legs: []rpc.RiskPlanCandidateLeg{{
				Action:              "SELL",
				Contract:            rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"},
				Quantity:            3,
				HeldQuantity:        10,
				PositionEffect:      rpc.OrderPositionEffectReduce,
				OrderType:           rpc.OrderTypeLMT,
				TIF:                 rpc.OrderTIFDay,
				LimitStrategy:       rpc.OrderStrategyPatientLimit,
				EstimatedLimitPrice: &limit,
				MarketValueBase:     1350.75,
			}},
		}},
	}, nil
}

func (routeFakeClient) OrderPreview(_ context.Context, params rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	limit := 0.0
	if params.LimitPrice != nil {
		limit = *params.LimitPrice
	}
	return &rpc.OrderPreviewResult{
		PreviewToken:          "redacted-test-token",
		PreviewTokenID:        "tok-1",
		PreviewTokenExpiresAt: time.Now().UTC().Add(time.Minute),
		TokenMinted:           true,
		SubmitEligible:        true,
		Executable:            true,
		Mode:                  "paper",
		Account:               "DU123",
		Endpoint:              "127.0.0.1:7497",
		ClientID:              7,
		Draft: rpc.OrderDraft{
			Action:     params.Action,
			Contract:   params.Contract,
			Quantity:   params.Quantity,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: limit,
			TIF:        rpc.OrderTIFDay,
			Strategy:   params.Strategy,
			OrderRef:   "ord-1",
		},
		WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true},
		AsOf:   time.Now().UTC(),
	}, nil
}

func (routeFakeClient) OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return nil, nil
}

func (routeFakeClient) OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	return nil, nil
}

func (routeFakeClient) OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	return nil, nil
}

func (routeFakeClient) OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	return &rpc.OrdersOpenResult{Orders: []rpc.OrderView{routeOpenOrderView()}}, nil
}

func (routeFakeClient) OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	return &rpc.OrderStatusResult{Found: true, Order: routeOpenOrderView(), AsOf: time.Now().UTC()}, nil
}

type routeWriteFakeClient struct {
	routeFakeClient
}

func (routeWriteFakeClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	return &rpc.TradingStatus{
		Enabled:         true,
		Mode:            "paper",
		Account:         "DU123",
		Endpoint:        "127.0.0.1:7497",
		ClientID:        7,
		PreviewRequired: true,
		CanPreview:      true,
		CanTransmit:     true,
		CanModify:       true,
		CanCancel:       true,
	}, nil
}

func (routeWriteFakeClient) OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return &rpc.OrderPlaceResult{
		Accepted:        true,
		Mode:            "paper",
		Account:         "DU123",
		Endpoint:        "127.0.0.1:7497",
		ClientID:        7,
		OrderRef:        "ord-1",
		PreviewTokenID:  "tok-1",
		ReservedOrderID: 42,
		Draft: rpc.OrderDraft{
			Action:     rpc.OrderActionSell,
			Contract:   rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:   3,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: 450.25,
			TIF:        rpc.OrderTIFDay,
			Strategy:   rpc.OrderStrategyPatientLimit,
			OrderRef:   "ord-1",
		},
		Status:          "submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       "sent",
		AsOf:            time.Now().UTC(),
	}, nil
}

func (routeWriteFakeClient) OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	return &rpc.OrderModifyResult{
		Accepted:        true,
		Mode:            "paper",
		Account:         "DU123",
		Endpoint:        "127.0.0.1:7497",
		ClientID:        7,
		OrderRef:        "ord-1",
		PreviewTokenID:  "modify-token",
		ReservedOrderID: 42,
		Draft: rpc.OrderDraft{
			Action:     rpc.OrderActionSell,
			Contract:   rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:   1,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: 449.50,
			TIF:        rpc.OrderTIFDay,
			Strategy:   rpc.OrderStrategyExplicitLimit,
			OrderRef:   "ord-1",
		},
		Status:          "submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       "sent",
		AsOf:            time.Now().UTC(),
	}, nil
}

func (routeWriteFakeClient) OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	return &rpc.OrderCancelResult{
		Accepted: true,
		Order: rpc.OrderView{
			OrderRef:        "ord-1",
			Symbol:          "SPY",
			Status:          "cancelled",
			LifecycleStatus: rpc.OrderLifecycleCancelled,
			SendState:       "sent",
			UpdatedAt:       time.Now().UTC(),
		},
		Status:          "cancelled",
		LifecycleStatus: rpc.OrderLifecycleCancelled,
		SendState:       "sent",
		AsOf:            time.Now().UTC(),
	}, nil
}

func routeOpenOrderView() rpc.OrderView {
	return rpc.OrderView{
		OrderRef:        "ord-1",
		PreviewTokenID:  "tok-1",
		Account:         "DU123",
		Endpoint:        "127.0.0.1:7497",
		Mode:            "paper",
		Symbol:          "SPY",
		SecType:         "STK",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        2,
		LimitPrice:      450.25,
		Status:          "submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       "sent",
		Open:            true,
		UpdatedAt:       time.Now().UTC(),
	}
}

func newRouteTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func routeTestJWK(t *testing.T, key *ecdsa.PrivateKey) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(routeLeftPad32(key.X)),
		Y:   base64.RawURLEncoding.EncodeToString(routeLeftPad32(key.Y)),
	})
	if err != nil {
		t.Fatalf("marshal jwk: %v", err)
	}
	return raw
}

func routeTestSignature(t *testing.T, key *ecdsa.PrivateKey, message string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(message))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

func routeLeftPad32(v *big.Int) []byte {
	b := v.Bytes()
	if len(b) >= 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}
