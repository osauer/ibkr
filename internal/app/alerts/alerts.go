package alerts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/app/push"
	"github.com/osauer/ibkr/internal/app/state"
	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

type Monitor struct {
	Store         *state.Store
	Sender        push.Sender
	URL           string
	Now           func() time.Time
	TradingStatus func() *rpc.TradingStatus
}

func (m Monitor) Observe(ctx context.Context, canary rpc.CanaryResult) (*state.AlertRecord, []state.PushAttempt) {
	if m.Store == nil {
		return nil, nil
	}
	mode := m.Store.AlertSettings().Mode
	if !ShouldAlert(mode, canary) {
		return nil, nil
	}
	fp := canary.Fingerprint.Key
	if fp == "" {
		fp = canary.Fingerprint.Version + ":" + canary.Action + ":" + string(canary.Severity)
	}
	if m.Store.HasAlertFingerprint(fp) {
		return nil, nil
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	rec := RedactedRecord(canary, now)
	rec.Fingerprint = fp
	if m.TradingStatus != nil {
		if trading := m.TradingStatus(); trading != nil {
			rec.Account = trading.Account
			rec.Mode = trading.Mode
		}
	}
	if err := m.Store.RecordAlert(rec); err != nil {
		return nil, []state.PushAttempt{{At: now, AlertID: rec.ID, Error: err.Error()}}
	}
	payload := push.Payload{
		Title:    rec.Title,
		Body:     rec.Body,
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
		return severityAtLeast(canary.Severity, risk.SeverityAct) ||
			canary.Action == "defend" ||
			canary.Action == "rebalance" ||
			canary.Action == "confirm_inputs"
	case state.AlertModeWatchAndAct, "":
		return severityAtLeast(canary.Severity, risk.SeverityWatch)
	default:
		return false
	}
}

func RedactedRecord(canary rpc.CanaryResult, now time.Time) state.AlertRecord {
	action := strings.TrimSpace(canary.Action)
	if action == "" {
		action = "canary"
	}
	severity := string(canary.Severity)
	title := "ibkr canary: " + strings.ReplaceAll(action, "_", " ")
	body := fmt.Sprintf("%s severity", nonEmpty(severity, "watch"))
	if canary.MarketConfirmation != "" {
		body += "; market confirmation " + canary.MarketConfirmation
	}
	body += ". Open ibkr for portfolio details."
	return state.AlertRecord{
		ID:        fmt.Sprintf("%d", now.UnixNano()),
		Action:    action,
		Severity:  severity,
		Title:     title,
		Body:      body,
		CreatedAt: now,
	}
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
