package state

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	AlertDeliveryVersion = "alert-delivery-v1"

	AlertDeliveryAttemptReserved    = "reserved"
	AlertDeliveryAttemptConfirmed   = "confirmed_pending_outcome"
	AlertDeliveryAttemptAccepted    = "push_service_accepted"
	AlertDeliveryAttemptRetry       = "retryable_failure"
	AlertDeliveryAttemptRejected    = "rejected"
	AlertDeliveryAttemptInterrupted = "interrupted_uncertain"
	AlertDeliveryAttemptRetired     = "target_retired"
	AlertDeliveryAttemptInactive    = "occurrence_inactive"
	AlertDeliveryAttemptExhausted   = "retry_exhausted"
	AlertDeliveryAttemptUnapproved  = "policy_unapproved"

	AlertDeliveryHealthShadow      = "shadow"
	AlertDeliveryHealthHealthy     = "healthy"
	AlertDeliveryHealthDegraded    = "degraded"
	AlertDeliveryHealthUnavailable = "unavailable"
	AlertDeliveryHealthOverflow    = "overflow"

	AlertDeliveryHealthClassShadow         = "policy_unapproved"
	AlertDeliveryHealthClassRetry          = "retry_pending"
	AlertDeliveryHealthClassRejected       = "transport_rejected"
	AlertDeliveryHealthClassInterrupted    = "interrupted_uncertain"
	AlertDeliveryHealthClassStateWrite     = "state_write_failure"
	AlertDeliveryHealthClassOverflow       = "capacity_overflow"
	AlertDeliveryHealthClassNoSubscription = "no_active_subscription"
	AlertDeliveryHealthClassSigningKeys    = "signing_keys_unavailable"
	AlertDeliveryHealthClassSender         = "sender_unavailable"

	AlertDeliveryEndRecovered  = "recovered"
	AlertDeliveryEndOmitted    = "authoritative_omission"
	AlertDeliveryEndSuperseded = "qualified_escalation"

	AlertDeliveryCompletionAccepted  AlertDeliveryCompletion = "accepted"
	AlertDeliveryCompletionRetryable AlertDeliveryCompletion = "retryable_failure"
	AlertDeliveryCompletionRejected  AlertDeliveryCompletion = "rejected"

	AlertDeliveryCompletionApplied         AlertDeliveryCompletionDisposition = "applied"
	AlertDeliveryCompletionAlreadyComplete AlertDeliveryCompletionDisposition = "already_complete"
	AlertDeliveryCompletionInactive        AlertDeliveryCompletionDisposition = "occurrence_inactive"
	AlertDeliveryCompletionRetired         AlertDeliveryCompletionDisposition = "target_retired"
)

const (
	defaultAlertDeliveryMaxItems = 4096
	alertDeliveryRetention       = 90 * 24 * time.Hour
)

var (
	ErrAlertDeliveryOverflow          = errors.New("alert delivery evidence overflow")
	ErrAlertDeliveryOldSnapshot       = errors.New("alert delivery snapshot is older than source authority")
	ErrAlertDeliveryUnknownOccurrence = errors.New("alert delivery occurrence not found")
	ErrAlertDeliveryInvalidTransition = errors.New("invalid alert delivery lifecycle transition")
	ErrAlertDeliveryAttentionRead     = errors.New("alert delivery attention read cursor is invalid")
	ErrAlertDeliveryUnavailable       = errors.New("alert delivery state is unavailable")
)

// alertDeliveryEligibilityFunc is intentionally private. Nil is the production
// default, which makes the new ledger a record-only shadow: no observed
// candidate can become transport eligible until a separately approved policy
// is wired in by the app owner.
type alertDeliveryEligibilityFunc func(rpc.AlertCandidate) bool

// alertDeliveryData is an optional, independently versioned section of the
// existing app state file. It never participates in the legacy Canary or
// Governance arrays/cursors.
type alertDeliveryData struct {
	Version                 string                        `json:"version"`
	Generation              uint64                        `json:"generation"`
	Snapshot                rpc.AlertCandidateSnapshot    `json:"snapshot"`
	SourceWatermarks        map[rpc.AlertSource]time.Time `json:"source_watermarks"`
	Episodes                []alertDeliveryEpisode        `json:"episodes,omitempty"`
	Occurrences             []alertDeliveryOccurrence     `json:"occurrences,omitempty"`
	Attempts                []alertDeliveryAttempt        `json:"attempts,omitempty"`
	Receipts                []alertDeliveryReceipt        `json:"receipts,omitempty"`
	RetiredTargets          map[string]time.Time          `json:"retired_targets"`
	Health                  AlertDeliveryHealth           `json:"delivery_health"`
	AttentionHighWaterSeq   uint64                        `json:"attention_v2_high_water_seq"`
	AttentionReadThroughSeq uint64                        `json:"attention_v2_read_through_seq"`
}

type alertDeliveryEpisode struct {
	EpisodeKey           string                `json:"episode_key"`
	Source               rpc.AlertSource       `json:"source"`
	Kind                 rpc.AlertKind         `json:"kind"`
	CurrentOccurrenceKey string                `json:"current_occurrence_key"`
	State                rpc.AlertEpisodeState `json:"state"`
	FirstSeenAt          time.Time             `json:"first_seen_at"`
	LastSeenAt           time.Time             `json:"last_seen_at"`
}

type alertDeliveryOccurrence struct {
	OccurrenceKey       string                      `json:"occurrence_key"`
	EpisodeKey          string                      `json:"episode_key"`
	EvidenceFingerprint string                      `json:"evidence_fingerprint"`
	DisplayID           string                      `json:"display_id"`
	Source              rpc.AlertSource             `json:"source"`
	Kind                rpc.AlertKind               `json:"kind"`
	State               rpc.AlertEpisodeState       `json:"state"`
	Severity            rpc.AlertSeverity           `json:"severity"`
	DeliveryPreference  rpc.AlertDeliveryPreference `json:"delivery_preference"`
	EvidenceHealth      rpc.AlertEvidenceHealth     `json:"evidence_health"`
	Destination         rpc.AlertDestination        `json:"destination"`
	EvidenceAsOf        time.Time                   `json:"evidence_as_of"`
	StateChangedAt      time.Time                   `json:"state_changed_at"`
	ObservedAt          time.Time                   `json:"observed_at"`
	FirstSeenAt         time.Time                   `json:"first_seen_at"`
	LastSeenAt          time.Time                   `json:"last_seen_at"`
	EndedAt             time.Time                   `json:"ended_at,omitzero"`
	EndReason           string                      `json:"end_reason,omitempty"`
	AttentionSeq        uint64                      `json:"attention_v2_seq"`
	TransportEligible   bool                        `json:"transport_eligible"`
}

type alertDeliveryAttempt struct {
	ID            string                             `json:"id"`
	OccurrenceKey string                             `json:"occurrence_key"`
	TargetRef     string                             `json:"target_ref"`
	ReceiptKey    string                             `json:"receipt_key"`
	AttemptNumber int                                `json:"attempt_number"`
	ReservedAt    time.Time                          `json:"reserved_at"`
	CompletedAt   time.Time                          `json:"completed_at,omitzero"`
	Class         string                             `json:"class"`
	Disposition   AlertDeliveryCompletionDisposition `json:"disposition,omitempty"`
	RetryAt       time.Time                          `json:"retry_at,omitzero"`
	RetiredAt     time.Time                          `json:"retired_at,omitzero"`
}

type alertDeliveryReceipt struct {
	OccurrenceKey string    `json:"occurrence_key"`
	TargetRef     string    `json:"target_ref"`
	ReceiptKey    string    `json:"receipt_key"`
	AcceptedAt    time.Time `json:"accepted_at"`
	RetiredAt     time.Time `json:"retired_at,omitzero"`
}

// AlertDeliveryOccurrenceView is safe for HTTP/SSE projection. Producer keys,
// evidence fingerprints, target identities, attempt IDs, and receipt keys are
// deliberately absent.
type AlertDeliveryOccurrenceView struct {
	DisplayID          string                      `json:"display_id"`
	Source             rpc.AlertSource             `json:"source"`
	Kind               rpc.AlertKind               `json:"kind"`
	State              rpc.AlertEpisodeState       `json:"state"`
	Severity           rpc.AlertSeverity           `json:"severity"`
	DeliveryPreference rpc.AlertDeliveryPreference `json:"delivery_preference"`
	EvidenceHealth     rpc.AlertEvidenceHealth     `json:"evidence_health"`
	Destination        rpc.AlertDestination        `json:"destination"`
	EvidenceAsOf       time.Time                   `json:"evidence_as_of"`
	StateChangedAt     time.Time                   `json:"state_changed_at"`
	FirstSeenAt        time.Time                   `json:"first_seen_at"`
	LastSeenAt         time.Time                   `json:"last_seen_at"`
	EndedAt            time.Time                   `json:"ended_at,omitzero"`
	EndReason          string                      `json:"end_reason,omitempty"`
	AttentionSeq       uint64                      `json:"attention_seq"`
	TransportEligible  bool                        `json:"-"`
}

type AlertDeliveryAttentionRef struct {
	DisplayID string          `json:"display_id"`
	Source    rpc.AlertSource `json:"source"`
	Kind      rpc.AlertKind   `json:"kind"`
}

type AlertDeliveryAttention struct {
	UnreadCount    int                         `json:"unread_count"`
	HighWaterSeq   uint64                      `json:"high_water_seq"`
	ReadThroughSeq uint64                      `json:"read_through_seq"`
	UnreadRefs     []AlertDeliveryAttentionRef `json:"unread_refs"`
}

type AlertDeliveryAttemptTotals struct {
	Attempts       int `json:"attempts"`
	Confirmed      int `json:"confirmed_pending_outcome"`
	Accepted       int `json:"push_service_accepted"`
	RetryPending   int `json:"retry_pending"`
	Rejected       int `json:"rejected"`
	Interrupted    int `json:"interrupted_uncertain"`
	TargetRetired  int `json:"target_retired"`
	Inactive       int `json:"occurrence_inactive"`
	RetryExhausted int `json:"retry_exhausted"`
	Unapproved     int `json:"policy_unapproved"`
}

