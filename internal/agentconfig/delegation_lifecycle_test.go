package agentconfig

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The delegation runner's lifecycle invariants are behavior, not prose:
// a bad brief must strand no git state, and --cleanup must recover every
// state it can encounter, including a worktree directory removed
// out-of-band (the 2026-07-12 review's F-02: stale worktree metadata made
// `git branch -D` fail before the prune that would have cleared it).
// These tests drive the real script against a throwaway repo with a
// stubbed `codex` on PATH; they never touch the primary repo's worktrees.

// lifecycleEnv builds a temp git repo (branch main, one commit) and a PATH
// whose `codex` is a stub that consumes the brief, honors -o, and emits a
// thread.started event like the real CLI.
func lifecycleEnv(t *testing.T) (repo string, env []string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false",
			"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	stubDir := filepath.Join(root, "stub-bin")
	if err := os.Mkdir(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := `#!/usr/bin/env bash
out="" prev=""
for a in "$@"; do
  [[ "$prev" == "-o" ]] && out="$a"
  prev="$a"
done
cat >/dev/null
[[ -n "$out" ]] && echo "stub last message" > "$out"
echo '{"type":"thread.started","thread_id":"stub-thread-1"}'
`
	if err := os.WriteFile(filepath.Join(stubDir, "codex"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return repo, env
}

// runRunner executes the delegation runner with cwd=repo. stdin == nil
// leaves stdin empty (the runner's stdin-brief path).
func runRunner(t *testing.T, repo string, env []string, stdin string, args ...string) (int, string) {
	t.Helper()
	script, err := filepath.Abs(repoPath("scripts", "codex-implement.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, args...)
	cmd.Dir = repo
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("runner did not execute: %v\n%s", err, out)
		}
		return ee.ExitCode(), string(out)
	}
	return 0, string(out)
}

func gitBranchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repo
	return cmd.Run() == nil
}

func assertNoTaskState(t *testing.T, repo, task string) {
	t.Helper()
	worktree := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-codex-"+task)
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree %s stranded (stat err=%v)", worktree, err)
	}
	if gitBranchExists(t, repo, "codex/"+task) {
		t.Fatalf("branch codex/%s stranded", task)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "codex-runs", task)); !os.IsNotExist(err) {
		t.Fatalf("task dir for %s stranded (stat err=%v)", task, err)
	}
}

func TestDelegationRunnerLifecycle(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not on PATH; the runner requires it: %v", err)
	}
	repo, env := lifecycleEnv(t)
	worktree := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-codex-t1")

	// A missing brief file must fail before any git mutation.
	code, out := runRunner(t, repo, env, "", "--task", "t1", "--brief", filepath.Join(repo, "no-such-brief.md"))
	if code == 0 {
		t.Fatalf("missing brief accepted:\n%s", out)
	}
	assertNoTaskState(t, repo, "t1")

	// An empty stdin brief must fail the same way.
	code, out = runRunner(t, repo, env, "", "--task", "t1")
	if code != 2 {
		t.Fatalf("empty brief: exit = %d, want 2\n%s", code, out)
	}
	assertNoTaskState(t, repo, "t1")

	// A valid fresh run creates the worktree, branch, and artifacts.
	brief := filepath.Join(repo, "brief.md")
	if err := os.WriteFile(brief, []byte("do the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = runRunner(t, repo, env, "", "--task", "t1", "--brief", brief)
	if code != 0 {
		t.Fatalf("fresh run: exit = %d\n%s", code, out)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree missing after fresh run: %v", err)
	}
	if !gitBranchExists(t, repo, "codex/t1") {
		t.Fatal("branch codex/t1 missing after fresh run")
	}
	runs, err := filepath.Glob(filepath.Join(repo, ".claude", "codex-runs", "t1", "*Z"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("run dirs = %v (err=%v), want exactly 1", runs, err)
	}
	for _, artifact := range []string{"brief.md", "events.jsonl", "thread-id", "diff.patch"} {
		if _, err := os.Stat(filepath.Join(runs[0], artifact)); err != nil {
			t.Fatalf("artifact %s missing: %v", artifact, err)
		}
	}
	threadID, err := os.ReadFile(filepath.Join(runs[0], "thread-id"))
	if err != nil || strings.TrimSpace(string(threadID)) != "stub-thread-1" {
		t.Fatalf("thread-id = %q (err=%v), want stub-thread-1", threadID, err)
	}

	// A second fresh run of the same task must refuse.
	code, out = runRunner(t, repo, env, "", "--task", "t1", "--brief", brief)
	if code != 2 || !strings.Contains(out, "--cleanup") {
		t.Fatalf("fresh-over-leftover: exit = %d, want 2 mentioning --cleanup\n%s", code, out)
	}

	// Resume with the worktree present succeeds and adds a second run dir.
	time.Sleep(1100 * time.Millisecond) // run dirs are second-granular UTC stamps
	code, out = runRunner(t, repo, env, "more work\n", "--task", "t1", "--resume", "stub-thread-1")
	if code != 0 {
		t.Fatalf("resume run: exit = %d\n%s", code, out)
	}
	if runs, _ = filepath.Glob(filepath.Join(repo, ".claude", "codex-runs", "t1", "*Z")); len(runs) != 2 {
		t.Fatalf("run dirs after resume = %v, want 2", runs)
	}

	// Cleanup removes the worktree and branch but keeps artifacts.
	code, out = runRunner(t, repo, env, "", "--task", "t1", "--cleanup")
	if code != 0 {
		t.Fatalf("cleanup: exit = %d\n%s", code, out)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree survived cleanup (stat err=%v)", err)
	}
	if gitBranchExists(t, repo, "codex/t1") {
		t.Fatal("branch survived cleanup")
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "codex-runs", "t1")); err != nil {
		t.Fatalf("artifacts pruned by cleanup: %v", err)
	}

	// Resume after cleanup must refuse: the thread's file state is gone.
	code, out = runRunner(t, repo, env, "again\n", "--task", "t1", "--resume", "stub-thread-1")
	if code != 2 {
		t.Fatalf("resume-after-cleanup: exit = %d, want 2\n%s", code, out)
	}
}

func TestDelegationRunnerCleanupSurvivesMissingWorktreeDir(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not on PATH; the runner requires it: %v", err)
	}
	repo, env := lifecycleEnv(t)
	brief := filepath.Join(repo, "brief.md")
	if err := os.WriteFile(brief, []byte("do the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runRunner(t, repo, env, "", "--task", "t2", "--brief", brief); code != 0 {
		t.Fatalf("fresh run: exit = %d\n%s", code, out)
	}

	// Simulate the worktree directory vanishing out-of-band (manual rm,
	// interrupted cleanup, cleaned parent). Stale .git/worktrees metadata
	// remains, which is exactly the state that used to wedge cleanup.
	worktree := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-codex-t2")
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}

	code, out := runRunner(t, repo, env, "", "--task", "t2", "--cleanup")
	if code != 0 {
		t.Fatalf("cleanup with missing worktree dir: exit = %d\n%s", code, out)
	}
	if gitBranchExists(t, repo, "codex/t2") {
		t.Fatal("branch codex/t2 survived cleanup")
	}

	// And cleanup stays idempotent: a second invocation is a no-op success.
	if code, out = runRunner(t, repo, env, "", "--task", "t2", "--cleanup"); code != 0 {
		t.Fatalf("repeat cleanup: exit = %d\n%s", code, out)
	}
}
