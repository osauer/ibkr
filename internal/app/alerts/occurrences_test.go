package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/push"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

var _ OccurrenceStore = (*state.Store)(nil)

func TestOccurrenceObservePersistsSnapshotWhenWakeAlreadyFull(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.July, 20, 22, 0, 0, 0, time.UTC)
	candidate := occurrenceCandidate(t, at)
	snapshot := occurrenceSnapshot(at, candidate)
	dispatcher := &OccurrenceDispatcher{Store: store, Now: func() time.Time { return at }}
	dispatcher.signalWake()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	view, err := dispatcher.Observe(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Initialized || !view.AsOf.Equal(at) || dispatcher.Pending() != 1 {
		t.Fatalf("observation was not committed before coalesced wake: view=%+v pending=%d", view, dispatcher.Pending())
	}
	reopened, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted := reopened.AlertDelivery(at)
	if !persisted.Initialized || !persisted.AsOf.Equal(at) || len(persisted.Occurrences) != 1 {
		t.Fatalf("snapshot was not durable: %+v", persisted)
	}
}

func TestOccurrenceRunSweepsStartupAndTimerWithoutFreshSnapshot(t *testing.T) {
	at := time.Date(2026, time.July, 20, 22, 10, 0, 0, time.UTC)
	work := occurrenceDueWork(t, at)
	t.Run("startup", func(t *testing.T) {
		store := newFakeOccurrenceStore(work)
		sender := &recordingOccurrenceSender{sent: make(chan struct{}, 2), attempt: state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }, SweepInterval: time.Hour}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go dispatcher.Run(ctx)
		select {
		case <-sender.sent:
		case <-time.After(time.Second):
			t.Fatal("startup did not sweep durable due work")
		}
		cancel()
	})

	t.Run("timer", func(t *testing.T) {
		store := newFakeOccurrenceStore()
		store.confirmCandidate = work.Candidate
		store.dueByCall = func(call int) []state.AlertDeliveryDueWork {
			if call >= 2 {
				return []state.AlertDeliveryDueWork{work}
			}
			return nil
		}
		sender := &recordingOccurrenceSender{sent: make(chan struct{}, 2), attempt: state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }, SweepInterval: 10 * time.Millisecond}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go dispatcher.Run(ctx)
		select {
		case <-sender.sent:
		case <-time.After(time.Second):
			t.Fatal("timer did not rescan durable due work")
		}
		cancel()
		if store.observeCalls != 0 {
			t.Fatalf("retry sweep depended on a fresh snapshot: observe_calls=%d", store.observeCalls)
		}
	})
}

func TestOccurrenceDispatchNoDueAndProductionNilEligibilityNeverReachSender(t *testing.T) {
	t.Run("no due fake", func(t *testing.T) {
		store := newFakeOccurrenceStore()
		sender := &recordingOccurrenceSender{}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender}
		result, err := dispatcher.DispatchDue(context.Background())
		if err != nil || result.Due != 0 || sender.callCount() != 0 {
			t.Fatalf("result=%+v sends=%d err=%v", result, sender.callCount(), err)
		}
	})

	t.Run("real store default policy", func(t *testing.T) {
		store, err := state.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, time.July, 20, 22, 20, 0, 0, time.UTC)
		candidate := occurrenceCandidate(t, at)
		candidate.DeliveryPreference = rpc.AlertDeliveryPage
		if _, err := store.ObserveAlertSnapshot(occurrenceSnapshot(at, candidate)); err != nil {
			t.Fatal(err)
		}
		sender := &recordingOccurrenceSender{}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
		result, err := dispatcher.DispatchDue(context.Background())
		if err != nil || result.Due != 0 || sender.callCount() != 0 {
			t.Fatalf("nil production eligibility reached transport: result=%+v sends=%d err=%v", result, sender.callCount(), err)
		}
	})
}

