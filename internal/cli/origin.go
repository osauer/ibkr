package cli

import (
	"io"
	"os"

	"github.com/osauer/ibkr/internal/rpc"
)

// DetectWriteOrigin classifies this process for broker-write authorization.
// Any agent marker or a non-TTY stdin classifies as agent; nothing can force
// a human classification — the daemon treats unknown origins as agent, and
// live routes refuse agent-origin writes outright.
func DetectWriteOrigin(stdin io.Reader) string {
	// docgen:env IBKR_AGENT_CONTEXT | When set (any value), broker writes from this process are classified as agent-origin. The variable can only restrict: no environment variable can claim a human origin, and live routes refuse agent-origin writes.
	if os.Getenv("IBKR_AGENT_CONTEXT") != "" ||
		os.Getenv("CLAUDECODE") != "" ||
		os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" ||
		os.Getenv("CODEX_SANDBOX") != "" ||
		os.Getenv("OPENAI_CODEX") != "" {
		return rpc.OrderOriginAgent
	}
	if !isStdinTTY(stdin) {
		return rpc.OrderOriginAgent
	}
	return rpc.OrderOriginHumanTTY
}
