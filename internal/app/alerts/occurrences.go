package alerts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/push"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	defaultOccurrenceSendTimeout   = 10 * time.Second
	defaultOccurrenceSweepInterval = time.Minute
)

// Source-neutral dispatcher errors intentionally omit occurrence, target,
// subscription, endpoint, and transport-error identity.
var (
	ErrOccurrenceDispatcherUnavailable = errors.New("alert occurrence dispatcher unavailable")
	ErrOccurrenceObservationFailed     = errors.New("alert occurrence observation failed")
	ErrOccurrenceDeliveryState         = errors.New("alert occurrence delivery state unavailable")
	ErrOccurrencePayload               = errors.New("alert occurrence payload unavailable")
)

// OccurrenceStore is the complete state authority needed by the source-neutral
// dispatcher. It deliberately omits the store's private eligibility seam: a
// production Store with no approved policy returns no due work, which makes
// Sender mechanically unreachable.
type OccurrenceStore interface {
	ObserveAlertSnapshot(rpc.AlertCandidateSnapshot) (state.AlertDeliveryView, error)
	RecoverAlertDeliveries(time.Time) error
	CompactAlertDelivery(time.Time) error
	SetAlertDeliveryPrerequisiteHealth(string, time.Time) error
	AlertDeliveriesDue(time.Time) []state.AlertDeliveryDueWork
	ActivePushSubscriptions() []state.PushSubscription
	VAPID() (state.VAPIDKeys, bool)
	BeginAlertDelivery(string, string, time.Time) (state.AlertDeliveryReservation, bool, error)
	ConfirmAlertTransport(string, time.Time) (state.AlertDeliveryReservation, bool, error)
	CompleteAlertDelivery(string, state.AlertDeliveryCompletion, time.Time) (state.AlertDeliveryCompletionOutcome, error)
	RemovePushSubscriptionAt(string, time.Time) error
}

// OccurrenceDispatchResult is safe to expose to orchestration and tests. It
// contains counts and dispositions only: no producer, attempt, target,
// subscription, endpoint, key, or transport-error identity.
type OccurrenceDispatchResult struct {
	Due       int `json:"due"`
	Reserved  int `json:"reserved"`
	Sent      int `json:"sent"`
	Accepted  int `json:"accepted"`
	Retryable int `json:"retryable"`
	Rejected  int `json:"rejected"`
	Retired   int `json:"retired"`
	Inactive  int `json:"inactive"`
	Skipped   int `json:"skipped"`
	Deferred  int `json:"deferred"`
}

// OccurrenceDispatcher persists daemon-authored snapshots synchronously and
// transports only work reconstructed from durable app state. Wakeups are
// hints; dropping or coalescing one can delay a sweep but cannot lose a
// snapshot, occurrence, reservation, retry, or receipt.
type OccurrenceDispatcher struct {
	Store         OccurrenceStore
	Sender        push.Sender
	Now           func() time.Time
	SendTimeout   time.Duration
	SweepInterval time.Duration

	wakeOnce sync.Once
	wake     chan struct{}
	runOnce  sync.Once
	run      chan struct{}
}

// Observe commits the complete producer snapshot before it signals delivery.
// The caller context is intentionally not used to gate this local durable
// observation: delivery cancellation must not discard lifecycle authority.
func (d *OccurrenceDispatcher) Observe(_ context.Context, snapshot rpc.AlertCandidateSnapshot) (state.AlertDeliveryView, error) {
	if d == nil || d.Store == nil {
		return state.AlertDeliveryView{}, ErrOccurrenceDispatcherUnavailable
	}
	view, err := d.Store.ObserveAlertSnapshot(snapshot)
	if err != nil {
		return state.AlertDeliveryView{}, ErrOccurrenceObservationFailed
	}
	d.signalWake()
	return view, nil
}

// Pending reports whether one coalesced sweep hint is waiting. It says
// nothing about durable due work and must never be used as delivery authority.
func (d *OccurrenceDispatcher) Pending() int {
	if d == nil {
		return 0
	}
	return len(d.wakeChannel())
}