func TestOccurrenceDispatchMaintainsRecoveryCompactionAndPrerequisiteHealth(t *testing.T) {
	at := time.Date(2026, time.July, 20, 22, 25, 0, 0, time.UTC)
	work := occurrenceDueWork(t, at)
	for _, tc := range []struct {
		name      string
		configure func(*fakeOccurrenceStore) push.Sender
		wantClass string
	}{
		{
			name: "no active subscription",
			configure: func(store *fakeOccurrenceStore) push.Sender {
				store.subscriptions = nil
				return &recordingOccurrenceSender{}
			},
			wantClass: state.AlertDeliveryHealthClassNoSubscription,
		},
		{
			name: "signing keys unavailable",
			configure: func(store *fakeOccurrenceStore) push.Sender {
				store.hasKeys = false
				return &recordingOccurrenceSender{}
			},
			wantClass: state.AlertDeliveryHealthClassSigningKeys,
		},
		{
			name:      "sender unavailable",
			configure: func(*fakeOccurrenceStore) push.Sender { return nil },
			wantClass: state.AlertDeliveryHealthClassSender,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeOccurrenceStore(work)
			dispatcher := &OccurrenceDispatcher{Store: store, Sender: tc.configure(store), Now: func() time.Time { return at }}
			result, err := dispatcher.DispatchDue(context.Background())
			if err != nil || result.Deferred != 1 || store.recoveryCalls != 1 || store.compactCalls != 1 {
				t.Fatalf("result=%+v recover=%d compact=%d err=%v", result, store.recoveryCalls, store.compactCalls, err)
			}
			if len(store.prerequisiteCalls) != 1 || store.prerequisiteCalls[0] != tc.wantClass {
				t.Fatalf("prerequisite calls=%v", store.prerequisiteCalls)
			}
		})
	}

	t.Run("automatic recovery", func(t *testing.T) {
		store := newFakeOccurrenceStore(work)
		store.subscriptions = nil
		sender := &recordingOccurrenceSender{attempt: state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
		if result, err := dispatcher.DispatchDue(context.Background()); err != nil || result.Deferred != 1 {
			t.Fatalf("outage result=%+v err=%v", result, err)
		}
		store.subscriptions = newFakeOccurrenceStore().subscriptions
		if result, err := dispatcher.DispatchDue(context.Background()); err != nil || result.Accepted != 1 || sender.callCount() != 1 {
			t.Fatalf("recovery result=%+v sends=%d err=%v", result, sender.callCount(), err)
		}
		if !reflect.DeepEqual(store.prerequisiteCalls, []string{state.AlertDeliveryHealthClassNoSubscription, ""}) {
			t.Fatalf("prerequisite recovery calls=%v", store.prerequisiteCalls)
		}
	})
}

func TestOccurrenceDispatchFailsClosedBeforeDueAndRunStopsOnMaintenanceError(t *testing.T) {
	secret := "private maintenance detail"
	for _, failure := range []string{"recover", "compact"} {
		t.Run(failure, func(t *testing.T) {
			store := newFakeOccurrenceStore(occurrenceDueWork(t, time.Now().UTC()))
			if failure == "recover" {
				store.recoveryErr = errors.New(secret)
			} else {
				store.compactErr = errors.New(secret)
			}
			dispatcher := &OccurrenceDispatcher{Store: store, Sender: &recordingOccurrenceSender{}}
			_, err := dispatcher.DispatchDue(context.Background())
			if !errors.Is(err, ErrOccurrenceDeliveryState) || strings.Contains(err.Error(), secret) || store.dueCalls != 0 {
				t.Fatalf("failure=%s due=%d err=%v", failure, store.dueCalls, err)
			}
			if runErr := dispatcher.Run(context.Background()); !errors.Is(runErr, ErrOccurrenceDeliveryState) {
				t.Fatalf("Run did not stop on %s error: %v", failure, runErr)
			}
		})
	}
}

func TestOccurrencePayloadFailureReleasesConfirmedOwnershipThroughRetryCompletion(t *testing.T) {
	at := time.Date(2026, time.July, 20, 22, 27, 0, 0, time.UTC)
	store := newFakeOccurrenceStore(occurrenceDueWork(t, at))
	store.confirmCandidate = rpc.AlertCandidate{}
	sender := &recordingOccurrenceSender{}
	dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
	result, err := dispatcher.DispatchDue(context.Background())
	if !errors.Is(err, ErrOccurrencePayload) || result.Reserved != 1 || result.Sent != 0 || sender.callCount() != 0 {
		t.Fatalf("result=%+v sends=%d err=%v", result, sender.callCount(), err)
	}
	if !reflect.DeepEqual(store.completions, []state.AlertDeliveryCompletion{state.AlertDeliveryCompletionRetryable}) {
		t.Fatalf("confirmed attempt remained owned after payload failure: completions=%v", store.completions)
	}
}

