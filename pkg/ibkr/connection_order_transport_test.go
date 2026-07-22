package ibkr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type partialOrderWriteError struct {
	calls atomic.Int32
}

type zeroOrderWriteError struct {
	calls atomic.Int32
}

func (w *zeroOrderWriteError) Write([]byte) (int, error) {
	w.calls.Add(1)
	return 0, io.ErrClosedPipe
}

type protectedTransportOperation struct {
	name string
	run  func(context.Context, *Connector, ConnectorSessionBinding, PaperOrderGate, func() error) error
}

func protectedTransportOperations() []protectedTransportOperation {
	contract := func() *Contract {
		return &Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	}
	order := func(id int) *RawOrder {
		return &RawOrder{OrderID: id, Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: "DU7654321"}
	}
	exercise := func() OptionExerciseRequest {
		return OptionExerciseRequest{
			Contract: &Contract{
				ConID: 12345, Symbol: "TEST", SecType: "OPT", Expiry: "20260717", Strike: 100,
				Right: "C", Multiplier: 100, Exchange: "SMART", Currency: "USD", TradingClass: "TEST",
			},
			ExerciseAction: OptionExerciseActionExercise, ExerciseQuantity: 1, Account: "DU7654321",
		}
	}
	return []protectedTransportOperation{
		{name: "place", run: func(ctx context.Context, c *Connector, binding ConnectorSessionBinding, gate PaperOrderGate, guard func() error) error {
			return c.SubmitPaperOrderForSessionGuarded(ctx, binding, gate, contract(), order(0), guard)
		}},
		{name: "modify", run: func(ctx context.Context, c *Connector, binding ConnectorSessionBinding, gate PaperOrderGate, guard func() error) error {
			return c.SubmitPaperOrderForSessionGuarded(ctx, binding, gate, contract(), order(150), guard)
		}},
		{name: "cancel", run: func(ctx context.Context, c *Connector, binding ConnectorSessionBinding, gate PaperOrderGate, guard func() error) error {
			return c.CancelPaperOrderForSessionGuarded(ctx, binding, gate, 99, guard)
		}},
		{name: "exercise", run: func(ctx context.Context, _ *Connector, binding ConnectorSessionBinding, _ PaperOrderGate, guard func() error) error {
			req := exercise()
			var err error
			var epoch uint64
			req.TickerID, epoch, err = binding.connection.reserveNextRequestIDForEpoch(binding.epoch)
			if err != nil {
				return err
			}
			defer binding.connection.discardRequestIDReservation(req.TickerID)
			return binding.connection.exerciseOptionsForEpochGuarded(ctx, req, &epoch, guard)
		}},
	}
}

func waitForProtectedDispatch(t *testing.T, conn *Connection, before uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for conn.rateLimiter.GetMetrics().TotalRequests == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests; got != before+1 {
		t.Fatalf("protected dispatches=%d, want %d", got, before+1)
	}
}

