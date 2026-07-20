package alerts

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/push"
	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

type Monitor struct {
	Store         *state.Store
	Sender        push.Sender
	URL           string
	Now           func() time.Time
	TradingStatus func() *rpc.TradingStatus
	// afterRecord is a synchronous test seam for delivery-mode transitions.
	afterRecord func()
}

// GovernanceDispatcher transports daemon-authored candidates independently
// from Canary alert history and fingerprint caps. The daemon remains the sole
// policy evaluator; this type owns only per-target delivery evidence.
type GovernanceDispatcher struct {
	Store       *state.Store
	Sender      push.Sender
	Now         func() time.Time
	SendTimeout time.Duration
	mu          sync.Mutex
}

func (d *GovernanceDispatcher) Observe(ctx context.Context, snapshot rpc.NudgesSnapshotResult) (state.GovernanceView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return state.GovernanceView{}, nil
	}
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now().UTC()
	}
	if err := d.Store.CompactGovernance(now); err != nil {
		return d.Store.Governance(now), err
	}
	records := make([]state.GovernanceOccurrence, 0, len(snapshot.Candidates))
	for _, candidate := range snapshot.Candidates {
		// The payload constructor is the app's mandatory canonicalization
		// boundary. Its temporary display id is replaced by the store's stable
		// id after the occurrence has been persisted.
		probe, err := push.GovernancePayload(candidate, governanceProbeDisplayID)
		if err != nil {
			_ = d.Store.SetGovernanceDeliveryHealth(state.GovernanceDeliveryHealth{
				State: state.GovernanceDeliveryUnavailable, Class: state.GovernanceTransportAllFailed, UpdatedAt: now,
			})
			return d.Store.Governance(now), fmt.Errorf("invalid governance candidate: %w", err)
		}
		records = append(records, state.GovernanceOccurrence{
			Fingerprint: candidate.Fingerprint, Kind: probe.Kind, State: candidate.State, Severity: probe.Severity,
			Title: probe.Title, Body: probe.Body, Destination: probe.Destination,
			OccurredAt: candidate.OccurredAt, DueAt: candidate.DueAt, ExpiresAt: candidate.ExpiresAt,
		})
	}
	normalizedHealth := rpc.NormalizeNudgeSourceHealth(snapshot.SourceHealth, len(snapshot.Candidates))
	occurrences, err := d.Store.ObserveGovernanceOccurrences(records, normalizedHealth.Aggregate == rpc.NudgeAggregateReady, now)
	if err != nil {
		return d.Store.Governance(now), err
	}
	if len(occurrences) == 0 {
		return d.Store.Governance(now), nil
	}

	mode := d.Store.AlertSettings().Mode
	deliverable := occurrences[:0]
	for _, occurrence := range occurrences {
		if occurrence.DeliveryDisposition != state.GovernanceDispositionEligible {
			target := governanceEpisodeSuppressedTarget
			if occurrence.DeliveryDisposition == state.GovernanceDispositionLegacyUnknown {
				target = governanceLegacyUnknownTarget
			}
			if err := d.recordMissed(occurrence, target, state.GovernanceTransportSuppressed, now); err != nil {
				return d.Store.Governance(now), err
			}
			continue
		}
		if mode == state.AlertModeNone {
			if err := d.recordMissed(occurrence, governanceSuppressedTarget, state.GovernanceTransportSuppressed, now); err != nil {
				return d.Store.Governance(now), err
			}
			continue
		}
		deliverable = append(deliverable, occurrence)
	}
	if len(deliverable) == 0 {
		err := d.Store.SetGovernanceDeliveryHealth(state.GovernanceDeliveryHealth{
			State: state.GovernanceDeliverySuppressed, Class: state.GovernanceTransportSuppressed, UpdatedAt: now,
		})
		return d.Store.Governance(now), err
	}
	occurrences = deliverable

	subscriptions := d.Store.ActivePushSubscriptions()
	if len(subscriptions) == 0 {
		for _, occurrence := range occurrences {
			if err := d.recordMissed(occurrence, "no-subscription", state.GovernanceTransportNoSubscription, now); err != nil {
				return d.Store.Governance(now), err
			}
		}
		err := d.Store.SetGovernanceDeliveryHealth(state.GovernanceDeliveryHealth{
			State: state.GovernanceDeliveryUnavailable, Class: state.GovernanceTransportNoSubscription, UpdatedAt: now,
		})
		return d.Store.Governance(now), err
	}

	keys, hasKeys := d.Store.VAPID()
	accepted, failed, retired := 0, 0, 0
	var acceptedAt time.Time
	for _, occurrence := range occurrences {
		candidate := rpc.NudgeCandidate{
			Fingerprint: occurrence.Fingerprint, Kind: occurrence.Kind, State: occurrence.State,
			Severity: occurrence.Severity, Title: occurrence.Title, Body: occurrence.Body,
			OccurredAt: occurrence.OccurredAt, DueAt: occurrence.DueAt, Destination: occurrence.Destination,
		}
		payload, err := push.GovernancePayload(candidate, occurrence.DisplayID)
		if err != nil {
			return d.Store.Governance(now), err
		}
		for _, subscription := range subscriptions {
			targetRef := state.GovernanceTargetRef(subscription.DeviceID, subscription.ID)
			receiptKey := state.GovernanceReceiptKey(occurrence.DisplayID, targetRef)
			if d.Store.HasGovernanceReceipt(receiptKey) {
				accepted++
				continue
			}
			reservation, send, err := d.Store.ReserveGovernanceAttempt(occurrence.DisplayID, targetRef, now)
			if err != nil {
				return d.Store.Governance(now), err
			}
			if !send {
				failed++
				continue
			}
			if !d.Store.BeginGovernanceTransport(reservation.ID) {
				retired++
				continue
			}
			class := state.GovernanceTransportSenderMissing
			result := state.PushAttempt{Class: class}
			if !hasKeys {
				result.Class = state.GovernanceTransportMissingKeys
			} else if d.Sender != nil {
				timeout := d.SendTimeout
				if timeout <= 0 {
					timeout = 10 * time.Second
				}
				sendCtx, cancel := context.WithTimeout(ctx, timeout)
				result = d.Sender.Send(sendCtx, subscription, keys, payload)
				cancel()
			}
			class = normalizeGovernanceResult(result)
			if result.OK {
				class = state.GovernanceTransportAccepted
			}
			acceptedResult := result.OK && class == state.GovernanceTransportAccepted
			outcome, err := d.Store.CompleteGovernanceAttempt(reservation.ID, class, acceptedResult, now)
			if err != nil {
				return d.Store.Governance(now), err
			}
			if acceptedResult {
				acceptedAt = now
			}
			if outcome.Disposition == state.GovernanceCompletionRetired {
				retired++
			} else if acceptedResult {
				accepted++
			} else {
				failed++
			}
			if class == state.GovernanceTransportDead {
				if err := d.Store.RemovePushSubscriptionAt(subscription.ID, now); err != nil {
					return d.Store.Governance(now), err
				}
			}
		}
	}
	health := state.GovernanceDeliveryHealth{UpdatedAt: now}
	switch {
	case accepted > 0 && failed == 0:
		health.State = state.GovernanceDeliveryHealthy
		health.Class = state.GovernanceTransportAccepted
		health.LastAcceptedAt = acceptedAt
	case accepted > 0:
		health.State = state.GovernanceDeliveryDegraded
		health.Class = state.GovernanceTransportPartial
		health.LastAcceptedAt = acceptedAt
	case retired > 0 && failed == 0:
		health.State = state.GovernanceDeliveryUnavailable
		health.Class = state.GovernanceTransportTargetRetired
		health.LastAcceptedAt = acceptedAt
	default:
		health.State = state.GovernanceDeliveryUnavailable
		health.Class = state.GovernanceTransportAllFailed
		health.LastAcceptedAt = acceptedAt
	}
	if err := d.Store.SetGovernanceDeliveryHealth(health); err != nil {
		return d.Store.Governance(now), err
	}
	return d.Store.Governance(now), nil
}

