package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