type AlertDeliveryHealth struct {
	State          string    `json:"state"`
	Class          string    `json:"class,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastAcceptedAt time.Time `json:"last_push_service_acceptance_at,omitzero"`
}

type AlertDeliveryView struct {
	Initialized      bool                          `json:"initialized"`
	Version          string                        `json:"version,omitempty"`
	Generation       uint64                        `json:"generation"`
	AsOf             time.Time                     `json:"as_of,omitzero"`
	CurrentState     rpc.AlertSnapshotState        `json:"current_state,omitempty"`
	Coverage         rpc.AlertCoverage             `json:"coverage,omitzero"`
	SourceWatermarks map[rpc.AlertSource]time.Time `json:"-"`
	Occurrences      []AlertDeliveryOccurrenceView `json:"occurrences"`
	Attention        AlertDeliveryAttention        `json:"attention"`
	AttemptTotals    AlertDeliveryAttemptTotals    `json:"-"`
	DeliveryHealth   AlertDeliveryHealth           `json:"delivery_health"`
}

type AlertDeliveryReservation struct {
	AttemptID     string    `json:"-"`
	DisplayID     string    `json:"display_id"`
	AttemptNumber int       `json:"attempt_number"`
	ReservedAt    time.Time `json:"reserved_at"`
	RetryAt       time.Time `json:"retry_at,omitzero"`
	// Candidate is populated only by a successful ConfirmAlertTransport.
	// It is the exact current candidate checked under the same store lock as
	// the persisted confirmed-pending-outcome transition. Dispatchers must
	// build transport copy from this value, never from a prior due-work scan.
	Candidate rpc.AlertCandidate `json:"-"`
}

type AlertDeliveryCompletion string
type AlertDeliveryCompletionDisposition string

type AlertDeliveryCompletionOutcome struct {
	Disposition AlertDeliveryCompletionDisposition `json:"disposition"`
	Class       string                             `json:"class"`
	RetryAt     time.Time                          `json:"retry_at,omitzero"`
}

// AlertDeliveryDueWork is an app-internal dispatch record. The private
// producer occurrence and validated candidate are available to Go callers but
// are excluded from JSON; DisplayID is the only public identity.
type AlertDeliveryDueWork struct {
	OccurrenceKey string             `json:"-"`
	Candidate     rpc.AlertCandidate `json:"-"`
	DisplayID     string             `json:"display_id"`
}

func (s *Store) initAlertDeliveryRuntime() {
	if s.alertDeliveryMaxItems <= 0 {
		s.alertDeliveryMaxItems = defaultAlertDeliveryMaxItems
	}
	if s.alertDeliveryInFlight == nil {
		s.alertDeliveryInFlight = make(map[string]bool)
	}
}

func newAlertDeliveryData() *alertDeliveryData {
	return &alertDeliveryData{
		Version:          AlertDeliveryVersion,
		SourceWatermarks: make(map[rpc.AlertSource]time.Time),
		RetiredTargets:   make(map[string]time.Time),
	}
}

func bumpAlertDeliveryGeneration(data *alertDeliveryData) error {
	if data.Generation == ^uint64(0) {
		return ErrAttentionSequenceExhausted
	}
	data.Generation++
	return nil
}

func (s *Store) bumpAlertDeliveryGenerationLocked(data *alertDeliveryData) error {
	if err := bumpAlertDeliveryGeneration(data); err != nil {
		return err
	}
	// A volatile state-write failure is itself a public health generation.
	// Skip over it when persistence recovers so SSE consumers see both edges.
	if s.alertDeliveryVolatile != nil {
		return bumpAlertDeliveryGeneration(data)
	}
	return nil
}

func (s *Store) noteAlertDeliverySaveFailureLocked(at time.Time) {
	// Preserve one stable public outage generation until a successful write
	// proves recovery. Re-stamping the same persisted generation with a new
	// timestamp would create equal-generation equivocation for SSE/reconnect
	// consumers and can hide the later recovery edge.
	if s.alertDeliveryVolatile != nil {
		return
	}
	lastAccepted := time.Time{}
	if s.data.AlertDelivery != nil {
		lastAccepted = s.data.AlertDelivery.Health.LastAcceptedAt
	}
	health := AlertDeliveryHealth{State: AlertDeliveryHealthUnavailable, Class: AlertDeliveryHealthClassStateWrite, UpdatedAt: at.UTC(), LastAcceptedAt: lastAccepted}
	s.alertDeliveryVolatile = &health
	if s.data.AlertDelivery == nil {
		s.alertDeliveryVolatileGeneration = 1
	} else if s.data.AlertDelivery.Generation < ^uint64(0) {
		s.alertDeliveryVolatileGeneration = s.data.AlertDelivery.Generation + 1
	}
}

func (s *Store) clearAlertDeliveryVolatileLocked() {
	if s.alertDeliveryQuarantinedLocked() {
		return
	}
	s.alertDeliveryVolatile = nil
	s.alertDeliveryVolatileGeneration = 0
}

func (s *Store) setAlertDeliveryOverflowLocked(prior *alertDeliveryData, at time.Time) error {
	rollback := s.data.AlertDelivery
	next := cloneAlertDeliveryData(prior)
	if next == nil {
		next = newAlertDeliveryData()
	}
	next.Version = AlertDeliveryVersion
	next.Health = AlertDeliveryHealth{State: AlertDeliveryHealthOverflow, Class: AlertDeliveryHealthClassOverflow, UpdatedAt: at.UTC(), LastAcceptedAt: next.Health.LastAcceptedAt}
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return ErrAlertDeliveryOverflow
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = rollback
		s.noteAlertDeliverySaveFailureLocked(at)
		return fmt.Errorf("%w: persist overflow health: %v", ErrAlertDeliveryOverflow, err)
	}
	s.clearAlertDeliveryVolatileLocked()
	return ErrAlertDeliveryOverflow
}

// ObserveAlertSnapshot validates and commits one complete producer contract.
// All lifecycle, authority-watermark, attention, and snapshot-view changes are
// written by the same atomic state-file replacement. Valid observations are
// persisted even when they only advance generation/coverage.
func (s *Store) ObserveAlertSnapshot(snapshot rpc.AlertCandidateSnapshot) (AlertDeliveryView, error) {
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		return AlertDeliveryView{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return s.alertDeliveryViewLocked(nil, time.Now().UTC()), err
	}
	prior := s.data.AlertDelivery
	if prior != nil && prior.Generation > 0 && snapshot.AsOf.Equal(prior.Snapshot.AsOf) {
		if !equalAlertCandidateSnapshot(prior.Snapshot, snapshot) {
			return AlertDeliveryView{}, fmt.Errorf("%w: snapshot %s conflicts with the persisted authority generation", ErrAlertDeliveryOldSnapshot, snapshot.AsOf.UTC().Format(time.RFC3339Nano))
		}
		return s.alertDeliveryViewLocked(prior, time.Now().UTC()), nil
	}
	next := cloneAlertDeliveryData(prior)
	if next == nil {
		next = newAlertDeliveryData()
	}
	if err := rejectOldAlertSnapshot(next, snapshot); err != nil {
		return AlertDeliveryView{}, err
	}
	if err := s.applyAlertSnapshotLocked(next, snapshot); err != nil {
		if errors.Is(err, ErrAlertDeliveryOverflow) {
			overflowBase := prior
			if overflowBase == nil {
				overflowBase = newAlertDeliveryData()
				overflowBase.Version = AlertDeliveryVersion
				overflowBase.Snapshot = unavailableAlertSnapshot(snapshot)
			}
			return AlertDeliveryView{}, s.setAlertDeliveryOverflowLocked(overflowBase, snapshot.AsOf)
		}
		return AlertDeliveryView{}, err
	}
	if s.alertDeliveryEligible == nil {
		for i := range next.Occurrences {
			next.Occurrences[i].TransportEligible = false
		}
	}
	s.recomputeAlertDeliveryHealthLocked(next, snapshot.AsOf)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return AlertDeliveryView{}, err
	}
	next.Snapshot = cloneAlertSnapshot(snapshot)
	next.Version = AlertDeliveryVersion
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(snapshot.AsOf)
		return AlertDeliveryView{}, err
	}
	s.clearAlertDeliveryVolatileLocked()
	return s.alertDeliveryViewLocked(next, time.Now().UTC()), nil
}

func unavailableAlertSnapshot(snapshot rpc.AlertCandidateSnapshot) rpc.AlertCandidateSnapshot {
	return rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion,
		AsOf:          snapshot.AsOf,
		CurrentState:  rpc.AlertSnapshotUnknown,
		Coverage: rpc.AlertCoverage{
			State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: snapshot.AsOf,
			ExpectedSources: append([]rpc.AlertSource{}, snapshot.Coverage.ExpectedSources...),
			CoveredSources:  []rpc.AlertSource{},
		},
		Candidates: []rpc.AlertCandidate{},
	}
}

func rejectOldAlertSnapshot(data *alertDeliveryData, snapshot rpc.AlertCandidateSnapshot) error {
	if data == nil {
		return nil
	}
	if data.Generation > 0 && snapshot.AsOf.Before(data.Snapshot.AsOf) {
		return fmt.Errorf("%w: snapshot %s precedes current view %s", ErrAlertDeliveryOldSnapshot, snapshot.AsOf.UTC().Format(time.RFC3339Nano), data.Snapshot.AsOf.UTC().Format(time.RFC3339Nano))
	}
	for _, candidate := range snapshot.Candidates {
		if watermark := data.SourceWatermarks[candidate.Source]; !watermark.IsZero() && candidate.ObservedAt.Before(watermark) {
			return fmt.Errorf("%w: source %s observed_at %s precedes %s", ErrAlertDeliveryOldSnapshot, candidate.Source, candidate.ObservedAt.UTC().Format(time.RFC3339Nano), watermark.UTC().Format(time.RFC3339Nano))
		}
	}
	if snapshot.Coverage.Freshness == rpc.AlertCoverageCurrent {
		for _, source := range snapshot.Coverage.CoveredSources {
			if watermark := data.SourceWatermarks[source]; !watermark.IsZero() && snapshot.Coverage.AsOf.Before(watermark) {
				return fmt.Errorf("%w: source %s coverage %s precedes %s", ErrAlertDeliveryOldSnapshot, source, snapshot.Coverage.AsOf.UTC().Format(time.RFC3339Nano), watermark.UTC().Format(time.RFC3339Nano))
			}
		}
	}
	return nil
}

func equalAlertCandidateSnapshot(a, b rpc.AlertCandidateSnapshot) bool {
	if a.SchemaVersion != b.SchemaVersion || !a.AsOf.Equal(b.AsOf) || a.CurrentState != b.CurrentState ||
		a.Coverage.State != b.Coverage.State || a.Coverage.Freshness != b.Coverage.Freshness || !a.Coverage.AsOf.Equal(b.Coverage.AsOf) ||
		!slices.Equal(a.Coverage.ExpectedSources, b.Coverage.ExpectedSources) || !slices.Equal(a.Coverage.CoveredSources, b.Coverage.CoveredSources) ||
		len(a.Candidates) != len(b.Candidates) {
		return false
	}
	for i := range a.Candidates {
		left, right := a.Candidates[i], b.Candidates[i]
		if left.EpisodeKey != right.EpisodeKey || left.OccurrenceKey != right.OccurrenceKey || left.EvidenceFingerprint != right.EvidenceFingerprint ||
			left.Source != right.Source || left.Kind != right.Kind || left.State != right.State || left.Severity != right.Severity ||
			left.DeliveryPreference != right.DeliveryPreference || left.EvidenceHealth != right.EvidenceHealth || left.Destination != right.Destination ||
			!left.EvidenceAsOf.Equal(right.EvidenceAsOf) || !left.StateChangedAt.Equal(right.StateChangedAt) || !left.ObservedAt.Equal(right.ObservedAt) {
			return false
		}
	}
	return true
}

func (s *Store) applyAlertSnapshotLocked(data *alertDeliveryData, snapshot rpc.AlertCandidateSnapshot) error {
	episodes := make(map[string]int, len(data.Episodes))
	occurrences := make(map[string]int, len(data.Occurrences))
	for i := range data.Episodes {
		episodes[data.Episodes[i].EpisodeKey] = i
	}
	for i := range data.Occurrences {
		occurrences[data.Occurrences[i].OccurrenceKey] = i
	}

	seenEpisodes := make(map[string]struct{}, len(snapshot.Candidates))
	for _, candidate := range snapshot.Candidates {
		seenEpisodes[candidate.EpisodeKey] = struct{}{}
		episodeIndex, exists := episodes[candidate.EpisodeKey]
		if !exists {
			if candidate.State == rpc.AlertEpisodeRecovered {
				return fmt.Errorf("%w: recovery for unknown episode", ErrAlertDeliveryInvalidTransition)
			}
			if _, reused := occurrences[candidate.OccurrenceKey]; reused {
				return fmt.Errorf("%w: occurrence key reused by another episode", ErrAlertDeliveryInvalidTransition)
			}
			occurrence, err := s.newAlertDeliveryOccurrenceLocked(data, candidate)
			if err != nil {
				return err
			}
			data.Episodes = append(data.Episodes, alertDeliveryEpisode{
				EpisodeKey: candidate.EpisodeKey, Source: candidate.Source, Kind: candidate.Kind,
				CurrentOccurrenceKey: candidate.OccurrenceKey, State: candidate.State,
				FirstSeenAt: candidate.ObservedAt, LastSeenAt: candidate.ObservedAt,
			})
			data.Occurrences = append(data.Occurrences, occurrence)
			episodes[candidate.EpisodeKey] = len(data.Episodes) - 1
			occurrences[candidate.OccurrenceKey] = len(data.Occurrences) - 1
			continue
		}

		episode := &data.Episodes[episodeIndex]
		if episode.Source != candidate.Source || episode.Kind != candidate.Kind {
			return fmt.Errorf("%w: episode source or kind changed", ErrAlertDeliveryInvalidTransition)
		}
		currentIndex, currentExists := occurrences[episode.CurrentOccurrenceKey]
		if !currentExists {
			return fmt.Errorf("%w: episode current occurrence is missing", ErrInvalidPersistedState)
		}
		current := &data.Occurrences[currentIndex]

		if candidate.OccurrenceKey == episode.CurrentOccurrenceKey {
			if !current.EndedAt.IsZero() && candidate.State != rpc.AlertEpisodeRecovered {
				return fmt.Errorf("%w: ended occurrence was reused", ErrAlertDeliveryInvalidTransition)
			}
			if err := validateAlertCandidateAdvance(*current, candidate); err != nil {
				return err
			}
			s.applyAlertCandidate(current, candidate)
			if candidate.State == rpc.AlertEpisodeRecovered {
				current.EndedAt = candidate.StateChangedAt
				current.EndReason = AlertDeliveryEndRecovered
			}
			episode.State = candidate.State
			episode.LastSeenAt = candidate.ObservedAt
			continue
		}

		if _, reused := occurrences[candidate.OccurrenceKey]; reused {
			return fmt.Errorf("%w: old occurrence replayed as current", ErrAlertDeliveryInvalidTransition)
		}
		if candidate.State == rpc.AlertEpisodeRecovered {
			return fmt.Errorf("%w: recovery occurrence key did not match", ErrAlertDeliveryInvalidTransition)
		}
		if candidate.ObservedAt.Before(current.ObservedAt) || !candidate.StateChangedAt.After(current.StateChangedAt) {
			return fmt.Errorf("%w: new occurrence regressed lifecycle time", ErrAlertDeliveryInvalidTransition)
		}
		switch {
		case current.State == rpc.AlertEpisodeRecovered || !current.EndedAt.IsZero():
			// A new daemon occurrence after exact recovery is a reopen.
		case candidate.State == rpc.AlertEpisodeEscalated:
			// Rotating the occurrence key while active is allowed only for a
			// producer-qualified escalation.
			current.EndedAt = candidate.StateChangedAt
			current.EndReason = AlertDeliveryEndSuperseded
			current.TransportEligible = false
		default:
			return fmt.Errorf("%w: active opening changed occurrence key without escalation", ErrAlertDeliveryInvalidTransition)
		}
		occurrence, err := s.newAlertDeliveryOccurrenceLocked(data, candidate)
		if err != nil {
			return err
		}
		data.Occurrences = append(data.Occurrences, occurrence)
		occurrences[candidate.OccurrenceKey] = len(data.Occurrences) - 1
		episode.CurrentOccurrenceKey = candidate.OccurrenceKey
		episode.State = candidate.State
		episode.LastSeenAt = candidate.ObservedAt
	}

	authoritative := make(map[rpc.AlertSource]struct{})
	if snapshot.Coverage.Freshness == rpc.AlertCoverageCurrent {
		for _, source := range snapshot.Coverage.CoveredSources {
			authoritative[source] = struct{}{}
			data.SourceWatermarks[source] = snapshot.Coverage.AsOf
		}
	}
	// Absence resolves only the source slices this observation proves current.
	// Partial or stale coverage can never clear an unrelated active episode.
	for i := range data.Episodes {
		episode := &data.Episodes[i]
		if episode.State == rpc.AlertEpisodeRecovered {
			continue
		}
		if _, covered := authoritative[episode.Source]; !covered {
			continue
		}
		if _, present := seenEpisodes[episode.EpisodeKey]; present {
			continue
		}
		occurrenceIndex, ok := occurrences[episode.CurrentOccurrenceKey]
		if !ok {
			return fmt.Errorf("%w: omitted episode current occurrence is missing", ErrInvalidPersistedState)
		}
		occurrence := &data.Occurrences[occurrenceIndex]
		occurrence.State = rpc.AlertEpisodeRecovered
		occurrence.EvidenceHealth = rpc.AlertEvidenceCurrent
		occurrence.EvidenceAsOf = snapshot.Coverage.AsOf
		occurrence.StateChangedAt = snapshot.AsOf
		occurrence.ObservedAt = snapshot.AsOf
		occurrence.LastSeenAt = snapshot.AsOf
		occurrence.EndedAt = snapshot.AsOf
		occurrence.EndReason = AlertDeliveryEndOmitted
		occurrence.TransportEligible = false
		episode.State = rpc.AlertEpisodeRecovered
		episode.LastSeenAt = snapshot.AsOf
	}
	return nil
}

func (s *Store) newAlertDeliveryOccurrenceLocked(data *alertDeliveryData, candidate rpc.AlertCandidate) (alertDeliveryOccurrence, error) {
	if s.alertDeliveryMaxItems <= 0 {
		s.alertDeliveryMaxItems = defaultAlertDeliveryMaxItems
	}
	if len(data.Occurrences) >= s.alertDeliveryMaxItems || len(data.Episodes) >= s.alertDeliveryMaxItems {
		return alertDeliveryOccurrence{}, ErrAlertDeliveryOverflow
	}
	if data.AttentionHighWaterSeq == ^uint64(0) {
		return alertDeliveryOccurrence{}, ErrAttentionSequenceExhausted
	}
	data.AttentionHighWaterSeq++
	eligible := s.alertDeliveryCandidateEligible(candidate)
	return alertDeliveryOccurrence{
		OccurrenceKey: candidate.OccurrenceKey, EpisodeKey: candidate.EpisodeKey,
		EvidenceFingerprint: candidate.EvidenceFingerprint, DisplayID: alertDeliveryDisplayID(candidate.OccurrenceKey),
		Source: candidate.Source, Kind: candidate.Kind, State: candidate.State, Severity: candidate.Severity,
		DeliveryPreference: candidate.DeliveryPreference, EvidenceHealth: candidate.EvidenceHealth, Destination: candidate.Destination,
		EvidenceAsOf: candidate.EvidenceAsOf, StateChangedAt: candidate.StateChangedAt, ObservedAt: candidate.ObservedAt,
		FirstSeenAt: candidate.ObservedAt, LastSeenAt: candidate.ObservedAt, AttentionSeq: data.AttentionHighWaterSeq,
		TransportEligible: eligible,
	}, nil
}

func (s *Store) applyAlertCandidate(occurrence *alertDeliveryOccurrence, candidate rpc.AlertCandidate) {
	occurrence.EvidenceFingerprint = candidate.EvidenceFingerprint
	occurrence.State = candidate.State
	occurrence.Severity = candidate.Severity
	occurrence.DeliveryPreference = candidate.DeliveryPreference
	occurrence.EvidenceHealth = candidate.EvidenceHealth
	occurrence.Destination = candidate.Destination
	occurrence.EvidenceAsOf = candidate.EvidenceAsOf
	occurrence.StateChangedAt = candidate.StateChangedAt
	occurrence.ObservedAt = candidate.ObservedAt
	occurrence.LastSeenAt = candidate.ObservedAt
	occurrence.TransportEligible = s.alertDeliveryCandidateEligible(candidate)
}

func validateAlertCandidateAdvance(current alertDeliveryOccurrence, candidate rpc.AlertCandidate) error {
	if candidate.ObservedAt.Before(current.ObservedAt) || candidate.EvidenceAsOf.Before(current.EvidenceAsOf) || candidate.StateChangedAt.Before(current.StateChangedAt) {
		return fmt.Errorf("%w: occurrence timestamps regressed", ErrAlertDeliveryInvalidTransition)
	}
	if candidate.State == current.State {
		if !candidate.StateChangedAt.Equal(current.StateChangedAt) {
			return fmt.Errorf("%w: unchanged state changed its transition time", ErrAlertDeliveryInvalidTransition)
		}
		return nil
	}
	if !candidate.StateChangedAt.After(current.StateChangedAt) {
		return fmt.Errorf("%w: lifecycle transition did not advance state_changed_at", ErrAlertDeliveryInvalidTransition)
	}
	allowed := (current.State == rpc.AlertEpisodeOpen && (candidate.State == rpc.AlertEpisodeEscalated || candidate.State == rpc.AlertEpisodeRecovered)) ||
		(current.State == rpc.AlertEpisodeEscalated && candidate.State == rpc.AlertEpisodeRecovered)
	if !allowed {
		return fmt.Errorf("%w: state transition %s to %s", ErrAlertDeliveryInvalidTransition, current.State, candidate.State)
	}
	return nil
}

func (s *Store) alertDeliveryCandidateEligible(candidate rpc.AlertCandidate) bool {
	return s.alertDeliveryEligible != nil && candidate.State != rpc.AlertEpisodeRecovered && s.alertDeliveryEligible(candidate)
}

func alertCandidateFromOccurrence(occurrence *alertDeliveryOccurrence) rpc.AlertCandidate {
	return rpc.AlertCandidate{
		EpisodeKey: occurrence.EpisodeKey, OccurrenceKey: occurrence.OccurrenceKey,
		EvidenceFingerprint: occurrence.EvidenceFingerprint, Source: occurrence.Source, Kind: occurrence.Kind,
		State: occurrence.State, Severity: occurrence.Severity, DeliveryPreference: occurrence.DeliveryPreference,
		EvidenceHealth: occurrence.EvidenceHealth, Destination: occurrence.Destination,
		EvidenceAsOf: occurrence.EvidenceAsOf, StateChangedAt: occurrence.StateChangedAt, ObservedAt: occurrence.ObservedAt,
	}
}

// AlertDelivery returns one atomic generation of coverage, current state,
// source authority, redacted occurrence history, v2 attention, and delivery
// totals. It contains no producer, target, attempt, or receipt identity.
func (s *Store) AlertDelivery(now time.Time) AlertDeliveryView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alertDeliveryViewLocked(s.data.AlertDelivery, now.UTC())
}

func (s *Store) alertDeliveryViewLocked(data *alertDeliveryData, now time.Time) AlertDeliveryView {
	view := alertDeliveryViewLocked(data, now)
	if s.alertDeliveryEligible == nil && view.Initialized {
		for i := range view.Occurrences {
			view.Occurrences[i].TransportEligible = false
		}
		if view.DeliveryHealth.State != AlertDeliveryHealthOverflow && view.DeliveryHealth.State != AlertDeliveryHealthUnavailable {
			view.DeliveryHealth = AlertDeliveryHealth{
				State: AlertDeliveryHealthShadow, Class: AlertDeliveryHealthClassShadow,
				UpdatedAt: view.DeliveryHealth.UpdatedAt, LastAcceptedAt: view.DeliveryHealth.LastAcceptedAt,
			}
		}
	}
	if s.alertDeliveryVolatile != nil {
		view.DeliveryHealth = *s.alertDeliveryVolatile
		if s.alertDeliveryVolatileGeneration > view.Generation {
			view.Generation = s.alertDeliveryVolatileGeneration
		}
	}
	return view
}

// AlertDeliveriesDue reconstructs active transport-eligible work from durable
// state, including after restart without a fresh daemon snapshot. Per-target
// receipt/retry dedupe remains authoritative in BeginAlertDelivery.
func (s *Store) AlertDeliveriesDue(now time.Time) []AlertDeliveryDueWork {
	_ = now
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alertDeliveryQuarantinedLocked() {
		return nil
	}
	data := s.data.AlertDelivery
	if data == nil || s.alertDeliveryEligible == nil || s.alertDeliveryVolatile != nil || data.Health.State == AlertDeliveryHealthOverflow ||
		(data.Health.State == AlertDeliveryHealthUnavailable && !validAlertDeliveryPrerequisiteClass(data.Health.Class)) {
		return nil
	}
	out := make([]AlertDeliveryDueWork, 0)
	for i := range data.Occurrences {
		occurrence := &data.Occurrences[i]
		_, episode, ok := findAlertDeliveryOccurrence(data, occurrence.OccurrenceKey)
		if !ok || !alertDeliveryOccurrenceActive(occurrence, episode) {
			continue
		}
		candidate := alertCandidateFromOccurrence(occurrence)
		if rpc.ValidateAlertCandidate(candidate) != nil || !s.alertDeliveryCandidateEligible(candidate) {
			continue
		}
		out = append(out, AlertDeliveryDueWork{OccurrenceKey: occurrence.OccurrenceKey, Candidate: candidate, DisplayID: occurrence.DisplayID})
	}
	slices.SortFunc(out, func(a, b AlertDeliveryDueWork) int { return cmp.Compare(a.DisplayID, b.DisplayID) })
	return out
}

// SetAlertDeliveryPrerequisiteHealth persists the current dispatcher's
// allowlisted prerequisite posture. An empty class clears only a prior
// prerequisite outage and recomputes health from durable attempts; it cannot
// clear overflow, a state-write failure, or interrupted transport evidence.
func (s *Store) SetAlertDeliveryPrerequisiteHealth(class string, now time.Time) error {
	if class != "" && !validAlertDeliveryPrerequisiteClass(class) {
		return errors.New("invalid alert delivery prerequisite health class")
	}
	now = now.UTC()
	if now.IsZero() {
		return errors.New("alert delivery prerequisite health time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	prior := s.data.AlertDelivery
	if prior == nil || s.alertDeliveryEligible == nil {
		return nil
	}
	if s.alertDeliveryVolatile != nil {
		return ErrAlertDeliveryUnavailable
	}
	if prior.Health.State == AlertDeliveryHealthOverflow ||
		(prior.Health.State == AlertDeliveryHealthUnavailable && prior.Health.Class == AlertDeliveryHealthClassStateWrite) {
		return ErrAlertDeliveryUnavailable
	}
	if class == "" && !validAlertDeliveryPrerequisiteClass(prior.Health.Class) {
		return nil
	}
	if class != "" && prior.Health.State == AlertDeliveryHealthUnavailable && prior.Health.Class == class {
		return nil
	}

	next := cloneAlertDeliveryData(prior)
	if class == "" {
		next.Health = AlertDeliveryHealth{}
		s.recomputeAlertDeliveryHealthLocked(next, now)
	} else {
		next.Health = AlertDeliveryHealth{
			State: AlertDeliveryHealthUnavailable, Class: class, UpdatedAt: now,
			LastAcceptedAt: prior.Health.LastAcceptedAt,
		}
	}
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	return nil
}

func alertDeliveryViewLocked(data *alertDeliveryData, now time.Time) AlertDeliveryView {
	view := AlertDeliveryView{Occurrences: []AlertDeliveryOccurrenceView{}, Attention: AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}}
	if data == nil {
		return view
	}
	view.Initialized = true
	view.Version = data.Version
	view.Generation = data.Generation
	view.AsOf = data.Snapshot.AsOf
	view.CurrentState = data.Snapshot.CurrentState
	view.Coverage = cloneAlertCoverage(data.Snapshot.Coverage)
	view.SourceWatermarks = cloneAlertSourceWatermarks(data.SourceWatermarks)
	for _, occurrence := range data.Occurrences {
		view.Occurrences = append(view.Occurrences, AlertDeliveryOccurrenceView{
			DisplayID: occurrence.DisplayID, Source: occurrence.Source, Kind: occurrence.Kind, State: occurrence.State,
			Severity: occurrence.Severity, DeliveryPreference: occurrence.DeliveryPreference,
			EvidenceHealth: occurrence.EvidenceHealth, Destination: occurrence.Destination,
			EvidenceAsOf: occurrence.EvidenceAsOf, StateChangedAt: occurrence.StateChangedAt,
			FirstSeenAt: occurrence.FirstSeenAt, LastSeenAt: occurrence.LastSeenAt,
			EndedAt: occurrence.EndedAt, EndReason: occurrence.EndReason,
			AttentionSeq: occurrence.AttentionSeq, TransportEligible: occurrence.TransportEligible,
		})
	}
	view.Attention = alertDeliveryAttentionLocked(data)
	view.AttemptTotals = alertDeliveryAttemptTotals(data, now)
	view.DeliveryHealth = data.Health
	return view
}

func alertDeliveryAttentionLocked(data *alertDeliveryData) AlertDeliveryAttention {
	if data == nil {
		return AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}
	}
	type entry struct {
		seq uint64
		ref AlertDeliveryAttentionRef
	}
	entries := make([]entry, 0)
	for _, occurrence := range data.Occurrences {
		if occurrence.AttentionSeq > data.AttentionReadThroughSeq && occurrence.AttentionSeq <= data.AttentionHighWaterSeq {
			entries = append(entries, entry{seq: occurrence.AttentionSeq, ref: AlertDeliveryAttentionRef{DisplayID: occurrence.DisplayID, Source: occurrence.Source, Kind: occurrence.Kind}})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return cmp.Compare(a.seq, b.seq) })
	refs := make([]AlertDeliveryAttentionRef, len(entries))
	for i := range entries {
		refs[i] = entries[i].ref
	}
	return AlertDeliveryAttention{UnreadCount: len(refs), HighWaterSeq: data.AttentionHighWaterSeq, ReadThroughSeq: data.AttentionReadThroughSeq, UnreadRefs: refs}
}

func (s *Store) MarkAlertDeliveryAttentionRead(throughSeq uint64) (AlertDeliveryAttention, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}, err
	}
	data := s.data.AlertDelivery
	if data == nil {
		if throughSeq == 0 {
			return AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}, nil
		}
		return AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}, ErrAlertDeliveryAttentionRead
	}
	if throughSeq < data.AttentionReadThroughSeq || throughSeq > data.AttentionHighWaterSeq || !alertDeliveryAttentionCompleteThrough(data, throughSeq) {
		return alertDeliveryAttentionLocked(data), ErrAlertDeliveryAttentionRead
	}
	if throughSeq == data.AttentionReadThroughSeq {
		return alertDeliveryAttentionLocked(data), nil
	}
	prior := s.data.AlertDelivery
	next := cloneAlertDeliveryData(prior)
	next.AttentionReadThroughSeq = throughSeq
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return alertDeliveryAttentionLocked(prior), err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(time.Now().UTC())
		return alertDeliveryAttentionLocked(prior), err
	}
	s.clearAlertDeliveryVolatileLocked()
	return alertDeliveryAttentionLocked(next), nil
}

func alertDeliveryAttentionCompleteThrough(data *alertDeliveryData, throughSeq uint64) bool {
	seen := make(map[uint64]struct{})
	for _, occurrence := range data.Occurrences {
		seq := occurrence.AttentionSeq
		if seq <= data.AttentionReadThroughSeq || seq > throughSeq {
			continue
		}
		if _, duplicate := seen[seq]; duplicate {
			return false
		}
		seen[seq] = struct{}{}
	}
	return uint64(len(seen)) == throughSeq-data.AttentionReadThroughSeq
}

// AlertDeliveryTargetRef hides device/subscription identities before they
// enter delivery state. A retired subscription must receive a new target ref.
func AlertDeliveryTargetRef(deviceID, subscriptionID string) string {
	return alertDeliveryHash("target", strings.TrimSpace(deviceID), strings.TrimSpace(subscriptionID))
}

// BeginAlertDelivery durably reserves one private occurrence+target attempt
// before a transport caller is allowed to send. DisplayID is never accepted as
// authority and receipt identity is constructed internally.
func (s *Store) BeginAlertDelivery(occurrenceKey, targetRef string, now time.Time) (AlertDeliveryReservation, bool, error) {
	if !validAlertPrivateKey(occurrenceKey, "alert-occurrence-v1:") {
		return AlertDeliveryReservation{}, false, errors.New("invalid private alert occurrence key")
	}
	if !validAlertHash(targetRef) {
		return AlertDeliveryReservation{}, false, errors.New("invalid alert delivery target ref")
	}
	now = now.UTC()
	if now.IsZero() {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery reservation time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return AlertDeliveryReservation{}, false, err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnknownOccurrence
	}
	if prior.Health.State == AlertDeliveryHealthOverflow {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryOverflow
	}
	if prior.Health.State == AlertDeliveryHealthUnavailable {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnavailable
	}
	if s.alertDeliveryVolatile != nil {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnavailable
	}
	occurrence, episode, ok := findAlertDeliveryOccurrence(prior, occurrenceKey)
	if !ok {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnknownOccurrence
	}
	if !alertDeliveryOccurrenceActive(occurrence, episode) || !s.alertDeliveryCandidateEligible(alertCandidateFromOccurrence(occurrence)) {
		return AlertDeliveryReservation{DisplayID: occurrence.DisplayID}, false, nil
	}
	if _, retired := prior.RetiredTargets[targetRef]; retired {
		return AlertDeliveryReservation{DisplayID: occurrence.DisplayID}, false, nil
	}
	receiptKey := alertDeliveryReceiptKey(occurrenceKey, targetRef)
	if alertDeliveryHasReceipt(prior, receiptKey) {
		return AlertDeliveryReservation{DisplayID: occurrence.DisplayID}, false, nil
	}
	// Reserve-before-send must also reserve the possibility of an accepted
	// receipt. Letting transport run at receipt capacity would discover the
	// overflow only after an external acceptance that cannot be persisted.
	if len(prior.Receipts) >= s.alertDeliveryMaxItems {
		return AlertDeliveryReservation{}, false, s.setAlertDeliveryOverflowLocked(prior, now)
	}
	latest, hasLatest := latestAlertDeliveryAttempt(prior, receiptKey)
	if hasLatest {
		if latest.Class == AlertDeliveryAttemptReserved || latest.Class == AlertDeliveryAttemptConfirmed || latest.Class == AlertDeliveryAttemptAccepted || latest.Class == AlertDeliveryAttemptRejected ||
			latest.Class == AlertDeliveryAttemptRetired || latest.Class == AlertDeliveryAttemptInactive || latest.Class == AlertDeliveryAttemptExhausted ||
			latest.Class == AlertDeliveryAttemptUnapproved ||
			(latest.Class == AlertDeliveryAttemptInterrupted && latest.RetryAt.IsZero()) {
			return alertDeliveryReservationView(latest, occurrence.DisplayID), false, nil
		}
		if !latest.RetryAt.IsZero() && now.Before(latest.RetryAt) {
			return alertDeliveryReservationView(latest, occurrence.DisplayID), false, nil
		}
	}
	if len(prior.Attempts) >= s.alertDeliveryMaxItems {
		return AlertDeliveryReservation{}, false, s.setAlertDeliveryOverflowLocked(prior, now)
	}
	next := cloneAlertDeliveryData(prior)
	attemptNumber := 1
	if hasLatest {
		attemptNumber = latest.AttemptNumber + 1
	}
	attempt := alertDeliveryAttempt{
		ID: alertDeliveryAttemptID(receiptKey, attemptNumber, now), OccurrenceKey: occurrenceKey,
		TargetRef: targetRef, ReceiptKey: receiptKey, AttemptNumber: attemptNumber,
		ReservedAt: now, Class: AlertDeliveryAttemptReserved,
	}
	next.Attempts = append(next.Attempts, attempt)
	s.recomputeAlertDeliveryHealthLocked(next, now)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return AlertDeliveryReservation{}, false, err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return AlertDeliveryReservation{}, false, err
	}
	// A successful durable reservation is owned by this process until Confirm
	// and Complete finish or a persistence failure explicitly releases it.
	// In-process orphan recovery must not rewrite a genuinely active caller.
	s.alertDeliveryInFlight[attempt.ID] = true
	s.clearAlertDeliveryVolatileLocked()
	return alertDeliveryReservationView(attempt, occurrence.DisplayID), true, nil
}

func alertDeliveryReservationView(attempt alertDeliveryAttempt, displayID string) AlertDeliveryReservation {
	return AlertDeliveryReservation{AttemptID: attempt.ID, DisplayID: displayID, AttemptNumber: attempt.AttemptNumber, ReservedAt: attempt.ReservedAt, RetryAt: attempt.RetryAt}
}

// ConfirmAlertTransport is the last durable-authority check immediately before
// an external Sender.Send call. Reservation alone never grants transport: a
// recovery or target retirement between Begin and Confirm finalizes the
// attempt without sending. The store cannot hold its lock across Sender.Send;
// a transition after Confirm is consequently recorded as interrupted
// uncertainty rather than proof that transport did or did not occur.
func (s *Store) ConfirmAlertTransport(attemptID string, now time.Time) (AlertDeliveryReservation, bool, error) {
	if !validAlertAttemptID(attemptID) {
		return AlertDeliveryReservation{}, false, errors.New("invalid alert delivery attempt id")
	}
	now = now.UTC()
	if now.IsZero() {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery confirmation time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return AlertDeliveryReservation{}, false, err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery attempt not found")
	}
	attemptIndex := -1
	for i := range prior.Attempts {
		if prior.Attempts[i].ID == attemptID {
			attemptIndex = i
			break
		}
	}
	if attemptIndex < 0 {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery attempt not found")
	}
	stored := prior.Attempts[attemptIndex]
	if now.Before(stored.ReservedAt) {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery confirmation precedes reservation")
	}
	occurrence, episode, found := findAlertDeliveryOccurrence(prior, stored.OccurrenceKey)
	displayID := ""
	if found {
		displayID = occurrence.DisplayID
	}
	if stored.Class != AlertDeliveryAttemptReserved || !s.alertDeliveryInFlight[attemptID] {
		return alertDeliveryReservationView(stored, displayID), false, nil
	}
	class := ""
	retiredAt := time.Time{}
	switch {
	case s.alertDeliveryVolatile != nil:
		delete(s.alertDeliveryInFlight, attemptID)
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnavailable
	case !found || !alertDeliveryOccurrenceActive(occurrence, episode):
		class = AlertDeliveryAttemptInactive
	case !prior.RetiredTargets[stored.TargetRef].IsZero():
		class = AlertDeliveryAttemptRetired
		retiredAt = prior.RetiredTargets[stored.TargetRef]
	case !s.alertDeliveryCandidateEligible(alertCandidateFromOccurrence(occurrence)):
		class = AlertDeliveryAttemptUnapproved
	}
	if class != "" {
		next := cloneAlertDeliveryData(prior)
		attempt := &next.Attempts[attemptIndex]
		attempt.Class = class
		attempt.CompletedAt = now
		attempt.RetiredAt = retiredAt
		if class == AlertDeliveryAttemptInactive {
			attempt.Disposition = AlertDeliveryCompletionInactive
		} else if class == AlertDeliveryAttemptRetired {
			attempt.Disposition = AlertDeliveryCompletionRetired
		}
		s.recomputeAlertDeliveryHealthLocked(next, now)
		if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
			delete(s.alertDeliveryInFlight, attemptID)
			return AlertDeliveryReservation{}, false, err
		}
		s.data.AlertDelivery = next
		if err := s.save(); err != nil {
			s.data.AlertDelivery = prior
			delete(s.alertDeliveryInFlight, attemptID)
			s.noteAlertDeliverySaveFailureLocked(now)
			return AlertDeliveryReservation{}, false, err
		}
		delete(s.alertDeliveryInFlight, attemptID)
		s.clearAlertDeliveryVolatileLocked()
		return alertDeliveryReservationView(*attempt, displayID), false, nil
	}
	// Persist the narrow confirmed-pending-outcome window. A crash or a
	// lifecycle change after this return cannot prove whether Sender.Send ran;
	// recovery therefore reports interrupted_uncertain for this attempt. The
	// caller must use the returned DisplayID as the transport collapse/tag key.
	next := cloneAlertDeliveryData(prior)
	attempt := &next.Attempts[attemptIndex]
	attempt.Class = AlertDeliveryAttemptConfirmed
	s.recomputeAlertDeliveryHealthLocked(next, now)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		delete(s.alertDeliveryInFlight, attemptID)
		return AlertDeliveryReservation{}, false, err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		delete(s.alertDeliveryInFlight, attemptID)
		s.noteAlertDeliverySaveFailureLocked(now)
		return AlertDeliveryReservation{}, false, err
	}
	s.clearAlertDeliveryVolatileLocked()
	confirmed := alertDeliveryReservationView(*attempt, displayID)
	confirmed.Candidate = alertCandidateFromOccurrence(occurrence)
	return confirmed, true, nil
}

// CompleteAlertDelivery completes only a persisted reservation. It rechecks
// occurrence and target authority under the store lock before recording a
// receipt. The caller supplies neither occurrence, target, receipt key nor
// DisplayID, so it cannot redirect acceptance evidence.
func (s *Store) CompleteAlertDelivery(attemptID string, completion AlertDeliveryCompletion, now time.Time) (AlertDeliveryCompletionOutcome, error) {
	if !validAlertAttemptID(attemptID) {
		return AlertDeliveryCompletionOutcome{}, errors.New("invalid alert delivery attempt id")
	}
	if completion != AlertDeliveryCompletionAccepted && completion != AlertDeliveryCompletionRetryable && completion != AlertDeliveryCompletionRejected {
		return AlertDeliveryCompletionOutcome{}, errors.New("invalid alert delivery completion")
	}
	now = now.UTC()
	if now.IsZero() {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery completion time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return AlertDeliveryCompletionOutcome{}, err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery attempt not found")
	}
	attemptIndex := -1
	for i := range prior.Attempts {
		if prior.Attempts[i].ID == attemptID {
			attemptIndex = i
			break
		}
	}
	if attemptIndex < 0 {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery attempt not found")
	}
	stored := prior.Attempts[attemptIndex]
	if now.Before(stored.ReservedAt) {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery completion precedes reservation")
	}
	if stored.Class != AlertDeliveryAttemptConfirmed {
		return AlertDeliveryCompletionOutcome{Disposition: AlertDeliveryCompletionAlreadyComplete, Class: stored.Class, RetryAt: stored.RetryAt}, nil
	}
	if !s.alertDeliveryInFlight[attemptID] {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery transport was not confirmed")
	}
	next := cloneAlertDeliveryData(prior)
	attempt := &next.Attempts[attemptIndex]
	occurrence, episode, active := findAlertDeliveryOccurrence(next, attempt.OccurrenceKey)
	disposition := AlertDeliveryCompletionApplied
	if retiredAt := next.RetiredTargets[attempt.TargetRef]; !retiredAt.IsZero() {
		disposition = AlertDeliveryCompletionRetired
		attempt.RetiredAt = retiredAt
	} else if !active || !alertDeliveryOccurrenceActive(occurrence, episode) {
		disposition = AlertDeliveryCompletionInactive
	}
	attempt.Disposition = disposition
	attempt.CompletedAt = now
	switch completion {
	case AlertDeliveryCompletionAccepted:
		if !alertDeliveryHasAnyReceipt(next, attempt.ReceiptKey) {
			if len(next.Receipts) >= s.alertDeliveryMaxItems {
				err := s.setAlertDeliveryOverflowLocked(prior, now)
				delete(s.alertDeliveryInFlight, attemptID)
				return AlertDeliveryCompletionOutcome{}, err
			}
			next.Receipts = append(next.Receipts, alertDeliveryReceipt{
				OccurrenceKey: attempt.OccurrenceKey, TargetRef: attempt.TargetRef,
				ReceiptKey: attempt.ReceiptKey, AcceptedAt: now, RetiredAt: attempt.RetiredAt,
			})
		}
		attempt.Class = AlertDeliveryAttemptAccepted
	case AlertDeliveryCompletionRetryable:
		if disposition == AlertDeliveryCompletionApplied {
			if delay, ok := alertDeliveryRetryDelay(attempt.AttemptNumber); ok {
				attempt.Class = AlertDeliveryAttemptRetry
				attempt.RetryAt = now.Add(delay)
			} else {
				attempt.Class = AlertDeliveryAttemptExhausted
			}
		} else {
			attempt.Class = AlertDeliveryAttemptRetry
		}
	case AlertDeliveryCompletionRejected:
		attempt.Class = AlertDeliveryAttemptRejected
	}
	s.recomputeAlertDeliveryHealthLocked(next, now)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		delete(s.alertDeliveryInFlight, attemptID)
		return AlertDeliveryCompletionOutcome{}, err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		delete(s.alertDeliveryInFlight, attemptID)
		s.noteAlertDeliverySaveFailureLocked(now)
		return AlertDeliveryCompletionOutcome{}, err
	}
	delete(s.alertDeliveryInFlight, attemptID)
	s.clearAlertDeliveryVolatileLocked()
	return AlertDeliveryCompletionOutcome{Disposition: disposition, Class: attempt.Class, RetryAt: attempt.RetryAt}, nil
}

func alertDeliveryRetryDelay(attemptNumber int) (time.Duration, bool) {
	delays := [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	if attemptNumber < 1 || attemptNumber > len(delays) {
		return 0, false
	}
	return delays[attemptNumber-1], true
}

// RetireAlertDeliveryTarget permanently closes delivery evidence for one
// private target ref. A re-paired/re-subscribed endpoint must use a new ref.
func (s *Store) RetireAlertDeliveryTarget(targetRef string, at time.Time) error {
	if !validAlertHash(targetRef) {
		return errors.New("invalid alert delivery target ref")
	}
	at = at.UTC()
	if at.IsZero() {
		return errors.New("alert delivery target retirement time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return nil
	}
	release, changed, err := s.retireAlertDeliveryTargetsLocked(map[string]bool{targetRef: true}, at)
	if errors.Is(err, ErrAlertDeliveryOverflow) {
		return s.setAlertDeliveryOverflowLocked(prior, at)
	}
	if err != nil || !changed {
		return err
	}
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(at)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	for _, attemptID := range release {
		delete(s.alertDeliveryInFlight, attemptID)
	}
	return nil
}

// retireAlertDeliveryTargetsLocked stages one atomic ledger retirement without
// saving. Callers already hold s.mu and must either persist every surrounding
// device/subscription mutation in the same save or restore the prior ledger.
func (s *Store) retireAlertDeliveryTargetsLocked(targets map[string]bool, at time.Time) ([]string, bool, error) {
	s.initAlertDeliveryRuntime()
	if s.alertDeliveryQuarantinedLocked() {
		// Device/subscription lifecycle remains available to legacy Canary.
		// The quarantined raw ledger is reinserted unchanged by save().
		return nil, false, nil
	}
	prior := s.data.AlertDelivery
	if prior == nil || len(targets) == 0 {
		return nil, false, nil
	}
	if prior.Health.State == AlertDeliveryHealthOverflow {
		return nil, false, ErrAlertDeliveryOverflow
	}
	newTargets := 0
	for target := range targets {
		if !validAlertHash(target) {
			return nil, false, errors.New("invalid alert delivery target ref")
		}
		if prior.RetiredTargets[target].IsZero() {
			newTargets++
		}
	}
	if newTargets == 0 {
		return nil, false, nil
	}
	if len(prior.RetiredTargets)+newTargets > s.alertDeliveryMaxItems {
		return nil, false, ErrAlertDeliveryOverflow
	}
	for _, attempt := range prior.Attempts {
		if targets[attempt.TargetRef] && attempt.RetiredAt.IsZero() &&
			(attempt.Class == AlertDeliveryAttemptReserved || attempt.Class == AlertDeliveryAttemptRetry) && at.Before(attempt.ReservedAt) {
			return nil, false, errors.New("alert delivery retirement precedes reservation")
		}
	}

	next := cloneAlertDeliveryData(prior)
	for target := range targets {
		if next.RetiredTargets[target].IsZero() {
			next.RetiredTargets[target] = at
		}
	}
	release := make([]string, 0)
	for i := range next.Attempts {
		attempt := &next.Attempts[i]
		if !targets[attempt.TargetRef] || !attempt.RetiredAt.IsZero() {
			continue
		}
		attempt.RetiredAt = at
		attempt.RetryAt = time.Time{}
		switch attempt.Class {
		case AlertDeliveryAttemptConfirmed:
			// Sender.Send may already be running. Keep the confirmed attempt
			// completable so known transport truth is never replaced by inference.
			attempt.Disposition = AlertDeliveryCompletionRetired
		case AlertDeliveryAttemptInterrupted:
			// An unknown confirmed outcome remains operationally relevant even
			// after retirement; keep its class while closing future retries.
			attempt.Disposition = AlertDeliveryCompletionRetired
			release = append(release, attempt.ID)
		case AlertDeliveryAttemptReserved, AlertDeliveryAttemptRetry:
			attempt.Class = AlertDeliveryAttemptRetired
			attempt.CompletedAt = at
			attempt.Disposition = AlertDeliveryCompletionRetired
			release = append(release, attempt.ID)
		}
	}
	for i := range next.Receipts {
		if targets[next.Receipts[i].TargetRef] && next.Receipts[i].RetiredAt.IsZero() {
			next.Receipts[i].RetiredAt = at
		}
	}
	s.recomputeAlertDeliveryHealthLocked(next, at)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return nil, false, err
	}
	s.data.AlertDelivery = next
	return release, true, nil
}

func (s *Store) finishAlertDeliveryRetirementLocked(release []string, changed bool) {
	if !changed {
		return
	}
	s.clearAlertDeliveryVolatileLocked()
	for _, attemptID := range release {
		delete(s.alertDeliveryInFlight, attemptID)
	}
}

// RecoverAlertDeliveries converts unowned reserve-before-send records into
// definite no-send retries. Only confirmed-pending-outcome records become
// interrupted uncertainty because Sender is unreachable before Confirm has
// durably committed that class. Both paths use the same bounded retry sequence.
// It is safe both after restart (the ownership map is empty) and in-process
// after Confirm or Complete persistence failures release their reservation.
// Genuinely owned work is never rewritten under an active dispatcher.
func (s *Store) RecoverAlertDeliveries(now time.Time) error {
	now = now.UTC()
	if now.IsZero() {
		return errors.New("alert delivery recovery time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return nil
	}
	next := cloneAlertDeliveryData(prior)
	changed := false
	for i := range next.Attempts {
		attempt := &next.Attempts[i]
		if (attempt.Class != AlertDeliveryAttemptReserved && attempt.Class != AlertDeliveryAttemptConfirmed) || !attempt.CompletedAt.IsZero() {
			continue
		}
		if s.alertDeliveryInFlight[attempt.ID] {
			continue
		}
		if now.Before(attempt.ReservedAt) {
			return errors.New("alert delivery recovery precedes reservation")
		}
		occurrence, episode, active := findAlertDeliveryOccurrence(next, attempt.OccurrenceKey)
		switch {
		case !active || !alertDeliveryOccurrenceActive(occurrence, episode):
			if attempt.Class == AlertDeliveryAttemptConfirmed {
				attempt.Class = AlertDeliveryAttemptInterrupted
			} else {
				attempt.Class = AlertDeliveryAttemptInactive
			}
			attempt.Disposition = AlertDeliveryCompletionInactive
		case !next.RetiredTargets[attempt.TargetRef].IsZero():
			if attempt.Class == AlertDeliveryAttemptConfirmed {
				attempt.Class = AlertDeliveryAttemptInterrupted
			} else {
				attempt.Class = AlertDeliveryAttemptRetired
			}
			attempt.RetiredAt = next.RetiredTargets[attempt.TargetRef]
			attempt.Disposition = AlertDeliveryCompletionRetired
		default:
			delay, retry := alertDeliveryRetryDelay(attempt.AttemptNumber)
			if attempt.Class == AlertDeliveryAttemptConfirmed {
				attempt.Class = AlertDeliveryAttemptInterrupted
				if retry {
					attempt.RetryAt = now.Add(delay)
				}
			} else if retry {
				attempt.Class = AlertDeliveryAttemptRetry
				attempt.RetryAt = now.Add(delay)
			} else {
				attempt.Class = AlertDeliveryAttemptExhausted
			}
		}
		attempt.CompletedAt = now
		changed = true
	}
	probeVolatileRecovery := s.alertDeliveryVolatile != nil
	if !changed && !probeVolatileRecovery {
		return nil
	}
	if changed {
		s.recomputeAlertDeliveryHealthLocked(next, now)
	}
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	return nil
}

// enforceAlertDeliveryRuntimePolicy makes a reopened store default-deny even
// if a previous process persisted an occurrence while an experimental policy
// hook was enabled. Historical delivery attempts and acceptance time remain
// evidence; only current eligibility and healthy/degraded policy posture are
// demoted. Overflow and unavailable remain fail-loud until explicitly healed.
func (s *Store) enforceAlertDeliveryRuntimePolicy(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	if s.alertDeliveryEligible != nil || s.data.AlertDelivery == nil {
		return nil
	}
	prior := s.data.AlertDelivery
	next := cloneAlertDeliveryData(prior)
	changed := false
	for i := range next.Occurrences {
		if next.Occurrences[i].TransportEligible {
			next.Occurrences[i].TransportEligible = false
			changed = true
		}
	}
	if next.Health.State != AlertDeliveryHealthShadow && next.Health.State != AlertDeliveryHealthOverflow && next.Health.State != AlertDeliveryHealthUnavailable {
		next.Health = AlertDeliveryHealth{
			State: AlertDeliveryHealthShadow, Class: AlertDeliveryHealthClassShadow,
			UpdatedAt: now, LastAcceptedAt: next.Health.LastAcceptedAt,
		}
		changed = true
	}
	if !changed {
		return nil
	}
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	return nil
}

func (s *Store) CompactAlertDelivery(now time.Time) error {
	now = now.UTC()
	cutoff := now.Add(-alertDeliveryRetention)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return nil
	}
	next := cloneAlertDeliveryData(prior)
	removedOccurrences := make(map[string]struct{})
	next.Occurrences = slices.DeleteFunc(next.Occurrences, func(occurrence alertDeliveryOccurrence) bool {
		read := occurrence.AttentionSeq == 0 || occurrence.AttentionSeq <= next.AttentionReadThroughSeq
		remove := read && !occurrence.EndedAt.IsZero() && occurrence.EndedAt.Before(cutoff)
		if remove {
			removedOccurrences[occurrence.OccurrenceKey] = struct{}{}
		}
		return remove
	})
	if len(removedOccurrences) > 0 {
		next.Attempts = slices.DeleteFunc(next.Attempts, func(attempt alertDeliveryAttempt) bool {
			_, remove := removedOccurrences[attempt.OccurrenceKey]
			return remove
		})
		next.Receipts = slices.DeleteFunc(next.Receipts, func(receipt alertDeliveryReceipt) bool {
			_, remove := removedOccurrences[receipt.OccurrenceKey]
			return remove
		})
	}
	activeTargets := s.activeAlertDeliveryTargetsLocked()
	retainedTargets := make(map[string]bool, len(next.Attempts)+len(next.Receipts))
	for _, attempt := range next.Attempts {
		retainedTargets[attempt.TargetRef] = true
	}
	for _, receipt := range next.Receipts {
		retainedTargets[receipt.TargetRef] = true
	}
	removedTargets := 0
	for target, retiredAt := range next.RetiredTargets {
		if retiredAt.Before(cutoff) && !activeTargets[target] && !retainedTargets[target] {
			delete(next.RetiredTargets, target)
			removedTargets++
		}
	}
	remainingOccurrences := make(map[string]struct{}, len(next.Occurrences))
	for _, occurrence := range next.Occurrences {
		remainingOccurrences[occurrence.OccurrenceKey] = struct{}{}
	}
	next.Episodes = slices.DeleteFunc(next.Episodes, func(episode alertDeliveryEpisode) bool {
		_, remains := remainingOccurrences[episode.CurrentOccurrenceKey]
		return !remains && episode.State == rpc.AlertEpisodeRecovered
	})
	recoveredCapacity := prior.Health.State == AlertDeliveryHealthOverflow && alertDeliveryBelowCapacity(next, s.alertDeliveryMaxItems)
	if len(removedOccurrences) == 0 && len(next.Episodes) == len(prior.Episodes) && removedTargets == 0 && !recoveredCapacity {
		return nil
	}
	if recoveredCapacity {
		// Overflow is fail-loud but not a permanent human ritual. Only the
		// retention compactor may clear it, and only after every capped ledger
		// collection is demonstrably below its bound.
		next.Health = AlertDeliveryHealth{}
	}
	s.recomputeAlertDeliveryHealthLocked(next, now)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	return nil
}

func (s *Store) activeAlertDeliveryTargetsLocked() map[string]bool {
	activeDevices := make(map[string]bool, len(s.data.Devices))
	for _, device := range s.data.Devices {
		if device.RevokedAt.IsZero() {
			activeDevices[device.ID] = true
		}
	}
	targets := make(map[string]bool, len(s.data.PushSubscriptions))
	for _, subscription := range s.data.PushSubscriptions {
		if activeDevices[subscription.DeviceID] {
			targets[AlertDeliveryTargetRef(subscription.DeviceID, subscription.ID)] = true
		}
	}
	return targets
}

func alertDeliveryBelowCapacity(data *alertDeliveryData, maximum int) bool {
	return data != nil && maximum > 0 && data.Snapshot.Coverage.State != rpc.AlertCoverageUnavailable && len(data.Episodes) < maximum && len(data.Occurrences) < maximum && len(data.Attempts) < maximum && len(data.Receipts) < maximum && len(data.RetiredTargets) < maximum
}

func alertDeliveryAttemptTotals(data *alertDeliveryData, now time.Time) AlertDeliveryAttemptTotals {
	_ = now
	var totals AlertDeliveryAttemptTotals
	latest := make(map[string]alertDeliveryAttempt)
	for _, attempt := range data.Attempts {
		totals.Attempts++
		switch attempt.Class {
		case AlertDeliveryAttemptAccepted:
			totals.Accepted++
		case AlertDeliveryAttemptConfirmed:
			totals.Confirmed++
		case AlertDeliveryAttemptRejected:
			totals.Rejected++
		case AlertDeliveryAttemptInterrupted:
			totals.Interrupted++
		case AlertDeliveryAttemptRetired:
			totals.TargetRetired++
		case AlertDeliveryAttemptInactive:
			totals.Inactive++
		case AlertDeliveryAttemptExhausted:
			totals.RetryExhausted++
		case AlertDeliveryAttemptUnapproved:
			totals.Unapproved++
		}
		latest[attempt.ReceiptKey] = attempt
	}
	for receiptKey, attempt := range latest {
		if alertDeliveryHasReceipt(data, receiptKey) || attempt.RetryAt.IsZero() {
			continue
		}
		occurrence, episode, ok := findAlertDeliveryOccurrence(data, attempt.OccurrenceKey)
		if ok && alertDeliveryOccurrenceActive(occurrence, episode) {
			totals.RetryPending++
		}
	}
	return totals
}

func findAlertDeliveryOccurrence(data *alertDeliveryData, occurrenceKey string) (*alertDeliveryOccurrence, *alertDeliveryEpisode, bool) {
	if data == nil {
		return nil, nil, false
	}
	occurrenceIndex := -1
	for i := range data.Occurrences {
		if data.Occurrences[i].OccurrenceKey == occurrenceKey {
			occurrenceIndex = i
			break
		}
	}
	if occurrenceIndex < 0 {
		return nil, nil, false
	}
	episodeIndex := -1
	for i := range data.Episodes {
		if data.Episodes[i].EpisodeKey == data.Occurrences[occurrenceIndex].EpisodeKey {
			episodeIndex = i
			break
		}
	}
	if episodeIndex < 0 {
		return &data.Occurrences[occurrenceIndex], nil, false
	}
	return &data.Occurrences[occurrenceIndex], &data.Episodes[episodeIndex], true
}

func alertDeliveryOccurrenceActive(occurrence *alertDeliveryOccurrence, episode *alertDeliveryEpisode) bool {
	return occurrence != nil && episode != nil && occurrence.OccurrenceKey == episode.CurrentOccurrenceKey &&
		episode.State != rpc.AlertEpisodeRecovered && occurrence.EndedAt.IsZero()
}

func alertDeliveryHasReceipt(data *alertDeliveryData, receiptKey string) bool {
	for _, receipt := range data.Receipts {
		if receipt.ReceiptKey == receiptKey && receipt.RetiredAt.IsZero() {
			return true
		}
	}
	return false
}

func alertDeliveryHasAnyReceipt(data *alertDeliveryData, receiptKey string) bool {
	for _, receipt := range data.Receipts {
		if receipt.ReceiptKey == receiptKey {
			return true
		}
	}
	return false
}

// recomputeAlertDeliveryHealthLocked derives current transport posture across
// targets. It deliberately does not use the most recent callback as global
// truth: a successful target cannot hide another target's pending retry,
// terminal rejection, or crash ambiguity. Overflow and unavailable are sticky
// fail-loud states; runtime policy absence is always shadow/default-deny.
func (s *Store) recomputeAlertDeliveryHealthLocked(data *alertDeliveryData, now time.Time) {
	if data == nil {
		return
	}
	lastAccepted := data.Health.LastAcceptedAt
	for _, receipt := range data.Receipts {
		if receipt.AcceptedAt.After(lastAccepted) {
			lastAccepted = receipt.AcceptedAt
		}
	}
	if data.Health.State == AlertDeliveryHealthOverflow || data.Health.State == AlertDeliveryHealthUnavailable {
		data.Health.LastAcceptedAt = lastAccepted
		return
	}
	if s.alertDeliveryEligible == nil {
		data.Health = AlertDeliveryHealth{State: AlertDeliveryHealthShadow, Class: AlertDeliveryHealthClassShadow, UpdatedAt: now, LastAcceptedAt: lastAccepted}
		return
	}

	interrupted := false
	latest := make(map[string]alertDeliveryAttempt)
	for _, attempt := range data.Attempts {
		// Confirmed transport with an unknown outcome remains operationally
		// relevant even if recovery or retirement happened afterward. It is
		// intentional uncertainty, not a retry/rejection that can be dismissed
		// merely because the occurrence or target is no longer active.
		if attempt.Class == AlertDeliveryAttemptInterrupted {
			interrupted = true
			continue
		}
		// Retired targets are no longer part of current transport posture.
		// Keep their attempts and terminal outcomes as durable history, but do
		// not let a dead subscription's retry/rejection degrade every active
		// target forever. RetiredAt is stamped on historical attempts; the
		// target map also covers callback races while retirement is persisted.
		if !attempt.RetiredAt.IsZero() || !data.RetiredTargets[attempt.TargetRef].IsZero() {
			continue
		}
		occurrence, episode, active := findAlertDeliveryOccurrence(data, attempt.OccurrenceKey)
		if !active || !alertDeliveryOccurrenceActive(occurrence, episode) {
			// A completed retry/rejection/exhaustion for an inactive occurrence is
			// retained as history but can no longer describe current delivery.
			continue
		}
		latest[attempt.ReceiptKey] = attempt
	}
	class := ""
	if interrupted {
		class = AlertDeliveryHealthClassInterrupted
	} else {
		for _, attempt := range latest {
			switch attempt.Class {
			case AlertDeliveryAttemptExhausted:
				class = AlertDeliveryAttemptExhausted
			case AlertDeliveryAttemptRejected:
				if class == "" || class == AlertDeliveryHealthClassRetry {
					class = AlertDeliveryHealthClassRejected
				}
			case AlertDeliveryAttemptRetry:
				if class == "" {
					class = AlertDeliveryHealthClassRetry
				}
			}
		}
	}
	if class != "" {
		data.Health = AlertDeliveryHealth{State: AlertDeliveryHealthDegraded, Class: class, UpdatedAt: now, LastAcceptedAt: lastAccepted}
		return
	}
	data.Health = AlertDeliveryHealth{State: AlertDeliveryHealthHealthy, UpdatedAt: now, LastAcceptedAt: lastAccepted}
}

func latestAlertDeliveryAttempt(data *alertDeliveryData, receiptKey string) (alertDeliveryAttempt, bool) {
	for _, attempt := range slices.Backward(data.Attempts) {
		if attempt.ReceiptKey == receiptKey {
			return attempt, true
		}
	}
	return alertDeliveryAttempt{}, false
}

func alertDeliveryDisplayID(occurrenceKey string) string {
	sum := sha256.Sum256([]byte("alert-display-v1\x00" + occurrenceKey))
	return fmt.Sprintf("alert-%x", sum[:8])
}

func alertDeliveryAttemptID(receiptKey string, number int, at time.Time) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "alert-attempt-v1\x00%s\x00%d\x00%s", receiptKey, number, at.UTC().Format(time.RFC3339Nano)))
	return fmt.Sprintf("alert-attempt-%x", sum[:8])
}

func alertDeliveryReceiptKey(occurrenceKey, targetRef string) string {
	return alertDeliveryHash("receipt", occurrenceKey, targetRef)
}

func alertDeliveryHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func validAlertHash(value string) bool {
	return validAlertPrivateKey(value, "sha256:")
}

func validAlertPrivateKey(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil && value == strings.ToLower(value)
}

func validAlertAttemptID(value string) bool {
	if !strings.HasPrefix(value, "alert-attempt-") || len(value) != len("alert-attempt-")+16 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "alert-attempt-"))
	return err == nil
}

func cloneAlertDeliveryData(in *alertDeliveryData) *alertDeliveryData {
	if in == nil {
		return nil
	}
	out := *in
	out.Snapshot = cloneAlertSnapshot(in.Snapshot)
	out.SourceWatermarks = cloneAlertSourceWatermarks(in.SourceWatermarks)
	out.Episodes = append([]alertDeliveryEpisode(nil), in.Episodes...)
	out.Occurrences = append([]alertDeliveryOccurrence(nil), in.Occurrences...)
	out.Attempts = append([]alertDeliveryAttempt(nil), in.Attempts...)
	out.Receipts = append([]alertDeliveryReceipt(nil), in.Receipts...)
	out.RetiredTargets = make(map[string]time.Time, len(in.RetiredTargets))
	maps.Copy(out.RetiredTargets, in.RetiredTargets)
	return &out
}

func cloneAlertSnapshot(in rpc.AlertCandidateSnapshot) rpc.AlertCandidateSnapshot {
	out := in
	if in.Candidates != nil {
		out.Candidates = make([]rpc.AlertCandidate, len(in.Candidates))
		copy(out.Candidates, in.Candidates)
	}
	out.Coverage = cloneAlertCoverage(in.Coverage)
	return out
}

func cloneAlertCoverage(in rpc.AlertCoverage) rpc.AlertCoverage {
	out := in
	if in.ExpectedSources != nil {
		out.ExpectedSources = make([]rpc.AlertSource, len(in.ExpectedSources))
		copy(out.ExpectedSources, in.ExpectedSources)
	}
	if in.CoveredSources != nil {
		out.CoveredSources = make([]rpc.AlertSource, len(in.CoveredSources))
		copy(out.CoveredSources, in.CoveredSources)
	}
	return out
}

func cloneAlertSourceWatermarks(in map[rpc.AlertSource]time.Time) map[rpc.AlertSource]time.Time {
	out := make(map[rpc.AlertSource]time.Time, len(in))
	maps.Copy(out, in)
	return out
}

func (s *Store) validateAlertDeliveryState() error {
	data := s.data.AlertDelivery
	if data == nil {
		return nil
	}
	s.initAlertDeliveryRuntime()
	if data.Version != AlertDeliveryVersion || data.Generation == 0 {
		return fmt.Errorf("%w: invalid alert delivery version or generation", ErrInvalidPersistedState)
	}
	if err := rpc.ValidateAlertCandidateSnapshot(data.Snapshot); err != nil {
		return fmt.Errorf("%w: invalid alert delivery snapshot: %v", ErrInvalidPersistedState, err)
	}
	// JSON written before retired_targets became non-omitempty can decode an
	// empty map as nil. Empty is unambiguous and safe to normalize.
	if data.SourceWatermarks == nil {
		data.SourceWatermarks = make(map[rpc.AlertSource]time.Time)
	}
	if data.RetiredTargets == nil {
		data.RetiredTargets = make(map[string]time.Time)
	}
	if len(data.Episodes) > s.alertDeliveryMaxItems || len(data.Occurrences) > s.alertDeliveryMaxItems ||
		len(data.Attempts) > s.alertDeliveryMaxItems || len(data.Receipts) > s.alertDeliveryMaxItems || len(data.RetiredTargets) > s.alertDeliveryMaxItems {
		return fmt.Errorf("%w: alert delivery evidence exceeds capacity", ErrInvalidPersistedState)
	}
	if !validAlertDeliveryHealth(data.Health) {
		return fmt.Errorf("%w: invalid alert delivery health", ErrInvalidPersistedState)
	}
	episodes := make(map[string]alertDeliveryEpisode, len(data.Episodes))
	for _, episode := range data.Episodes {
		if _, duplicate := episodes[episode.EpisodeKey]; duplicate || episode.CurrentOccurrenceKey == "" || episode.FirstSeenAt.IsZero() || episode.LastSeenAt.IsZero() {
			return fmt.Errorf("%w: invalid alert delivery episode", ErrInvalidPersistedState)
		}
		episodes[episode.EpisodeKey] = episode
	}
	displays := map[string]struct{}{}
	occurrences := make(map[string]alertDeliveryOccurrence, len(data.Occurrences))
	unreadSeqs := make(map[uint64]struct{})
	for _, occurrence := range data.Occurrences {
		candidate := rpc.AlertCandidate{
			EpisodeKey: occurrence.EpisodeKey, OccurrenceKey: occurrence.OccurrenceKey,
			EvidenceFingerprint: occurrence.EvidenceFingerprint, Source: occurrence.Source, Kind: occurrence.Kind,
			State: occurrence.State, Severity: occurrence.Severity, DeliveryPreference: occurrence.DeliveryPreference,
			EvidenceHealth: occurrence.EvidenceHealth, Destination: occurrence.Destination,
			EvidenceAsOf: occurrence.EvidenceAsOf, StateChangedAt: occurrence.StateChangedAt, ObservedAt: occurrence.ObservedAt,
		}
		if err := rpc.ValidateAlertCandidate(candidate); err != nil {
			return fmt.Errorf("%w: invalid alert delivery occurrence: %v", ErrInvalidPersistedState, err)
		}
		if occurrence.DisplayID != alertDeliveryDisplayID(occurrence.OccurrenceKey) || occurrence.FirstSeenAt.IsZero() || occurrence.LastSeenAt.IsZero() || occurrence.AttentionSeq == 0 {
			return fmt.Errorf("%w: invalid alert delivery occurrence metadata", ErrInvalidPersistedState)
		}
		if occurrence.FirstSeenAt.After(occurrence.LastSeenAt) || !occurrence.ObservedAt.Equal(occurrence.LastSeenAt) {
			return fmt.Errorf("%w: invalid alert delivery occurrence observation interval", ErrInvalidPersistedState)
		}
		if occurrence.EndedAt.IsZero() {
			if occurrence.EndReason != "" || occurrence.State == rpc.AlertEpisodeRecovered {
				return fmt.Errorf("%w: invalid alert delivery occurrence end state", ErrInvalidPersistedState)
			}
		} else {
			if occurrence.EndedAt.Before(occurrence.StateChangedAt) {
				return fmt.Errorf("%w: invalid alert delivery occurrence end time", ErrInvalidPersistedState)
			}
			coherent := (occurrence.State == rpc.AlertEpisodeRecovered && (occurrence.EndReason == AlertDeliveryEndRecovered || occurrence.EndReason == AlertDeliveryEndOmitted)) ||
				(occurrence.State != rpc.AlertEpisodeRecovered && occurrence.EndReason == AlertDeliveryEndSuperseded)
			if !coherent {
				return fmt.Errorf("%w: invalid alert delivery occurrence end reason", ErrInvalidPersistedState)
			}
		}
		if _, duplicate := occurrences[occurrence.OccurrenceKey]; duplicate {
			return fmt.Errorf("%w: duplicate alert delivery occurrence", ErrInvalidPersistedState)
		}
		if _, duplicate := displays[occurrence.DisplayID]; duplicate {
			return fmt.Errorf("%w: duplicate alert delivery display id", ErrInvalidPersistedState)
		}
		if occurrence.AttentionSeq > data.AttentionHighWaterSeq {
			return fmt.Errorf("%w: alert delivery attention exceeds high-water", ErrInvalidPersistedState)
		}
		if occurrence.AttentionSeq > data.AttentionReadThroughSeq {
			if _, duplicate := unreadSeqs[occurrence.AttentionSeq]; duplicate {
				return fmt.Errorf("%w: duplicate alert delivery attention sequence", ErrInvalidPersistedState)
			}
			unreadSeqs[occurrence.AttentionSeq] = struct{}{}
		}
		occurrences[occurrence.OccurrenceKey] = occurrence
		displays[occurrence.DisplayID] = struct{}{}
	}
	if data.AttentionReadThroughSeq > data.AttentionHighWaterSeq || uint64(len(unreadSeqs)) != data.AttentionHighWaterSeq-data.AttentionReadThroughSeq {
		return fmt.Errorf("%w: invalid alert delivery attention cursor", ErrInvalidPersistedState)
	}
	for _, episode := range episodes {
		occurrence, ok := occurrences[episode.CurrentOccurrenceKey]
		if !ok || occurrence.EpisodeKey != episode.EpisodeKey || occurrence.Source != episode.Source || occurrence.Kind != episode.Kind || occurrence.State != episode.State {
			return fmt.Errorf("%w: alert delivery episode current state mismatch", ErrInvalidPersistedState)
		}
	}
	attemptIDs := map[string]struct{}{}
	nextAttemptNumber := map[string]int{}
	previousByReceipt := map[string]alertDeliveryAttempt{}
	acceptedByReceipt := map[string]time.Time{}
	latestByReceipt := map[string]alertDeliveryAttempt{}
	for _, attempt := range data.Attempts {
		if !validAlertAttemptID(attempt.ID) || !validAlertHash(attempt.TargetRef) || attempt.ReceiptKey != alertDeliveryReceiptKey(attempt.OccurrenceKey, attempt.TargetRef) || attempt.AttemptNumber < 1 || attempt.AttemptNumber > 4 || attempt.ReservedAt.IsZero() || !validAlertDeliveryAttemptClass(attempt.Class) {
			return fmt.Errorf("%w: invalid alert delivery attempt", ErrInvalidPersistedState)
		}
		expectedNumber := nextAttemptNumber[attempt.ReceiptKey] + 1
		if attempt.AttemptNumber != expectedNumber || attempt.ID != alertDeliveryAttemptID(attempt.ReceiptKey, attempt.AttemptNumber, attempt.ReservedAt) {
			return fmt.Errorf("%w: invalid alert delivery attempt sequence or identity", ErrInvalidPersistedState)
		}
		if !validAlertDeliveryAttemptLifecycle(attempt) {
			return fmt.Errorf("%w: invalid alert delivery attempt lifecycle", ErrInvalidPersistedState)
		}
		retiredAt := data.RetiredTargets[attempt.TargetRef]
		if !attempt.RetiredAt.IsZero() && (retiredAt.IsZero() || !attempt.RetiredAt.Equal(retiredAt)) {
			return fmt.Errorf("%w: alert delivery attempt retirement mismatches target tombstone", ErrInvalidPersistedState)
		}
		if previous, ok := previousByReceipt[attempt.ReceiptKey]; ok {
			if !validAlertDeliveryAttemptTransition(previous, attempt, data.RetiredTargets) {
				return fmt.Errorf("%w: invalid alert delivery attempt transition chain", ErrInvalidPersistedState)
			}
		}
		if _, ok := occurrences[attempt.OccurrenceKey]; !ok {
			return fmt.Errorf("%w: alert delivery attempt occurrence missing", ErrInvalidPersistedState)
		}
		if _, duplicate := attemptIDs[attempt.ID]; duplicate {
			return fmt.Errorf("%w: duplicate alert delivery attempt id", ErrInvalidPersistedState)
		}
		attemptIDs[attempt.ID] = struct{}{}
		nextAttemptNumber[attempt.ReceiptKey] = attempt.AttemptNumber
		previousByReceipt[attempt.ReceiptKey] = attempt
		latestByReceipt[attempt.ReceiptKey] = attempt
		if attempt.Class == AlertDeliveryAttemptAccepted {
			if attempt.CompletedAt.IsZero() {
				return fmt.Errorf("%w: accepted alert delivery attempt is incomplete", ErrInvalidPersistedState)
			}
			if _, duplicate := acceptedByReceipt[attempt.ReceiptKey]; duplicate {
				return fmt.Errorf("%w: duplicate accepted alert delivery attempt", ErrInvalidPersistedState)
			}
			acceptedByReceipt[attempt.ReceiptKey] = attempt.CompletedAt
		}
	}
	receiptKeys := map[string]struct{}{}
	for _, receipt := range data.Receipts {
		if !validAlertHash(receipt.TargetRef) || receipt.ReceiptKey != alertDeliveryReceiptKey(receipt.OccurrenceKey, receipt.TargetRef) || receipt.AcceptedAt.IsZero() {
			return fmt.Errorf("%w: invalid alert delivery receipt", ErrInvalidPersistedState)
		}
		if _, ok := occurrences[receipt.OccurrenceKey]; !ok {
			return fmt.Errorf("%w: alert delivery receipt occurrence missing", ErrInvalidPersistedState)
		}
		if _, duplicate := receiptKeys[receipt.ReceiptKey]; duplicate {
			return fmt.Errorf("%w: duplicate alert delivery receipt", ErrInvalidPersistedState)
		}
		latest, ok := latestByReceipt[receipt.ReceiptKey]
		if !ok || latest.Class != AlertDeliveryAttemptAccepted || !latest.CompletedAt.Equal(receipt.AcceptedAt) {
			return fmt.Errorf("%w: alert delivery receipt lacks matching latest acceptance", ErrInvalidPersistedState)
		}
		retiredAt := data.RetiredTargets[receipt.TargetRef]
		if (!receipt.RetiredAt.IsZero() && (retiredAt.IsZero() || !receipt.RetiredAt.Equal(retiredAt))) || !receipt.RetiredAt.Equal(latest.RetiredAt) {
			return fmt.Errorf("%w: alert delivery receipt retirement mismatches latest acceptance", ErrInvalidPersistedState)
		}
		receiptKeys[receipt.ReceiptKey] = struct{}{}
	}
	for receiptKey, acceptedAt := range acceptedByReceipt {
		if _, ok := receiptKeys[receiptKey]; !ok || acceptedAt.IsZero() {
			return fmt.Errorf("%w: accepted alert delivery attempt lacks receipt", ErrInvalidPersistedState)
		}
	}
	for target, retiredAt := range data.RetiredTargets {
		if !validAlertHash(target) || retiredAt.IsZero() {
			return fmt.Errorf("%w: invalid alert delivery retired target", ErrInvalidPersistedState)
		}
	}
	for source, watermark := range data.SourceWatermarks {
		if !validAlertDeliverySource(source) || watermark.IsZero() {
			return fmt.Errorf("%w: invalid alert delivery source watermark", ErrInvalidPersistedState)
		}
	}
	return nil
}

func validAlertDeliveryAttemptTransition(previous, current alertDeliveryAttempt, retiredTargets map[string]time.Time) bool {
	if (previous.Class == AlertDeliveryAttemptRetry || previous.Class == AlertDeliveryAttemptInterrupted) &&
		!previous.RetryAt.IsZero() && !current.ReservedAt.Before(previous.RetryAt) {
		return true
	}

	// Retirement closes all attempts for a target, including historical retry
	// evidence. The writer therefore clears RetryAt and rewrites retryable
	// predecessors after a successor may already have been reserved. Accept
	// that otherwise-terminal predecessor only when the exact target tombstone
	// stamps both records and proves the successor existed by retirement time.
	retiredAt := retiredTargets[previous.TargetRef]
	if retiredAt.IsZero() || !previous.RetiredAt.Equal(retiredAt) || !current.RetiredAt.Equal(retiredAt) ||
		previous.Disposition != AlertDeliveryCompletionRetired || !previous.RetryAt.IsZero() || current.ReservedAt.After(retiredAt) {
		return false
	}
	return previous.Class == AlertDeliveryAttemptRetired || previous.Class == AlertDeliveryAttemptInterrupted
}

func validAlertDeliveryAttemptLifecycle(attempt alertDeliveryAttempt) bool {
	completed := !attempt.CompletedAt.IsZero()
	retrying := !attempt.RetryAt.IsZero()
	retired := !attempt.RetiredAt.IsZero()
	if completed && attempt.CompletedAt.Before(attempt.ReservedAt) {
		return false
	}

	scheduledRetry := func() bool {
		delay, ok := alertDeliveryRetryDelay(attempt.AttemptNumber)
		return ok && retrying && attempt.RetryAt.Equal(attempt.CompletedAt.Add(delay))
	}

	switch attempt.Class {
	case AlertDeliveryAttemptReserved:
		return !completed && !retrying && !retired && attempt.Disposition == ""
	case AlertDeliveryAttemptConfirmed:
		return !completed && !retrying && ((!retired && attempt.Disposition == "") || (retired && attempt.Disposition == AlertDeliveryCompletionRetired))
	case AlertDeliveryAttemptAccepted, AlertDeliveryAttemptRejected:
		if !completed || retrying {
			return false
		}
		switch attempt.Disposition {
		case AlertDeliveryCompletionApplied, AlertDeliveryCompletionInactive:
			// A later target retirement stamps RetiredAt while preserving the
			// transport-time disposition, so retirement is optional here.
			return true
		case AlertDeliveryCompletionRetired:
			return retired
		default:
			return false
		}
	case AlertDeliveryAttemptRetry:
		if !completed {
			return false
		}
		switch attempt.Disposition {
		case "", AlertDeliveryCompletionApplied:
			return !retired && scheduledRetry()
		case AlertDeliveryCompletionInactive:
			return !retired && !retrying
		case AlertDeliveryCompletionRetired:
			return retired && !retrying
		default:
			return false
		}
	case AlertDeliveryAttemptInterrupted:
		if !completed {
			return false
		}
		switch attempt.Disposition {
		case "":
			if retired {
				return false
			}
			if _, ok := alertDeliveryRetryDelay(attempt.AttemptNumber); ok {
				return scheduledRetry()
			}
			return !retrying
		case AlertDeliveryCompletionInactive:
			return !retrying
		case AlertDeliveryCompletionRetired:
			return retired && !retrying
		default:
			return false
		}
	case AlertDeliveryAttemptRetired:
		return completed && !retrying && retired && attempt.Disposition == AlertDeliveryCompletionRetired
	case AlertDeliveryAttemptInactive:
		return completed && !retrying && attempt.Disposition == AlertDeliveryCompletionInactive
	case AlertDeliveryAttemptExhausted:
		return attempt.AttemptNumber == 4 && completed && !retrying && (attempt.Disposition == "" || attempt.Disposition == AlertDeliveryCompletionApplied)
	case AlertDeliveryAttemptUnapproved:
		return completed && !retrying && attempt.Disposition == ""
	default:
		return false
	}
}

func validAlertDeliveryAttemptClass(class string) bool {
	switch class {
	case AlertDeliveryAttemptReserved, AlertDeliveryAttemptConfirmed, AlertDeliveryAttemptAccepted, AlertDeliveryAttemptRetry,
		AlertDeliveryAttemptRejected, AlertDeliveryAttemptInterrupted, AlertDeliveryAttemptRetired,
		AlertDeliveryAttemptInactive, AlertDeliveryAttemptExhausted, AlertDeliveryAttemptUnapproved:
		return true
	default:
		return false
	}
}

func validAlertDeliverySource(source rpc.AlertSource) bool {
	switch source {
	case rpc.AlertSourceCanary, rpc.AlertSourceRegime, rpc.AlertSourceRulebook, rpc.AlertSourceRiskPolicy,
		rpc.AlertSourceProtection, rpc.AlertSourceOrderIntegrity, rpc.AlertSourceReconciliation,
		rpc.AlertSourceGovernance, rpc.AlertSourceDataHealth, rpc.AlertSourceDelivery:
		return true
	default:
		return false
	}
}

func validAlertDeliveryHealth(health AlertDeliveryHealth) bool {
	if health.UpdatedAt.IsZero() {
		return false
	}
	switch health.State {
	case AlertDeliveryHealthShadow:
		return health.Class == AlertDeliveryHealthClassShadow
	case AlertDeliveryHealthHealthy:
		return health.Class == ""
	case AlertDeliveryHealthDegraded:
		switch health.Class {
		case AlertDeliveryHealthClassRetry, AlertDeliveryHealthClassRejected, AlertDeliveryHealthClassInterrupted, AlertDeliveryAttemptExhausted:
			return true
		default:
			return false
		}
	case AlertDeliveryHealthUnavailable:
		return health.Class == AlertDeliveryHealthClassStateWrite || validAlertDeliveryPrerequisiteClass(health.Class)
	case AlertDeliveryHealthOverflow:
		return health.Class == AlertDeliveryHealthClassOverflow
	default:
		return false
	}
}

func validAlertDeliveryPrerequisiteClass(class string) bool {
	switch class {
	case AlertDeliveryHealthClassNoSubscription, AlertDeliveryHealthClassSigningKeys, AlertDeliveryHealthClassSender:
		return true
	default:
		return false
	}
}
