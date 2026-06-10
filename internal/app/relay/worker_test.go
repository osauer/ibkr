package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWorkerPairingURLAddsRemoteRoute(t *testing.T) {
	t.Parallel()

	w := &Worker{routeID: "r_test"}
	got := w.PairingURL("https://remote.osauer.dev/pair.html?pair=p1&nonce=n1")
	if !strings.Contains(got, "remote=r_test") {
		t.Fatalf("PairingURL = %q, want remote route", got)
	}
	if !strings.Contains(got, "pair=p1") || !strings.Contains(got, "nonce=n1") {
		t.Fatalf("PairingURL = %q, lost pairing parameters", got)
	}
}

func TestForwardableAppPathBlocksPairingSessionCreation(t *testing.T) {
	t.Parallel()

	if forwardableAppPath("/api/pairing/sessions") {
		t.Fatalf("pairing session creation should not be forwarded through remote relay")
	}
	for _, path := range []string{"/", "/pair.html?remote=r1&pair=p&nonce=n", "/api/bootstrap", "/api/events", "/app.js?v=1"} {
		if !forwardableAppPath(path) {
			t.Fatalf("path %q should be forwardable", path)
		}
	}
}

func TestWorkerServeRequestForwardsAllowedPath(t *testing.T) {
	t.Parallel()

	var gotProto, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	worker := &Worker{
		originURL:  srv.URL,
		publicURL:  "https://remote.osauer.dev",
		httpClient: srv.Client(),
	}
	var frames []frame
	err := worker.serveRequest(context.Background(), frame{
		Type:   "request",
		ID:     "req-1",
		Method: http.MethodPost,
		Path:   "/api/settings",
		Body:   base64.StdEncoding.EncodeToString([]byte(`{"x":1}`)),
	}, func(_ context.Context, f frame) error {
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatalf("serveRequest: %v", err)
	}
	if gotProto != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want https", gotProto)
	}
	if gotBody != `{"x":1}` {
		t.Fatalf("body = %q, want forwarded JSON", gotBody)
	}
	if len(frames) != 3 {
		t.Fatalf("frames len = %d, want 3: %#v", len(frames), frames)
	}
	if frames[0].Type != "response_start" || frames[0].Status != http.StatusOK {
		t.Fatalf("start frame = %#v", frames[0])
	}
	var payload map[string]bool
	chunk, err := base64.StdEncoding.DecodeString(frames[1].Body)
	if err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		t.Fatalf("decode response chunk: %v", err)
	}
	if !payload["ok"] || frames[2].Type != "response_end" {
		t.Fatalf("unexpected response frames: %#v", frames)
	}
}

// fakeRegisterJSON mimics the relay's /api/register response, pointing the
// connector websocket URL back at the fake relay server.
func fakeRegisterJSON(baseURL, routeID string, expiresAt time.Time) []byte {
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http")
	return fmt.Appendf(nil,
		`{"route_id":%q,"public_url":%q,"connector_url":%q,"connector_token":%q,"expires_at":%q}`,
		routeID, baseURL, wsURL+"/api/connect?route_id="+routeID, "tok-"+routeID,
		expiresAt.UTC().Format(time.RFC3339))
}

// acceptAndHold upgrades the connection, calls onAccept, then holds the
// connection open until the connector closes it (forced cycle or shutdown).
func acceptAndHold(wr http.ResponseWriter, r *http.Request, onAccept func()) {
	conn, err := websocket.Accept(wr, r, nil)
	if err != nil {
		return
	}
	if onAccept != nil {
		onAccept()
	}
	_, _, _ = conn.Read(r.Context())
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func TestWorkerRunCyclesConnectionToSlideRouteTTL(t *testing.T) {
	t.Parallel()

	var accepts atomic.Int64
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, _ *http.Request) {
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, "r_cycle", time.Now().Add(defaultRouteTTL)))
	})
	mux.HandleFunc("/api/connect", func(wr http.ResponseWriter, r *http.Request) {
		acceptAndHold(wr, r, func() { accepts.Add(1) })
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewWorker(ctx, WorkerOptions{BaseURL: srv.URL, OriginURL: "http://127.0.0.1:1", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.mu.Lock()
	w.cycleEvery = 50 * time.Millisecond // bypass the prod minCycleEvery floor
	w.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	// Three accepts within 2s prove forced cycles reconnect promptly with
	// reset backoff (the backoff path would only manage two: 0s, 1s, 3s).
	deadline := time.Now().Add(2 * time.Second)
	for accepts.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("relay accepted %d connector connections, want >= 3 (forced cycle must reconnect without backoff)", accepts.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after parent cancel")
	}
}

func TestWorkerRunReRegistersOnGoneRoute(t *testing.T) {
	t.Parallel()

	var (
		mu        sync.Mutex
		registers int
		goneSent  bool
	)
	connected := make(chan struct{}, 4)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		registers++
		routeID := fmt.Sprintf("r_%d", registers)
		mu.Unlock()
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, routeID, time.Now().Add(defaultRouteTTL)))
	})
	mux.HandleFunc("/api/connect", func(wr http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := !goneSent
		goneSent = true
		mu.Unlock()
		if first {
			// Simulate a reaped route: the Mac was offline past the TTL.
			wr.Header().Set("Content-Type", "application/json")
			wr.WriteHeader(http.StatusGone)
			_, _ = wr.Write([]byte(`{"error":"route expired"}`))
			return
		}
		acceptAndHold(wr, r, func() { connected <- struct{}{} })
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewWorker(ctx, WorkerOptions{BaseURL: srv.URL, OriginURL: "http://127.0.0.1:1", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	if got := w.PairingURL("https://relay.example/pair.html?pair=p"); !strings.Contains(got, "remote=r_1") {
		t.Fatalf("initial PairingURL = %q, want remote=r_1", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatalf("connector did not reconnect after 410 re-register")
	}
	mu.Lock()
	gotRegisters := registers
	mu.Unlock()
	if gotRegisters != 2 {
		t.Fatalf("registers = %d, want 2 (one at construction, one after 410)", gotRegisters)
	}
	if got := w.PairingURL("https://relay.example/pair.html?pair=p"); !strings.Contains(got, "remote=r_2") {
		t.Fatalf("PairingURL after re-register = %q, want remote=r_2 (new pairings must use the fresh route)", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after parent cancel")
	}
}

func TestWorkerRunReturnsOnParentCancel(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/register", func(wr http.ResponseWriter, _ *http.Request) {
		wr.Header().Set("Content-Type", "application/json")
		_, _ = wr.Write(fakeRegisterJSON(srv.URL, "r_stop", time.Now().Add(defaultRouteTTL)))
	})
	mux.HandleFunc("/api/connect", func(wr http.ResponseWriter, r *http.Request) {
		acceptAndHold(wr, r, nil)
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewWorker(ctx, WorkerOptions{BaseURL: srv.URL, OriginURL: "http://127.0.0.1:1", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !w.Status().Connected {
		if time.Now().After(deadline) {
			t.Fatalf("connector never connected: %#v", w.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after parent cancel")
	}
	if w.Status().Connected {
		t.Fatalf("status still connected after shutdown: %#v", w.Status())
	}
}
