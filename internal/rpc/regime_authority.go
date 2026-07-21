package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// RegimeAuthorityStatus classifies the availability of the daemon-owned
// last-good regime snapshot. It is intentionally independent of any one
// indicator or upstream source.
type RegimeAuthorityStatus string

// Regime authority failure codes identify why a complete last-good snapshot is
// unavailable or why a refresh could not be published.
const (
	// RegimeAuthorityUnavailable means no complete last-good snapshot exists.
	// A regime.snapshot request in this state fails with CodeRegimeUnavailable;
	// the value is retained for typed diagnostics and cache-state tests.
	RegimeAuthorityUnavailable RegimeAuthorityStatus = "unavailable"
	// RegimeAuthorityFresh means the response is the current last-good snapshot
	// and remains within the daemon's configured freshness window.
	RegimeAuthorityFresh RegimeAuthorityStatus = "fresh"
	// RegimeAuthorityStale means the daemon served an intact last-good snapshot
	// outside its freshness window. It must never mean a partial refresh.
	RegimeAuthorityStale RegimeAuthorityStatus = "stale"
)

// RegimeAuthorityFailureCode is a stable, redacted classification of why the
// authority has no newer complete snapshot. Raw source, broker, path, or
// persistence error text does not belong on this contract.
type RegimeAuthorityFailureCode string

// Regime authority failure codes distinguish absence, refresh failure, publish
// failure, invalid persistence, and invalid wall-clock evidence.
const (
	RegimeAuthorityFailureNone                  RegimeAuthorityFailureCode = ""
	RegimeAuthorityFailureNoLastGood            RegimeAuthorityFailureCode = "no_last_good"
	RegimeAuthorityFailureRefreshTimeout        RegimeAuthorityFailureCode = "refresh_timeout"
	RegimeAuthorityFailureRefreshIncomplete     RegimeAuthorityFailureCode = "refresh_incomplete"
	RegimeAuthorityFailureRefreshFailed         RegimeAuthorityFailureCode = "refresh_failed"
	RegimeAuthorityFailurePublishFailed         RegimeAuthorityFailureCode = "publish_failed"
	RegimeAuthorityFailureInvalidPersistedState RegimeAuthorityFailureCode = "invalid_persisted_state"
	// RegimeAuthorityFailureClockInvalid means the authority's last successful
	// commit is ahead of the daemon's current wall clock. The intact snapshot is
	// retained as stale context, but refresh and publication stay fail-closed
	// until the clock catches up.
	RegimeAuthorityFailureClockInvalid RegimeAuthorityFailureCode = "clock_invalid"
)

// RegimeAuthorityHealth is the source-neutral projection of the daemon's
// regime snapshot authority. LastSuccessAgeSeconds is a pointer because zero
// is meaningful immediately after a successful publish, while nil means no
// last-good snapshot has ever been accepted.
//
// Refreshing reports an authority-owned refresh. It is not tied to the
// lifetime of the request that observed it. FailureCode classifies the latest
// failed attempt or the reason no last-good snapshot exists; it does not
// invalidate an existing last-good snapshot.
type RegimeAuthorityHealth struct {
	Status                RegimeAuthorityStatus      `json:"status"`
	Refreshing            bool                       `json:"refreshing"`
	LastSuccessAt         *time.Time                 `json:"last_success_at,omitempty"`
	LastSuccessAgeSeconds *int64                     `json:"last_success_age_seconds,omitempty"`
	FailureCode           RegimeAuthorityFailureCode `json:"failure_code,omitempty"`
}

