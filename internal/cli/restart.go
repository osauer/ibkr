package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/rpc"
	"github.com/osauer/ibkr/internal/update"
)

const restartDefaultTimeout = 15 * time.Second

type restartOptions struct {
	jsonOut bool
	force   bool
	app     bool
	timeout time.Duration
	out     io.Writer
	err     io.Writer
}

type restartDeps struct {
	find           func(context.Context, string) (update.DaemonProcess, error)
	stop           func(int, time.Duration) error
	kill           func(int, time.Duration) error
	startAndHealth func(context.Context, string, io.Writer, bool) (int, rpc.HealthResult, error)
}

type restartResult struct {
	Action     string           `json:"action"`
	Target     string           `json:"target"`
	WasRunning bool             `json:"was_running"`
	Started    bool             `json:"started"`
	Forced     bool             `json:"forced"`
	Graceful   bool             `json:"graceful"`
	OldPID     int              `json:"old_pid,omitempty"`
	NewPID     int              `json:"new_pid,omitempty"`
	OldCommand string           `json:"old_command,omitempty"`
	Foreground bool             `json:"foreground,omitempty"`
	SocketPath string           `json:"socket_path"`
	LockPath   string           `json:"lock_path"`
	Health     rpc.HealthResult `json:"health"`
	ElapsedMS  int64            `json:"elapsed_ms"`
}

type appProcess struct {
	PID     int
	Command string
	Args    []string
}

type appRestartDeps struct {
	find  func(context.Context) (appProcess, error)
	stop  func(int, time.Duration) error
	kill  func(int, time.Duration) error
	start func(context.Context, []string) (int, error)
}

type appRestartResult struct {
	Action     string   `json:"action"`
	Target     string   `json:"target"`
	WasRunning bool     `json:"was_running"`
	Started    bool     `json:"started"`
	Forced     bool     `json:"forced"`
	Graceful   bool     `json:"graceful"`
	OldPID     int      `json:"old_pid,omitempty"`
	NewPID     int      `json:"new_pid,omitempty"`
	OldCommand string   `json:"old_command,omitempty"`
	Args       []string `json:"args,omitempty"`
	ElapsedMS  int64    `json:"elapsed_ms"`
}

var (
	errAppNotRunning  = errors.New("app not running")
	errAppUnverified  = errors.New("app process could not be verified")
	errAppStopTimeout = errors.New("app stop timed out")
)

// RunRestart is the top-level `ibkr restart` entrypoint. It intentionally does
// not take an Env: restart is local process management and must run before the
// normal autospawn+dial path in cmd/ibkr/main.go.
func RunRestart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts := restartOptions{
		timeout: restartDefaultTimeout,
		out:     stdout,
		err:     stderr,
	}
	env := &Env{Stdout: stdout, Stderr: stderr}
	fs := flagSet(env, "restart")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit machine-readable restart result")
	fs.BoolVar(&opts.force, "force", false, "send SIGKILL if graceful SIGTERM does not stop the target process before --timeout")
	fs.BoolVar(&opts.app, "app", false, "restart the HyperServe app process instead of the daemon")
	fs.DurationVar(&opts.timeout, "timeout", restartDefaultTimeout, "how long to wait for graceful process stop before failing or forcing")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "ibkr restart: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if opts.timeout <= 0 {
		fmt.Fprintln(stderr, "ibkr restart: --timeout must be positive")
		return 2
	}
	if opts.app {
		return runRestartAppCore(ctx, &opts, appRestartDeps{
			find:  findAppProcess,
			stop:  stopAppProcess,
			kill:  killAppProcess,
			start: startAppProcess,
		})
	}
	return runRestartCore(ctx, &opts, restartDeps{
		find:           update.FindDaemonProcess,
		stop:           update.StopDaemon,
		kill:           update.KillDaemon,
		startAndHealth: startDaemonAndFetchHealth,
	})
}

