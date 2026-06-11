package cli

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	skillPath         = "../../skills/ibkr/SKILL.md"
	skillSettingsPath = "../../settings/ibkr.settings.json"
)

// skillExcluded lists CLI commands deliberately absent from the agent
// skill. Every other Commands() entry must appear in SKILL.md as
// `ibkr <name>` (body or allowed-tools); adding a CLI command without
// updating the skill fails `make check` via parity-check.
var skillExcluded = map[string]string{
	"daemon":   "lifecycle plumbing, not an agent data command",
	"app":      "long-lived local app host, not an agent data command",
	"mcp":      "MCP server bootstrap, not an agent data command",
	"setup":    "interactive first-run wizard",
	"update":   "binary self-update is a human decision",
	"purge":    "destructive emergency workflow, deliberately human-only",
	"backtest": "offline research harness, deliberately out of the skill",
}

// forbiddenAllowPrefixes are invocation shapes that must never be
// allowlisted in the skill: broker/state writes stay outside it so the
// PreToolUse hook and the daemon origin gate remain the deciding layers.
var forbiddenAllowPrefixes = []string{
	"ibkr order place", "ibkr order modify", "ibkr order cancel",
	"ibkr proposals preview", "ibkr proposals submit", "ibkr proposals ignore",
	"ibkr settings set",
	"ibkr watch --add", "ibkr watch --remove", "ibkr watch --clear",
	"ibkr purge",
}

func readSkill(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillPath, err)
	}
	return string(data)
}

func TestSkillMentionsEveryCommand(t *testing.T) {
	t.Parallel()
	skill := readSkill(t)
	names := map[string]bool{}
	for _, c := range Commands() {
		names[c.Name] = true
		if _, excluded := skillExcluded[c.Name]; excluded {
			continue
		}
		if !strings.Contains(skill, "ibkr "+c.Name) {
			t.Errorf("CLI command %q is not mentioned in %s; document it there or add it to skillExcluded with a reason", c.Name, skillPath)
		}
	}
	for name := range skillExcluded {
		if !names[name] {
			t.Errorf("skillExcluded entry %q is not a CLI command; remove the stale exclusion", name)
		}
	}
}

var bashPatternRE = regexp.MustCompile(`Bash\(([^)]*)\)`)

// TestSkillAllowlistMirrorsSettingsAndCLI pins the three-way contract
// between the SKILL.md allowed-tools frontmatter, the shipped permission
// allowlist in settings/, and the real CLI surface: the two lists must be
// identical, every pattern must name a real command, and no pattern may
// allowlist a broker/state write.
func TestSkillAllowlistMirrorsSettingsAndCLI(t *testing.T) {
	t.Parallel()
	skill := readSkill(t)
	parts := strings.SplitN(skill, "---", 3)
	if len(parts) < 3 {
		t.Fatalf("%s: expected YAML frontmatter delimited by ---", skillPath)
	}
	skillAllows := map[string]bool{}
	for _, m := range bashPatternRE.FindAllStringSubmatch(parts[1], -1) {
		skillAllows["Bash("+m[1]+")"] = true
	}
	if len(skillAllows) == 0 {
		t.Fatalf("%s: no Bash(...) patterns found in frontmatter allowed-tools", skillPath)
	}

	var settings struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	data, err := os.ReadFile(skillSettingsPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillSettingsPath, err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse %s: %v", skillSettingsPath, err)
	}
	settingsAllows := map[string]bool{}
	for _, p := range settings.Permissions.Allow {
		settingsAllows[p] = true
	}

	for p := range skillAllows {
		if !settingsAllows[p] {
			t.Errorf("allowed-tools pattern %q is missing from %s permissions.allow", p, skillSettingsPath)
		}
	}
	for p := range settingsAllows {
		if !skillAllows[p] {
			t.Errorf("settings allow pattern %q is missing from %s allowed-tools", p, skillPath)
		}
	}

	cliNames := map[string]bool{}
	for _, c := range Commands() {
		cliNames[c.Name] = true
	}
	for p := range skillAllows {
		inner := strings.TrimSuffix(strings.TrimPrefix(p, "Bash("), ")")
		rest, ok := strings.CutPrefix(inner, "ibkr ")
		if !ok {
			t.Errorf("allowed-tools pattern %q is not an ibkr invocation", p)
			continue
		}
		first := strings.TrimSuffix(strings.Fields(rest)[0], "*")
		if !cliNames[first] {
			t.Errorf("allowed-tools pattern %q names %q, which is not a CLI command", p, first)
		}
		for _, bad := range forbiddenAllowPrefixes {
			if strings.HasPrefix(inner, bad) {
				t.Errorf("allowed-tools pattern %q allowlists write path %q; broker/state writes stay outside the skill", p, bad)
			}
		}
	}
}