func assertProtectedZeroWire(t *testing.T, conn *Connection, oldSocket, newSocket *safeBuffer, before uint64) {
	t.Helper()
	buffered := 0
	if conn.writer != nil {
		buffered = conn.writer.Buffered()
	}
	if oldSocket.Len() != 0 || newSocket.Len() != 0 || buffered != 0 {
		t.Fatalf("protected send wrote bytes old=%d new=%d buffered=%d", oldSocket.Len(), newSocket.Len(), buffered)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests - before; got != 1 {
		t.Fatalf("protected dispatches=%d, want exactly one", got)
	}
}

func TestProtectedBrokerWireGuardRejectsQueuedAuthorityDrift(t *testing.T) {
	for _, blocker := range []string{"freeze", "storage"} {
		for _, operation := range protectedTransportOperations() {
			t.Run(blocker+"/"+operation.name, func(t *testing.T) {
				conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
				binding, ok := connector.CaptureSession()
				if !ok {
					t.Fatal("capture exact connector session")
				}
				conn.pauseTransport()
				t.Cleanup(conn.resumeTransport)
				before := conn.rateLimiter.GetMetrics().TotalRequests
				var blocked atomic.Bool
				var guardCalls atomic.Int32
				done := make(chan error, 1)
				go func() {
					done <- operation.run(context.Background(), connector, binding, gate, func() error {
						guardCalls.Add(1)
						if blocked.Load() {
							return fmt.Errorf("%s authority engaged", blocker)
						}
						return nil
					})
				}()
				waitForProtectedDispatch(t, conn, before)
				blocked.Store(true)
				conn.resumeTransport()
				select {
				case err := <-done:
					if err == nil || brokerSendMayHaveBeenWritten(err) {
						t.Fatalf("queued %s %s err=%v, want definite pre-wire refusal", blocker, operation.name, err)
					}
				case <-time.After(time.Second):
					t.Fatalf("queued %s %s did not return", blocker, operation.name)
				}
				if guardCalls.Load() != 1 {
					t.Fatalf("wire guard calls=%d, want exactly one", guardCalls.Load())
				}
				assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
			})
		}
	}
}

func TestProtectedBrokerOperationsHonorCallerCancellationWhileQueued(t *testing.T) {
	for _, operation := range protectedTransportOperations() {
		t.Run(operation.name, func(t *testing.T) {
			conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
			binding, ok := connector.CaptureSession()
			if !ok {
				t.Fatal("capture exact connector session")
			}
			conn.pauseTransport()
			t.Cleanup(conn.resumeTransport)
			before := conn.rateLimiter.GetMetrics().TotalRequests
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- operation.run(ctx, connector, binding, gate, nil) }()
			waitForProtectedDispatch(t, conn, before)
			cancel()
			conn.resumeTransport()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("queued %s err=%v, want caller cancellation", operation.name, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("queued %s did not return after cancellation", operation.name)
			}
			assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
		})
	}
}

func TestProtectedBrokerOperationsHonorLimiterTimeoutAfterAdmission(t *testing.T) {
	for _, operation := range protectedTransportOperations() {
		t.Run(operation.name, func(t *testing.T) {
			conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
			binding, ok := connector.CaptureSession()
			if !ok {
				t.Fatal("capture exact connector session")
			}
			conn.rateLimiter.submitTimeoutFn = func(RequestType) time.Duration { return 20 * time.Millisecond }
			conn.pauseTransport()
			t.Cleanup(conn.resumeTransport)
			before := conn.rateLimiter.GetMetrics().TotalRequests
			done := make(chan error, 1)
			go func() { done <- operation.run(context.Background(), connector, binding, gate, nil) }()
			waitForProtectedDispatch(t, conn, before)
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "request timeout") {
					t.Fatalf("queued %s err=%v, want limiter timeout", operation.name, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("queued %s did not return at limiter timeout", operation.name)
			}
			conn.resumeTransport()
			time.Sleep(20 * time.Millisecond)
			assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
		})
	}
}

func TestProtectedBrokerOperationsRecheckCancellationAfterWireGuard(t *testing.T) {
	for _, operation := range protectedTransportOperations() {
		t.Run(operation.name, func(t *testing.T) {
			conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
			binding, ok := connector.CaptureSession()
			if !ok {
				t.Fatal("capture exact connector session")
			}
			before := conn.rateLimiter.GetMetrics().TotalRequests
			ctx, cancel := context.WithCancel(context.Background())
			guardEntered := make(chan struct{})
			releaseGuard := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- operation.run(ctx, connector, binding, gate, func() error {
					close(guardEntered)
					<-releaseGuard
					return nil
				})
			}()
			select {
			case <-guardEntered:
			case <-time.After(time.Second):
				t.Fatalf("%s wire guard was not reached", operation.name)
			}
			cancel()
			close(releaseGuard)
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("guard-paused %s err=%v, want cancellation", operation.name, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("guard-paused %s did not return", operation.name)
			}
			assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
		})
	}
}