// DispatchDue performs one bounded scan of the durable due set. Concurrent
// callers serialize, and a caller waiting for the sweep token remains
// cancelable. Each occurrence-target pair is considered at most once in this
// scan; retry timing remains wholly store-owned.
func (d *OccurrenceDispatcher) DispatchDue(ctx context.Context) (OccurrenceDispatchResult, error) {
	var result OccurrenceDispatchResult
	confirmedAttemptID := ""
	defer func() {
		if confirmedAttemptID != "" && d != nil && d.Store != nil {
			// Confirm is the point where transport outcome becomes uncertain. Any
			// return or panic before the normal completion path must release process
			// ownership durably; no-send setup failures use the bounded retry path.
			_, _ = d.Store.CompleteAlertDelivery(confirmedAttemptID, state.AlertDeliveryCompletionRetryable, d.now())
		}
	}()
	if d == nil || d.Store == nil {
		return result, ErrOccurrenceDispatcherUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case d.runChannel() <- struct{}{}:
		defer func() { <-d.runChannel() }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	now := d.now()
	if err := d.Store.RecoverAlertDeliveries(now); err != nil {
		return result, ErrOccurrenceDeliveryState
	}
	if err := d.Store.CompactAlertDelivery(now); err != nil {
		return result, ErrOccurrenceDeliveryState
	}
	due := d.Store.AlertDeliveriesDue(now)
	result.Due = len(due)
	if len(due) == 0 {
		if err := d.Store.SetAlertDeliveryPrerequisiteHealth("", now); err != nil {
			return result, ErrOccurrenceDeliveryState
		}
		return result, nil
	}

	subscriptions := d.Store.ActivePushSubscriptions()
	keys, hasKeys := d.Store.VAPID()
	prerequisiteClass := ""
	switch {
	case len(subscriptions) == 0:
		prerequisiteClass = state.AlertDeliveryHealthClassNoSubscription
	case !hasKeys || strings.TrimSpace(keys.PublicKey) == "" || strings.TrimSpace(keys.PrivateKey) == "":
		prerequisiteClass = state.AlertDeliveryHealthClassSigningKeys
	case d.Sender == nil:
		prerequisiteClass = state.AlertDeliveryHealthClassSender
	}
	if prerequisiteClass != "" {
		if err := d.Store.SetAlertDeliveryPrerequisiteHealth(prerequisiteClass, now); err != nil {
			return result, ErrOccurrenceDeliveryState
		}
		result.Deferred = len(due)
		return result, nil
	}
	if err := d.Store.SetAlertDeliveryPrerequisiteHealth("", now); err != nil {
		return result, ErrOccurrenceDeliveryState
	}

	for _, work := range due {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		for _, subscription := range subscriptions {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			targetRef := state.AlertDeliveryTargetRef(subscription.DeviceID, subscription.ID)
			if err := push.ValidateSubscription(subscription); err != nil {
				if err := d.retireSubscription(subscription.ID, d.now()); err != nil {
					return result, err
				}
				result.Retired++
				continue
			}

			reservation, send, err := d.Store.BeginAlertDelivery(work.OccurrenceKey, targetRef, d.now())
			if err != nil {
				return result, ErrOccurrenceDeliveryState
			}
			if !send {
				result.Skipped++
				continue
			}
			result.Reserved++

			sendCtx, cancel := context.WithTimeout(ctx, d.sendTimeout())
			confirmed, allowed, err := d.Store.ConfirmAlertTransport(reservation.AttemptID, d.now())
			if err != nil {
				cancel()
				return result, ErrOccurrenceDeliveryState
			}
			if !allowed {
				cancel()
				result.Skipped++
				continue
			}
			confirmedAttemptID = reservation.AttemptID
			// The store returns the exact current candidate checked atomically with
			// confirmation. Building from pre-scan work here would let a same-
			// occurrence severity or destination revision send stale copy and then
			// suppress the corrected payload through receipt dedupe.
			payload, err := AlertPushPayload(confirmed.Candidate, confirmed.DisplayID)
			if err != nil {
				cancel()
				return result, ErrOccurrencePayload
			}
			attempt := d.Sender.Send(sendCtx, subscription, keys, payload)
			cancel()
			result.Sent++

			completion, retire := occurrenceCompletion(attempt)
			outcome, err := d.Store.CompleteAlertDelivery(reservation.AttemptID, completion, d.now())
			confirmedAttemptID = ""
			if err != nil {
				return result, ErrOccurrenceDeliveryState
			}
			switch completion {
			case state.AlertDeliveryCompletionAccepted:
				result.Accepted++
			case state.AlertDeliveryCompletionRetryable:
				result.Retryable++
			case state.AlertDeliveryCompletionRejected:
				result.Rejected++
			}
			switch outcome.Disposition {
			case state.AlertDeliveryCompletionInactive:
				result.Inactive++
			case state.AlertDeliveryCompletionRetired:
				result.Retired++
			}
			if retire {
				if err := d.retireSubscription(subscription.ID, d.now()); err != nil {
					return result, err
				}
				if outcome.Disposition != state.AlertDeliveryCompletionRetired {
					result.Retired++
				}
			}
			if err := ctx.Err(); err != nil {
				return result, err
			}
		}
	}
	if len(d.Store.ActivePushSubscriptions()) == 0 {
		if err := d.Store.SetAlertDeliveryPrerequisiteHealth(state.AlertDeliveryHealthClassNoSubscription, d.now()); err != nil {
			return result, ErrOccurrenceDeliveryState
		}
	}
	return result, nil
}

// Run performs a restart sweep immediately, then one bounded sweep for each
// coalesced observation wake or timer tick. Due work is always reloaded from
// durable state, so retries continue after restart without a fresh snapshot.
func (d *OccurrenceDispatcher) Run(ctx context.Context) error {
	if d == nil || d.Store == nil {
		return ErrOccurrenceDispatcherUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := d.DispatchDue(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(d.sweepInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.wakeChannel():
			if _, err := d.DispatchDue(ctx); err != nil {
				return err
			}
		case <-ticker.C:
			if _, err := d.DispatchDue(ctx); err != nil {
				return err
			}
		}
	}
}

func (d *OccurrenceDispatcher) signalWake() {
	select {
	case d.wakeChannel() <- struct{}{}:
	default:
	}
}

func (d *OccurrenceDispatcher) wakeChannel() chan struct{} {
	d.wakeOnce.Do(func() { d.wake = make(chan struct{}, 1) })
	return d.wake
}

func (d *OccurrenceDispatcher) runChannel() chan struct{} {
	d.runOnce.Do(func() { d.run = make(chan struct{}, 1) })
	return d.run
}

func (d *OccurrenceDispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d *OccurrenceDispatcher) sendTimeout() time.Duration {
	if d.SendTimeout > 0 {
		return d.SendTimeout
	}
	return defaultOccurrenceSendTimeout
}

func (d *OccurrenceDispatcher) sweepInterval() time.Duration {
	if d.SweepInterval > 0 {
		return d.SweepInterval
	}
	return defaultOccurrenceSweepInterval
}

func (d *OccurrenceDispatcher) retireSubscription(subscriptionID string, at time.Time) error {
	// Store owns the single atomic mutation that retires both delivery ledgers
	// and removes the raw subscription. A partial retirement must never escape.
	if err := d.Store.RemovePushSubscriptionAt(subscriptionID, at); err != nil {
		return ErrOccurrenceDeliveryState
	}
	return nil
}

func occurrenceCompletion(attempt state.PushAttempt) (state.AlertDeliveryCompletion, bool) {
	// A receipt is transport truth, so the sender's accepted class alone is
	// insufficient when its explicit status says the request was not accepted.
	if attempt.OK && attempt.Class == state.GovernanceTransportAccepted {
		return state.AlertDeliveryCompletionAccepted, false
	}
	switch attempt.Class {
	case state.GovernanceTransportDeadlineRetry,
		state.GovernanceTransportCanceledRetry,
		state.GovernanceTransportNetworkRetry,
		state.GovernanceTransportHTTPRetry,
		state.GovernanceTransportTimeoutRetry,
		state.GovernanceTransportSenderMissing:
		return state.AlertDeliveryCompletionRetryable, false
	case state.GovernanceTransportDead, state.GovernanceTransportMissingKeys:
		return state.AlertDeliveryCompletionRejected, true
	default:
		return state.AlertDeliveryCompletionRejected, false
	}
}
