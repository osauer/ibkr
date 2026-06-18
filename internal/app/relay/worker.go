package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const DefaultWorkerURL = "https://remote.osauer.dev"

// defaultRouteTTL mirrors ROUTE_TTL_MS in the Cloudflare relay worker. The
// relay slides the route's expiry window on every authenticated connector
// connection, so reconnecting at half the TTL keeps an idle-but-connected
// route alive indefinitely without minting a new route_id.
const defaultRouteTTL = 7 * 24 * time.Hour

// minCycleEvery floors the forced reconnect interval so a misbehaving relay
// (tiny or clock-skewed expires_at) cannot make the connector hot-cycle.
const minCycleEvery = time.Minute

// errRouteExpired marks a connect attempt rejected with HTTP 410: the relay
// reaped the route (e.g. the Mac was offline past the TTL) and only a fresh
// registration — with a NEW route_id — can restore remote access.
var errRouteExpired = errors.New("remote relay route expired")

type WorkerOptions struct {
	BaseURL              string
	OriginURL            string
	Version              string
	HTTPClient           *http.Client
	ResumeRouteID        string
	ResumeConnectorToken string
	OnRoute              func(RouteRegistration) error
}

type RouteRegistration struct {
	RouteID        string
	PublicURL      string
	ConnectorURL   string
	ConnectorToken string
	ExpiresAt      time.Time
}

type Worker struct {
	baseURL    string
	originURL  string
	version    string
	httpClient *http.Client
	onRoute    func(RouteRegistration) error

	// mu guards the route fields too: register() runs again after a 410
	// while HTTP handlers concurrently read PairingURL/PublicURL/Status.
	mu           sync.RWMutex
	routeID      string
	publicURL    string
	connectorURL string
	token        string
	routeTTL     time.Duration
	cycleEvery   time.Duration
	connected    bool
	message      string
}

type registerRequest struct {
	Version        string `json:"version,omitempty"`
	RouteID        string `json:"route_id,omitempty"`
	ConnectorToken string `json:"connector_token,omitempty"`
}

