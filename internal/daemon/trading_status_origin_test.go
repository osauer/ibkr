package daemon

import (
	"testing"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

func TestLiveOriginBlockersMatrix(t *testing.T) {
	t.Parallel()
	live := rpc.TradingStatus{Mode: config.TradingModeLive, Account: "U7654321"}
	paper := rpc.TradingStatus{Mode: config.TradingModePaper, Account: "DU1234567"}

	cases := []struct {
		name     string
		status   rpc.TradingStatus
		origin   string
		wantCode string
	}{
		{name: "paper agent unrestricted", status: paper, origin: rpc.OrderOriginAgent},
		{name: "paper empty origin unrestricted", status: paper, origin: ""},
		{name: "live agent hard-blocked", status: live, origin: rpc.OrderOriginAgent, wantCode: "live_agent_origin_blocked"},
		{name: "live empty origin fails closed as agent", status: live, origin: "", wantCode: "live_agent_origin_blocked"},
		{name: "live unknown origin fails closed as agent", status: live, origin: "human-definitely", wantCode: "live_agent_origin_blocked"},
		{name: "live human passes with no confirmation", status: live, origin: rpc.OrderOriginHumanTTY},
		{name: "live paired device passes with no confirmation", status: live, origin: rpc.OrderOriginPairedDevice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blockers := liveOriginBlockers(tc.status, tc.origin)
			if tc.wantCode == "" {
				if len(blockers) != 0 {
					t.Fatalf("liveOriginBlockers = %+v, want none", blockers)
				}
				return
			}
			if len(blockers) != 1 || blockers[0].Code != tc.wantCode {
				t.Fatalf("liveOriginBlockers = %+v, want single %s", blockers, tc.wantCode)
			}
			if blockers[0].Action == "" {
				t.Fatalf("blocker %s has no remediation action", tc.wantCode)
			}
		})
	}
}

func TestNormalizedWriteOrigin(t *testing.T) {
	t.Parallel()
	if got := normalizedWriteOrigin(""); got != rpc.OrderOriginAgent {
		t.Fatalf("empty origin = %q, want agent", got)
	}
	if got := normalizedWriteOrigin("script"); got != rpc.OrderOriginAgent {
		t.Fatalf("unknown origin = %q, want agent", got)
	}
	if got := normalizedWriteOrigin(rpc.OrderOriginHumanTTY); got != rpc.OrderOriginHumanTTY {
		t.Fatalf("human-tty origin = %q, want passthrough", got)
	}
	if got := normalizedWriteOrigin(rpc.OrderOriginPairedDevice); got != rpc.OrderOriginPairedDevice {
		t.Fatalf("paired origin = %q, want passthrough", got)
	}
}