func TestOccurrenceSenderPanicReleasesConfirmedOwnershipBeforeRepanic(t *testing.T) {
	at := time.Date(2026, time.July, 20, 22, 28, 0, 0, time.UTC)
	store := newFakeOccurrenceStore(occurrenceDueWork(t, at))
	sender := occurrenceSenderFunc(func(context.Context, state.PushSubscription, state.VAPIDKeys, push.Payload) state.PushAttempt {
		panic("injected sender panic")
	})
	dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("sender panic was swallowed")
			}
		}()
		_, _ = dispatcher.DispatchDue(context.Background())
	}()
	if !reflect.DeepEqual(store.completions, []state.AlertDeliveryCompletion{state.AlertDeliveryCompletionRetryable}) {
		t.Fatalf("sender panic left confirmed attempt owned: completions=%v", store.completions)
	}
}

func TestOccurrenceReserveAndConfirmGatesPreventSender(t *testing.T) {
	at := time.Date(2026, time.July, 20, 22, 30, 0, 0, time.UTC)
	work := occurrenceDueWork(t, at)
	t.Run("reserve error", func(t *testing.T) {
		store := newFakeOccurrenceStore(work)
		secret := "https://private.push.example/device-secret"
		store.beginErr = errors.New(secret)
		sender := &recordingOccurrenceSender{}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
		result, err := dispatcher.DispatchDue(context.Background())
		if !errors.Is(err, ErrOccurrenceDeliveryState) || sender.callCount() != 0 || result.Sent != 0 {
			t.Fatalf("result=%+v sends=%d err=%v", result, sender.callCount(), err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("public error leaked store prose: %v", err)
		}
	})

	t.Run("recovery between begin and confirm", func(t *testing.T) {
		store := newFakeOccurrenceStore(work)
		store.confirmAllowed = false
		sender := &recordingOccurrenceSender{}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
		result, err := dispatcher.DispatchDue(context.Background())
		if err != nil || result.Reserved != 1 || result.Skipped != 1 || result.Sent != 0 || sender.callCount() != 0 {
			t.Fatalf("result=%+v sends=%d err=%v", result, sender.callCount(), err)
		}
		if store.confirmCalls != 1 || len(store.completions) != 0 {
			t.Fatalf("confirm/completion calls=%d/%d", store.confirmCalls, len(store.completions))
		}
	})
}

func TestOccurrenceTransportClassMappingsAndAcceptedDisposition(t *testing.T) {
	at := time.Date(2026, time.July, 20, 22, 40, 0, 0, time.UTC)
	tests := []struct {
		name       string
		attempt    state.PushAttempt
		completion state.AlertDeliveryCompletion
	}{
		{name: "accepted", attempt: state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}, completion: state.AlertDeliveryCompletionAccepted},
		{name: "accepted class without ok fails closed", attempt: state.PushAttempt{Class: state.GovernanceTransportAccepted}, completion: state.AlertDeliveryCompletionRejected},
		{name: "ok with contradictory class fails closed", attempt: state.PushAttempt{OK: true, Class: state.GovernanceTransportNetworkRetry}, completion: state.AlertDeliveryCompletionRetryable},
		{name: "network retry", attempt: state.PushAttempt{Class: state.GovernanceTransportNetworkRetry}, completion: state.AlertDeliveryCompletionRetryable},
		{name: "deadline retry", attempt: state.PushAttempt{Class: state.GovernanceTransportDeadlineRetry}, completion: state.AlertDeliveryCompletionRetryable},
		{name: "http rejection", attempt: state.PushAttempt{Class: state.GovernanceTransportHTTPRejected}, completion: state.AlertDeliveryCompletionRejected},
		{name: "unknown fails closed", attempt: state.PushAttempt{Class: "broker supplied free text"}, completion: state.AlertDeliveryCompletionRejected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeOccurrenceStore(occurrenceDueWork(t, at))
			if tc.completion == state.AlertDeliveryCompletionAccepted {
				store.completeOutcome.Disposition = state.AlertDeliveryCompletionInactive
			}
			sender := &recordingOccurrenceSender{attempt: tc.attempt}
			dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
			result, err := dispatcher.DispatchDue(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(store.completions) != 1 || store.completions[0] != tc.completion || store.confirmCalls != sender.callCount() || result.Sent != 1 {
				t.Fatalf("completion=%v confirms=%d sends=%d result=%+v", store.completions, store.confirmCalls, sender.callCount(), result)
			}
			if tc.completion == state.AlertDeliveryCompletionAccepted && (result.Accepted != 1 || result.Inactive != 1) {
				t.Fatalf("transport acceptance was collapsed into lifecycle disposition: %+v", result)
			}
		})
	}
}

