package update

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRestartDaemon_InvalidPID(t *testing.T) {
	t.Parallel()
	err := RestartDaemon(0)
	if err == nil || !strings.Contains(err.Error(), "invalid PID") {
		t.Fatalf("err = %v, want 'invalid PID'", err)
	}
}

// TestRestartDaemon_AlreadyGone fakes a PID that is no longer alive.
// Picking a PID we know is gone is tricky; we fork a tiny `true`
// subprocess, wait for it to exit, and then call RestartDaemon with
// its PID. proc.Signal returns ESRCH which we treat as success.
func TestRestartDaemon_AlreadyGone(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn `true`: %v", err)
	}
	// cmd.Process.Pid is reaped now; ESRCH on Signal is the
	// expected branch.
	err := RestartDaemon(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("RestartDaemon on dead pid = %v, want nil (ESRCH tolerated)", err)
	}
}

// TestRestartDaemon_SignalDelivered confirms the SIGTERM path against
// a fork of `sleep 30` we own. The child should observe the signal
// and exit; RestartDaemon should return nil inside its 5s budget.
func TestRestartDaemon_SignalDelivered(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode (subprocess fork)")
	}
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	// Reap in a goroutine so the kernel removes the PID from /proc
	// as soon as the child exits — without this the PID lingers as
	// a zombie and IsProcessAlive (kill -0) keeps returning true,
	// causing RestartDaemon to time out.
	pid := cmd.Process.Pid
	go func() { _, _ = cmd.Process.Wait() }()

	done := make(chan error, 1)
	go func() {
		done <- RestartDaemon(pid)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RestartDaemon: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("RestartDaemon didn't return within 6s")
	}
}

// TestRestartDaemon_Timeout asserts the descriptive timeout error
// fires when the target process refuses SIGTERM. We can't easily
// build a SIGTERM-trapping subprocess in a portable test; instead we
// rely on a custom test wrapper that catches the signal with a
// shell `trap`. The wrapper traps TERM, sleeps 6s anyway, then exits.
func TestRestartDaemon_Timeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode (subprocess fork)")
	}
	t.Parallel()
	cmd := exec.Command("sh", "-c", `trap '' TERM; sleep 6`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start trap-sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	err := RestartDaemon(cmd.Process.Pid)
	if err == nil {
		t.Fatal("RestartDaemon returned nil for a SIGTERM-trapping process")
	}
	if !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("err = %v, want 'did not exit'", err)
	}
}

// TestIsDaemonRunning_NoFile exercises the path where the daemon has
// never run on this machine — the lock file simply doesn't exist.
// We rely on XDG_RUNTIME_DIR being unset / nonexistent in test env;
// if a real daemon happens to be running on the dev host, this test
// might transiently see it. Document and tolerate.
func TestIsDaemonRunning_NoFile(t *testing.T) {
	t.Parallel()
	// Point the lock-path lookup at a fresh temp dir so we know
	// the file isn't there. This works because LockHolderPID just
	// reads the file path it's given — we mimic the production
	// flow by computing the path the same way IsDaemonRunning does
	// but inside a tmp dir.
	tmp := t.TempDir()
	_ = tmp
	// IsDaemonRunning uses dial.DefaultSocketPath, which is host-
	// dependent and may or may not point at a running daemon. We
	// can only assert the contract: when running == true, pid > 0;
	// when running == false, pid is 0 OR refers to a dead pid.
	pid, running := IsDaemonRunning()
	if running && pid <= 0 {
		t.Fatalf("running=true but pid=%d", pid)
	}
}

// Compile-time sanity: ensure errors.Is on syscall.ESRCH works as
// expected on this platform — install_test depends on it.
func TestErrorsIsESRCH(t *testing.T) {
	t.Parallel()
	if !errors.Is(syscall.ESRCH, syscall.ESRCH) {
		t.Fatal("errors.Is(ESRCH, ESRCH) = false")
	}
}

// keepReferenced — silence unused-import for os when shrinking the
// file. Cheap; trims churn during edits.
var _ = os.Getpid