func TestProtectedBrokerOperationsRejectSameEpochConnectionLossAndStop(t *testing.T) {
	for _, lifecycle := range []string{"connection_loss", "stop"} {
		for _, operation := range protectedTransportOperations() {
			t.Run(lifecycle+"/"+operation.name, func(t *testing.T) {
				conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
				binding, ok := connector.CaptureSession()
				if !ok {
					t.Fatal("capture exact connector session")
				}
				conn.pauseTransport()
				before := conn.rateLimiter.GetMetrics().TotalRequests
				done := make(chan error, 1)
				go func() { done <- operation.run(context.Background(), connector, binding, gate, nil) }()
				waitForProtectedDispatch(t, conn, before)

				lifecycleDone := make(chan struct{})
				go func() {
					if lifecycle == "stop" {
						_ = conn.Disconnect()
					} else {
						conn.handleDisconnection(io.EOF)
						conn.resumeTransport()
					}
					close(lifecycleDone)
				}()
				select {
				case err := <-done:
					if err == nil || (lifecycle != "stop" && brokerSendMayHaveBeenWritten(err)) {
						t.Fatalf("same-epoch %s %s err=%v, want zero-wire refusal", lifecycle, operation.name, err)
					}
				case <-time.After(2 * time.Second):
					t.Fatalf("same-epoch %s %s did not return", lifecycle, operation.name)
				}
				select {
				case <-lifecycleDone:
				case <-time.After(2 * time.Second):
					t.Fatalf("%s did not finish", lifecycle)
				}
				assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
			})
		}
	}
}

