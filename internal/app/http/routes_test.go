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
	"github.com/osauer/ibkr/internal/app/live"
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
	liveSvc := live.New(routeFakeClient{}, time.Minute, time.Minute)
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
		Live:      liveSvc,
		Relay:     relay.Noop{PublicURL: "https://relay.example"},
		PublicURL: "https://relay.example",
		Version:   "test-version",
	})
	return srv
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

func (routeFakeClient) Account(context.Context) (*rpc.AccountResult, error) {
	return &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000}, nil
}

func (routeFakeClient) Positions(context.Context) (*rpc.PositionsResult, error) {
	return &rpc.PositionsResult{}, nil
}

func (routeFakeClient) Canary(context.Context) (*rpc.CanaryResult, error) {
	return &rpc.CanaryResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}}, nil
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
