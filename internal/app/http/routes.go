package apphttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	nethttp "net/http"
	"net/url"
	"strings"
	"time"

	hyperserve "github.com/osauer/hyperserve/pkg/server"

	"github.com/osauer/ibkr/v2/internal/app/auth"
	"github.com/osauer/ibkr/v2/internal/app/daemonclient"
	"github.com/osauer/ibkr/v2/internal/app/live"
	"github.com/osauer/ibkr/v2/internal/app/relay"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
	appweb "github.com/osauer/ibkr/v2/web/app"
)

type Dependencies struct {
	Server    *hyperserve.Server
	Store     *state.Store
	Auth      *auth.Manager
	Daemon    daemonclient.Client
	Live      *live.Service
	Relay     relay.Client
	PublicURL string
	Version   string
}

type handler struct {
	deps Dependencies
	web  nethttp.Handler
}

type contextKey string

const sessionKey contextKey = "ibkr-app-session"

func Register(deps Dependencies) {
	h := &handler{deps: deps}
	sub, err := fs.Sub(appweb.Files, ".")
	if err != nil {
		panic(err)
	}
	h.web = nethttp.FileServer(nethttp.FS(sub))

	srv := deps.Server
	srv.GET("/", h.serveIndex)
	srv.GET("/pair.html", h.serveIndex)
	srv.GET("/manifest.webmanifest", h.serveStatic)
	// Imported modules do not inherit app.js's query string. All embedded JS
	// therefore uses serveStatic's no-cache revalidation; new embedded bytes
	// become visible after reinstalling, restarting the app, and reloading.
	for _, name := range appweb.EmbeddedJavaScriptFileNames() {
		srv.GET("/"+name, h.serveStatic)
	}
	srv.GET("/styles.css", h.serveStatic)
	srv.GET("/icon-192.png", h.serveStatic)
	srv.GET("/icon-512.png", h.serveStatic)
	srv.GET("/favicon-16.png", h.serveStatic)
	srv.GET("/favicon-32.png", h.serveStatic)
	srv.GET("/favicon-64.png", h.serveStatic)
	srv.GET("/favicon.ico", h.serveIcon)

	srv.POST("/api/pairing/sessions", h.handleStartPairing)
	srv.GET("/api/devices", h.handleDevicesList)
	srv.POST("/api/devices/prune", h.handleDevicesPrune)
	srv.POST("/api/pairing/complete", h.handleCompletePairing)
	srv.POST("/api/auth/challenge", h.handleAuthChallenge)
	srv.POST("/api/auth/session", h.handleAuthSession)

	srv.GET("/api/bootstrap", h.requireAuth(h.handleBootstrap))
	srv.GET("/api/snapshot", h.requireAuth(h.handleSnapshot))
	srv.GET("/api/settings", h.requireAuth(h.handleGetSettings))
	srv.PATCH("/api/settings", h.requireAuth(h.handlePatchSettings))
	srv.GET("/api/market-calendar", h.requireAuth(h.handleMarketCalendar))
	srv.GET("/api/events", h.requireAuth(h.handleEvents))
	srv.GET("/api/alerts/settings", h.requireAuth(h.handleGetAlertSettings))
	srv.PUT("/api/alerts/settings", h.requireAuth(h.handlePutAlertSettings))
	srv.GET("/api/alerts", h.requireAuth(h.handleAlerts))
	srv.DELETE("/api/alerts", h.requireAuth(h.handleClearAlerts))
	srv.GET("/api/orders/open", h.requireAuth(h.handleOrdersOpen))
	srv.GET("/api/orders/{id}", h.requireAuth(h.handleOrderStatus))
	srv.POST("/api/orders/{id}/cancel", h.requireAuth(h.handleOrderCancel))
	srv.POST("/api/orders/{id}/preview-modify", h.requireAuth(h.handleOrderPreviewModify))
	srv.POST("/api/orders/{id}/modify", h.requireAuth(h.handleOrderModify))
	srv.GET("/api/purge/status", h.requireAuth(h.handlePurgeStatus))
	srv.POST("/api/purge/execute", h.requireAuth(h.handlePurgeExecute))
	srv.POST("/api/purge/restore/preview", h.requireAuth(h.handlePurgeRestorePreview))
	srv.POST("/api/purge/restore/execute", h.requireAuth(h.handlePurgeRestoreExecute))
	srv.GET("/api/proposals", h.requireAuth(h.handleProposalsSnapshot))
	srv.POST("/api/proposals/refresh", h.requireAuth(h.handleProposalsRefresh))
	srv.POST("/api/proposals/preview", h.requireAuth(h.handleProposalsPreview))
	srv.POST("/api/proposals/submit", h.requireAuth(h.handleProposalsSubmit))
	srv.POST("/api/proposals/reduce/preview", h.requireAuth(h.handleProposalsReducePreview))
	srv.POST("/api/proposals/reduce/submit", h.requireAuth(h.handleProposalsReduceSubmit))
	srv.POST("/api/proposals/reduce-portfolio/preview", h.requireAuth(h.handleProposalsReducePortfolioPreview))
	srv.POST("/api/proposals/reduce-portfolio/submit", h.requireAuth(h.handleProposalsReducePortfolioSubmit))
	srv.POST("/api/proposals/ignore", h.requireAuth(h.handleProposalsIgnore))
	srv.GET("/api/opportunities", h.requireAuth(h.handleOpportunitiesSnapshot))
	srv.POST("/api/opportunities/refresh", h.requireAuth(h.handleOpportunitiesRefresh))
	srv.POST("/api/opportunities/preview-exercise", h.requireAuth(h.handleOpportunitiesPreviewExercise))
	srv.POST("/api/opportunities/exercise", h.requireAuth(h.handleOpportunitiesSubmitExercise))
	srv.POST("/api/opportunities/ignore", h.requireAuth(h.handleOpportunitiesIgnore))
	srv.POST("/api/brief/seen", h.requireAuth(h.handleBriefSeen))
	srv.POST("/api/recon/signoff", h.requireAuth(h.handleReconcileSignoff))
	srv.POST("/api/push/subscribe", h.requireAuth(h.handlePushSubscribe))
	srv.DELETE("/api/push/{id}", h.requireAuth(h.handlePushDelete))
}

