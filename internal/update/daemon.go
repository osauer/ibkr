package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osauer/ibkr/internal/dial"
)

var (
	ErrDaemonNotRunning = errors.New("daemon not running")
	ErrDaemonUnverified = errors.New("daemon process could not be verified")
	ErrStopTimeout      = errors.New("daemon stop timed out")
)

// DaemonProcess is the verified process currently holding the daemon pidfile.
type DaemonProcess struct {
	PID        int
	Command    string
	SocketPath string
	LockPath   string
	Foreground bool
}

// IsDaemonRunning reads the daemon's PID file (co-located with the
// socket, written by internal/daemon/lock.go) and reports whether the
// PID is currently a live process.
//
// Returns (0, false) on any missing/malformed/stale case so the caller
// can treat the absence the same way regardless of cause — "no daemon
// to restart, skip the step."
func IsDaemonRunning() (int, bool) {
	pid := dial.LockHolderPID(dial.LockPath(dial.DefaultSocketPath()))
	if pid <= 0 {
		return 0, false
	}
	if !dial.IsProcessAlive(pid) {
		return pid, false
	}
	return pid, true
}

// FindDaemonProcess returns the live ibkr daemon process for socketPath.
//
// It is intentionally stricter than IsDaemonRunning: commands that send
// signals must not trust a stale or forged pidfile. A live pidfile holder is
// accepted only when its command line looks like `ibkr daemon`. A responding
// socket without a verifiable pidfile is treated as unverified rather than
// killed.
func FindDaemonProcess(ctx context.Context, socketPath string) (DaemonProcess, error) {
	if socketPath == "" {
		socketPath = dial.DefaultSocketPath()
	}
	lockPath := dial.LockPath(socketPath)
	proc := DaemonProcess{SocketPath: socketPath, LockPath: lockPath}

	pid := dial.LockHolderPID(lockPath)
	if pid <= 0 || !dial.IsProcessAlive(pid) {
		conn, err := dial.Connect(socketPath)
		if err == nil {
			_ = conn.Close()
			return proc, fmt.Errorf("%w: socket %s is serving but %s has no live PID", ErrDaemonUnverified, socketPath, lockPath)
		}
		if errors.Is(err, dial.ErrSocketMissing) {
			return proc, ErrDaemonNotRunning
		}
		return proc, err
	}

	cmdline, err := lookupProcessCommandLine(ctx, pid)
	if err != nil {
		return proc, fmt.Errorf("%w: pid %d: %v", ErrDaemonUnverified, pid, err)
	}
	if !looksLikeIBKRDaemon(cmdline) {
		return proc, fmt.Errorf("%w: pid %d command %q is not `ibkr daemon`", ErrDaemonUnverified, pid, cmdline)
	}

	proc.PID = pid
	proc.Command = cmdline
	proc.Foreground = commandHasFlag(cmdline, "foreground")
	return proc, nil
}

var lookupProcessCommandLine = processCommandLine

func processCommandLine(ctx context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid PID")
	}
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	cmdline := strings.TrimSpace(string(out))
	if cmdline == "" {
		return "", errors.New("empty command line")
	}
	return cmdline, nil
}

func looksLikeIBKRDaemon(cmdline string) bool {
	fields := strings.Fields(cmdline)
	for i := 0; i+1 < len(fields); i++ {
		if filepath.Base(fields[i]) == "ibkr" && fields[i+1] == "daemon" {
			return true
		}
	}
	return false
}

func commandHasFlag(cmdline, name string) bool {
	long := "--" + name
	short := "-" + name
	for field := range strings.FieldsSeq(cmdline) {
		if field == long || field == short || strings.HasPrefix(field, long+"=") || strings.HasPrefix(field, short+"=") {
			return true
		}
	}
	return false
}

// RestartDaemon sends SIGTERM to the given PID and waits up to
// restartTimeout for it to exit. The daemon's autospawn path means
// no explicit re-start is needed: the next `ibkr <command>` invocation
// notices the missing socket and re-spawns the daemon, which loads
// the freshly-installed binary.
//
// Polling cadence is 100ms so a quick-shutting-down daemon (<200ms)
// returns immediately rather than waiting out the full timeout.
//
// Returns a descriptive error on timeout so the caller can suggest
// `ibkr restart --force`. Returns nil immediately if the PID is already
// gone (e.g. daemon exited between IsDaemonRunning and this call).
var (
	restartTimeout = 5 * time.Second
	restartPoll    = 100 * time.Millisecond
)

func RestartDaemon(pid int) error {
	return StopDaemon(pid, restartTimeout)
}

// StopDaemon sends SIGTERM to pid and waits until it exits. It does not
// verify the PID; callers that read a pidfile should call FindDaemonProcess
// first. On timeout it returns an error wrapping ErrStopTimeout so callers can
// decide whether to escalate to SIGKILL.
func StopDaemon(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return errors.New("StopDaemon: invalid PID")
	}
	if timeout <= 0 {
		timeout = restartTimeout
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// ESRCH or os.ErrProcessDone = process already gone — treat
		// as success. The Go runtime returns ErrProcessDone for PIDs
		// it has already reaped (e.g. test subprocess after Wait);
		// ESRCH is the raw syscall result on an unreaped PID we no
		// longer own. Either way, the post-condition we want is
		// "process is not running" and we have it.
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		// EPERM = we don't own the process. The daemon should always be
		// owned by the same user as the CLI; if not, the user is doing
		// something the update flow shouldn't paper over.
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !dial.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(restartPoll)
	}
	return fmt.Errorf("%w: daemon (pid %d) did not exit within %s after SIGTERM", ErrStopTimeout, pid, timeout)
}

// KillDaemon sends SIGKILL to pid and waits until it exits. It is intended
// only as an explicit --force fallback after StopDaemon timed out.
func KillDaemon(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return errors.New("KillDaemon: invalid PID")
	}
	if timeout <= 0 {
		timeout = restartTimeout
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !dial.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(restartPoll)
	}
	return fmt.Errorf("daemon (pid %d) did not exit within %s after SIGKILL", pid, timeout)
}
