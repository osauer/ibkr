package agentconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

func TestProjectCodexDefaultsStayBounded(t *testing.T) {
	var cfg struct {
		SandboxMode    string `toml:"sandbox_mode"`
		ApprovalPolicy string `toml:"approval_policy"`
		Agents         struct {
			MaxThreads int `toml:"max_threads"`
			MaxDepth   int `toml:"max_depth"`
		} `toml:"agents"`
	}
	if _, err := toml.DecodeFile(repoPath(".codex", "config.toml"), &cfg); err != nil {
		t.Fatalf("decode project Codex config: %v", err)
	}
	if cfg.SandboxMode != "workspace-write" {
		t.Fatalf("sandbox_mode=%q, want workspace-write; host-wide access must be a bounded user choice", cfg.SandboxMode)
	}
	if cfg.ApprovalPolicy != "on-request" {
		t.Fatalf("approval_policy=%q, want on-request", cfg.ApprovalPolicy)
	}
	if cfg.Agents.MaxThreads < 1 || cfg.Agents.MaxThreads > 4 {
		t.Fatalf("agents.max_threads=%d, want 1..4", cfg.Agents.MaxThreads)
	}
	if cfg.Agents.MaxDepth != 1 {
		t.Fatalf("agents.max_depth=%d, want 1", cfg.Agents.MaxDepth)
	}
}

func TestReviewerAgentsStayReadOnly(t *testing.T) {
	paths, err := filepath.Glob(repoPath(".codex", "agents", "*.toml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob reviewer agents: paths=%v err=%v", paths, err)
	}
	for _, path := range paths {
		var cfg struct {
			Name        string `toml:"name"`
			SandboxMode string `toml:"sandbox_mode"`
		}
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			t.Errorf("decode %s: %v", path, err)
			continue
		}
		if cfg.Name == "" || cfg.SandboxMode != "read-only" {
			t.Errorf("%s: name=%q sandbox_mode=%q, want named read-only reviewer", path, cfg.Name, cfg.SandboxMode)
		}
	}
}

func TestCodexHookAndBrowserPolicyAreWired(t *testing.T) {
	data, err := os.ReadFile(repoPath(".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode .codex/hooks.json: %v", err)
	}
	if !strings.Contains(string(data), ".codex/hooks/ibkr-pre-tool-use.sh") || !strings.Contains(string(data), "exec_command") {
		t.Fatal(".codex/hooks.json does not wire the project shell hook to exec_command")
	}
	for _, path := range []string{
		repoPath(".codex", "hooks", "ibkr-pre-tool-use.sh"),
		repoPath("hooks", "ibkr-pre-tool-use.sh"),
	} {
		if info, err := os.Stat(path); err != nil || info.Mode()&0o111 == 0 {
			t.Errorf("hook %s missing or not executable: info=%v err=%v", path, info, err)
		}
	}
	browserRules, err := os.ReadFile(repoPath("web", "app", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	normalizedRules := strings.Join(strings.Fields(string(browserRules)), " ")
	for _, required := range []string{"Browser QA is read-only", "human-paired-device", "gated CLI path"} {
		if !strings.Contains(normalizedRules, required) {
			t.Errorf("web/app/AGENTS.md missing browser safety phrase %q", required)
		}
	}
}

// The delegation runner hands a headless Codex agent write access to a
// worktree. Its safety comes from what it refuses to pass through: no
// approval-policy overrides and no sandbox/hook-trust bypasses, so denials
// fail closed instead of asking nobody. A bypass flag added "temporarily"
// would silently convert every delegated run into an unsandboxed agent.
func TestDelegationRunnerStaysFailClosed(t *testing.T) {
	path := repoPath("scripts", "codex-implement.sh")
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("runner %s missing or not executable: info=%v err=%v", path, info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.Contains(script, "codex exec") {
		t.Fatal("runner no longer invokes codex exec; update this gate with the new shape")
	}
	for _, banned := range []string{
		"--dangerously-bypass-approvals-and-sandbox",
		"--dangerously-bypass-hook-trust",
		"danger-full-access",
		"approval_policy",
		"--yolo",
	} {
		if strings.Contains(script, banned) {
			t.Errorf("runner contains %q; headless delegation must stay fail-closed", banned)
		}
	}
}

func TestRepoSkillDoesNotShadowInstalledIBKRSkill(t *testing.T) {
	canonical, err := os.ReadFile(repoPath("skills", "ibkr", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	repoSkill, err := os.ReadFile(repoPath(".agents", "skills", "ibkr-harness", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), "\nname: ibkr\n") {
		t.Fatal("canonical installed skill must keep name ibkr")
	}
	if !strings.Contains(string(repoSkill), "\nname: ibkr-harness\n") {
		t.Fatal("repo development skill must use unique name ibkr-harness")
	}
}
