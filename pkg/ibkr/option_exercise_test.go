package ibkr

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

func validOptionExerciseRequestForTest() OptionExerciseRequest {
	return OptionExerciseRequest{
		TickerID: 41,
		Contract: &Contract{
			ConID:        12345,
			Symbol:       "TEST",
			SecType:      "OPT",
			Expiry:       "20260717",
			Strike:       100,
			Right:        "C",
			Multiplier:   100,
			Exchange:     "SMART",
			Currency:     "USD",
			TradingClass: "TEST",
		},
		ExerciseAction:   OptionExerciseActionExercise,
		ExerciseQuantity: 1,
		Account:          "TEST-ACCOUNT",
	}
}

func assertExerciseSendDisposition(t *testing.T, err error, want SendDisposition, cause error) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want disposition %q", want)
	}
	if got := SendDispositionOf(err); got != want {
		t.Fatalf("SendDispositionOf(%v)=%q, want %q", err, got, want)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("error=%v, want errors.Is(..., %v)", err, cause)
	}
}

type partialExerciseWriteError struct {
	calls atomic.Int32
}

func (w *partialExerciseWriteError) Write(p []byte) (int, error) {
	w.calls.Add(1)
	if len(p) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := len(p) / 2
	if n == 0 {
		n = 1
	}
	return n, io.ErrUnexpectedEOF
}

func newExerciseTransportFixture(t *testing.T, namespaceReady bool) (*Connection, *Connector, ConnectorSessionBinding, *partialExerciseWriteError) {
	t.Helper()
	conn := NewConnection(&ConnectionConfig{
		Host:     "127.0.0.1",
		Port:     7497,
		ClientID: 41,
		Account:  "TEST-ACCOUNT",
	})
	t.Cleanup(conn.rateLimiter.Stop)
	conn.serverVersion = 99
	conn.signalHandshakeReady()
	if namespaceReady {
		conn.observeNextValidOrderID(1)
	}
	conn.setStatus(StatusConnected)
	writer := &partialExerciseWriteError{}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)

	connector := &Connector{conn: conn, ready: true}
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exercise transport session")
	}
	return conn, connector, binding, writer
}

func TestExerciseOptionsTradingDisabledIsDefinitelyUnsent(t *testing.T) {
	if tradingEnabled {
		t.Skip("default-build contract")
	}
	req := validOptionExerciseRequestForTest()
	var conn *Connection
	var connector *Connector
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "connection", run: func() error { return conn.ExerciseOptions(req) }},
		{name: "connector", run: func() error { return connector.ExerciseOptions(context.Background(), req) }},
		{name: "session", run: func() error {
			return connector.ExerciseOptionsForSession(context.Background(), ConnectorSessionBinding{}, req)
		}},
		{name: "guarded_session", run: func() error {
			return connector.ExerciseOptionsForSessionGuarded(context.Background(), ConnectorSessionBinding{}, req, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertExerciseSendDisposition(t, tc.run(), SendDispositionDefinitelyUnsent, ErrTradingDisabled)
		})
	}
}

func TestExerciseOptionsPreWireErrorsAreDefinitelyUnsent(t *testing.T) {
	if !tradingEnabled {
		t.Skip("trading-build wire contract")
	}
	req := validOptionExerciseRequestForTest()
	disconnected := NewConnection(nil)
	t.Cleanup(disconnected.rateLimiter.Stop)
	invalid := req
	invalid.Contract = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var nilCtx context.Context

	for _, tc := range []struct {
		name  string
		run   func() error
		cause error
	}{
		{name: "connector_nil_context", run: func() error { return (&Connector{}).ExerciseOptions(nilCtx, req) }},
		{name: "connector_canceled_context", run: func() error { return (&Connector{}).ExerciseOptions(ctx, req) }, cause: context.Canceled},
		{name: "connector_disconnected", run: func() error { return (&Connector{}).ExerciseOptions(context.Background(), req) }},
		{name: "session_nil_context", run: func() error {
			return (&Connector{}).ExerciseOptionsForSession(nilCtx, ConnectorSessionBinding{}, req)
		}},
		{name: "session_canceled_context", run: func() error {
			return (&Connector{}).ExerciseOptionsForSession(ctx, ConnectorSessionBinding{}, req)
		}, cause: context.Canceled},
		{name: "session_not_current", run: func() error {
			return (&Connector{}).ExerciseOptionsForSession(context.Background(), ConnectorSessionBinding{}, req)
		}},
		{name: "connection_validation", run: func() error { return disconnected.ExerciseOptions(invalid) }},
		{name: "connection_disconnected", run: func() error { return disconnected.ExerciseOptions(req) }},
		{name: "nil_connection", run: func() error {
			var conn *Connection
			return conn.ExerciseOptions(req)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertExerciseSendDisposition(t, tc.run(), SendDispositionDefinitelyUnsent, tc.cause)
		})
	}

	t.Run("request_id_reservation", func(t *testing.T) {
		_, connector, binding, writer := newExerciseTransportFixture(t, false)
		req := req
		req.TickerID = 0
		err := connector.ExerciseOptionsForSession(context.Background(), binding, req)
		assertExerciseSendDisposition(t, err, SendDispositionDefinitelyUnsent, nil)
		if got := writer.calls.Load(); got != 0 {
			t.Fatalf("wire writes=%d, want 0", got)
		}
	})

	t.Run("request_id_claim", func(t *testing.T) {
		conn, _, _, writer := newExerciseTransportFixture(t, true)
		conn.observeNextValidOrderID(100)
		req := req
		req.TickerID = 41
		err := conn.ExerciseOptions(req)
		assertExerciseSendDisposition(t, err, SendDispositionDefinitelyUnsent, ErrBrokerIDNamespaceConflict)
		if got := writer.calls.Load(); got != 0 {
			t.Fatalf("wire writes=%d, want 0", got)
		}
	})

	t.Run("wire_guard", func(t *testing.T) {
		_, connector, binding, writer := newExerciseTransportFixture(t, true)
		req := req
		req.TickerID = 0
		guardErr := errors.New("exercise authority withdrawn")
		err := connector.ExerciseOptionsForSessionGuarded(context.Background(), binding, req, func() error {
			return guardErr
		})
		assertExerciseSendDisposition(t, err, SendDispositionDefinitelyUnsent, guardErr)
		if got := writer.calls.Load(); got != 0 {
			t.Fatalf("wire writes=%d, want 0", got)
		}
	})
}