func normalizeGovernanceResult(result state.PushAttempt) string {
	switch result.Class {
	case state.GovernanceTransportAccepted, state.GovernanceTransportDeadlineRetry,
		state.GovernanceTransportCanceledRetry, state.GovernanceTransportNetworkRetry,
		state.GovernanceTransportHTTPRetry, state.GovernanceTransportHTTPRejected,
		state.GovernanceTransportMissingKeys, state.GovernanceTransportSenderMissing,
		state.GovernanceTransportTimeoutRetry, state.GovernanceTransportRejected,
		state.GovernanceTransportDead:
		return result.Class
	default:
		return state.GovernanceTransportHTTPRejected
	}
}

const (
	governanceProbeDisplayID   = "gov-0000000000000000"
	governanceSuppressedTarget = "suppressed"
	// This target distinguishes forensic accounting for an episode whose
	// terminal suppression boundary lives on the occurrence row itself.
	governanceEpisodeSuppressedTarget = "episode-suppressed"
	governanceLegacyUnknownTarget     = "legacy-unknown"
)

func (d *GovernanceDispatcher) recordMissed(occurrence state.GovernanceOccurrence, target, class string, now time.Time) error {
	targetRef := state.GovernanceTargetRef("governance", target)
	receiptKey := state.GovernanceReceiptKey(occurrence.DisplayID, targetRef)
	if !d.Store.GovernanceAttemptDue(receiptKey, now) {
		return nil
	}
	return d.Store.RecordGovernanceAttempt(state.GovernanceAttempt{
		OccurrenceID: occurrence.DisplayID, TargetRef: targetRef, ReceiptKey: receiptKey, At: now, Class: class,
	}, false)
}