func TestOccurrenceRetiresDeadAndStructurallyInvalidSubscriptions(t *testing.T) {
	at := time.Date(2026, time.July, 20, 22, 50, 0, 0, time.UTC)
	t.Run("dead", func(t *testing.T) {
		store := newFakeOccurrenceStore(occurrenceDueWork(t, at))
		sender := &recordingOccurrenceSender{attempt: state.PushAttempt{Class: state.GovernanceTransportDead}}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
		result, err := dispatcher.DispatchDue(context.Background())
		if err != nil || result.Rejected != 1 || result.Retired != 1 || len(store.removedSubscriptions) != 1 {
			t.Fatalf("result=%+v removed=%d err=%v", result, len(store.removedSubscriptions), err)
		}
		if !reflect.DeepEqual(store.prerequisiteCalls, []string{"", state.AlertDeliveryHealthClassNoSubscription}) {
			t.Fatalf("dead target health=%v", store.prerequisiteCalls)
		}
	})

	t.Run("invalid before reservation", func(t *testing.T) {
		store := newFakeOccurrenceStore(occurrenceDueWork(t, at))
		store.subscriptions[0].Auth = ""
		sender := &recordingOccurrenceSender{}
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
		result, err := dispatcher.DispatchDue(context.Background())
		if err != nil || result.Retired != 1 || store.beginCalls != 0 || store.confirmCalls != 0 || sender.callCount() != 0 {
			t.Fatalf("result=%+v begin=%d confirm=%d sends=%d err=%v", result, store.beginCalls, store.confirmCalls, sender.callCount(), err)
		}
		if !reflect.DeepEqual(store.prerequisiteCalls, []string{"", state.AlertDeliveryHealthClassNoSubscription}) {
			t.Fatalf("invalid target health=%v", store.prerequisiteCalls)
		}
	})
}

func TestOccurrencePayloadUsesStableDisplayTagAndLeaksNoPrivateTransportData(t *testing.T) {
	at := time.Date(2026, time.July, 20, 23, 0, 0, 0, time.UTC)
	work := occurrenceDueWork(t, at)
	store := newFakeOccurrenceStore(work)
	store.subscriptions = append(store.subscriptions, state.PushSubscription{
		ID: "second-secret-subscription", DeviceID: "second-secret-device", Endpoint: "https://push.example/second-secret-endpoint", P256DH: "second-secret-key", Auth: "second-secret-auth",
	})
	sender := &recordingOccurrenceSender{attempt: state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}}
	dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
	result, err := dispatcher.DispatchDue(context.Background())
	if err != nil || result.Sent != 2 || store.confirmCalls != 2 || sender.callCount() != 2 {
		t.Fatalf("result=%+v confirms=%d sends=%d err=%v", result, store.confirmCalls, sender.callCount(), err)
	}
	for _, payload := range sender.payloadCopies() {
		if payload.DisplayID != work.DisplayID {
			t.Fatalf("unstable payload tag: %+v", payload)
		}
		raw, _ := json.Marshal(payload)
		for _, private := range []string{
			work.OccurrenceKey, work.Candidate.EpisodeKey, work.Candidate.EvidenceFingerprint,
			"secret-endpoint", "secret-device", "secret-subscription", "secret-key", "secret-auth",
		} {
			if strings.Contains(string(raw), private) {
				t.Fatalf("payload leaked %q: %s", private, raw)
			}
		}
	}
	public, _ := json.Marshal(result)
	for _, private := range []string{work.OccurrenceKey, work.Candidate.EpisodeKey, "secret-endpoint", "secret-device", "secret-key"} {
		if strings.Contains(string(public), private) {
			t.Fatalf("dispatch result leaked %q: %s", private, public)
		}
	}
}