type registerResponse struct {
	RouteID      string    `json:"route_id"`
	PublicURL    string    `json:"public_url"`
	ConnectorURL string    `json:"connector_url"`
	Token        string    `json:"connector_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type frame struct {
	Type    string              `json:"type"`
	ID      string              `json:"id,omitempty"`
	Method  string              `json:"method,omitempty"`
	Path    string              `json:"path,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"`
	Status  int                 `json:"status,omitempty"`
	Error   string              `json:"error,omitempty"`
}

type frameSender func(context.Context, frame) error

func NewWorker(ctx context.Context, opts WorkerOptions) (*Worker, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultWorkerURL
	}
	originURL := strings.TrimRight(strings.TrimSpace(opts.OriginURL), "/")
	if originURL == "" {
		return nil, errors.New("relay origin URL required")
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = dualStackHTTPClient()
	}
	w := &Worker{
		baseURL:    baseURL,
		originURL:  originURL,
		version:    strings.TrimSpace(opts.Version),
		httpClient: httpClient,
		onRoute:    opts.OnRoute,
	}
	resumeRouteID := strings.TrimSpace(opts.ResumeRouteID)
	resumeToken := strings.TrimSpace(opts.ResumeConnectorToken)
	switch {
	case resumeRouteID != "" && resumeToken != "":
		if err := w.register(ctx, registerRequest{
			Version:        w.version,
			RouteID:        resumeRouteID,
			ConnectorToken: resumeToken,
		}); err != nil {
			if !errors.Is(err, errRouteExpired) {
				return nil, err
			}
			if err := w.register(ctx, registerRequest{Version: w.version}); err != nil {
				return nil, err
			}
		}
	case resumeRouteID != "" || resumeToken != "":
		return nil, errors.New("relay resume route requires both route id and connector token")
	default:
		if err := w.register(ctx, registerRequest{Version: w.version}); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *Worker) register(ctx context.Context, rrq registerRequest) error {
	body, err := json.Marshal(rrq)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+"/api/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("register remote relay at %s: %w", w.baseURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		if res.StatusCode == http.StatusGone {
			return fmt.Errorf("register remote relay: %s: %w", res.Status, errRouteExpired)
		}
		return fmt.Errorf("register remote relay: %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
	var rr registerResponse
	if err := json.NewDecoder(res.Body).Decode(&rr); err != nil {
		return err
	}
	if rr.RouteID == "" || rr.PublicURL == "" || rr.ConnectorURL == "" || rr.Token == "" {
		return fmt.Errorf("register remote relay: incomplete response %#v", rr)
	}
	ttl := defaultRouteTTL
	cycle := defaultRouteTTL / 2
	if observedTTL := time.Until(rr.ExpiresAt); !rr.ExpiresAt.IsZero() && observedTTL > 0 {
		ttl = observedTTL
		cycle = ttl / 2
	}
	cycle = max(cycle, minCycleEvery)
	w.mu.Lock()
	w.routeID = rr.RouteID
	w.publicURL = strings.TrimRight(rr.PublicURL, "/")
	w.connectorURL = rr.ConnectorURL
	w.token = rr.Token
	w.routeTTL = ttl
	w.cycleEvery = cycle
	w.connected = false
	w.message = "registered remote relay route"
	w.mu.Unlock()
	if err := w.persistRoute(RouteRegistration{
		RouteID:        rr.RouteID,
		PublicURL:      strings.TrimRight(rr.PublicURL, "/"),
		ConnectorURL:   rr.ConnectorURL,
		ConnectorToken: rr.Token,
		ExpiresAt:      rr.ExpiresAt,
	}); err != nil {
		return err
	}
	return nil
}

func (w *Worker) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		// Force the connection to cycle at half the route TTL: the relay
		// slides the route's expiry window on every authenticated connector
		// connection, so a long-lived quiet connection must reconnect
		// periodically or the route ages into the 410 reaper.
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		cycleTimer := time.AfterFunc(w.cycleDuration(), cancelAttempt)
		err := w.connectOnce(attemptCtx)
		cycled := !cycleTimer.Stop() // Stop() == false: the cycle deadline fired.
		cancelAttempt()
		if ctx.Err() != nil {
			return // parent cancelled: real shutdown, not a forced cycle
		}
		if cycled {
			backoff = time.Second
			continue // reconnect promptly so the relay-side TTL slides
		}
		if errors.Is(err, errRouteExpired) {
			regErr := w.register(ctx, registerRequest{Version: w.version})
			if regErr == nil {
				w.mu.RLock()
				routeID := w.routeID
				w.mu.RUnlock()
				log.Printf("ibkr app relay: route expired at the relay and was re-registered as %s; previously paired devices must re-pair (their old remote route is gone)", routeID)
				backoff = time.Second
				continue
			}
			err = fmt.Errorf("relay route expired; re-register failed: %w", regErr)
		}
		if err == nil {
			backoff = time.Second
			continue
		}
		w.setStatus(false, err.Error())
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (w *Worker) cycleDuration() time.Duration {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.cycleEvery <= 0 {
		return defaultRouteTTL / 2
	}
	return w.cycleEvery
}

func (w *Worker) connectOnce(ctx context.Context) error {
	w.mu.RLock()
	connectorURL, token := w.connectorURL, w.token
	w.mu.RUnlock()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, res, err := websocket.Dial(ctx, connectorURL, &websocket.DialOptions{
		HTTPClient: w.httpClient,
		HTTPHeader: header,
	})
	if err != nil {
		if res != nil && res.StatusCode == http.StatusGone {
			return fmt.Errorf("connect remote relay: %s: %w", res.Status, errRouteExpired)
		}
		if res != nil {
			return fmt.Errorf("connect remote relay: %s: %w", res.Status, err)
		}
		return fmt.Errorf("connect remote relay: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "ibkr app relay reconnect")
	w.setStatus(true, "connected")
	if err := w.persistRouteExtension(time.Now().UTC()); err != nil {
		log.Printf("ibkr app relay: persist route extension: %v", err)
	}

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	send := func(ctx context.Context, f frame) error {
		data, err := json.Marshal(f)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		writeCtx, writeCancel := context.WithTimeout(ctx, 30*time.Second)
		defer writeCancel()
		return conn.Write(writeCtx, websocket.MessageText, data)
	}

	var wg sync.WaitGroup
	for connCtx.Err() == nil {
		typ, data, err := conn.Read(connCtx)
		if err != nil {
			cancel()
			wg.Wait()
			w.setStatus(false, "disconnected")
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type != "request" || f.ID == "" {
			continue
		}
		wg.Go(func() {
			_ = w.serveRequest(connCtx, f, send)
		})
	}
	wg.Wait()
	return connCtx.Err()
}

func (w *Worker) serveRequest(ctx context.Context, reqFrame frame, send frameSender) error {
	if !forwardableAppPath(reqFrame.Path) {
		return send(ctx, frame{
			Type:   "response_error",
			ID:     reqFrame.ID,
			Status: http.StatusForbidden,
			Error:  "route is local-control only",
		})
	}
	body, err := base64.StdEncoding.DecodeString(reqFrame.Body)
	if err != nil {
		return send(ctx, frame{Type: "response_error", ID: reqFrame.ID, Status: http.StatusBadRequest, Error: "invalid request body"})
	}
	method := strings.TrimSpace(reqFrame.Method)
	if method == "" {
		method = http.MethodGet
	}
	target := w.originURL + reqFrame.Path
	localReq, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return send(ctx, frame{Type: "response_error", ID: reqFrame.ID, Status: http.StatusBadRequest, Error: err.Error()})
	}
	copyForwardHeaders(localReq.Header, reqFrame.Headers)
	localReq.Header.Set("X-Forwarded-Proto", "https")
	if u, err := url.Parse(w.PublicURL()); err == nil && u.Host != "" {
		localReq.Header.Set("X-Forwarded-Host", u.Host)
		localReq.Host = u.Host
	}

	res, err := w.httpClient.Do(localReq)
	if err != nil {
		return send(ctx, frame{Type: "response_error", ID: reqFrame.ID, Status: http.StatusBadGateway, Error: err.Error()})
	}
	defer res.Body.Close()
	if err := send(ctx, frame{
		Type:    "response_start",
		ID:      reqFrame.ID,
		Status:  res.StatusCode,
		Headers: responseHeaders(res.Header),
	}); err != nil {
		return err
	}
	buf := make([]byte, 16*1024)
	for {
		n, readErr := res.Body.Read(buf)
		if n > 0 {
			if err := send(ctx, frame{
				Type: "response_chunk",
				ID:   reqFrame.ID,
				Body: base64.StdEncoding.EncodeToString(buf[:n]),
			}); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return send(ctx, frame{Type: "response_end", ID: reqFrame.ID})
		}
		if readErr != nil {
			return send(ctx, frame{Type: "response_error", ID: reqFrame.ID, Status: http.StatusBadGateway, Error: readErr.Error()})
		}
	}
}

func (w *Worker) Status() Status {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return Status{
		Mode:      "cloudflare-worker",
		URL:       w.publicURL,
		Connected: w.connected,
		Message:   w.message,
	}
}

func (w *Worker) PairingURL(raw string) string {
	if w == nil {
		return raw
	}
	w.mu.RLock()
	routeID := w.routeID
	w.mu.RUnlock()
	if routeID == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("remote") == "" {
		q.Set("remote", routeID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (w *Worker) PublicURL() string {
	if w == nil {
		return ""
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.publicURL
}

func (w *Worker) setStatus(connected bool, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.connected = connected
	w.message = message
}

func (w *Worker) persistRoute(reg RouteRegistration) error {
	if w.onRoute == nil {
		return nil
	}
	return w.onRoute(reg)
}

func (w *Worker) persistRouteExtension(now time.Time) error {
	if w.onRoute == nil {
		return nil
	}
	w.mu.RLock()
	reg := RouteRegistration{
		RouteID:        w.routeID,
		PublicURL:      w.publicURL,
		ConnectorURL:   w.connectorURL,
		ConnectorToken: w.token,
		ExpiresAt:      now.Add(w.routeTTLDuration()),
	}
	w.mu.RUnlock()
	if reg.RouteID == "" || reg.ConnectorToken == "" {
		return nil
	}
	return w.onRoute(reg)
}

func (w *Worker) routeTTLDuration() time.Duration {
	if w.routeTTL <= 0 {
		return defaultRouteTTL
	}
	return w.routeTTL
}

func forwardableAppPath(raw string) bool {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}
	p := u.Path
	if p == "/api/pairing/sessions" {
		return false
	}
	if p == "/" || p == "/pair.html" || strings.HasPrefix(p, "/api/") {
		return true
	}
	switch p {
	case "/manifest.webmanifest", "/service-worker.js", "/app.js", "/styles.css",
		"/icon-192.png", "/icon-512.png", "/favicon-16.png", "/favicon-32.png", "/favicon-64.png", "/favicon.ico":
		return true
	default:
		return false
	}
}

func copyForwardHeaders(dst http.Header, src map[string][]string) {
	for k, values := range src {
		if skipForwardHeader(k) {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func responseHeaders(src http.Header) map[string][]string {
	out := make(map[string][]string, len(src))
	for k, values := range src {
		if skipForwardHeader(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		out[k] = append([]string(nil), values...)
	}
	return out
}

func skipForwardHeader(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func dualStackHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:       30 * time.Second,
		KeepAlive:     30 * time.Second,
		FallbackDelay: 300 * time.Millisecond,
		Resolver: &net.Resolver{
			PreferGo: true,
		},
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}