// GovernanceWorker bounds poll-triggered delivery to one active observation
// and one coalesced latest snapshot.
type GovernanceWorker struct {
	dispatcher *GovernanceDispatcher
	submitMu   sync.Mutex
	generation uint64
	latest     chan governanceWork
}

type governanceWork struct {
	generation uint64
	snapshot   rpc.NudgesSnapshotResult
}

func NewGovernanceWorker(dispatcher *GovernanceDispatcher) *GovernanceWorker {
	return &GovernanceWorker{dispatcher: dispatcher, latest: make(chan governanceWork, 1)}
}

func (w *GovernanceWorker) Submit(snapshot rpc.NudgesSnapshotResult) uint64 {
	w.submitMu.Lock()
	defer w.submitMu.Unlock()
	w.generation++
	work := governanceWork{generation: w.generation, snapshot: snapshot}
	select {
	case w.latest <- work:
		return work.generation
	default:
	}
	select {
	case <-w.latest:
	default:
	}
	select {
	case w.latest <- work:
	default:
	}
	return work.generation
}

func (w *GovernanceWorker) Pending() int { return len(w.latest) }

func (w *GovernanceWorker) Run(ctx context.Context) {
	if w == nil || w.dispatcher == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case work := <-w.latest:
			_, _ = w.dispatcher.Observe(ctx, work.snapshot)
		}
	}
}

