package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

func TestRunRestartAppCoreKickstartsLaunchdJob(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, jsonOut: true, timeout: time.Second, out: &out, err: &errBuf}
	kicked := ""
	supCalls := 0
	startCalled := false
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{PID: 90, Command: "/tmp/ibkr app --remote", Args: []string{"app", "--remote"}}, nil
		},
		stop: func(int, time.Duration) error {
			t.Fatal("supervised restart must not SIGTERM the supervised process by hand")
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			startCalled = true
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			supCalls++
			pid := 90
			if kicked != "" {
				pid = 91
			}
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: pid, Args: []string{"app", "--remote"}}, true
		},
		kickstart: func(_ context.Context, target string) error {
			kicked = target
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if kicked != "gui/501/com.osauer.ibkr-app" {
		t.Fatalf("kickstart target = %q", kicked)
	}
	if startCalled {
		t.Fatal("supervised restart must not spawn an unsupervised app process")
	}
	if supCalls < 2 {
		t.Fatalf("supervisor calls = %d, want detection plus respawn wait", supCalls)
	}
	var res appRestartResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out.String())
	}
	if res.Action != "restarted" || res.Supervisor != "gui/501/com.osauer.ibkr-app" || !res.WasRunning || res.OldPID != 90 || res.NewPID != 91 || !res.Started {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunRestartAppCoreStopsOrphanBeforeKickstart(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf}
	stoppedPID := 0
	kicked := false
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			// The orphan (pid 4098-style) is NOT the supervised process:
			// launchd shows no live pid while it crash-loops on the lock.
			return appProcess{PID: 70, Command: "/tmp/ibkr app --remote", Args: []string{"app", "--remote"}}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			stoppedPID = pid
			return nil
		},
		start: func(context.Context, []string) (int, error) {
			t.Fatal("must not spawn a fresh orphan")
			return 0, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			pid := 0
			if kicked {
				pid = 71
			}
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: pid, Args: []string{"app", "--remote"}}, true
		},
		kickstart: func(context.Context, string) error {
			if stoppedPID == 0 {
				t.Fatal("kickstart before the orphan was stopped")
			}
			kicked = true
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if stoppedPID != 70 {
		t.Fatalf("stoppedPID = %d, want the orphan 70", stoppedPID)
	}
	if !strings.Contains(out.String(), "restarted supervised app pid 71") {
		t.Fatalf("output missing supervised restart confirmation:\n%s", out.String())
	}
}

func TestRunRestartAppCoreLeavesIsolatedInstanceToUnsupervisedPath(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf}
	isolatedArgs := []string{"app", "--addr", "127.0.0.1:18765", "--state-dir", "/tmp/ibkr-smoke"}
	stoppedPID := 0
	startedArgs := []string{}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			if stoppedPID != 0 {
				return appProcess{}, errAppNotRunning
			}
			return appProcess{PID: 60, Command: "/tmp/ibkr " + strings.Join(isolatedArgs, " "), Args: isolatedArgs}, nil
		},
		stop: func(pid int, _ time.Duration) error {
			stoppedPID = pid
			return nil
		},
		start: func(_ context.Context, args []string) (int, error) {
			startedArgs = append([]string(nil), args...)
			return 61, nil
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			// The shared host's LaunchAgent is loaded, but the running app
			// is an isolated smoke/preview instance with its own state dir.
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: 0, Args: []string{"app", "--remote"}}, true
		},
		kickstart: func(context.Context, string) error {
			t.Fatal("must not kickstart the shared host for an isolated app instance")
			return nil
		},
	})
	if exit != 0 {
		t.Fatalf("exit = %d, stderr=%s", exit, errBuf.String())
	}
	if stoppedPID != 60 {
		t.Fatalf("stoppedPID = %d, want the isolated instance 60", stoppedPID)
	}
	if !slices.Equal(startedArgs, isolatedArgs) {
		t.Fatalf("restarted args = %q, want the isolated instance's own args", strings.Join(startedArgs, " "))
	}
}