func TestOutboundSessionRevocationRejectsSendersStartingBeforeTransportLock(t *testing.T) {
	for _, lifecycle := range []string{"disconnect", "reconnect"} {
		t.Run(lifecycle, func(t *testing.T) {
			conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
			binding, ok := connector.CaptureSession()
			if !ok {
				t.Fatal("capture exact connector session")
			}
			beforeState := conn.outboundSessionState.Load()
			conn.transportMu.Lock()
			lifecycleResult := make(chan uint64, 1)
			go func() {
				if lifecycle == "reconnect" {
					lifecycleResult <- conn.beginOutboundSession()
					return
				}
				conn.invalidateOutboundSession(false)
				lifecycleResult <- 0
			}()
			deadline := time.Now().Add(time.Second)
			for conn.outboundSessionState.Load() == beforeState && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			revokedState := conn.outboundSessionState.Load()
			if revokedState == beforeState || revokedState&1 == 0 {
				conn.transportMu.Unlock()
				t.Fatalf("%s did not atomically publish revoked outbound state: before=%d after=%d", lifecycle, beforeState, revokedState)
			}

			before := conn.rateLimiter.GetMetrics().TotalRequests
			done := make(chan error, 1)
			go func() {
				done <- protectedTransportOperations()[0].run(context.Background(), connector, binding, gate, nil)
			}()
			waitForProtectedDispatch(t, conn, before)
			conn.transportMu.Unlock()
			state := <-lifecycleResult
			if lifecycle == "reconnect" {
				if !conn.activateOutboundSession(state) {
					t.Fatal("activate current reconnect generation")
				}
			}
			select {
			case err := <-done:
				if err == nil || brokerSendMayHaveBeenWritten(err) {
					t.Fatalf("post-revocation %s sender err=%v, want definite refusal", lifecycle, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("post-revocation %s sender did not return", lifecycle)
			}
			assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
		})
	}
}

func TestStaleOutboundActivationCannotClearConcurrentRevocation(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	stale := conn.beginOutboundSession()
	newer := conn.publishRevokedOutboundSession()
	if conn.activateOutboundSession(stale) {
		t.Fatal("stale outbound generation unexpectedly activated")
	}
	if got := conn.outboundSessionState.Load(); got != newer || got&1 == 0 {
		t.Fatalf("outbound state=%d, want newer revoked state %d", got, newer)
	}
	conn.resumeTransport()
}

func TestWhatIfPreviewHonorsCancellationAndLimiterTimeoutWhileQueued(t *testing.T) {
	for _, mode := range []string{"caller_cancel", "limiter_timeout"} {
		t.Run(mode, func(t *testing.T) {
			conn, connector, oldSocket, newSocket, _ := newQueuedInstructionReconnectFixture(t)
			conn.pauseTransport()
			t.Cleanup(conn.resumeTransport)
			if mode == "limiter_timeout" {
				conn.rateLimiter.submitTimeoutFn = func(RequestType) time.Duration { return 20 * time.Millisecond }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			before := conn.rateLimiter.GetMetrics().TotalRequests
			type outcome struct {
				result OrderWhatIfResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := connector.PreviewOrderWhatIf(ctx,
					&Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"},
					&RawOrder{Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: "DU7654321"},
				)
				done <- outcome{result: result, err: err}
			}()
			waitForProtectedDispatch(t, conn, before)
			if mode == "caller_cancel" {
				cancel()
			}
			select {
			case got := <-done:
				if got.err != nil || got.result.Status != OrderWhatIfStatusUnavailable {
					t.Fatalf("queued WhatIf result=%+v err=%v, want unavailable", got.result, got.err)
				}
			case <-time.After(time.Second):
				t.Fatalf("queued WhatIf did not return for %s", mode)
			}
			conn.resumeTransport()
			time.Sleep(20 * time.Millisecond)
			assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
		})
	}
}

func (w *partialOrderWriteError) Write(p []byte) (int, error) {
	w.calls.Add(1)
	if len(p) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	return max(1, len(p)/2), io.ErrUnexpectedEOF
}

func newPartialOrderTransportConnection(t *testing.T) (*Connection, *partialOrderWriteError, PaperOrderGate) {
	t.Helper()
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU7654321"}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(100)
	writer := &partialOrderWriteError{}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)
	gate := PaperOrderGate{
		Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID,
	}
	return conn, writer, gate
}

func assertOneUncertainOrderTransportAttempt(t *testing.T, conn *Connection, writer *partialOrderWriteError, send func() error) {
	t.Helper()
	before := conn.rateLimiter.GetMetrics().TotalRequests
	err := send()
	if err == nil {
		t.Fatal("partial broker-instruction write unexpectedly succeeded")
	}
	if !brokerSendMayHaveBeenWritten(err) {
		t.Fatalf("partial broker-instruction write err=%v, want uncertain-send disposition", err)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests - before; got != 1 {
		t.Fatalf("rate-limiter dispatches=%d, want exactly one", got)
	}
	if got := writer.calls.Load(); got != 1 {
		t.Fatalf("underlying write calls=%d, want exactly one partial attempt", got)
	}
}

func TestBrokerInstructionTransportsDoNotRetryPartialWrites(t *testing.T) {
	t.Run("place", func(t *testing.T) {
		conn, writer, gate := newPartialOrderTransportConnection(t)
		assertOneUncertainOrderTransportAttempt(t, conn, writer, func() error {
			return conn.PlacePaperOrder(gate, &IBKROrder{
				Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
				Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account,
			})
		})
	})

	t.Run("what-if", func(t *testing.T) {
		conn, writer, _ := newPartialOrderTransportConnection(t)
		before := conn.rateLimiter.GetMetrics().TotalRequests
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result, err := conn.PreviewOrderWhatIf(ctx, &IBKROrder{
			Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
			Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: "DU7654321",
		})
		if err != nil || result.Status != OrderWhatIfStatusUnavailable {
			t.Fatalf("partial WhatIf result=%+v err=%v, want unavailable result", result, err)
		}
		if got := conn.rateLimiter.GetMetrics().TotalRequests - before; got != 1 {
			t.Fatalf("WhatIf rate-limiter dispatches=%d, want exactly one", got)
		}
		if got := writer.calls.Load(); got != 1 {
			t.Fatalf("WhatIf underlying write calls=%d, want exactly one partial attempt", got)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		conn, writer, gate := newPartialOrderTransportConnection(t)
		assertOneUncertainOrderTransportAttempt(t, conn, writer, func() error {
			return conn.CancelPaperOrder(gate, 99)
		})
	})

	t.Run("exercise", func(t *testing.T) {
		conn, writer, _ := newPartialOrderTransportConnection(t)
		req := OptionExerciseRequest{
			TickerID: 101,
			Contract: &Contract{
				ConID: 12345, Symbol: "TEST", SecType: "OPT", Expiry: "20260717", Strike: 100,
				Right: "C", Multiplier: 100, Exchange: "SMART", Currency: "USD", TradingClass: "TEST",
			},
			ExerciseAction: OptionExerciseActionExercise, ExerciseQuantity: 1, Account: "DU7654321",
		}
		if err := validateOptionExerciseRequest(req); err != nil {
			t.Fatalf("exercise fixture: %v", err)
		}
		epoch, err := conn.captureBrokerInstructionEpoch()
		if err != nil {
			t.Fatalf("capture exercise epoch: %v", err)
		}
		assertOneUncertainOrderTransportAttempt(t, conn, writer, func() error {
			return conn.sendExerciseOptionsFrame(req, epoch)
		})
	})
}

func TestProtectedSendPreWireErrorKeepsDefiniteDisposition(t *testing.T) {
	conn := NewConnection(nil)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	conn.writer = nil
	err := conn.sendMessageWithType(conn.encodeMsg(reqAllOpenOrders, "1"), RequestTypeOrder)
	if err == nil {
		t.Fatal("send without writer unexpectedly succeeded")
	}
	if brokerSendMayHaveBeenWritten(err) {
		t.Fatalf("pre-wire writer absence reported uncertain: %v", err)
	}
	if _, ok := errors.AsType[*brokerSendDispositionError](err); !ok {
		t.Fatalf("pre-wire protected send lacked typed disposition: %v", err)
	}
}

func TestBrokerInstructionZeroByteWriterFailureIsDefinitelyUnsent(t *testing.T) {
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU7654321"}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(100)
	writer := &zeroOrderWriteError{}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)
	gate := PaperOrderGate{Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID}
	err := conn.PlacePaperOrder(gate, &IBKROrder{
		Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account,
	})
	if err == nil || SendDispositionOf(err) != SendDispositionDefinitelyUnsent || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("zero-byte place err=%v disposition=%q", err, SendDispositionOf(err))
	}
	if writer.calls.Load() != 1 {
		t.Fatalf("underlying writes=%d, want one zero-byte attempt", writer.calls.Load())
	}
}

func TestConnectorRetainsOnlyPossiblyWrittenOrderCorrelation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guard      func() error
		partial    bool
		wantRetain bool
	}{
		{name: "definite guard refusal rolls back", guard: func() error { return errors.New("refused") }},
		{name: "partial write retains", partial: true, wantRetain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, connector, _, _, gate := newQueuedInstructionReconnectFixture(t)
			if tc.partial {
				conn.writer = bufio.NewWriterSize(&partialOrderWriteError{}, 64*1024)
			}
			binding, ok := connector.CaptureSession()
			if !ok {
				t.Fatal("capture session")
			}
			order := &RawOrder{OrderID: 150, OrderRef: "ord-correlation", Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account}
			err := connector.SubmitPaperOrderForSessionGuarded(context.Background(), binding, gate,
				&Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"}, order, tc.guard)
			if err == nil {
				t.Fatal("order unexpectedly succeeded")
			}
			connector.orderMu.RLock()
			_, hasOrder := connector.openOrders["ord-correlation"]
			indexed := connector.brokerOrderIndex["150"] == "ord-correlation"
			connector.orderMu.RUnlock()
			if hasOrder != tc.wantRetain || indexed != tc.wantRetain {
				t.Fatalf("correlation retained order=%v index=%v, want %v (disposition %q)", hasOrder, indexed, tc.wantRetain, SendDispositionOf(err))
			}
			if tc.wantRetain && order.OrderID != 150 {
				t.Fatalf("uncertain order id=%d, want 150", order.OrderID)
			}
		})
	}
}

