package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

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

// confirmLiveBrokerWrite returns the live confirmation phrase for a broker
// write. Paper and disabled routes return immediately with no prompt. A
// human-TTY origin on a live route is prompted to type live/<account>
// verbatim; declining aborts. Agent origins skip the prompt — the daemon
// refuses their live writes regardless of any phrase.
func confirmLiveBrokerWrite(ctx context.Context, env *Env, verb string) (string, bool) {
	if env == nil || env.Conn == nil || env.Origin != rpc.OrderOriginHumanTTY || env.Stdin == nil {
		return "", true
	}
	var st rpc.TradingStatus
	if err := env.Conn.Call(ctx, rpc.MethodTradingStatus, nil, &st); err != nil {
		// Let the write itself surface the daemon error.
		return "", true
	}
	if st.Mode != "live" {
		return "", true
	}
	phrase := "live/" + st.Account
	fmt.Fprintf(env.Stderr, "LIVE broker write (%s) against %s. Type %q to confirm: ", verb, st.Account, phrase)
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil {
		fmt.Fprintln(env.Stderr, "no confirmation read; aborting live write")
		return "", false
	}
	if strings.TrimSpace(line) != phrase {
		fmt.Fprintln(env.Stderr, "confirmation mismatch; aborting live write")
		return "", false
	}
	return phrase, true
}