func TestOccurrencePayloadUsesAtomicConfirmCandidateAfterSameOccurrenceRevision(t *testing.T) {
	at := time.Date(2026, time.July, 20, 23, 5, 0, 0, time.UTC)
	work := occurrenceDueWork(t, at)
	if work.Candidate.Severity != rpc.AlertSeverityWatch || work.Candidate.Destination != rpc.AlertDestinationAlerts {
		t.Fatalf("test precondition drifted: %+v", work.Candidate)
	}
	revised := work.Candidate
	revised.EvidenceFingerprint = "sha256:" + strings.Repeat("b", 64)
	revised.Severity = rpc.AlertSeverityUrgent
	revised.Destination = rpc.AlertDestinationBrief
	revised.EvidenceAsOf = at.Add(time.Second)
	revised.ObservedAt = at.Add(time.Second)
	if err := rpc.ValidateAlertCandidate(revised); err != nil {
		t.Fatal(err)
	}

	store := newFakeOccurrenceStore(work)
	store.confirmCandidate = revised
	sender := &recordingOccurrenceSender{attempt: state.PushAttempt{OK: true, Class: state.GovernanceTransportAccepted}}
	dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at.Add(2 * time.Second) }}
	result, err := dispatcher.DispatchDue(context.Background())
	if err != nil || result.Sent != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	payloads := sender.payloadCopies()
	if len(payloads) != 1 {
		t.Fatalf("payload count=%d", len(payloads))
	}
	payload := payloads[0]
	if payload.Severity != string(revised.Severity) || payload.Destination != string(revised.Destination) || !strings.Contains(payload.Body, "Open Brief") {
		t.Fatalf("sent stale pre-scan presentation: %+v", payload)
	}
	if payload.DisplayID != work.DisplayID {
		t.Fatalf("same occurrence changed stable display tag: %+v", payload)
	}
}

func TestOccurrenceDispatchSerializesConcurrentSweepsAndWaitingCancellation(t *testing.T) {
	store := newFakeOccurrenceStore()
	entered := make(chan struct{})
	release := make(chan struct{})
	store.dueHook = func(call int) {
		if call == 1 {
			close(entered)
			<-release
		}
	}
	dispatcher := &OccurrenceDispatcher{Store: store}
	firstDone := make(chan error, 1)
	go func() {
		_, err := dispatcher.DispatchDue(context.Background())
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first sweep did not enter store")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := dispatcher.DispatchDue(waitCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting sweep ignored cancellation: %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if store.maxDueConcurrent != 1 || store.dueCalls != 1 {
		t.Fatalf("sweeps overlapped or canceled waiter entered store: max=%d calls=%d", store.maxDueConcurrent, store.dueCalls)
	}
}

func TestOccurrenceSendTimeoutRetriesAndExternalErrorProseIsNotReturned(t *testing.T) {
	at := time.Date(2026, time.July, 20, 23, 10, 0, 0, time.UTC)
	t.Run("child timeout", func(t *testing.T) {
		store := newFakeOccurrenceStore(occurrenceDueWork(t, at))
		secret := "raw network failure at https://private.push.example/device-token"
		sender := occurrenceSenderFunc(func(ctx context.Context, _ state.PushSubscription, _ state.VAPIDKeys, _ push.Payload) state.PushAttempt {
			<-ctx.Done()
			return state.PushAttempt{Class: state.GovernanceTransportDeadlineRetry, Error: secret}
		})
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }, SendTimeout: 10 * time.Millisecond}
		result, err := dispatcher.DispatchDue(context.Background())
		if err != nil || result.Retryable != 1 || result.Sent != 1 || len(store.completions) != 1 || store.completions[0] != state.AlertDeliveryCompletionRetryable {
			t.Fatalf("result=%+v completions=%v err=%v", result, store.completions, err)
		}
		public, _ := json.Marshal(result)
		if strings.Contains(string(public), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
			t.Fatalf("external error prose escaped: result=%s err=%v", public, err)
		}
	})

	t.Run("parent cancellation completes transport truth", func(t *testing.T) {
		store := newFakeOccurrenceStore(occurrenceDueWork(t, at))
		ctx, cancel := context.WithCancel(context.Background())
		sender := occurrenceSenderFunc(func(context.Context, state.PushSubscription, state.VAPIDKeys, push.Payload) state.PushAttempt {
			cancel()
			return state.PushAttempt{Class: state.GovernanceTransportCanceledRetry}
		})
		dispatcher := &OccurrenceDispatcher{Store: store, Sender: sender, Now: func() time.Time { return at }}
		result, err := dispatcher.DispatchDue(ctx)
		if !errors.Is(err, context.Canceled) || result.Sent != 1 || result.Retryable != 1 || len(store.completions) != 1 || store.completions[0] != state.AlertDeliveryCompletionRetryable {
			t.Fatalf("cancellation lost completion: result=%+v completions=%v err=%v", result, store.completions, err)
		}
	})
}