func TestCancelWireSuccessDoesNotFabricateTerminalState(t *testing.T) {
	conn, connector, _, _, gate := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session")
	}
	conn.ordersMu.Lock()
	conn.openOrders[99] = &IBKROrder{OrderID: 99, Status: "Submitted"}
	conn.ordersMu.Unlock()
	connector.orderMu.Lock()
	connector.openOrders["99"] = &trackedOrder{ID: "99", BrokerID: "99", Status: OrderStatusSubmitted}
	connector.orderMu.Unlock()
	if err := connector.CancelPaperOrderForSessionGuarded(context.Background(), binding, gate, 99, nil); err != nil {
		t.Fatal(err)
	}
	conn.ordersMu.RLock()
	connectionOrder := conn.openOrders[99]
	conn.ordersMu.RUnlock()
	connector.orderMu.RLock()
	tracked := connector.openOrders["99"]
	connector.orderMu.RUnlock()
	if connectionOrder == nil || connectionOrder.Status != "Submitted" || connectionOrder.CancelledTime != nil {
		t.Fatalf("connection fabricated cancel state: %+v", connectionOrder)
	}
	if tracked == nil || tracked.Status != OrderStatusSubmitted || tracked.CancelledAt != nil {
		t.Fatalf("connector fabricated cancel state: %+v", tracked)
	}
}