func runRestartCore(ctx context.Context, opts *restartOptions, deps restartDeps) int {
	startedAt := time.Now()
	socketPath := dial.DefaultSocketPath()
	lockPath := dial.LockPath(socketPath)
	res := restartResult{
		Action:     "started",
		Target:     "daemon",
		SocketPath: socketPath,
		LockPath:   lockPath,
	}

	proc, err := deps.find(ctx, socketPath)
	switch {
	case err == nil:
		res.Action = "restarted"
		res.WasRunning = true
		res.OldPID = proc.PID
		res.OldCommand = proc.Command
		res.Foreground = proc.Foreground
		res.SocketPath = proc.SocketPath
		res.LockPath = proc.LockPath
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "ibkr restart: stopping daemon pid %d gracefully\n", proc.PID)
		}
		stopErr := deps.stop(proc.PID, opts.timeout)
		if stopErr != nil {
			if !opts.force || !errors.Is(stopErr, update.ErrStopTimeout) {
				fmt.Fprintf(opts.err, "ibkr restart: %v\n", stopErr)
				if !opts.force && errors.Is(stopErr, update.ErrStopTimeout) {
					fmt.Fprintln(opts.err, "ibkr restart: re-run with --force to send SIGKILL after the graceful timeout")
				}
				return 1
			}
			if !opts.jsonOut {
				fmt.Fprintf(opts.out, "ibkr restart: daemon pid %d ignored SIGTERM; forcing SIGKILL\n", proc.PID)
			}
			if err := deps.kill(proc.PID, opts.timeout); err != nil {
				fmt.Fprintf(opts.err, "ibkr restart: %v\n", err)
				return 1
			}
			res.Forced = true
		} else {
			res.Graceful = true
		}
		if !opts.jsonOut {
			mode := "gracefully"
			if res.Forced {
				mode = "with SIGKILL"
			}
			fmt.Fprintf(opts.out, "ibkr restart: stopped daemon pid %d %s\n", proc.PID, mode)
			fmt.Fprintln(opts.out, "ibkr restart: starting daemon")
		}
	case errors.Is(err, update.ErrDaemonNotRunning):
		if !opts.jsonOut {
			fmt.Fprintln(opts.out, "ibkr restart: no daemon was running; starting daemon")
		}
	default:
		fmt.Fprintf(opts.err, "ibkr restart: %v\n", err)
		return 1
	}

	newPID, health, err := deps.startAndHealth(ctx, socketPath, opts.err, opts.jsonOut)
	if err != nil {
		fmt.Fprintf(opts.err, "ibkr restart: start daemon: %v\n", err)
		return 1
	}
	res.Started = true
	res.NewPID = newPID
	res.Health = health
	res.ElapsedMS = time.Since(startedAt).Milliseconds()

	if opts.jsonOut {
		return printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res)
	}
	renderRestartStarted(opts.out, res)
	return 0
}

func runRestartAppCore(ctx context.Context, opts *restartOptions, deps appRestartDeps) int {
	startedAt := time.Now()
	res := appRestartResult{Action: "started", Target: "app", Args: []string{"app"}}
	args := []string{"app"}

	proc, err := deps.find(ctx)
	switch {
	case err == nil:
		res.Action = "restarted"
		res.WasRunning = true
		res.OldPID = proc.PID
		res.OldCommand = proc.Command
		if len(proc.Args) > 0 {
			args = append([]string(nil), proc.Args...)
			res.Args = append([]string(nil), proc.Args...)
		}
		if !opts.jsonOut {
			fmt.Fprintf(opts.out, "ibkr restart --app: stopping app pid %d gracefully\n", proc.PID)
		}
		stopErr := deps.stop(proc.PID, opts.timeout)
		if stopErr != nil {
			if !opts.force || !errors.Is(stopErr, errAppStopTimeout) {
				fmt.Fprintf(opts.err, "ibkr restart --app: %v\n", stopErr)
				if !opts.force && errors.Is(stopErr, errAppStopTimeout) {
					fmt.Fprintln(opts.err, "ibkr restart --app: re-run with --force to send SIGKILL after the graceful timeout")
				}
				return 1
			}
			if !opts.jsonOut {
				fmt.Fprintf(opts.out, "ibkr restart --app: app pid %d ignored SIGTERM; forcing SIGKILL\n", proc.PID)
			}
			if err := deps.kill(proc.PID, opts.timeout); err != nil {
				fmt.Fprintf(opts.err, "ibkr restart --app: %v\n", err)
				return 1
			}
			res.Forced = true
		} else {
			res.Graceful = true
		}
		if !opts.jsonOut {
			mode := "gracefully"
			if res.Forced {
				mode = "with SIGKILL"
			}
			fmt.Fprintf(opts.out, "ibkr restart --app: stopped app pid %d %s\n", proc.PID, mode)
		}
		if restarted, ok, err := waitForAppRespawn(ctx, deps.find, args, 2*time.Second); err != nil {
			fmt.Fprintf(opts.err, "ibkr restart --app: %v\n", err)
			return 1
		} else if ok {
			res.Started = true
			res.NewPID = restarted.PID
			res.ElapsedMS = time.Since(startedAt).Milliseconds()
			if opts.jsonOut {
				return printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res)
			}
			fmt.Fprintf(opts.out, "ibkr restart --app: app respawned by supervisor pid %d\n", restarted.PID)
			return 0
		}
		if !opts.jsonOut {
			fmt.Fprintln(opts.out, "ibkr restart --app: starting app")
		}
	case errors.Is(err, errAppNotRunning):
		if !opts.jsonOut {
			fmt.Fprintln(opts.out, "ibkr restart --app: no app was running; starting app")
		}
	default:
		fmt.Fprintf(opts.err, "ibkr restart --app: %v\n", err)
		return 1
	}

	newPID, err := deps.start(ctx, args)
	if err != nil {
		fmt.Fprintf(opts.err, "ibkr restart --app: start app: %v\n", err)
		return 1
	}
	res.Started = true
	res.NewPID = newPID
	res.Args = append([]string(nil), args...)
	res.ElapsedMS = time.Since(startedAt).Milliseconds()
	if opts.jsonOut {
		return printJSON(&Env{Stdout: opts.out, Stderr: opts.err}, res)
	}
	fmt.Fprintf(opts.out, "ibkr restart --app: started app pid %d\n", newPID)
	fmt.Fprintln(opts.out, "ibkr restart --app: pair a phone with `ibkr app pair`")
	return 0
}

