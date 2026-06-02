package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
	"github.com/osauer/ibkr/internal/update"
)

func TestRunRestartCoreStartsWhenNoDaemonWasRunning(t *testing.T) {
	t.Setenv("IBKR_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	started := false
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			started = true
			return 42, rpc.HealthResult{DaemonVersion: "test", Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 4001, ClientID: 15}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if !started {
		t.Fatal("daemon was not started")
	}
	if !strings.Contains(out.String(), "no daemon was running") || !strings.Contains(out.String(), "started daemon pid 42") {
		t.Fatalf("output missing start message:\n%s", out.String())
	}
}

func TestRunRestartCoreRestartsGracefullyWithJSON(t *testing.T) {
	t.Setenv("IBKR_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	stoppedPID := 0
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 11, Command: "/tmp/ibkr daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			stoppedPID = pid
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 12, rpc.HealthResult{DaemonVersion: "test", Connected: false, LastError: "no gateway"}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if stoppedPID != 11 {
		t.Fatalf("stoppedPID = %d, want 11", stoppedPID)
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "restarted" || !res.WasRunning || !res.Graceful || res.OldPID != 11 || res.NewPID != 12 {
		t.Fatalf("result = %+v", res)
	}
	if res.Health.LastError != "no gateway" {
		t.Fatalf("health = %+v", res.Health)
	}
}

func TestRunRestartCoreForceEscalatesOnlyAfterGracefulTimeout(t *testing.T) {
	t.Setenv("IBKR_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{force: true, timeout: time.Second, out: &out, err: &errBuf}
	killedPID := 0
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 21, Command: "/tmp/ibkr daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			return fmt.Errorf("wrapped: %w", update.ErrStopTimeout)
		},
		kill: func(pid int, _ time.Duration) error {
			killedPID = pid
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 22, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if killedPID != 21 {
		t.Fatalf("killedPID = %d, want 21", killedPID)
	}
	if !strings.Contains(out.String(), "forcing SIGKILL") {
		t.Fatalf("output missing force message:\n%s", out.String())
	}
}

func TestRunRestartCoreTimeoutWithoutForceFails(t *testing.T) {
	t.Setenv("IBKR_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 31, Command: "/tmp/ibkr daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			return fmt.Errorf("wrapped: %w", update.ErrStopTimeout)
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			t.Fatal("startAndHealth should not run after non-forced timeout")
			return 0, rpc.HealthResult{}, nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "--force") {
		t.Fatalf("stderr missing force hint:\n%s", errBuf.String())
	}
}

func TestRunRestartRejectsUnexpectedArgument(t *testing.T) {
	var out, errBuf bytes.Buffer
	exit := RunRestart(context.Background(), []string{"extra"}, &out, &errBuf)
	if exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if !strings.Contains(errBuf.String(), "unexpected argument") {
		t.Fatalf("stderr missing argument error:\n%s", errBuf.String())
	}
}