// ValidateRegimeAuthorityHealth rejects ambiguous or internally inconsistent
// projections before they cross an adapter boundary.
func ValidateRegimeAuthorityHealth(health RegimeAuthorityHealth) error {
	switch health.Status {
	case RegimeAuthorityUnavailable, RegimeAuthorityFresh, RegimeAuthorityStale:
	default:
		return fmt.Errorf("invalid regime authority status %q", health.Status)
	}
	if !validRegimeAuthorityFailureCode(health.FailureCode) {
		return fmt.Errorf("invalid regime authority failure code %q", health.FailureCode)
	}

	hasSuccessTime := health.LastSuccessAt != nil
	hasSuccessAge := health.LastSuccessAgeSeconds != nil
	if hasSuccessTime != hasSuccessAge {
		return errors.New("regime authority last-success time and age must appear together")
	}
	if hasSuccessTime {
		if health.LastSuccessAt.IsZero() {
			return errors.New("regime authority last_success_at must not be zero")
		}
		if *health.LastSuccessAgeSeconds < 0 {
			return errors.New("regime authority last_success_age_seconds must not be negative")
		}
	}

	switch health.Status {
	case RegimeAuthorityUnavailable:
		if hasSuccessTime {
			return errors.New("unavailable regime authority must not claim a last-good snapshot")
		}
		if !health.Refreshing && health.FailureCode == RegimeAuthorityFailureNone {
			return errors.New("idle unavailable regime authority requires a failure code")
		}
	case RegimeAuthorityFresh, RegimeAuthorityStale:
		if !hasSuccessTime {
			return errors.New("available regime authority requires last-success time and age")
		}
		if health.FailureCode == RegimeAuthorityFailureNoLastGood || health.FailureCode == RegimeAuthorityFailureInvalidPersistedState {
			return errors.New("available regime authority cannot report a no-last-good failure")
		}
	}
	return nil
}

func validRegimeAuthorityFailureCode(code RegimeAuthorityFailureCode) bool {
	switch code {
	case RegimeAuthorityFailureNone,
		RegimeAuthorityFailureNoLastGood,
		RegimeAuthorityFailureRefreshTimeout,
		RegimeAuthorityFailureRefreshIncomplete,
		RegimeAuthorityFailureRefreshFailed,
		RegimeAuthorityFailurePublishFailed,
		RegimeAuthorityFailureInvalidPersistedState,
		RegimeAuthorityFailureClockInvalid:
		return true
	default:
		return false
	}
}

// MarshalJSON validates authority-state coherence before encoding.
func (health RegimeAuthorityHealth) MarshalJSON() ([]byte, error) {
	if err := ValidateRegimeAuthorityHealth(health); err != nil {
		return nil, err
	}
	type wire RegimeAuthorityHealth
	return json.Marshal(wire(health))
}

// UnmarshalJSON rejects unknown, missing, null, trailing, or incoherent data.
func (health *RegimeAuthorityHealth) UnmarshalJSON(data []byte) error {
	if health == nil {
		return errors.New("cannot unmarshal regime authority health into nil receiver")
	}
	type wire RegimeAuthorityHealth
	var decoded wire
	if err := decodeExactRegimeAuthorityJSONObject(data, &decoded); err != nil {
		return err
	}
	value := RegimeAuthorityHealth(decoded)
	if err := ValidateRegimeAuthorityHealth(value); err != nil {
		return err
	}
	*health = value
	return nil
}

func decodeExactRegimeAuthorityJSONObject(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("regime authority health must be a JSON object")
	}

	required := map[string]bool{
		"status":     false,
		"refreshing": false,
	}
	allowed := map[string]struct{}{
		"status":                   {},
		"refreshing":               {},
		"last_success_at":          {},
		"last_success_age_seconds": {},
		"failure_code":             {},
	}
	seen := make(map[string]struct{}, len(allowed))
	object := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("regime authority health contains a non-string key")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("regime authority health contains unknown key %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("regime authority health contains duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("regime authority health key %q must not be null", key)
		}
		object[key] = raw
		if _, ok := required[key]; ok {
			required[key] = true
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("regime authority health object is not closed")
	}
	for key, present := range required {
		if !present {
			return fmt.Errorf("regime authority health is missing key %q", key)
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("regime authority health has trailing JSON")
		}
		return err
	}

	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, destination); err != nil {
		return err
	}
	return nil
}