func TestExerciseOptionsTransportDispositionIsPreserved(t *testing.T) {
	if !tradingEnabled {
		t.Skip("trading-build wire contract")
	}
	for _, tc := range []struct {
		name string
		run  func(context.Context, *Connection, *Connector, ConnectorSessionBinding, OptionExerciseRequest) error
	}{
		{name: "connection", run: func(_ context.Context, conn *Connection, _ *Connector, _ ConnectorSessionBinding, req OptionExerciseRequest) error {
			return conn.ExerciseOptions(req)
		}},
		{name: "connector", run: func(ctx context.Context, _ *Connection, connector *Connector, _ ConnectorSessionBinding, req OptionExerciseRequest) error {
			req.TickerID = 0
			return connector.ExerciseOptions(ctx, req)
		}},
		{name: "session", run: func(ctx context.Context, _ *Connection, connector *Connector, binding ConnectorSessionBinding, req OptionExerciseRequest) error {
			req.TickerID = 0
			return connector.ExerciseOptionsForSession(ctx, binding, req)
		}},
		{name: "guarded_session", run: func(ctx context.Context, _ *Connection, connector *Connector, binding ConnectorSessionBinding, req OptionExerciseRequest) error {
			req.TickerID = 0
			return connector.ExerciseOptionsForSessionGuarded(ctx, binding, req, func() error { return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, connector, binding, writer := newExerciseTransportFixture(t, true)
			err := tc.run(context.Background(), conn, connector, binding, validOptionExerciseRequestForTest())
			assertExerciseSendDisposition(t, err, SendDispositionMayHaveWritten, io.ErrUnexpectedEOF)
			if got := writer.calls.Load(); got != 1 {
				t.Fatalf("wire writes=%d, want 1", got)
			}
		})
	}
}

func TestEncodeExerciseOptionsMessage(t *testing.T) {
	t.Parallel()
	conn := &Connection{serverVersion: 99}
	req := OptionExerciseRequest{
		TickerID: 7,
		Contract: &Contract{
			ConID:        12345,
			Symbol:       "aapl",
			SecType:      "OPT",
			Expiry:       "20260619",
			Strike:       100,
			Right:        "c",
			Multiplier:   100,
			Exchange:     "",
			Currency:     "",
			LocalSymbol:  "AAPL  260619C00100000",
			TradingClass: "AAPL",
		},
		ExerciseAction:   OptionExerciseActionExercise,
		ExerciseQuantity: 2,
		Account:          "DU123",
		Override:         0,
	}

	msg, err := conn.encodeExerciseOptionsMessage(req)
	if err != nil {
		t.Fatalf("encodeExerciseOptionsMessage: %v", err)
	}
	fields := strings.Split(string(msg), "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	want := []string{
		"21",
		"2",
		"7",
		"12345",
		"AAPL",
		"OPT",
		"20260619",
		"100",
		"C",
		"100",
		"SMART",
		"USD",
		"AAPL  260619C00100000",
		"AAPL",
		"1",
		"2",
		"DU123",
		"0",
	}
	if strings.Join(fields, "|") != strings.Join(want, "|") {
		t.Fatalf("fields=%q, want %q", fields, want)
	}
}

func TestValidateOptionExerciseRequest(t *testing.T) {
	t.Parallel()
	valid := OptionExerciseRequest{
		TickerID: 1,
		Contract: &Contract{
			Symbol:   "AAPL",
			SecType:  "OPT",
			Expiry:   "20260619",
			Strike:   100,
			Right:    "C",
			Currency: "USD",
		},
		ExerciseAction:   OptionExerciseActionExercise,
		ExerciseQuantity: 1,
		Account:          "DU123",
	}
	if err := validateOptionExerciseRequest(valid); err != nil {
		t.Fatalf("valid exercise request failed: %v", err)
	}
	invalid := valid
	invalid.Override = 2
	if err := validateOptionExerciseRequest(invalid); err == nil || !strings.Contains(err.Error(), "override") {
		t.Fatalf("invalid override err=%v, want override", err)
	}
	invalid = valid
	invalid.ExerciseAction = 9
	if err := validateOptionExerciseRequest(invalid); err == nil || !strings.Contains(err.Error(), "action") {
		t.Fatalf("invalid action err=%v, want action", err)
	}
}