func newQueuedInstructionReconnectFixture(t *testing.T) (*Connection, *Connector, *safeBuffer, *safeBuffer, PaperOrderGate) {
	t.Helper()
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU7654321"}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(100)

	oldSocket := &safeBuffer{}
	newSocket := &safeBuffer{}
	conn.writer = bufio.NewWriter(oldSocket)

	connector := NewConnector(&ConnectorConfig{BaseConfig: cfg})
	connector.conn.rateLimiter.Stop()
	connector.conn = conn
	connector.running = true
	connector.ready = true
	conn.evidenceBarrier = &connector.evidenceBarrier
	conn.publicationBarrier = &connector.publicationBarrier

	gate := PaperOrderGate{
		Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID,
	}
	return conn, connector, oldSocket, newSocket, gate
}

func reconnectBeforeQueuedInstructionWrite(t *testing.T, conn *Connection, newSocket *safeBuffer, requestsBefore uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for conn.rateLimiter.GetMetrics().TotalRequests == requestsBefore && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests; got != requestsBefore+1 {
		t.Fatalf("queued broker-instruction dispatches=%d, want %d", got, requestsBefore+1)
	}

	oldEpoch := conn.BrokerSessionEpoch()
	conn.resetOrderIDReadiness()
	newEpoch := conn.BrokerSessionEpoch()
	if newEpoch == oldEpoch {
		t.Fatalf("socket epoch did not advance: %d", oldEpoch)
	}
	conn.writer = bufio.NewWriter(newSocket)
	conn.observeNextValidOrderIDAtEpoch(500, newEpoch)
	conn.resumeTransport()
}

