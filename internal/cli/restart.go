package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/rpc"
	"github.com/osauer/ibkr/internal/update"
)

const restartDefaultTimeout = 15 * time.Second

type restartOptions struct {
	jsonOut bool
	force   bool
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
	fs.BoolVar(&opts.force, "force", false, "send SIGKILL if graceful SIGTERM does not stop the daemon before --timeout")
	fs.DurationVar(&opts.timeout, "timeout", restartDefaultTimeout, "how long to wait for graceful daemon stop before failing or forcing")
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
	renderRestartText(opts.out, res)
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

func renderRestartText(w io.Writer, res restartResult) {
	if res.WasRunning {
		mode := "gracefully"
		if res.Forced {
			mode = "with SIGKILL"
		}
		fmt.Fprintf(w, "ibkr restart: stopped daemon pid %d %s\n", res.OldPID, mode)
	} else {
		fmt.Fprintln(w, "ibkr restart: no daemon was running before this command")
	}
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
