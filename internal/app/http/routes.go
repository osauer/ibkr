package apphttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	nethttp "net/http"
	"net/url"
	"strings"
	"time"

	hyperserve "github.com/osauer/hyperserve/pkg/server"

	"github.com/osauer/ibkr/internal/app/auth"
	"github.com/osauer/ibkr/internal/app/live"
	"github.com/osauer/ibkr/internal/app/relay"
	"github.com/osauer/ibkr/internal/app/state"
	appweb "github.com/osauer/ibkr/web/app"
)

type Dependencies struct {
	Server    *hyperserve.Server
	Store     *state.Store
	Auth      *auth.Manager
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
	srv.GET("/service-worker.js", h.serveStatic)
	srv.GET("/app.js", h.serveStatic)
	srv.GET("/styles.css", h.serveStatic)
	srv.GET("/icon.svg", h.serveStatic)
	srv.GET("/favicon.ico", h.serveIcon)

	srv.POST("/api/pairing/sessions", h.handleStartPairing)
	srv.POST("/api/pairing/complete", h.handleCompletePairing)
	srv.POST("/api/auth/challenge", h.handleAuthChallenge)
	srv.POST("/api/auth/session", h.handleAuthSession)

	srv.GET("/api/bootstrap", h.requireAuth(h.handleBootstrap))
	srv.GET("/api/snapshot", h.requireAuth(h.handleSnapshot))
	srv.GET("/api/events", h.requireAuth(h.handleEvents))
	srv.GET("/api/alerts/settings", h.requireAuth(h.handleGetAlertSettings))
	srv.PUT("/api/alerts/settings", h.requireAuth(h.handlePutAlertSettings))
	srv.GET("/api/alerts", h.requireAuth(h.handleAlerts))
	srv.DELETE("/api/alerts", h.requireAuth(h.handleClearAlerts))
	srv.POST("/api/push/subscribe", h.requireAuth(h.handlePushSubscribe))
	srv.DELETE("/api/push/{id}", h.requireAuth(h.handlePushDelete))
	srv.POST("/api/tools/{name}", h.requireAuth(h.handleTool))
}

func (h *handler) serveIndex(w nethttp.ResponseWriter, r *nethttp.Request) {
	nethttp.ServeFileFS(w, r, appweb.Files, "index.html")
}

func (h *handler) serveStatic(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.web.ServeHTTP(w, r)
}

func (h *handler) serveIcon(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	nethttp.ServeFileFS(w, r, appweb.Files, "icon.svg")
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
	if strings.TrimSpace(req.PublicURL) != "" {
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
	writeJSON(w, session)
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
		writeError(w, nethttp.StatusUnauthorized, err.Error())
		return
	}
	setSessionCookie(w, r, res.Token, res.ExpiresAt)
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
		writeError(w, nethttp.StatusUnauthorized, err.Error())
		return
	}
	setSessionCookie(w, r, sess.Token, sess.ExpiresAt)
	writeJSON(w, sess)
}

func (h *handler) handleBootstrap(w nethttp.ResponseWriter, r *nethttp.Request) {
	vapid, _ := h.deps.Store.VAPID()
	writeJSON(w, map[string]any{
		"version":          h.deps.Version,
		"public_url":       h.deps.PublicURL,
		"snapshot":         h.deps.Live.Snapshot(),
		"alert_settings":   h.deps.Store.AlertSettings(),
		"alerts":           h.deps.Store.AlertHistory(20),
		"last_push":        h.deps.Store.LastPush(),
		"relay":            h.deps.Relay.Status(),
		"vapid_public_key": vapid.PublicKey,
		"auth":             h.authStatus(r),
	})
}

func (h *handler) handleSnapshot(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, h.deps.Live.Snapshot())
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

func (h *handler) handleClearAlerts(w nethttp.ResponseWriter, _ *nethttp.Request) {
	if err := h.deps.Store.ClearAlertHistory(); err != nil {
		writeError(w, nethttp.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
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

func (h *handler) handleTool(w nethttp.ResponseWriter, r *nethttp.Request) {
	name := r.PathValue("name")
	switch name {
	case "status":
		if snap := h.deps.Live.Snapshot(); snap.Status != nil {
			writeJSON(w, snap.Status)
			return
		}
		writeJSON(w, map[string]any{"status": "unavailable", "snapshot": h.deps.Live.Snapshot()})
	case "snapshot":
		writeJSON(w, h.deps.Live.Snapshot())
	case "events":
		writeJSON(w, h.deps.Live.Diagnostics())
	case "auth":
		writeJSON(w, h.authStatus(r))
	case "push":
		writeJSON(w, map[string]any{
			"subscriptions": h.deps.Store.PushSubscriptions(),
			"last_push":     h.deps.Store.LastPush(),
		})
	case "relay":
		writeJSON(w, h.deps.Relay.Status())
	default:
		writeError(w, nethttp.StatusNotFound, "unknown debug tool "+name)
	}
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
			writeError(w, nethttp.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	}
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
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	nethttp.SetCookie(w, &nethttp.Cookie{
		Name:     "ibkr_app_session",
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: nethttp.SameSiteStrictMode,
		Secure:   secure,
	})
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