func assertNoQueuedInstructionCrossedEpoch(t *testing.T, conn *Connection, oldSocket, newSocket *safeBuffer, requestsBefore uint64) {
	t.Helper()
	if got := oldSocket.Len(); got != 0 {
		t.Fatalf("queued instruction wrote %d bytes on old socket", got)
	}
	if got := newSocket.Len(); got != 0 {
		t.Fatalf("queued instruction wrote %d bytes on new socket", got)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests - requestsBefore; got != 1 {
		t.Fatalf("queued instruction dispatches=%d, want exactly one without retry", got)
	}
}

func TestBrokerInstructionsQueuedBeforeReconnectNeverWriteNewSocket(t *testing.T) {
	t.Run("place", func(t *testing.T) {
		conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
		conn.pauseTransport()
		t.Cleanup(conn.resumeTransport)
		before := conn.rateLimiter.GetMetrics().TotalRequests
		done := make(chan error, 1)
		go func() {
			done <- connector.SubmitPaperOrder(gate,
				&Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"},
				&RawOrder{Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account},
			)
		}()

		reconnectBeforeQueuedInstructionWrite(t, conn, newSocket, before)
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cross-epoch place unexpectedly succeeded")
			}
			if brokerSendMayHaveBeenWritten(err) {
				t.Fatalf("cross-epoch place reported possible wire write: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cross-epoch place did not return")
		}
		assertNoQueuedInstructionCrossedEpoch(t, conn, oldSocket, newSocket, before)
	})

	t.Run("what-if", func(t *testing.T) {
		conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
		conn.pauseTransport()
		t.Cleanup(conn.resumeTransport)
		before := conn.rateLimiter.GetMetrics().TotalRequests
		type outcome struct {
			result OrderWhatIfResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := connector.PreviewOrderWhatIf(context.Background(),
				&Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"},
				&RawOrder{Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account},
			)
			done <- outcome{result: result, err: err}
		}()

		reconnectBeforeQueuedInstructionWrite(t, conn, newSocket, before)
		select {
		case got := <-done:
			if got.err != nil || got.result.Status != OrderWhatIfStatusUnavailable {
				t.Fatalf("cross-epoch WhatIf result=%+v err=%v, want unavailable", got.result, got.err)
			}
		case <-time.After(time.Second):
			t.Fatal("cross-epoch WhatIf did not return")
		}
		assertNoQueuedInstructionCrossedEpoch(t, conn, oldSocket, newSocket, before)
	})

	t.Run("cancel", func(t *testing.T) {
		conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
		conn.pauseTransport()
		t.Cleanup(conn.resumeTransport)
		before := conn.rateLimiter.GetMetrics().TotalRequests
		done := make(chan error, 1)
		go func() { done <- connector.CancelPaperOrder(gate, 99) }()

		reconnectBeforeQueuedInstructionWrite(t, conn, newSocket, before)
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cross-epoch cancel unexpectedly succeeded")
			}
			if brokerSendMayHaveBeenWritten(err) {
				t.Fatalf("cross-epoch cancel reported possible wire write: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cross-epoch cancel did not return")
		}
		assertNoQueuedInstructionCrossedEpoch(t, conn, oldSocket, newSocket, before)
	})

	t.Run("exercise", func(t *testing.T) {
		conn, _, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
		conn.pauseTransport()
		t.Cleanup(conn.resumeTransport)
		before := conn.rateLimiter.GetMetrics().TotalRequests
		done := make(chan error, 1)
		go func() {
			tickerID, epoch, err := conn.nextRequestIDForForwardingWithEpoch()
			if err != nil {
				done <- err
				return
			}
			defer conn.discardRequestIDReservation(tickerID)
			done <- conn.exerciseOptionsForEpoch(OptionExerciseRequest{
				TickerID: tickerID,
				Contract: &Contract{
					ConID: 12345, Symbol: "TEST", SecType: "OPT", Expiry: "20260717", Strike: 100,
					Right: "C", Multiplier: 100, Exchange: "SMART", Currency: "USD", TradingClass: "TEST",
				},
				ExerciseAction: OptionExerciseActionExercise, ExerciseQuantity: 1, Account: gate.Account,
			}, &epoch)
		}()

		reconnectBeforeQueuedInstructionWrite(t, conn, newSocket, before)
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cross-epoch exercise unexpectedly succeeded")
			}
			if brokerSendMayHaveBeenWritten(err) {
				t.Fatalf("cross-epoch exercise reported possible wire write: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cross-epoch exercise did not return")
		}
		assertNoQueuedInstructionCrossedEpoch(t, conn, oldSocket, newSocket, before)
	})
}