func (m Monitor) Observe(ctx context.Context, canary rpc.CanaryResult) (*state.AlertRecord, []state.PushAttempt) {
	if m.Store == nil {
		return nil, nil
	}
	if !canaryOccurrenceEligible(canary) {
		return nil, nil
	}
	modeBeforeRecord := m.Store.AlertSettings().Mode
	fp := canary.Fingerprint.Key
	if fp == "" {
		fp = canary.Fingerprint.Version + ":" + canary.Action + ":" + string(canary.Severity)
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	rec := RedactedRecord(canary, fp, now)
	rec.Fingerprint = fp
	if m.TradingStatus != nil {
		if trading := m.TradingStatus(); trading != nil {
			rec.Account = trading.Account
			rec.Mode = trading.Mode
		}
	}
	created, err := m.Store.RecordAlertIfNew(rec)
	if err != nil {
		return nil, []state.PushAttempt{{At: now, AlertID: rec.ID, Error: err.Error()}}
	}
	if !created {
		return nil, nil
	}
	if m.afterRecord != nil {
		m.afterRecord()
	}
	modeAfterRecord := m.Store.AlertSettings().Mode
	if !ShouldAlert(modeBeforeRecord, canary) || !ShouldAlert(modeAfterRecord, canary) {
		return &rec, nil
	}
	payload := push.Payload{
		Title:    rec.Title,
		Body:     rec.Body + " Open ibkr for portfolio details.",
		URL:      m.URL,
		AlertID:  rec.ID,
		Action:   rec.Action,
		Severity: rec.Severity,
	}
	keys, hasKeys := m.Store.VAPID()
	var attempts []state.PushAttempt
	if m.Sender != nil && hasKeys {
		for _, sub := range m.Store.PushSubscriptions() {
			attempt := m.Sender.Send(ctx, sub, keys, payload)
			attempts = append(attempts, attempt)
			_ = m.Store.RecordPush(attempt)
		}
	}
	return &rec, attempts
}

func ShouldAlert(mode string, canary rpc.CanaryResult) bool {
	switch mode {
	case state.AlertModeNone:
		return false
	case state.AlertModeActOnly:
		return canaryOccurrenceEligible(canary) && canaryActEligible(canary)
	case state.AlertModeWatchAndAct, "":
		return canaryOccurrenceEligible(canary)
	default:
		return false
	}
}

func canaryOccurrenceEligible(canary rpc.CanaryResult) bool {
	return portfolioAlertRelevant(canary) &&
		(severityAtLeast(canary.Severity, risk.SeverityWatch) || canaryActEligible(canary))
}

func canaryActEligible(canary rpc.CanaryResult) bool {
	return severityAtLeast(canary.Severity, risk.SeverityAct) ||
		canary.Action == "defend" ||
		canary.Action == "rebalance" ||
		canary.Action == "confirm_inputs"
}

// portfolioAlertRelevant reads the daemon-stamped relevance verdict; the
// policy's single copy lives in internal/canary. An unstamped snapshot (a
// producer predating the field) fails open to relevant: version skew may add
// market-weather noise but must never silently suppress alert delivery.
func portfolioAlertRelevant(canary rpc.CanaryResult) bool {
	return canary.PortfolioAlertRelevant == nil || *canary.PortfolioAlertRelevant
}

// RedactedRecord authors the stored in-app history copy. It stays deliberately
// free of symbols, quantities, and account data because the same record feeds
// the web-push payload; the push-only call to action is appended in Observe.
func RedactedRecord(canary rpc.CanaryResult, fingerprint string, now time.Time) state.AlertRecord {
	action := strings.TrimSpace(canary.Action)
	if action == "" {
		action = "canary"
	}
	severity := string(canary.Severity)
	title := "Canary: " + strings.ReplaceAll(action, "_", " ")
	// The severity is restated only when it differs from the action in the
	// title; "Canary: watch — Watch severity" says one thing twice.
	body := ""
	if s := nonEmpty(severity, "watch"); !strings.EqualFold(s, action) {
		body = capitalize(s) + " severity"
	}
	if phrase := marketConfirmationPhrase(canary.MarketConfirmation); phrase != "" {
		if body == "" {
			body = capitalize(phrase)
		} else {
			body += " — " + phrase
		}
	}
	if body == "" {
		body = "Canary state recorded"
	}
	body += "."
	idHash := sha256.Sum256([]byte(fingerprint + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	return state.AlertRecord{
		ID:        fmt.Sprintf("canary-%x", idHash[:12]),
		Action:    action,
		Severity:  severity,
		Title:     title,
		Body:      body,
		CreatedAt: now,
	}
}

// marketConfirmationPhrase translates the wire enum into reader terms; the
// enum values themselves must never reach rendered copy.
func marketConfirmationPhrase(confirmation string) string {
	switch confirmation {
	case "confirmed":
		return "market stress confirmed"
	case "partial":
		return "market stress partly confirmed"
	case "none":
		return "market stress not confirmed"
	case "blocked":
		return "market data blocked"
	case "":
		return ""
	default:
		return "market confirmation " + confirmation
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func severityAtLeast(got risk.SignalSeverity, want risk.SignalSeverity) bool {
	rank := map[risk.SignalSeverity]int{
		risk.SeverityObserve: 0,
		risk.SeverityWatch:   1,
		risk.SeverityAct:     2,
		risk.SeverityUrgent:  3,
	}
	return rank[got] >= rank[want]
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
