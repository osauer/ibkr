package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
)

func TestRenderTradingStatusTextDisabled(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderTradingStatusText(env, &rpc.TradingStatus{
		Mode:           "disabled",
		Endpoint:       "127.0.0.1:4002",
		AccountOrigin:  "auto",
		ClientID:       15,
		ClientIDOrigin: "default",
		MCPTrading:     rpc.TradingMCPDisabled,
	})
	got := stdout.String()
	for _, want := range []string{
		"IBKR Trading  DISABLED",
		"Mode           disabled",
		"MCP trading    disabled",
		"Capabilities   preview=false write=false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trading status missing %q:\n%s", want, got)
		}
	}
}

func TestRenderTradingStatusTextBlocked(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderTradingStatusText(env, &rpc.TradingStatus{
		Mode:           "paper",
		Endpoint:       "127.0.0.1:4002",
		Account:        "DU1234567",
		AccountOrigin:  "pinned",
		ClientID:       15,
		ClientIDOrigin: "default",
		MCPTrading:     rpc.TradingMCPDisabled,
		CanPreview:     false,
		Blocked:        true,
		Blockers: []rpc.TradingBlocker{{
			Code:    "gateway_client_id_unpinned",
			Message: "order submission requires a pinned client ID",
			Action:  "Set [gateway].client_id.",
		}},
	})
	got := stdout.String()
	for _, want := range []string{
		"IBKR Trading  BLOCKED",
		"Mode           paper blocked",
		"Capabilities   preview=false write=false",
		"gateway_client_id_unpinned: order submission requires a pinned client ID",
		"action: Set [gateway].client_id.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trading status missing %q:\n%s", want, got)
		}
	}
}
