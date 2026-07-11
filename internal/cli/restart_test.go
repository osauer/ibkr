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

	"github.com/osauer/ibkr/v2/internal/rpc"
	"github.com/osauer/ibkr/v2/internal/update"
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

func TestRunRestartCoreReportsStoppedBeforeHealthWait(t *testing.T) {
	t.Setenv("IBKR_SOCKET", t.TempDir()+"/ibkr.sock")

	var combined bytes.Buffer
	opts := &restartOptions{timeout: time.Second, out: &combined, err: &combined}
	exit := runRestartCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 15, Command: "/tmp/ibkr daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(int, time.Duration) error {
			return nil
		},
		startAndHealth: func(_ context.Context, _ string, progress io.Writer, _ bool) (int, rpc.HealthResult, error) {
			fmt.Fprintln(progress, "health wait")
			return 16, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d\n%s", exit, combined.String())
	}
	got := combined.String()
	stopped := strings.Index(got, "stopped daemon pid 15 gracefully")
	wait := strings.Index(got, "health wait")
	started := strings.Index(got, "started daemon pid 16")
	if stopped < 0 || wait < 0 || started < 0 {
		t.Fatalf("missing expected output:\n%s", got)
	}
	if strings.Contains(got, "stopping daemon pid 15 gracefully") {
		t.Fatalf("graceful restart should not print duplicate stop progress and confirmation lines:\n%s", got)
	}
	if stopped > wait || wait > started {
		t.Fatalf("output order is wrong:\n%s", got)
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

func TestRunRestartAllCoreRestartsDaemonAndRunningApp(t *testing.T) {
	// Empty = default daemon scope: a set IBKR_SOCKET makes plain restart
	// skip app management entirely, which is tested separately below. All
	// deps are fakes, so the default scope touches nothing real.
	t.Setenv("IBKR_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	daemonStopped := 0
	appStopped := 0
	appFindCalls := 0
	appStartCalled := false
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{PID: 41, Command: "/tmp/ibkr daemon", SocketPath: "sock", LockPath: "lock"}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			daemonStopped = pid
			return nil
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 42, rpc.HealthResult{DaemonVersion: "test", Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7496, ClientID: 15}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			appFindCalls++
			if appFindCalls == 1 {
				return appProcess{
					PID:     51,
					Command: "/tmp/ibkr app --remote",
					Args:    []string{"app", "--remote"},
				}, nil
			}
			return appProcess{
				PID:     52,
				Command: "/tmp/ibkr app --remote",
				Args:    []string{"app", "--remote"},
			}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			appStopped = pid
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			appStartCalled = true
			return 0, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if daemonStopped != 41 {
		t.Fatalf("daemonStopped = %d, want 41", daemonStopped)
	}
	if appStopped != 51 {
		t.Fatalf("appStopped = %d, want 51", appStopped)
	}
	if appStartCalled {
		t.Fatal("manual app start should not run when supervisor respawned the app")
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "restarted" || res.Target != "daemon" || res.OldPID != 41 || res.NewPID != 42 || !res.Graceful {
		t.Fatalf("daemon result = %+v", res)
	}
	if res.App == nil {
		t.Fatalf("app result missing: %+v", res)
	}
	if res.App.Action != "restarted" || res.App.Target != "app" || res.App.OldPID != 51 || res.App.NewPID != 52 || !res.App.Graceful {
		t.Fatalf("app result = %+v", *res.App)
	}
	if strings.Join(res.App.Args, " ") != "app --remote" {
		t.Fatalf("app args = %q", strings.Join(res.App.Args, " "))
	}
}

func TestRunRestartAllCoreSkipsAppWhenNotRunning(t *testing.T) {
	t.Setenv("IBKR_SOCKET", "")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	appStartCalled := false
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 61, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		start: func(context.Context, []string) (int, error) {
			appStartCalled = true
			return 0, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if appStartCalled {
		t.Fatal("plain restart should not start a new app when none was running")
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App != nil {
		t.Fatalf("app result = %+v, want omitted", res.App)
	}
}

func TestRunRestartAllCoreSkipsAppWhenSocketOverridden(t *testing.T) {
	t.Setenv("IBKR_SOCKET", t.TempDir()+"/ibkr.sock")

	var out, errBuf bytes.Buffer
	opts := &restartOptions{jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	appFindCalled := false
	exit := runRestartAllCore(context.Background(), opts, restartDeps{
		find: func(context.Context, string) (update.DaemonProcess, error) {
			return update.DaemonProcess{}, update.ErrDaemonNotRunning
		},
		startAndHealth: func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error) {
			return 71, rpc.HealthResult{DaemonVersion: "test"}, nil
		},
	}, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			appFindCalled = true
			return appProcess{}, errAppNotRunning
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("app start must not run when IBKR_SOCKET is overridden")
			return 0, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if appFindCalled {
		t.Fatal("app discovery must not run when IBKR_SOCKET is overridden")
	}
	var res restartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.App == nil {
		t.Fatalf("app result missing, want explicit skip marker: %+v", res)
	}
	if res.App.Action != "skipped" || res.App.Reason != "socket_overridden" || res.App.Target != "app" {
		t.Fatalf("app result = %+v", *res.App)
	}
}

func TestRunRestartAppCoreStartsWhenNoAppWasRunning(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	startArgs := []string{}
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		start: func(_ context.Context, args []string) (int, error) {
			startArgs = append([]string(nil), args...)
			return 44, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if strings.Join(startArgs, " ") != "app" {
		t.Fatalf("start args = %q, want app", strings.Join(startArgs, " "))
	}
	if !strings.Contains(out.String(), "no app was running") || !strings.Contains(out.String(), "started app pid 44") {
		t.Fatalf("output missing app start messages:\n%s", out.String())
	}
}

func TestRunRestartAppCorePreservesArgsAndDetectsSupervisorRespawn(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	findCalls := 0
	stoppedPID := 0
	startCalled := false
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			findCalls++
			if findCalls == 1 {
				return appProcess{
					PID:     51,
					Command: "/tmp/ibkr app --addr 127.0.0.1:18765",
					Args:    []string{"app", "--addr", "127.0.0.1:18765"},
				}, nil
			}
			return appProcess{
				PID:     52,
				Command: "/tmp/ibkr app --addr 127.0.0.1:18765",
				Args:    []string{"app", "--addr", "127.0.0.1:18765"},
			}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			stoppedPID = pid
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			startCalled = true
			return 0, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if stoppedPID != 51 {
		t.Fatalf("stoppedPID = %d, want 51", stoppedPID)
	}
	if strings.Contains(out.String(), "stopping app pid 51 gracefully") {
		t.Fatalf("app restart should not print duplicate stop progress and confirmation lines:\n%s", out.String())
	}
	if startCalled {
		t.Fatal("manual start should not run when supervisor respawned the app")
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "restarted" || res.Target != "app" || !res.WasRunning || !res.Graceful || res.OldPID != 51 || res.NewPID != 52 {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(res.Args, " ") != "app --addr 127.0.0.1:18765" {
		t.Fatalf("args = %q", strings.Join(res.Args, " "))
	}
}

func TestRunRestartAppCoreDoesNotTreatDifferentArgsAsRespawn(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	findCalls := 0
	startArgs := []string{}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			findCalls++
			if findCalls == 1 {
				return appProcess{
					PID:     61,
					Command: "/tmp/ibkr app --addr 127.0.0.1:18765",
					Args:    []string{"app", "--addr", "127.0.0.1:18765"},
				}, nil
			}
			return appProcess{
				PID:     62,
				Command: "ibkr app",
				Args:    []string{"app"},
			}, nil
		},
		stop: func(int, time.Duration) error {
			return nil
		},
		start: func(_ context.Context, args []string) (int, error) {
			startArgs = append([]string(nil), args...)
			return 63, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if strings.Join(startArgs, " ") != "app --addr 127.0.0.1:18765" {
		t.Fatalf("start args = %q", strings.Join(startArgs, " "))
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.NewPID != 63 {
		t.Fatalf("new pid = %d, want manually started pid 63", res.NewPID)
	}
}

func TestRunRestartAppCoreOverridesAddrAndClearsStalePublicURL(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{
		app:        true,
		jsonOut:    true,
		timeout:    time.Second,
		appAddr:    "0.0.0.0:8765",
		appAddrSet: true,
		out:        &out,
		err:        &errBuf,
	}
	findCalls := 0
	startArgs := []string{}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			findCalls++
			if findCalls == 1 {
				return appProcess{
					PID:     71,
					Command: "/tmp/ibkr app --addr 127.0.0.1:8765 --public-url http://127.0.0.1:8765 --state-dir /tmp/app-state",
					Args:    []string{"app", "--addr", "127.0.0.1:8765", "--public-url", "http://127.0.0.1:8765", "--state-dir", "/tmp/app-state"},
				}, nil
			}
			return appProcess{PID: 72, Command: "ibkr app", Args: []string{"app"}}, nil
		},
		stop: func(int, time.Duration) error {
			return nil
		},
		start: func(_ context.Context, args []string) (int, error) {
			startArgs = append([]string(nil), args...)
			return 73, nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	want := "app --state-dir /tmp/app-state --addr 0.0.0.0:8765"
	if strings.Join(startArgs, " ") != want {
		t.Fatalf("start args = %q, want %q", strings.Join(startArgs, " "), want)
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if strings.Join(res.Args, " ") != want {
		t.Fatalf("result args = %q, want %q", strings.Join(res.Args, " "), want)
	}
}

func TestAppArgsWithRestartOverridesKeepsExplicitPublicURL(t *testing.T) {
	t.Parallel()

	opts := &restartOptions{
		appAddr:         "0.0.0.0:8765",
		appAddrSet:      true,
		appPublicURL:    "http://192.168.1.42:8765",
		appPublicURLSet: true,
	}
	got := appArgsWithRestartOverrides(
		[]string{"app", "--addr=127.0.0.1:8765", "--public-url=http://127.0.0.1:8765"},
		opts,
	)
	want := "app --addr 0.0.0.0:8765 --public-url http://192.168.1.42:8765"
	if strings.Join(got, " ") != want {
		t.Fatalf("args = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestAppArgsWithRestartOverridesRemoteFlags(t *testing.T) {
	t.Parallel()

	opts := &restartOptions{
		appRemote:       true,
		appRemoteSet:    true,
		appRemoteURL:    "https://remote.example.test",
		appRemoteURLSet: true,
	}
	got := appArgsWithRestartOverrides(
		[]string{"app", "--addr", "0.0.0.0:8765", "--remote-url=https://old.example.test"},
		opts,
	)
	want := "app --addr 0.0.0.0:8765 --remote --remote-url https://remote.example.test"
	if strings.Join(got, " ") != want {
		t.Fatalf("args = %q, want %q", strings.Join(got, " "), want)
	}

	opts.appRemote = false
	got = appArgsWithRestartOverrides(
		[]string{"app", "--remote", "--remote-url", "https://remote.example.test"},
		opts,
	)
	want = "app --remote-url https://remote.example.test"
	if strings.Join(got, " ") != want {
		t.Fatalf("disable remote args = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestAppValueArgReadsSplitAndEqualsForms(t *testing.T) {
	t.Parallel()

	if got := appValueArg([]string{"app", "--addr", "0.0.0.0:8765"}, "addr"); got != "0.0.0.0:8765" {
		t.Fatalf("split addr = %q", got)
	}
	if got := appValueArg([]string{"app", "serve", "--addr=127.0.0.1:8765"}, "addr"); got != "127.0.0.1:8765" {
		t.Fatalf("equals addr = %q", got)
	}
}

func TestAppCommandArgsIgnoresPairCommand(t *testing.T) {
	t.Parallel()

	if _, ok := appCommandArgs("/tmp/ibkr app pair --json"); ok {
		t.Fatalf("app pair should not be treated as the long-running app server")
	}
	args, ok := appCommandArgs("/tmp/ibkr app --addr 127.0.0.1:8765")
	if !ok {
		t.Fatalf("app server command was not detected")
	}
	if strings.Join(args, " ") != "app --addr 127.0.0.1:8765" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}
}

func TestAppCommandMatchReportsExactExecutable(t *testing.T) {
	t.Parallel()

	args, exact, ok := appCommandMatch("/tmp/ibkr app --addr 127.0.0.1:8765", map[string]struct{}{"/tmp/ibkr": {}})
	if !ok {
		t.Fatalf("app server command was not detected")
	}
	if !exact {
		t.Fatalf("expected exact executable match")
	}
	if strings.Join(args, " ") != "app --addr 127.0.0.1:8765" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}
	_, exact, ok = appCommandMatch("ibkr app", map[string]struct{}{"/tmp/ibkr": {}})
	if !ok {
		t.Fatalf("generic app command was not detected")
	}
	if exact {
		t.Fatalf("generic command should not be an exact executable match")
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

func TestRunRestartAppFlagOverridesRequireApp(t *testing.T) {
	var out, errBuf bytes.Buffer
	exit := RunRestart(context.Background(), []string{"--remote"}, &out, &errBuf)
	if exit != 2 {
		t.Fatalf("exit = %d, want 2", exit)
	}
	if !strings.Contains(errBuf.String(), "require --app") {
		t.Fatalf("stderr missing --app requirement:\n%s", errBuf.String())
	}
}
