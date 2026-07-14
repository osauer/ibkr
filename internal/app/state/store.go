package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const (
	AlertModeNone        = "none"
	AlertModeActOnly     = "act_only"
	AlertModeWatchAndAct = "watch_and_act"
)

type Store struct {
	path string
	mu   sync.Mutex
	data Data
}

type Data struct {
	Devices           []DeviceGrant       `json:"devices,omitempty"`
	AlertSettings     AlertSettings       `json:"alert_settings"`
	PushSubscriptions []PushSubscription  `json:"push_subscriptions,omitempty"`
	AlertHistory      []AlertRecord       `json:"alert_history,omitempty"`
	VAPID             *VAPIDKeys          `json:"vapid,omitempty"`
	LastPush          *PushAttempt        `json:"last_push,omitempty"`
	ProposalAudit     []ProposalAuditItem `json:"proposal_audit,omitempty"`
	RelayRoute        *RelayRoute         `json:"relay_route,omitempty"`
}

type DeviceGrant struct {
	ID               string `json:"id"`
	Name             string `json:"name,omitempty"`
	PublicKeyJWK     string `json:"public_key_jwk,omitempty"`
	DeviceSecretHash string `json:"device_secret_hash,omitempty"`
	// DeviceCookieHashes authenticate the long-lived HttpOnly device
	// cookie. Cookies are the only client storage that provably survives
	// the iOS home-screen web-app container split (localStorage/IndexedDB
	// written by Safari never reach the installed app), so session
	// continuity must not depend on script-visible storage. A capped list,
	// not a single value: Safari and the installed app hold twin copies of
	// the cookie jar, so issuing a fresh cookie to one twin must never
	// invalidate the other.
	DeviceCookieHashes []string  `json:"device_cookie_hashes,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	LastSeenAt         time.Time `json:"last_seen_at,omitzero"`
	RevokedAt          time.Time `json:"revoked_at,omitzero"`
}

type RelayRoute struct {
	RemoteURL      string    `json:"remote_url"`
	RouteID        string    `json:"route_id"`
	ConnectorToken string    `json:"connector_token"`
	PublicURL      string    `json:"public_url,omitempty"`
	ConnectorURL   string    `json:"connector_url,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type AlertSettings struct {
	Mode string `json:"mode"`
}

type PushSubscription struct {
	ID         string    `json:"id"`
	DeviceID   string    `json:"device_id"`
	Endpoint   string    `json:"endpoint"`
	P256DH     string    `json:"p256dh"`
	Auth       string    `json:"auth"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at,omitzero"`
}

type AlertRecord struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	Action      string    `json:"action,omitempty"`
	Severity    string    `json:"severity,omitempty"`
	Account     string    `json:"account,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
}

type PushAttempt struct {
	At             time.Time `json:"at"`
	SubscriptionID string    `json:"subscription_id,omitempty"`
	AlertID        string    `json:"alert_id,omitempty"`
	OK             bool      `json:"ok"`
	Status         string    `json:"status,omitempty"`
	Error          string    `json:"error,omitempty"`
}

type VAPIDKeys struct {
	PublicKey  string    `json:"public_key"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
}

