package dial

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLockPathColocatedWithSocket(t *testing.T) {
	t.Parallel()
	got := LockPath("/tmp/ibkr/ibkr.sock")
	want := "/tmp/ibkr/ibkr.lock"
	if got != want {
		t.Fatalf("LockPath = %q, want %q", got, want)
	}
}

func TestLockHolderPIDMissingReturnsZero(t *testing.T) {
	t.Parallel()
	if got := LockHolderPID("/nonexistent/path/ibkr.lock"); got != 0 {
		t.Fatalf("missing lock = %d, want 0", got)
	}
}

func TestLockHolderPIDMalformedReturnsZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, body := range []string{"", "  \n", "not-a-pid", "0", "-5"} {
		path := filepath.Join(dir, "ibkr.lock")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := LockHolderPID(path); got != 0 {
			t.Fatalf("body=%q LockHolderPID=%d, want 0", body, got)
		}
	}
}

func TestLockHolderPIDValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ibkr.lock")
	if err := os.WriteFile(path, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LockHolderPID(path); got != 12345 {
		t.Fatalf("LockHolderPID = %d, want 12345", got)
	}
}

func TestIsProcessAliveSelfReportsFalse(t *testing.T) {
	t.Parallel()
	// Self-PID intentionally returns false so the recovery path never
	// tries to SIGTERM the CLI itself.
	if IsProcessAlive(os.Getpid()) {
		t.Fatal("IsProcessAlive(self) = true, want false (defensive)")
	}
}

func TestIsProcessAliveZeroAndNegativeReportFalse(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1, -1000} {
		if IsProcessAlive(pid) {
			t.Fatalf("IsProcessAlive(%d) = true, want false", pid)
		}
	}
}

func TestIsProcessAliveZombieReportsFalse(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	savedLookup := lookupProcessStatus
	lookupProcessStatus = func(got int) (string, error) {
		if got != pid {
			t.Fatalf("lookup pid = %d, want %d", got, pid)
		}
		return "Z", nil
	}
	t.Cleanup(func() { lookupProcessStatus = savedLookup })

	if IsProcessAlive(pid) {
		t.Fatalf("IsProcessAlive(%d) = true for zombie status, want false", pid)
	}
}

func TestIsProcessAliveLifecycle(t *testing.T) {
	t.Parallel()
	// Spawn a sleep, observe alive=true, kill, observe alive=false.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if !IsProcessAlive(pid) {
		t.Fatalf("freshly-spawned pid %d should be alive", pid)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil && !strings.Contains(err.Error(), "signal: terminated") {
		// Wait may report the terminate signal as an error — that's
		// expected, not a test failure.
		t.Logf("wait returned: %v", err)
	}

	// Poll briefly: kernel reaping is async after Wait returns.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !IsProcessAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still reported alive after SIGTERM+Wait", pid)
}

func TestTailLastLineMissingReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := TailLastLine("/nonexistent/log/path", 0); got != "" {
		t.Fatalf("missing file = %q, want empty", got)
	}
}

func TestTailLastLineEmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TailLastLine(path, 0); got != "" {
		t.Fatalf("empty file = %q, want empty", got)
	}
}

func TestTailLastLineSingleLineNoNewline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("only line"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TailLastLine(path, 0); got != "only line" {
		t.Fatalf("single line = %q, want %q", got, "only line")
	}
}

func TestTailLastLineMultipleLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TailLastLine(path, 0); got != "third" {
		t.Fatalf("multi-line = %q, want %q", got, "third")
	}
}

func TestTailLastLineSkipsTrailingBlankLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	// Trailing empty lines should be skipped — caller wants the last
	// line with content, not an empty string.
	if err := os.WriteFile(path, []byte("real line\n\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TailLastLine(path, 0); got != "real line" {
		t.Fatalf("trailing blanks = %q, want %q", got, "real line")
	}
}

func TestTailLastLineCapsReadAtMaxBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	// Write 10K of filler then the last line we care about.
	var buf strings.Builder
	for i := range 1000 {
		buf.WriteString(strconv.Itoa(i))
		buf.WriteByte('\n')
	}
	buf.WriteString("FINAL LINE\n")
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// 256 bytes is enough to cover the last line + a few earlier lines.
	if got := TailLastLine(path, 256); got != "FINAL LINE" {
		t.Fatalf("capped tail = %q, want %q", got, "FINAL LINE")
	}
}
