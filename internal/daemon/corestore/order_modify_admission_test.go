package corestore

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestStagePreTransmitModifyCancelAttemptAdmission(t *testing.T) {
	tests := []struct {
		name               string
		outcomeType        string
		outcomeAttemptID   string
		sendDisposition    string
		wantModifyAccepted bool
	}{
		{name: "definitely unsent", outcomeType: "send-error", outcomeAttemptID: "cancel-1", sendDisposition: "definitely_unsent", wantModifyAccepted: true},
		{name: "may have written", outcomeType: "send-error", outcomeAttemptID: "cancel-1", sendDisposition: "may_have_written"},
		{name: "unknown", outcomeType: "send-error", outcomeAttemptID: "cancel-1", sendDisposition: "unknown"},
		{name: "incomplete send error", outcomeType: "send-error", outcomeAttemptID: "cancel-1"},
		{name: "uncorrelated definite send error", outcomeType: "send-error", outcomeAttemptID: "cancel-other", sendDisposition: "definitely_unsent"},
		{name: "send completed", outcomeType: "send-completed", outcomeAttemptID: "cancel-1"},
		{name: "pending"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := openTestStore(t)
			scope := testScope("modify-cancel-admission")
			const orderID int64 = 1001

			events := []OrderEventRecord{
				modifyAdmissionEvent(t, scope, orderID, "working", "broker-acknowledged", ActionPlace, "", "", "Submitted"),
				modifyAdmissionEvent(t, scope, orderID, "cancel-requested", "cancel-requested", ActionCancel, "cancel-1", "", ""),
			}
			if test.outcomeType != "" {
				events = append(events, modifyAdmissionEvent(t, scope, orderID, "cancel-outcome", test.outcomeType, ActionCancel, test.outcomeAttemptID, test.sendDisposition, ""))
			}
			seqs, err := store.AppendOrderEvents(t.Context(), events)
			if err != nil {
				t.Fatal(err)
			}
			expected := seqs[len(seqs)-1]
			modify := modifyAdmissionEvent(t, scope, orderID, "modify-requested", "modify-requested", ActionModify, "modify-1", "", "")
			result, err := store.StagePreTransmit(t.Context(), PreTransmitRequest{
				Scope: scope, RequestedOrderIDFloor: orderID, ReservedOrderID: orderID,
				ExpectedOrderEventSeq: &expected, Action: ActionModify, Origin: OriginAgentCLI,
				Events: []OrderEventRecord{modify},
			})

			if test.wantModifyAccepted {
				if err != nil {
					t.Fatalf("modify after definitely-unsent cancel: %v", err)
				}
				if len(result.EventSeqs) != 1 || result.EventSeqs[0] <= expected {
					t.Fatalf("modify result = %+v, prior frontier = %d", result, expected)
				}
				return
			}
			if !errors.Is(err, ErrOrderNotModifiable) {
				t.Fatalf("modify error = %v, want %v", err, ErrOrderNotModifiable)
			}
			var got int64
			if err := store.db.QueryRowContext(t.Context(), `SELECT MAX(event_seq) FROM order_events WHERE scope_key=? AND reserved_order_id=?`, scope.ScopeKey, orderID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != expected {
				t.Fatalf("rejected modify changed frontier: got %d want %d", got, expected)
			}
		})
	}
}

func TestStagePreTransmitModifyKeepsOlderUncertainCancelBlocking(t *testing.T) {
	store, _ := openTestStore(t)
	scope := testScope("modify-mixed-cancel-admission")
	const orderID int64 = 1002
	events := []OrderEventRecord{
		modifyAdmissionEvent(t, scope, orderID, "working", "broker-acknowledged", ActionPlace, "", "", "Submitted"),
		modifyAdmissionEvent(t, scope, orderID, "cancel-a", "cancel-requested", ActionCancel, "cancel-a", "", ""),
		modifyAdmissionEvent(t, scope, orderID, "cancel-a-error", "send-error", ActionCancel, "cancel-a", "may_have_written", ""),
		modifyAdmissionEvent(t, scope, orderID, "cancel-b", "cancel-requested", ActionCancel, "cancel-b", "", ""),
		modifyAdmissionEvent(t, scope, orderID, "cancel-b-error", "send-error", ActionCancel, "cancel-b", "definitely_unsent", ""),
	}
	seqs, err := store.AppendOrderEvents(t.Context(), events)
	if err != nil {
		t.Fatal(err)
	}
	expected := seqs[len(seqs)-1]
	modify := modifyAdmissionEvent(t, scope, orderID, "modify-requested", "modify-requested", ActionModify, "modify-1", "", "")
	_, err = store.StagePreTransmit(t.Context(), PreTransmitRequest{
		Scope: scope, RequestedOrderIDFloor: orderID, ReservedOrderID: orderID,
		ExpectedOrderEventSeq: &expected, Action: ActionModify, Origin: OriginAgentCLI,
		Events: []OrderEventRecord{modify},
	})
	if !errors.Is(err, ErrOrderNotModifiable) {
		t.Fatalf("modify error = %v, want older uncertain cancel to block", err)
	}
}

func modifyAdmissionEvent(t *testing.T, scope BrokerScope, orderID int64, eventKey, eventType string, action ActionKind, attemptID, disposition, status string) OrderEventRecord {
	t.Helper()
	payload := map[string]any{
		"version": 1, "type": eventType, "action_kind": action,
	}
	if attemptID != "" {
		payload["attempt_id"] = attemptID
	}
	if disposition != "" {
		payload["send_disposition"] = disposition
	}
	rawJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return OrderEventRecord{
		Scope: scope, EventKey: eventKey, AtMS: time.Now().UnixMilli(), Type: eventType,
		Action: action, Origin: OriginAgentCLI, ReservedOrderID: orderID, Status: status,
		RawJSON: rawJSON,
	}
}