func TestSupervisedRestartAppliesComparesStateLocks(t *testing.T) {
	// The orphan test is state-lock identity, not pid or argv equality —
	// pin the default state dir so the cases stay hermetic.
	t.Setenv("IBKR_APP_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shared := appSupervisor{Target: "gui/501/com.osauer.ibkr-app", PID: 4098, Args: []string{"app", "--remote"}}
	cases := []struct {
		name    string
		proc    appProcess
		findErr error
		sup     appSupervisor
		want    bool
	}{
		{"supervised pid itself", appProcess{PID: 4098, Args: []string{"app", "--remote"}}, nil, shared, true},
		{"ambiguous find defers to the job", appProcess{}, errAppUnverified, shared, true},
		{"default-dir orphan with different argv", appProcess{PID: 70, Args: []string{"app", "--addr", "0.0.0.0:8765"}}, nil, shared, true},
		{"isolated instance with its own state dir", appProcess{PID: 60, Args: []string{"app", "--state-dir", "/tmp/ibkr-smoke"}}, nil, shared, false},
		{"same explicit state dir is the job's orphan", appProcess{PID: 61, Args: []string{"app", "--state-dir", "/var/lib/ibkr-app"}}, nil, appSupervisor{Target: shared.Target, Args: []string{"app", "--state-dir", "/var/lib/ibkr-app"}}, true},
		{"unparsed plist args resolve to the default dir", appProcess{PID: 62, Args: []string{"app", "--state-dir", "/tmp/ibkr-smoke"}}, nil, appSupervisor{Target: shared.Target}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisedRestartApplies(tc.proc, tc.findErr, tc.sup); got != tc.want {
				t.Fatalf("supervisedRestartApplies = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAppArgsStateDirResolvesSymlinkedSpellings(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	a := appArgsStateDir([]string{"app", "--state-dir", real})
	b := appArgsStateDir([]string{"app", "--state-dir", link})
	if a != b {
		t.Fatalf("state dir identity should survive symlinked spellings: %q vs %q", a, b)
	}
}

func TestRunRestartAppCoreRejectsOverridesForSupervisedApp(t *testing.T) {
	t.Parallel()

	var out, errBuf bytes.Buffer
	opts := &restartOptions{app: true, timeout: time.Second, out: &out, err: &errBuf, appRemote: true, appRemoteSet: true}
	exit := runRestartAppCore(context.Background(), opts, appRestartDeps{
		find: func(context.Context) (appProcess, error) {
			return appProcess{}, errAppNotRunning
		},
		supervisor: func(context.Context) (appSupervisor, bool) {
			return appSupervisor{Target: "gui/501/com.osauer.ibkr-app"}, true
		},
		kickstart: func(context.Context, string) error {
			t.Fatal("must not kickstart when overrides were rejected")
			return nil
		},
	})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if !strings.Contains(errBuf.String(), "ibkr setup app") {
		t.Fatalf("stderr should point at `ibkr setup app`:\n%s", errBuf.String())
	}
}

func TestLaunchdProgramArgumentsParsesPrintOutput(t *testing.T) {
	t.Parallel()

	out := "gui/501/com.osauer.ibkr-app = {\n" +
		"\tactive count = 1\n" +
		"\tpid = 4098\n" +
		"\targuments = {\n" +
		"\t\t/Users/osauer/.local/bin/ibkr\n" +
		"\t\tapp\n" +
		"\t\t--remote\n" +
		"\t}\n" +
		"}\n"
	args := launchdProgramArguments(out)
	if strings.Join(args, " ") != "app --remote" {
		t.Fatalf("args = %q", strings.Join(args, " "))
	}
	if m := launchdPIDRe.FindStringSubmatch(out); m == nil || m[1] != "4098" {
		t.Fatalf("pid parse = %v", m)
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
