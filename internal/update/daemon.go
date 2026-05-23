package update

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/osauer/ibkr/internal/dial"
)

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

// RestartDaemon sends SIGTERM to the given PID and waits up to
// restartTimeout for it to exit. The daemon's autospawn path means
// no explicit re-start is needed: the next `ibkr <command>` invocation
// notices the missing socket and re-spawns the daemon, which loads
// the freshly-installed binary.
//
// Polling cadence is 100ms so a quick-shutting-down daemon (<200ms)
// returns immediately rather than waiting out the full timeout.
//
// Returns a descriptive error on timeout so the user can fall back to
// a manual `pkill -f "ibkr daemon"`. Returns nil immediately if the
// PID is already gone (e.g. daemon exited between IsDaemonRunning and
// this call).
const restartTimeout = 5 * time.Second
const restartPoll = 100 * time.Millisecond

func RestartDaemon(pid int) error {
	if pid <= 0 {
		return errors.New("RestartDaemon: invalid PID")
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
	deadline := time.Now().Add(restartTimeout)
	for time.Now().Before(deadline) {
		if !dial.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(restartPoll)
	}
	return fmt.Errorf("daemon (pid %d) did not exit within %s after SIGTERM — run `pkill -f \"ibkr daemon\"` manually", pid, restartTimeout)
}
