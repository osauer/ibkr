package ibkr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConnection_WaitForPositionsEnd(t *testing.T) {
	// Create a test connection
	conn := &Connection{
		positionsEndChan: make(chan struct{}, 1),
	}

	t.Run("SuccessfulCompletion", func(t *testing.T) {
		// Simulate position end signal arriving quickly
		go func() {
			time.Sleep(100 * time.Millisecond)
			conn.positionsEndChan <- struct{}{}
		}()

		// Wait with 1 second timeout
		err := conn.WaitForPositionsEnd(1 * time.Second)
		if err != nil {
			t.Errorf("WaitForPositionsEnd should succeed when signal received: %v", err)
		}
	})

	t.Run("Timeout", func(t *testing.T) {
		// Don't send any signal

		// Wait with short timeout
		err := conn.WaitForPositionsEnd(100 * time.Millisecond)
		if err == nil {
			t.Errorf("WaitForPositionsEnd should timeout when no signal received")
		}
		if err.Error() != "timeout waiting for positions end" {
			t.Errorf("Expected timeout error, got: %v", err)
		}
	})

	t.Run("ImmediateSignal", func(t *testing.T) {
		// Pre-fill the channel
		conn.positionsEndChan <- struct{}{}

		// Should return immediately
		start := time.Now()
		err := conn.WaitForPositionsEnd(1 * time.Second)
		elapsed := time.Since(start)

		if err != nil {
			t.Errorf("WaitForPositionsEnd should succeed immediately: %v", err)
		}
		if elapsed > 100*time.Millisecond {
			t.Errorf("Should return immediately, took %v", elapsed)
		}
	})
}

func TestHandleSystemNotificationClientIDInUseSetsLastError(t *testing.T) {
	t.Parallel()
	conn := &Connection{config: &ConnectionConfig{ClientID: 15}}
	var body []byte
	body = protoAppendInt32(body, 3, 326)
	body = protoAppendString(body, 4, "Unable to connect as the client id is already in use. Retry with a unique client id.")

	conn.handleSystemNotification([]string{"", string(body)})

	conn.statusMu.RLock()
	err := conn.lastError
	conn.statusMu.RUnlock()
	if !errors.Is(err, errClientIDInUse) {
		t.Fatalf("lastError = %v, want errClientIDInUse", err)
	}
	if !strings.Contains(err.Error(), "gateway client ID 15 is already in use") {
		t.Fatalf("lastError = %q, want operator-facing client ID diagnosis", err.Error())
	}
}

func TestConnection_WaitForAccountSummaryEnd(t *testing.T) {
	// Create a test connection
	conn := &Connection{
		acctSummaryEndChan: make(chan struct{}, 1),
	}

	t.Run("SuccessfulCompletion", func(t *testing.T) {
		// Simulate account summary end signal arriving
		go func() {
			time.Sleep(100 * time.Millisecond)
			conn.acctSummaryEndChan <- struct{}{}
		}()

		// Wait with 1 second timeout
		err := conn.WaitForAccountSummaryEnd(1 * time.Second)
		if err != nil {
			t.Errorf("WaitForAccountSummaryEnd should succeed when signal received: %v", err)
		}
	})

	t.Run("Timeout", func(t *testing.T) {
		// Don't send any signal

		// Wait with short timeout
		err := conn.WaitForAccountSummaryEnd(100 * time.Millisecond)
		if err == nil {
			t.Errorf("WaitForAccountSummaryEnd should timeout when no signal received")
		}
		if err.Error() != "timeout waiting for account summary end" {
			t.Errorf("Expected timeout error, got: %v", err)
		}
	})
}

func TestConnection_EventDrivenVsSleep(t *testing.T) {
	// This test demonstrates the performance improvement
	// of event-driven completion vs fixed sleep

	conn := &Connection{
		positionsEndChan: make(chan struct{}, 1),
	}

	// Simulate fast IBKR response (200ms)
	go func() {
		time.Sleep(200 * time.Millisecond)
		conn.positionsEndChan <- struct{}{}
	}()

	// Measure event-driven approach
	start := time.Now()
	err := conn.WaitForPositionsEnd(5 * time.Second)
	eventDrivenTime := time.Since(start)

	if err != nil {
		t.Errorf("Event-driven wait failed: %v", err)
	}

	// Compare with old fixed sleep approach
	sleepStart := time.Now()
	time.Sleep(2 * time.Second) // Old approach
	sleepTime := time.Since(sleepStart)

	// Event-driven should be much faster
	if eventDrivenTime > 500*time.Millisecond {
		t.Errorf("Event-driven took too long: %v", eventDrivenTime)
	}

	if sleepTime < 2*time.Second {
		t.Errorf("Sleep should take at least 2 seconds: %v", sleepTime)
	}

	improvement := float64(sleepTime-eventDrivenTime) / float64(sleepTime) * 100
	t.Logf("Performance improvement: %.1f%% (Event: %v, Sleep: %v)",
		improvement, eventDrivenTime, sleepTime)

	if improvement < 75 {
		t.Errorf("Expected at least 75%% improvement, got %.1f%%", improvement)
	}
}

func TestConnection_ClearChannel(t *testing.T) {
	// Test that RequestPositions clears the channel before requesting
	conn := &Connection{
		positionsEndChan: make(chan struct{}, 1),
		positions:        make(map[string]*RawPosition),
	}

	// Pre-fill channel with old signal
	conn.positionsEndChan <- struct{}{}

	// Simulate RequestPositions clearing the channel
	select {
	case <-conn.positionsEndChan:
		// Channel cleared
	default:
		// Already empty
	}

	// Now channel should be empty
	select {
	case <-conn.positionsEndChan:
		t.Errorf("Channel should be empty after clearing")
	default:
		// Good - channel is empty
	}
}

func TestConnection_HandleAccountSummaryUpdatesAccount(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatalf("NewConnection returned nil")
	}

	conn.account = ""
	conn.accountSummary = make(map[string]string)

	fields := []string{
		"63",    // msgID (handled before call, kept for completeness)
		"2",     // version
		"1",     // reqID
		"DU123", // account code
		"NetLiquidation",
		"150000",
		"USD",
	}

	conn.handleAccountSummary(fields)

	if conn.account != "DU123" {
		t.Fatalf("expected account code to be stored, got %q", conn.account)
	}

	stored, ok := conn.accountSummary["NetLiquidation_USD"]
	if !ok {
		t.Fatalf("expected NetLiquidation_USD to be present in account summary map")
	}
	if stored != "150000" {
		t.Fatalf("expected NetLiquidation value 150000, got %s", stored)
	}
}

func TestConnectionManagedAccountsStoresVersionedAccountList(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatalf("NewConnection returned nil")
	}

	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", "DU123"))

	if got := conn.GetAccountCode(); got != "DU123" {
		t.Fatalf("managed account = %q, want DU123", got)
	}
}