func (h *handler) serveIndex(w nethttp.ResponseWriter, r *nethttp.Request) {
	// GET / is a subtree pattern in net/http. Keep unknown JavaScript paths
	// from falling through to index.html; only exact embedded module routes
	// registered above may return JavaScript.
	if strings.HasSuffix(r.URL.Path, ".js") {
		nethttp.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	nethttp.ServeFileFS(w, r, appweb.Files, "index.html")
}

func (h *handler) serveStatic(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	h.web.ServeHTTP(w, r)
}

func (h *handler) serveIcon(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Content-Type", "image/png")
	nethttp.ServeFileFS(w, r, appweb.Files, "favicon-32.png")
}

func (h *handler) handleStartPairing(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !isLocalMac(r.RemoteAddr) {
		writeError(w, nethttp.StatusForbidden, "pairing sessions can only be created from the local Mac")
		return
	}
	publicURL := h.deps.PublicURL
	var req struct {
		PublicURL string `json:"public_url"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, nethttp.StatusBadRequest, err.Error())
			return
		}
	}
	explicitPublicURL := strings.TrimSpace(req.PublicURL) != ""
	if explicitPublicURL {
		clean, err := cleanPairingPublicURL(req.PublicURL)
		if err != nil {
			writeError(w, nethttp.StatusBadRequest, err.Error())
			return
		}
		publicURL = clean
	}
	session, err := h.deps.Auth.StartPairing(publicURL)
	if err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	if !explicitPublicURL {
		session.URL = h.deps.Relay.PairingURL(session.URL)
	}
	writeJSON(w, session)
}

type deviceSummary struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	CreatedAt       time.Time `json:"created_at"`
	LastSeenAt      time.Time `json:"last_seen_at,omitzero"`
	HasKey          bool      `json:"has_key"`
	HasSecret       bool      `json:"has_secret"`
	CookieCredCount int       `json:"cookie_credentials"`
}

// handleDevicesList and handleDevicesPrune are local-Mac management
// surfaces, like pairing-session creation: the relay refuses to forward
// them (forwardableAppPath), because relay-forwarded requests reach this
// process from 127.0.0.1 and would otherwise pass the local gate.
func (h *handler) handleDevicesList(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !isLocalMac(r.RemoteAddr) {
		writeError(w, nethttp.StatusForbidden, "device management is local-Mac only")
		return
	}
	grants := h.deps.Store.Devices()
	devices := make([]deviceSummary, 0, len(grants))
	for _, d := range grants {
		devices = append(devices, deviceSummary{
			ID:              d.ID,
			Name:            d.Name,
			CreatedAt:       d.CreatedAt,
			LastSeenAt:      d.LastSeenAt,
			HasKey:          strings.TrimSpace(d.PublicKeyJWK) != "",
			HasSecret:       strings.TrimSpace(d.DeviceSecretHash) != "",
			CookieCredCount: len(d.DeviceCookieHashes),
		})
	}
	writeJSON(w, map[string]any{"devices": devices, "total": len(devices)})
}

func (h *handler) handleDevicesPrune(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !isLocalMac(r.RemoteAddr) {
		writeError(w, nethttp.StatusForbidden, "device management is local-Mac only")
		return
	}
	var req struct {
		KeepDays int `json:"keep_days"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	if req.KeepDays < 1 {
		writeError(w, nethttp.StatusBadRequest, "keep_days must be at least 1")
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -req.KeepDays)
	removed, err := h.deps.Store.PruneDevices(cutoff)
	if err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	kept := len(h.deps.Store.Devices())
	log.Printf("ibkr app auth: pruned %d device grants older than %d days (%d kept)", removed, req.KeepDays, kept)
	writeJSON(w, map[string]any{"removed": removed, "kept": kept, "keep_days": req.KeepDays})
}

func cleanPairingPublicURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("public_url must use http or https")
	}
	if strings.TrimSpace(u.Host) == "" {
		return "", errors.New("public_url must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("public_url must be a base URL without query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (h *handler) handleCompletePairing(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req auth.CompletePairingRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	res, err := h.deps.Auth.CompletePairing(req)
	if err != nil {
		log.Printf("ibkr app auth: pairing rejected: %v", err)
		writeError(w, nethttp.StatusUnauthorized, err.Error())
		return
	}
	setSessionCookie(w, r, res.Token, res.ExpiresAt)
	if cookie, err := h.deps.Auth.IssueDeviceCookie(res.DeviceID); err == nil {
		setDeviceCookie(w, r, cookie)
	} else {
		log.Printf("ibkr app auth: issue device cookie for %s: %v", res.DeviceID, err)
	}
	log.Printf("ibkr app auth: paired device %s", res.DeviceID)
	writeJSON(w, res)
}

func (h *handler) handleAuthChallenge(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	ch, err := h.deps.Auth.StartChallenge(req.DeviceID)
	if err != nil {
		log.Printf("ibkr app auth: challenge rejected for device %s: %v", req.DeviceID, err)
		writeError(w, nethttp.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, ch)
}

func (h *handler) handleAuthSession(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req struct {
		DeviceID     string `json:"device_id"`
		Challenge    string `json:"challenge"`
		Signature    string `json:"signature"`
		DeviceSecret string `json:"device_secret"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	sess, err := h.deps.Auth.CompleteChallenge(req.DeviceID, req.Challenge, req.Signature, req.DeviceSecret)
	if err != nil {
		log.Printf("ibkr app auth: session rejected for device %s: %v", req.DeviceID, err)
		writeError(w, nethttp.StatusUnauthorized, err.Error())
		return
	}
	setSessionCookie(w, r, sess.Token, sess.ExpiresAt)
	// Re-provision the device cookie on every key/secret login: a client
	// that lost its cookie gets a fresh one, and because grants keep a
	// capped list of valid cookie hashes, issuing to this twin never
	// invalidates the copy Safari or the installed home-screen app holds.
	if cookie, err := h.deps.Auth.IssueDeviceCookie(sess.DeviceID); err == nil {
		setDeviceCookie(w, r, cookie)
	} else {
		log.Printf("ibkr app auth: issue device cookie for %s: %v", sess.DeviceID, err)
	}
	log.Printf("ibkr app auth: device login for %s", sess.DeviceID)
	writeJSON(w, sess)
}

func (h *handler) handleBootstrap(w nethttp.ResponseWriter, r *nethttp.Request) {
	vapid, _ := h.deps.Store.VAPID()
	writeJSON(w, map[string]any{
		"version":          h.deps.Version,
		"public_url":       h.deps.PublicURL,
		"snapshot":         h.deps.Live.Snapshot(),
		"settings":         h.settingsSnapshot(r.Context()),
		"alert_settings":   h.deps.Store.AlertSettings(),
		"alerts":           h.deps.Store.AlertHistory(20),
		"last_push":        h.deps.Store.LastPush(),
		"relay":            h.deps.Relay.Status(),
		"vapid_public_key": vapid.PublicKey,
		"auth":             h.authStatus(r),
	})
}

func (h *handler) settingsSnapshot(ctx context.Context) *rpc.PlatformSettings {
	settings, err := h.deps.Daemon.Settings(ctx)
	if err != nil {
		snap := h.deps.Live.Snapshot()
		return snap.Settings
	}
	return settings
}

func (h *handler) handleGetSettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	settings, err := h.deps.Daemon.Settings(r.Context())
	if err != nil {
		writeDaemonSettingsError(w, err)
		return
	}
	writeJSON(w, settings)
}

func (h *handler) handlePatchSettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	defer r.Body.Close()
	var raw json.RawMessage
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	settings, err := h.deps.Daemon.UpdateSettings(r.Context(), raw)
	if err != nil {
		writeDaemonSettingsError(w, err)
		return
	}
	writeJSON(w, settings)
}

func writeDaemonSettingsError(w nethttp.ResponseWriter, err error) {
	var rpcErr *rpc.Error
	if errors.As(err, &rpcErr) && rpcErr.Code == rpc.CodeBadRequest {
		writeError(w, nethttp.StatusBadRequest, rpcErr.Message)
		return
	}
	writeError(w, nethttp.StatusBadGateway, err.Error())
}

func (h *handler) handleSnapshot(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.deps.Live.Snapshot())
}

func (h *handler) handleMarketCalendar(w nethttp.ResponseWriter, r *nethttp.Request) {
	market := strings.TrimSpace(r.URL.Query().Get("market"))
	if market == "" {
		market = "us"
	}
	calendar, err := h.deps.Daemon.MarketCalendarFor(r.Context(), market)
	if err != nil {
		writeError(w, nethttp.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, calendar)
}

func (h *handler) handleEvents(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(nethttp.Flusher)
	if !ok {
		writeError(w, nethttp.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, release := h.deps.Live.Subscribe()
	defer release()
	msg := hyperserve.SSEMessage{Event: "snapshot", Data: h.deps.Live.Snapshot()}
	fmt.Fprint(w, msg.String())
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			msg := hyperserve.SSEMessage{Event: "heartbeat", Data: map[string]any{"at": time.Now().UTC()}}
			fmt.Fprint(w, msg.String())
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			msg := hyperserve.SSEMessage{Event: ev.Type, Data: ev.Data}
			fmt.Fprint(w, msg.String())
			flusher.Flush()
		}
	}
}

func (h *handler) handleGetAlertSettings(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.deps.Store.AlertSettings())
}

func (h *handler) handlePutAlertSettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req state.AlertSettings
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	if err := h.deps.Store.SetAlertMode(req.Mode); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, h.deps.Store.AlertSettings())
}

func (h *handler) handleAlerts(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.deps.Store.AlertHistory(50))
}

func (h *handler) handleClearAlerts(w nethttp.ResponseWriter, r *nethttp.Request) {
	cleared := len(h.deps.Store.AlertHistory(0))
	if err := h.deps.Store.ClearAlertHistory(); err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	sess, _ := h.session(r)
	log.Printf("ibkr app alerts.clear device_id=%s cleared=%d", sess.DeviceID, cleared)
	writeJSON(w, map[string]any{"ok": true, "cleared": cleared})
}

func (h *handler) handlePushSubscribe(w nethttp.ResponseWriter, r *nethttp.Request) {
	sess, _ := h.session(r)
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	sub := state.PushSubscription{
		ID:         fmt.Sprintf("%d", time.Now().UnixNano()),
		DeviceID:   sess.DeviceID,
		Endpoint:   req.Endpoint,
		P256DH:     req.Keys.P256DH,
		Auth:       req.Keys.Auth,
		CreatedAt:  time.Now().UTC(),
		LastSeenAt: time.Now().UTC(),
	}
	if err := h.deps.Store.AddPushSubscription(sub); err != nil {
		writeError(w, nethttp.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, sub)
}

func (h *handler) handlePushDelete(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.deps.Store.RemovePushSubscription(r.PathValue("id")); err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (h *handler) requireAuth(next nethttp.HandlerFunc) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		token := bearerToken(r)
		if token == "" {
			if c, err := r.Cookie("ibkr_app_session"); err == nil {
				token = c.Value
			}
		}
		sess, ok := h.deps.Auth.Authenticate(token)
		if !ok {
			sess, ok = h.deviceCookieSession(w, r)
		}
		if !ok {
			writeError(w, nethttp.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	}
}

// deviceCookieSession mints a session from the long-lived device cookie.
// This is the continuity path for clients whose script storage does not
// survive — the iOS home-screen web app inherits Safari's cookies but not
// its localStorage/IndexedDB, so the key/secret re-login can never run
// there. Every outcome is logged: a silent 401 on the phone is otherwise
// undiagnosable from the Mac.
func (h *handler) deviceCookieSession(w nethttp.ResponseWriter, r *nethttp.Request) (auth.Session, bool) {
	c, err := r.Cookie(deviceCookieName)
	if err != nil || strings.TrimSpace(c.Value) == "" {
		return auth.Session{}, false
	}
	sess, err := h.deps.Auth.AuthenticateDeviceCookie(c.Value)
	if err != nil {
		log.Printf("ibkr app auth: device cookie rejected (%s %s): %v", r.Method, r.URL.Path, err)
		return auth.Session{}, false
	}
	log.Printf("ibkr app auth: session minted from device cookie for %s (%s %s)", sess.DeviceID, r.Method, r.URL.Path)
	setSessionCookie(w, r, sess.Token, sess.ExpiresAt)
	// Slide the cookie's clock without rotating its value (twin jars).
	setDeviceCookie(w, r, c.Value)
	return sess, true
}

func (h *handler) session(r *nethttp.Request) (auth.Session, bool) {
	v := r.Context().Value(sessionKey)
	sess, ok := v.(auth.Session)
	return sess, ok
}

func (h *handler) authStatus(r *nethttp.Request) map[string]any {
	sess, ok := h.session(r)
	if !ok {
		return map[string]any{"authenticated": false}
	}
	device, _ := h.deps.Store.Device(sess.DeviceID)
	return map[string]any{
		"authenticated": true,
		"session":       sess,
		"device":        device,
	}
}

func decodeJSON(r *nethttp.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w nethttp.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w nethttp.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func bearerToken(r *nethttp.Request) string {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return ""
	}
	typ, token, ok := strings.Cut(raw, " ")
	if !ok || !strings.EqualFold(typ, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func setSessionCookie(w nethttp.ResponseWriter, r *nethttp.Request, token string, expires time.Time) {
	nethttp.SetCookie(w, &nethttp.Cookie{
		Name:     "ibkr_app_session",
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: nethttp.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
	})
}

const (
	deviceCookieName = "ibkr_app_device"
	// 400 days, the browser cookie-lifetime cap; the clock slides on every
	// device-cookie login.
	deviceCookieMaxAge = 400 * 24 * 60 * 60
)

func setDeviceCookie(w nethttp.ResponseWriter, r *nethttp.Request, value string) {
	nethttp.SetCookie(w, &nethttp.Cookie{
		Name:     deviceCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   deviceCookieMaxAge,
		HttpOnly: true,
		// Lax, not Strict: the first navigation into the app after a QR
		// scan is a cross-site top-level navigation and must still carry
		// the continuity credential.
		SameSite: nethttp.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})
}

func requestIsHTTPS(r *nethttp.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

type interfaceAddrsFunc func() ([]net.Addr, error)

func isLocalMac(remote string) bool {
	return isLocalMacWithAddrs(remote, net.InterfaceAddrs)
}

func isLocalMacWithAddrs(remote string, addrs interfaceAddrsFunc) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	return isLocalInterfaceIP(ip, addrs)
}

func isLocalInterfaceIP(ip net.IP, addrs interfaceAddrsFunc) bool {
	if addrs == nil {
		return false
	}
	items, err := addrs()
	if err != nil {
		return false
	}
	for _, item := range items {
		var local net.IP
		switch v := item.(type) {
		case *net.IPNet:
			local = v.IP
		case *net.IPAddr:
			local = v.IP
		default:
			continue
		}
		if local != nil && local.Equal(ip) {
			return true
		}
	}
	return false
}