func occurrenceCandidate(t *testing.T, at time.Time) rpc.AlertCandidate {
	t.Helper()
	candidate := presentationCandidate(t, at)
	candidate.DeliveryPreference = rpc.AlertDeliveryPage
	return candidate
}

func occurrenceSnapshot(at time.Time, candidates ...rpc.AlertCandidate) rpc.AlertCandidateSnapshot {
	return rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion,
		AsOf:          at,
		CurrentState:  rpc.AlertSnapshotActive,
		Coverage: rpc.AlertCoverage{
			State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: at,
			ExpectedSources: []rpc.AlertSource{rpc.AlertSourceCanary}, CoveredSources: []rpc.AlertSource{rpc.AlertSourceCanary},
		},
		Candidates: append([]rpc.AlertCandidate{}, candidates...),
	}
}

func occurrenceDueWork(t *testing.T, at time.Time) state.AlertDeliveryDueWork {
	t.Helper()
	candidate := occurrenceCandidate(t, at)
	return state.AlertDeliveryDueWork{OccurrenceKey: candidate.OccurrenceKey, Candidate: candidate, DisplayID: "alert-0123456789abcdef"}
}

type fakeOccurrenceStore struct {
	mu sync.Mutex

	observed          []rpc.AlertCandidateSnapshot
	observeCalls      int
	due               []state.AlertDeliveryDueWork
	dueByCall         func(int) []state.AlertDeliveryDueWork
	dueHook           func(int)
	dueCalls          int
	dueConcurrent     int
	maxDueConcurrent  int
	recoveryCalls     int
	recoveryErr       error
	compactCalls      int
	compactErr        error
	prerequisiteCalls []string
	prerequisiteErr   error

	subscriptions []state.PushSubscription
	keys          state.VAPIDKeys
	hasKeys       bool

	beginCalls           int
	beginErr             error
	beginAllowed         bool
	confirmCalls         int
	confirmAllowed       bool
	confirmCandidate     rpc.AlertCandidate
	completions          []state.AlertDeliveryCompletion
	completeOutcome      state.AlertDeliveryCompletionOutcome
	removedSubscriptions []string
}

func newFakeOccurrenceStore(due ...state.AlertDeliveryDueWork) *fakeOccurrenceStore {
	store := &fakeOccurrenceStore{
		due: append([]state.AlertDeliveryDueWork{}, due...), beginAllowed: true, confirmAllowed: true, hasKeys: true,
		keys: state.VAPIDKeys{PublicKey: "public-secret-key", PrivateKey: "private-secret-key"},
		subscriptions: []state.PushSubscription{{
			ID: "secret-subscription", DeviceID: "secret-device", Endpoint: "https://push.example/secret-endpoint", P256DH: "secret-key", Auth: "secret-auth",
		}},
		completeOutcome: state.AlertDeliveryCompletionOutcome{Disposition: state.AlertDeliveryCompletionApplied},
	}
	if len(due) > 0 {
		store.confirmCandidate = due[0].Candidate
	}
	return store
}

func (s *fakeOccurrenceStore) ObserveAlertSnapshot(snapshot rpc.AlertCandidateSnapshot) (state.AlertDeliveryView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeCalls++
	s.observed = append(s.observed, snapshot)
	return state.AlertDeliveryView{Initialized: true, AsOf: snapshot.AsOf}, nil
}

