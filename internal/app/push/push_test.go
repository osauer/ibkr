package push

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/app/state"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestGovernancePayloadIsExactCanonicalAllowlist(t *testing.T) {
	t.Parallel()
	candidate := rpc.NudgeCandidate{
		Fingerprint: "sha256:" + strings.Repeat("a", 64),
		Kind:        rpc.NudgeKindPolicyDrift, State: rpc.NudgeStateOpen,
		Severity: "HOSTILE-BALANCE-123", Title: "HOSTILE account DU123", Body: "HOSTILE AAPL token",
		OccurredAt:  time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC),
		Destination: "https://evil.example/steal",
	}
	payload, err := GovernancePayload(candidate, "gov-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	want := `{"title":"Policy pins need review","body":"Review the policy pin status.","severity":"act","kind":"policy_drift","destination":"alerts","display_id":"gov-0123456789abcdef"}`
	if got != want {
		t.Fatalf("payload=%s\nwant=%s", got, want)
	}
	for _, sentinel := range []string{"HOSTILE", "DU123", "AAPL", "evil.example", candidate.Fingerprint, "url", "endpoint", "token"} {
		if strings.Contains(got, sentinel) {
			t.Fatalf("payload leaked %q: %s", sentinel, got)
		}
	}
}

func TestSafeDiagnosticPayloadIsFixed(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(SafeDiagnosticPayload())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"title":"IBKR notification test","body":"Safe test notification. No account data is included.","destination":"alerts","display_id":"diagnostic-safe-test"}`
	if string(raw) != want {
		t.Fatalf("payload=%s, want=%s", raw, want)
	}
}

func TestTransportClassificationIsAllowlistedAndTruthful(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{name: "deadline", err: context.DeadlineExceeded, want: state.GovernanceTransportDeadlineRetry},
		{name: "cancellation", err: context.Canceled, want: state.GovernanceTransportCanceledRetry},
		{name: "network", err: errors.New("dial sentinel raw endpoint"), want: state.GovernanceTransportNetworkRetry},
		{name: "accepted", status: http.StatusCreated, want: state.GovernanceTransportAccepted},
		{name: "request timeout", status: http.StatusRequestTimeout, want: state.GovernanceTransportHTTPRetry},
		{name: "rate limited", status: http.StatusTooManyRequests, want: state.GovernanceTransportHTTPRetry},
		{name: "server failure", status: http.StatusBadGateway, want: state.GovernanceTransportHTTPRetry},
		{name: "ordinary 400 is terminal rejection", status: http.StatusBadRequest, want: state.GovernanceTransportHTTPRejected},
		{name: "dead not found", status: http.StatusNotFound, want: state.GovernanceTransportDead},
		{name: "dead gone", status: http.StatusGone, want: state.GovernanceTransportDead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyTransport(tt.err, tt.status); got != tt.want {
				t.Fatalf("classifyTransport()=%q, want %q", got, tt.want)
			}
		})
	}
}