type ProposalAuditItem struct {
	ID        string          `json:"id"`
	DeviceID  string          `json:"device_id,omitempty"`
	Action    string          `json:"action,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("state dir required")
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	if err := s.load(); err != nil {
		return nil, err
	}
	if s.data.AlertSettings.Mode == "" {
		s.data.AlertSettings.Mode = AlertModeWatchAndAct
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read app state: %w", err)
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return fmt.Errorf("decode app state: %w", err)
	}
	return nil
}

func (s *Store) AlertSettings() AlertSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.AlertSettings
}

func (s *Store) SetAlertMode(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validAlertMode(mode) {
		return fmt.Errorf("invalid alert mode %q", mode)
	}
	s.data.AlertSettings.Mode = mode
	return s.save()
}

func (s *Store) AddDevice(d DeviceGrant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Devices {
		if s.data.Devices[i].ID == d.ID {
			s.data.Devices[i] = d
			return s.save()
		}
	}
	s.data.Devices = append(s.data.Devices, d)
	return s.save()
}

func (s *Store) Device(id string) (DeviceGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.data.Devices {
		if d.ID == id && d.RevokedAt.IsZero() {
			return d, true
		}
	}
	return DeviceGrant{}, false
}

// maxDeviceCookieHashes bounds the valid cookie generations per device:
// enough for a few Safari/home-screen twins plus re-provisioned logins,
// small enough that a leaked state file exposes a bounded credential set.
const maxDeviceCookieHashes = 5

func (s *Store) AddDeviceCookieHash(id, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Devices {
		if s.data.Devices[i].ID != id {
			continue
		}
		hashes := s.data.Devices[i].DeviceCookieHashes
		if slices.Contains(hashes, hash) {
			return nil
		}
		hashes = append(hashes, hash)
		if len(hashes) > maxDeviceCookieHashes {
			hashes = hashes[len(hashes)-maxDeviceCookieHashes:]
		}
		s.data.Devices[i].DeviceCookieHashes = hashes
		return s.save()
	}
	return fmt.Errorf("device %s not found", id)
}

func (s *Store) SetDeviceSeen(id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Devices {
		if s.data.Devices[i].ID == id {
			s.data.Devices[i].LastSeenAt = at
			return s.save()
		}
	}
	return fmt.Errorf("device %s not found", id)
}

func (s *Store) AddPushSubscription(sub PushSubscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.PushSubscriptions {
		if s.data.PushSubscriptions[i].Endpoint == sub.Endpoint {
			sub.ID = s.data.PushSubscriptions[i].ID
			sub.CreatedAt = s.data.PushSubscriptions[i].CreatedAt
			s.data.PushSubscriptions[i] = sub
			return s.save()
		}
	}
	s.data.PushSubscriptions = append(s.data.PushSubscriptions, sub)
	return s.save()
}

func (s *Store) PushSubscriptions() []PushSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PushSubscription, len(s.data.PushSubscriptions))
	copy(out, s.data.PushSubscriptions)
	return out
}

func (s *Store) RemovePushSubscription(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.PushSubscriptions = slices.DeleteFunc(s.data.PushSubscriptions, func(sub PushSubscription) bool {
		return sub.ID == id || sub.Endpoint == id
	})
	return s.save()
}

func (s *Store) RecordAlert(rec AlertRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.AlertHistory = append([]AlertRecord{rec}, s.data.AlertHistory...)
	if len(s.data.AlertHistory) > 100 {
		s.data.AlertHistory = s.data.AlertHistory[:100]
	}
	return s.save()
}

func (s *Store) AlertHistory(limit int) []AlertRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.data.AlertHistory) {
		limit = len(s.data.AlertHistory)
	}
	out := make([]AlertRecord, limit)
	copy(out, s.data.AlertHistory[:limit])
	return out
}

func (s *Store) ClearAlertHistory() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.AlertHistory = nil
	return s.save()
}

func (s *Store) HasAlertFingerprint(fp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.data.AlertHistory {
		if rec.Fingerprint == fp {
			return true
		}
	}
	return false
}

func (s *Store) RecordPush(attempt PushAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastPush = &attempt
	return s.save()
}

func (s *Store) LastPush() *PushAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.LastPush == nil {
		return nil
	}
	cp := *s.data.LastPush
	return &cp
}

func (s *Store) EnsureVAPID(now time.Time, gen func() (privateKey, publicKey string, err error)) (VAPIDKeys, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.VAPID != nil && s.data.VAPID.PublicKey != "" && s.data.VAPID.PrivateKey != "" {
		return *s.data.VAPID, nil
	}
	priv, pub, err := gen()
	if err != nil {
		return VAPIDKeys{}, err
	}
	keys := VAPIDKeys{PublicKey: pub, PrivateKey: priv, CreatedAt: now}
	s.data.VAPID = &keys
	if err := s.save(); err != nil {
		return VAPIDKeys{}, err
	}
	return keys, nil
}

func (s *Store) VAPID() (VAPIDKeys, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.VAPID == nil {
		return VAPIDKeys{}, false
	}
	return *s.data.VAPID, true
}

func (s *Store) RelayRoute(remoteURL string) (RelayRoute, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.RelayRoute == nil {
		return RelayRoute{}, false
	}
	route := *s.data.RelayRoute
	if route.RemoteURL != remoteURL || route.RouteID == "" || route.ConnectorToken == "" {
		return RelayRoute{}, false
	}
	// An expired route is still returned: the relay revives a token-matched
	// resume, and abandoning the route id here would orphan every paired
	// phone. ExpiresAt is informational.
	return route, true
}

func (s *Store) SetRelayRoute(route RelayRoute) error {
	if route.RemoteURL == "" {
		return errors.New("relay remote URL required")
	}
	if route.RouteID == "" {
		return errors.New("relay route id required")
	}
	if route.ConnectorToken == "" {
		return errors.New("relay connector token required")
	}
	now := time.Now().UTC()
	route.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	if route.CreatedAt.IsZero() {
		// Route extensions re-persist the same route id; keep its birth
		// time so route age stays observable.
		if prev := s.data.RelayRoute; prev != nil && prev.RouteID == route.RouteID {
			route.CreatedAt = prev.CreatedAt
		}
		if route.CreatedAt.IsZero() {
			route.CreatedAt = now
		}
	}
	s.data.RelayRoute = &route
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(s.path, 0o600)
}

func validAlertMode(mode string) bool {
	switch mode {
	case AlertModeNone, AlertModeActOnly, AlertModeWatchAndAct:
		return true
	default:
		return false
	}
}