func TestRequestAllOpenOrdersQueuedBeforeReconnectNeverWritesNewSocket(t *testing.T) {
	conn, _, oldSocket, newSocket, _ := newQueuedInstructionReconnectFixture(t)
	conn.pauseTransport()
	t.Cleanup(conn.resumeTransport)
	before := conn.rateLimiter.GetMetrics().TotalRequests
	done := make(chan error, 1)
	go func() { done <- conn.RequestAllOpenOrders() }()

	reconnectBeforeQueuedInstructionWrite(t, conn, newSocket, before)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cross-epoch reqAllOpenOrders unexpectedly succeeded")
		}
		if brokerSendMayHaveBeenWritten(err) {
			t.Fatalf("cross-epoch reqAllOpenOrders reported possible wire write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-epoch reqAllOpenOrders did not return")
	}
	assertNoQueuedInstructionCrossedEpoch(t, conn, oldSocket, newSocket, before)
}

func TestBoundQueuedOrderAllowsDisconnectAndNeverWrites(t *testing.T) {
	conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
	conn.SetOnDisconnect(func(error) { connector.onConnectionLost(conn) })
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("ready connector did not capture session")
	}
	conn.pauseTransport()
	t.Cleanup(conn.resumeTransport)
	before := conn.rateLimiter.GetMetrics().TotalRequests
	done := make(chan error, 1)
	go func() {
		ran, err := connector.WithBoundBrokerSession(binding, func() error {
			return connector.SubmitPaperOrderForSession(binding, gate,
				&Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"},
				&RawOrder{OrderID: 100, Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account},
			)
		})
		if !ran && err == nil {
			err = ErrIBKRUnavailable
		}
		done <- err
	}()

	deadline := time.Now().Add(time.Second)
	for conn.rateLimiter.GetMetrics().TotalRequests == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests; got != before+1 {
		t.Fatalf("queued dispatches=%d, want %d", got, before+1)
	}

	disconnected := make(chan struct{})
	go func() {
		conn.handleDisconnection(io.EOF)
		close(disconnected)
	}()
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("disconnect deadlocked behind bound broker-session read lock")
	}
	conn.resetOrderIDReadiness()
	conn.writer = bufio.NewWriter(newSocket)
	conn.resumeTransport()

	select {
	case err := <-done:
		if err == nil || brokerSendMayHaveBeenWritten(err) {
			t.Fatalf("disconnected bound order err=%v, want definite pre-wire refusal", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bound queued order did not return after disconnect")
	}
	if oldSocket.Len() != 0 || newSocket.Len() != 0 {
		t.Fatalf("disconnected bound order wrote bytes old=%d new=%d", oldSocket.Len(), newSocket.Len())
	}
}

func TestBoundQueuedOrderDoesNotDeadlockUnpublicationWhileTransportPaused(t *testing.T) {
	conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("ready connector did not capture session")
	}
	conn.pauseTransport()
	before := conn.rateLimiter.GetMetrics().TotalRequests
	done := make(chan error, 1)
	go func() {
		ran, err := connector.WithBoundBrokerSession(binding, func() error {
			return connector.SubmitPaperOrderForSession(binding, gate,
				&Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"},
				&RawOrder{OrderID: 100, Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account},
			)
		})
		if !ran && err == nil {
			err = ErrIBKRUnavailable
		}
		done <- err
	}()
	waitForProtectedDispatch(t, conn, before)

	// Reconnect first revokes the old outbound generation and leaves transport
	// paused. Daemon unpublication must still acquire the exclusive publication
	// barrier; Stop releases the parked sender only after that transition.
	conn.invalidateOutboundSession(true)
	unpublished := make(chan struct{})
	go func() {
		connector.WithBrokerEvidenceMutation(func() {})
		close(unpublished)
	}()
	select {
	case <-unpublished:
	case <-time.After(time.Second):
		t.Fatal("connector unpublication deadlocked behind paused bound sender")
	}

	conn.invalidateOutboundSession(false)
	select {
	case err := <-done:
		if err == nil || brokerSendMayHaveBeenWritten(err) {
			t.Fatalf("retired paused order err=%v, want definite zero-wire refusal", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retired paused order did not return after final invalidation")
	}
	assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
}
