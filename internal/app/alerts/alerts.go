package alerts

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/push"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Dispatcher is the sole app-side alert delivery owner. Its mutex keeps the
// complete Observe -> Due -> Begin -> Confirm -> Send -> Complete sequence
// single-threaded while the store remains the durable authority at every
// transition.
type Dispatcher struct {
	Store       *state.Store
	Sender      push.Sender
	URL         string
	Now         func() time.Time
	SendTimeout time.Duration

	mu sync.Mutex
}

// Current returns the persisted redacted alert view used to prime a live
// service after restart. It never observes producer state or sends transport.
func (d *Dispatcher) Current(now time.Time) state.AlertDeliveryView {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return state.AlertDeliveryView{}
	}
	return d.Store.AlertDelivery(now.UTC())
}

// Observe atomically applies one daemon-authored snapshot and then dispatches
// only work the durable ledger still authorizes. It never evaluates risk or
// accepts producer-authored notification copy.
func (d *Dispatcher) Observe(ctx context.Context, snapshot rpc.AlertCandidateSnapshot) (state.AlertDeliveryView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return state.AlertDeliveryView{}, nil
	}
	if _, err := d.Store.ObserveAlertSnapshot(snapshot); err != nil {
		// A zero view distinguishes observation failure from a later dispatch
		// failure. Live may retain producer evidence only when the returned view
		// attests that this exact scope and timestamp were durably applied.
		return state.AlertDeliveryView{}, err
	}
	return d.dispatchLocked(ctx)
}

// DispatchPending retries already-persisted due work without fabricating a
// fresh producer observation. Observe is the normal poll path; this method is
// useful for a bounded startup or scheduler retry.
func (d *Dispatcher) DispatchPending(ctx context.Context) (state.AlertDeliveryView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return state.AlertDeliveryView{}, nil
	}
	return d.dispatchLocked(ctx)
}

func (d *Dispatcher) dispatchLocked(ctx context.Context) (state.AlertDeliveryView, error) {
	now := d.now()
	if err := d.Store.RecoverAlertDeliveries(now); err != nil {
		return d.Store.AlertDelivery(now), err
	}
	if err := d.Store.CompactAlertDelivery(now); err != nil {
		return d.Store.AlertDelivery(now), err
	}
	due := d.Store.AlertDeliveriesDue(now)
	if len(due) == 0 {
		if err := d.Store.SetAlertDeliveryPrerequisiteHealth("", now); err != nil {
			return d.Store.AlertDelivery(now), err
		}
		return d.Store.AlertDelivery(now), nil
	}

	subscriptions := d.Store.ActivePushSubscriptions()
	if len(subscriptions) == 0 {
		return d.failPrerequisite(now, state.AlertDeliveryHealthClassNoSubscription)
	}
	keys, hasKeys := d.Store.VAPID()
	if !hasKeys || strings.TrimSpace(keys.PublicKey) == "" || strings.TrimSpace(keys.PrivateKey) == "" {
		return d.failPrerequisite(now, state.AlertDeliveryHealthClassSigningKeys)
	}
	if d.Sender == nil {
		return d.failPrerequisite(now, state.AlertDeliveryHealthClassSender)
	}
	if err := d.Store.SetAlertDeliveryPrerequisiteHealth("", now); err != nil {
		return d.Store.AlertDelivery(now), err
	}

	for _, work := range due {
		for _, subscription := range subscriptions {
			target := state.AlertDeliveryTargetRef(subscription.DeviceID, subscription.ID)
			reservation, send, err := d.Store.BeginAlertDelivery(work.OccurrenceKey, target, d.now())
			if err != nil {
				return d.Store.AlertDelivery(d.now()), err
			}
			if !send {
				continue
			}
			// Nothing authority-relevant may occur between this durable recheck
			// and the external sender call.
			confirmed, allowed, err := d.Store.ConfirmAlertTransport(reservation.AttemptID, d.now())
			if err != nil {
				return d.Store.AlertDelivery(d.now()), err
			}
			if !allowed {
				continue
			}
			if confirmed.DisplayID != work.DisplayID {
				if _, err := d.Store.CompleteAlertDelivery(confirmed.AttemptID, state.AlertDeliveryCompletionRejected, d.now()); err != nil {
					return d.Store.AlertDelivery(d.now()), err
				}
				return d.Store.AlertDelivery(d.now()), fmt.Errorf("alert display authority changed before send")
			}
			presentation, ok := PresentationFor(confirmed.Candidate.PresentationCode, confirmed.Candidate.State)
			if !ok {
				if _, err := d.Store.CompleteAlertDelivery(confirmed.AttemptID, state.AlertDeliveryCompletionRejected, d.now()); err != nil {
					return d.Store.AlertDelivery(d.now()), err
				}
				return d.Store.AlertDelivery(d.now()), fmt.Errorf("unsupported alert presentation code %q", confirmed.Candidate.PresentationCode)
			}
			payload := push.Payload{
				Title: presentation.Title, Body: presentation.Body,
				Severity: string(confirmed.Candidate.Severity), Kind: string(confirmed.Candidate.Kind),
				Destination: string(confirmed.Candidate.Destination), DisplayID: confirmed.DisplayID, URL: d.URL,
			}
			sendCtx, cancel := context.WithTimeout(ctx, d.sendTimeout())
			result := d.Sender.Send(sendCtx, subscription, keys, payload)
			cancel()
			completion, dead := classifyAlertCompletion(result)
			completedAt := d.now()
			if _, err := d.Store.CompleteAlertDelivery(confirmed.AttemptID, completion, completedAt); err != nil {
				return d.Store.AlertDelivery(completedAt), err
			}
			if dead {
				if err := d.Store.RemovePushSubscriptionAt(subscription.ID, completedAt); err != nil {
					return d.Store.AlertDelivery(completedAt), err
				}
			}
		}
	}
	return d.Store.AlertDelivery(d.now()), nil
}

func (d *Dispatcher) failPrerequisite(now time.Time, class string) (state.AlertDeliveryView, error) {
	err := d.Store.SetAlertDeliveryPrerequisiteHealth(class, now)
	return d.Store.AlertDelivery(now), err
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d *Dispatcher) sendTimeout() time.Duration {
	if d.SendTimeout > 0 {
		return d.SendTimeout
	}
	return 10 * time.Second
}

func classifyAlertCompletion(result state.PushAttempt) (state.AlertDeliveryCompletion, bool) {
	if result.OK && result.Class == state.GovernanceTransportAccepted {
		return state.AlertDeliveryCompletionAccepted, false
	}
	switch result.Class {
	case state.GovernanceTransportDeadlineRetry, state.GovernanceTransportCanceledRetry,
		state.GovernanceTransportNetworkRetry, state.GovernanceTransportHTTPRetry,
		state.GovernanceTransportTimeoutRetry:
		return state.AlertDeliveryCompletionRetryable, false
	case state.GovernanceTransportDead:
		return state.AlertDeliveryCompletionRejected, true
	default:
		return state.AlertDeliveryCompletionRejected, false
	}
}
