package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const hookPath = "../../hooks/ibkr-pre-tool-use.sh"

func TestIBKRPreToolHookLiveReadyAllowsBrokerWrite(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","can_write":true,"blocked":false,"live_override":"ready","gateway_port":4001,"account":"U1234567","endpoint":"127.0.0.1:4001"}`
	res := runIBKRHook(t, hookRun{status: status, command: "ibkr order place --preview-token TOKEN --json"})
	if res.code != 0 {
		t.Fatalf("hook exit=%d stderr=%s", res.code, res.stderr)
	}
}

func TestIBKRPreToolHookBlocksChainedReadThenWrite(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","can_write":true,"blocked":false,"live_override":"ready","gateway_port":4001,"account":"U1234567","endpoint":"127.0.0.1:4001"}`
	res := runIBKRHook(t, hookRun{status: status, command: "ibkr order status abc; ibkr order place --preview-token TOKEN --json"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "without shell composition") {
		t.Fatalf("stderr missing composition block: %s", res.stderr)
	}
}

func TestIBKRPreToolHookGatesOpportunitiesExercise(t *testing.T) {
	t.Parallel()
	status := `{"mode":"paper","can_write":false,"blocked":false,"live_override":"blocked","gateway_port":4002,"account":"DU1234567","endpoint":"127.0.0.1:4002","write_blockers":[{"code":"trading_frozen"}]}`
	res := runIBKRHook(t, hookRun{status: status, command: "ibkr opportunities exercise option_exercise:a sha256:rev --json"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "Broker-adjacent") {
		t.Fatalf("stderr missing broker-write gate: %s", res.stderr)
	}
}

func TestIBKRPreToolHookGatesProposalReduceSubmit(t *testing.T) {
	t.Parallel()
	status := `{"mode":"disabled","can_write":false,"blocked":true,"live_override":"blocked","write_blockers":[{"code":"trading_disabled"}]}`
	res := runIBKRHook(t, hookRun{status: status, command: "ibkr proposals reduce BB --percent 25 --submit --json"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "Broker-adjacent") {
		t.Fatalf("stderr missing broker-write gate: %s", res.stderr)
	}
}

func TestIBKRPreToolHookBlocksComposedProposalReduceSubmit(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","can_write":true,"blocked":false,"live_override":"ready","gateway_port":4001,"account":"U1234567"}`
	res := runIBKRHook(t, hookRun{status: status, command: "ibkr proposals reduce BB --percent 25 --submit --json; echo done"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "without shell composition") {
		t.Fatalf("stderr missing composition block: %s", res.stderr)
	}
}

func TestIBKRPreToolHookDoesNotFreezeExemptFutureClose(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","blocked":false,"live_override":"ready","can_write":false,"account":"U1234567","gateway_port":7496,"write_blockers":[{"code":"trading_frozen"}]}`
	res := runIBKRHook(t, hookRun{status: status, command: "ibkr order close 42"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
}

func TestIBKRPreToolHookAllowsReadOnlyPipe(t *testing.T) {
	t.Parallel()
	status := `{"mode":"paper","can_write":true,"blocked":false,"live_override":"blocked","gateway_port":4002,"account":"DU1234567","endpoint":"127.0.0.1:4002"}`
	res := runIBKRHook(t, hookRun{status: status, command: "ibkr opportunities status --json | jq ."})
	if res.code != 0 {
		t.Fatalf("hook exit=%d stderr=%s", res.code, res.stderr)
	}
}

func TestIBKRPreToolHookAllowsHelpPipeForWriteShapedCommand(t *testing.T) {
	t.Parallel()
	res := runIBKRHook(t, hookRun{command: "ibkr settings set --help | sed -n 1,80p"})
	if res.code != 0 {
		t.Fatalf("hook exit=%d stderr=%s", res.code, res.stderr)
	}
}

func TestIBKRPreToolHookDoesNotLetHelpHideSecondIBKRWrite(t *testing.T) {
	t.Parallel()
	status := `{"mode":"live","can_write":true,"blocked":false,"live_override":"ready","gateway_port":4001,"account":"U1234567","endpoint":"127.0.0.1:4001"}`
	res := runIBKRHook(t, hookRun{status: status, command: "ibkr settings set --help; ibkr order place --preview-token TOKEN"})
	if res.code != 2 {
		t.Fatalf("hook exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "without shell composition") {
		t.Fatalf("stderr missing composition block: %s", res.stderr)
	}
}

func TestIBKRPreToolHookMissingJQOnlyBlocksIBKR(t *testing.T) {
	t.Parallel()
	path := pathWithoutJQ(t)
	if res := runIBKRHook(t, hookRun{command: "echo hi", path: path}); res.code != 0 {
		t.Fatalf("non-ibkr without jq exit=%d stderr=%s", res.code, res.stderr)
	}
	res := runIBKRHook(t, hookRun{command: "ibkr order place --preview-token TOKEN", path: path})
	if res.code != 2 {
		t.Fatalf("ibkr without jq exit=%d stderr=%s, want 2", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "jq is required") {
		t.Fatalf("stderr missing jq requirement: %s", res.stderr)
	}
}

type hookRun struct {
	status  string
	command string
	path    string
}

type hookResult struct {
	code   int
	stderr string
}

func runIBKRHook(t *testing.T, in hookRun) hookResult {
	t.Helper()
	temp := t.TempDir()
	fakeIBKR := filepath.Join(temp, "ibkr")
	script := `#!/bin/sh
if [ "$1" = "trading" ] && [ "$2" = "status" ] && [ "$3" = "--json" ]; then
  printf '%s\n' "$IBKR_FAKE_STATUS"
  exit 0
fi
printf 'unexpected fake ibkr call: %s\n' "$*" >&2
exit 44
`
	if err := os.WriteFile(fakeIBKR, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ibkr: %v", err)
	}
	payload := `{"tool_input":{"command":` + strconvQuote(in.command) + `}}`
	cmd := exec.Command("/bin/bash", hookPath)
	cmd.Stdin = strings.NewReader(payload)
	path := temp + string(os.PathListSeparator) + os.Getenv("PATH")
	if in.path != "" {
		path = in.path
	}
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"IBKR_FAKE_STATUS="+in.status,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run hook: %v", err)
		}
	}
	return hookResult{code: code, stderr: string(out)}
}

func pathWithoutJQ(t *testing.T) string {
	t.Helper()
	temp := t.TempDir()
	links := map[string]string{
		"cat":  "/bin/cat",
		"grep": "/usr/bin/grep",
	}
	if runtime.GOOS == "linux" {
		links["grep"] = "/bin/grep"
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(temp, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	return temp
}

func strconvQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
