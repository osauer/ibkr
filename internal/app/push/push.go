package push

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/osauer/ibkr/internal/app/state"
)

type Sender interface {
	Send(context.Context, state.PushSubscription, state.VAPIDKeys, Payload) state.PushAttempt
}

type Payload struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url,omitempty"`
	AlertID  string `json:"alert_id,omitempty"`
	Action   string `json:"action,omitempty"`
	Severity string `json:"severity,omitempty"`
}

type WebPushSender struct {
	Subscriber string
	Client     webpush.HTTPClient
}

func GenerateVAPIDKeys() (privateKey, publicKey string, err error) {
	return webpush.GenerateVAPIDKeys()
}

func (s WebPushSender) Send(ctx context.Context, sub state.PushSubscription, keys state.VAPIDKeys, payload Payload) state.PushAttempt {
	attempt := state.PushAttempt{At: time.Now().UTC(), SubscriptionID: sub.ID, AlertID: payload.AlertID}
	if err := ValidateSubscription(sub); err != nil {
		attempt.Error = err.Error()
		return attempt
	}
	body, err := json.Marshal(payload)
	if err != nil {
		attempt.Error = err.Error()
		return attempt
	}
	resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{Auth: sub.Auth, P256dh: sub.P256DH},
	}, &webpush.Options{
		HTTPClient:      s.Client,
		Subscriber:      s.Subscriber,
		TTL:             60,
		Urgency:         webpush.UrgencyHigh,
		VAPIDPublicKey:  keys.PublicKey,
		VAPIDPrivateKey: keys.PrivateKey,
	})
	if err != nil {
		attempt.Error = err.Error()
		return attempt
	}
	defer resp.Body.Close()
	attempt.Status = resp.Status
	attempt.OK = resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if !attempt.OK {
		attempt.Error = fmt.Sprintf("push service returned %s", resp.Status)
	}
	return attempt
}

func ValidateSubscription(sub state.PushSubscription) error {
	if sub.Endpoint == "" {
		return fmt.Errorf("endpoint required")
	}
	if sub.P256DH == "" {
		return fmt.Errorf("p256dh key required")
	}
	if sub.Auth == "" {
		return fmt.Errorf("auth key required")
	}
	return nil
}