func (s *fakeOccurrenceStore) RecoverAlertDeliveries(time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoveryCalls++
	return s.recoveryErr
}

func (s *fakeOccurrenceStore) CompactAlertDelivery(time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compactCalls++
	return s.compactErr
}

func (s *fakeOccurrenceStore) SetAlertDeliveryPrerequisiteHealth(class string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prerequisiteCalls = append(s.prerequisiteCalls, class)
	return s.prerequisiteErr
}

func (s *fakeOccurrenceStore) AlertDeliveriesDue(time.Time) []state.AlertDeliveryDueWork {
	s.mu.Lock()
	s.dueCalls++
	call := s.dueCalls
	s.dueConcurrent++
	if s.dueConcurrent > s.maxDueConcurrent {
		s.maxDueConcurrent = s.dueConcurrent
	}
	hook := s.dueHook
	byCall := s.dueByCall
	due := append([]state.AlertDeliveryDueWork{}, s.due...)
	s.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if byCall != nil {
		due = append([]state.AlertDeliveryDueWork{}, byCall(call)...)
	}
	s.mu.Lock()
	s.dueConcurrent--
	s.mu.Unlock()
	return due
}

func (s *fakeOccurrenceStore) ActivePushSubscriptions() []state.PushSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]state.PushSubscription{}, s.subscriptions...)
}

func (s *fakeOccurrenceStore) VAPID() (state.VAPIDKeys, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys, s.hasKeys
}

func (s *fakeOccurrenceStore) BeginAlertDelivery(_, _ string, now time.Time) (state.AlertDeliveryReservation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginCalls++
	if s.beginErr != nil {
		return state.AlertDeliveryReservation{}, false, s.beginErr
	}
	return state.AlertDeliveryReservation{AttemptID: "attempt-private", DisplayID: "alert-0123456789abcdef", AttemptNumber: s.beginCalls, ReservedAt: now}, s.beginAllowed, nil
}

func (s *fakeOccurrenceStore) ConfirmAlertTransport(attemptID string, now time.Time) (state.AlertDeliveryReservation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmCalls++
	reservation := state.AlertDeliveryReservation{AttemptID: attemptID, DisplayID: "alert-0123456789abcdef", ReservedAt: now}
	if s.confirmAllowed {
		reservation.Candidate = s.confirmCandidate
	}
	return reservation, s.confirmAllowed, nil
}

func (s *fakeOccurrenceStore) CompleteAlertDelivery(_ string, completion state.AlertDeliveryCompletion, _ time.Time) (state.AlertDeliveryCompletionOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completions = append(s.completions, completion)
	return s.completeOutcome, nil
}

func (s *fakeOccurrenceStore) RemovePushSubscriptionAt(subscription string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removedSubscriptions = append(s.removedSubscriptions, subscription)
	s.subscriptions = slices.DeleteFunc(s.subscriptions, func(candidate state.PushSubscription) bool {
		return candidate.ID == subscription || candidate.Endpoint == subscription
	})
	return nil
}

type recordingOccurrenceSender struct {
	mu       sync.Mutex
	attempt  state.PushAttempt
	payloads []push.Payload
	sent     chan struct{}
}

func (s *recordingOccurrenceSender) Send(_ context.Context, _ state.PushSubscription, _ state.VAPIDKeys, payload push.Payload) state.PushAttempt {
	s.mu.Lock()
	s.payloads = append(s.payloads, payload)
	s.mu.Unlock()
	if s.sent != nil {
		select {
		case s.sent <- struct{}{}:
		default:
		}
	}
	return s.attempt
}

func (s *recordingOccurrenceSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.payloads)
}

func (s *recordingOccurrenceSender) payloadCopies() []push.Payload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]push.Payload{}, s.payloads...)
}

type occurrenceSenderFunc func(context.Context, state.PushSubscription, state.VAPIDKeys, push.Payload) state.PushAttempt

func (f occurrenceSenderFunc) Send(ctx context.Context, sub state.PushSubscription, keys state.VAPIDKeys, payload push.Payload) state.PushAttempt {
	return f(ctx, sub, keys, payload)
}
