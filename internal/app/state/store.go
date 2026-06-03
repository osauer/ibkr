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
}

type DeviceGrant struct {
	ID               string    `json:"id"`
	Name             string    `json:"name,omitempty"`
	PublicKeyJWK     string    `json:"public_key_jwk,omitempty"`
	DeviceSecretHash string    `json:"device_secret_hash,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	LastSeenAt       time.Time `json:"last_seen_at,omitzero"`
	RevokedAt        time.Time `json:"revoked_at,omitzero"`
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

func (s *Store) Snapshot() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyData(s.data)
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

func copyData(in Data) Data {
	out := in
	out.Devices = append([]DeviceGrant(nil), in.Devices...)
	out.PushSubscriptions = append([]PushSubscription(nil), in.PushSubscriptions...)
	out.AlertHistory = append([]AlertRecord(nil), in.AlertHistory...)
	out.ProposalAudit = append([]ProposalAuditItem(nil), in.ProposalAudit...)
	if in.VAPID != nil {
		v := *in.VAPID
		out.VAPID = &v
	}
	if in.LastPush != nil {
		p := *in.LastPush
		out.LastPush = &p
	}
	return out
}