func startDaemonAndFetchHealth(ctx context.Context, socketPath string, progress io.Writer, quiet bool) (int, rpc.HealthResult, error) {
	conn, err := dial.AutospawnAndConnectContext(ctx, socketPath)
	if err != nil {
		return 0, rpc.HealthResult{}, err
	}
	defer conn.Close()

	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		var res rpc.HealthResult
		err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &res)
		return res, err
	}
	health, err := fetch(ctx)
	if err != nil {
		return 0, rpc.HealthResult{}, err
	}
	if isHandshakeInFlight(health) {
		if quiet {
			health = waitForHandshakeQuiet(ctx, fetch, health, handshakeWaitBudget, handshakePollInterval)
		} else {
			health = waitForHandshake(ctx, progress, fetch, health, handshakeWaitBudget, handshakePollInterval)
		}
	}
	return dial.LockHolderPID(dial.LockPath(socketPath)), health, nil
}

func waitForHandshakeQuiet(ctx context.Context, fetch healthFetcher, initial rpc.HealthResult, budget, pollInterval time.Duration) rpc.HealthResult {
	res := initial
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return res
		case <-time.After(pollInterval):
		}
		next, err := fetch(ctx)
		if err != nil {
			return res
		}
		res = next
		if !isHandshakeInFlight(res) {
			return res
		}
	}
	return res
}

func renderRestartStarted(w io.Writer, res restartResult) {
	fmt.Fprintf(w, "ibkr restart: started daemon pid %d", res.NewPID)
	if res.Health.DaemonVersion != "" {
		fmt.Fprintf(w, " (%s)", res.Health.DaemonVersion)
	}
	fmt.Fprintln(w)
	if res.Foreground {
		fmt.Fprintln(w, "ibkr restart: previous daemon was foreground; replacement is detached")
	}
	switch {
	case res.Health.Connected:
		fmt.Fprintf(w, "ibkr restart: gateway connected at %s:%d (client %d)\n", res.Health.GatewayHost, res.Health.GatewayPort, res.Health.ClientID)
	case res.Health.LastError != "":
		fmt.Fprintf(w, "ibkr restart: daemon is running; gateway not connected: %s\n", res.Health.LastError)
	default:
		fmt.Fprintln(w, "ibkr restart: daemon is running; gateway handshake still in progress")
	}
}

func waitForAppRespawn(ctx context.Context, find func(context.Context) (appProcess, error), expectedArgs []string, timeout time.Duration) (appProcess, bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := find(ctx)
		if err == nil {
			if slices.Equal(proc.Args, expectedArgs) {
				return proc, true, nil
			}
			return appProcess{}, false, nil
		}
		if !errors.Is(err, errAppNotRunning) {
			return appProcess{}, false, err
		}
		select {
		case <-ctx.Done():
			return appProcess{}, false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return appProcess{}, false, nil
}

func findAppProcess(ctx context.Context) (appProcess, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,args=")
	out, err := cmd.Output()
	if err != nil {
		return appProcess{}, err
	}
	self := os.Getpid()
	executablePaths := currentExecutablePaths()
	var exactMatches []appProcess
	var genericMatches []appProcess
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		pidText, cmdline, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil || pid == self || !dial.IsProcessAlive(pid) {
			continue
		}
		args, exact, ok := appCommandMatch(cmdline, executablePaths)
		if !ok {
			continue
		}
		proc := appProcess{PID: pid, Command: strings.TrimSpace(cmdline), Args: args}
		if exact {
			exactMatches = append(exactMatches, proc)
		} else {
			genericMatches = append(genericMatches, proc)
		}
	}
	if err := sc.Err(); err != nil {
		return appProcess{}, err
	}
	matches := genericMatches
	if len(exactMatches) > 0 {
		matches = exactMatches
	}
	switch len(matches) {
	case 0:
		return appProcess{}, errAppNotRunning
	case 1:
		return matches[0], nil
	default:
		return appProcess{}, fmt.Errorf("%w: multiple ibkr app processes found", errAppUnverified)
	}
}

func appCommandArgs(cmdline string) ([]string, bool) {
	args, _, ok := appCommandMatch(cmdline, nil)
	return args, ok
}

func appCommandMatch(cmdline string, exactPaths map[string]struct{}) ([]string, bool, bool) {
	fields := strings.Fields(cmdline)
	for i := range len(fields) - 1 {
		if filepath.Base(fields[i]) != "ibkr" || fields[i+1] != "app" {
			continue
		}
		args := append([]string(nil), fields[i+1:]...)
		if !isAppServerArgs(args) {
			return nil, false, false
		}
		_, exact := exactPaths[fields[i]]
		return args, exact, true
	}
	return nil, false, false
}

func currentExecutablePaths() map[string]struct{} {
	paths := map[string]struct{}{}
	bin, err := os.Executable()
	if err != nil || bin == "" {
		return paths
	}
	paths[bin] = struct{}{}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil && resolved != "" {
		paths[resolved] = struct{}{}
	}
	return paths
}

func isAppServerArgs(args []string) bool {
	if len(args) == 0 || args[0] != "app" {
		return false
	}
	if len(args) == 1 {
		return true
	}
	switch args[1] {
	case "pair", "help", "--help", "-h", "-help":
		return false
	default:
		return true
	}
}

func stopAppProcess(pid int, timeout time.Duration) error {
	return signalAppProcess(pid, syscall.SIGTERM, timeout, errAppStopTimeout)
}

func killAppProcess(pid int, timeout time.Duration) error {
	return signalAppProcess(pid, syscall.SIGKILL, timeout, errors.New("app kill timed out"))
}

func signalAppProcess(pid int, sig syscall.Signal, timeout time.Duration, timeoutErr error) error {
	if pid <= 0 {
		return errors.New("invalid app PID")
	}
	if timeout <= 0 {
		timeout = restartDefaultTimeout
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find app pid %d: %w", pid, err)
	}
	if err := proc.Signal(sig); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("signal app pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !dial.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%w: app (pid %d) did not exit within %s after %s", timeoutErr, pid, timeout, sig)
}

func startAppProcess(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		args = []string{"app"}
	}
	bin, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate self: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	logFile, err := openAppRestartLog()
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	} else {
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return 0, fmt.Errorf("open app stdio fallback: %w", err)
		}
		cmd.Stdout = devnull
		cmd.Stderr = devnull
		defer devnull.Close()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return 0, err
	}
	time.Sleep(250 * time.Millisecond)
	if !dial.IsProcessAlive(pid) {
		return 0, fmt.Errorf("app pid %d exited immediately; check %s", pid, appRestartLogPath())
	}
	return pid, nil
}

func openAppRestartLog() (*os.File, error) {
	path := appRestartLogPath()
	if path == "" {
		return nil, errors.New("no app log path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func appRestartLogPath() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "ibkr", "ibkr-app.log")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "ibkr", "ibkr-app.log")
}
