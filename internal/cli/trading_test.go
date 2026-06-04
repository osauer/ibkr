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
		Enabled:         false,
		LocalGate:       rpc.TradingLocalGateDisabled,
		BrokerGate:      rpc.BrokerTradingGateUnknown,
		Endpoint:        "127.0.0.1:4002",
		AccountOrigin:   "auto",
		ClientID:        15,
		ClientIDOrigin:  "default",
		MCPTrading:      rpc.TradingMCPDisabled,
		PreviewRequired: true,
	})
	got := stdout.String()
	for _, want := range []string{
		"IBKR Trading  DISABLED",
		"Local gate     disabled",
		"MCP trading    disabled",
		"Preview req    true",
		"Capabilities   preview=false transmit=false modify=false cancel=false",
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
		Enabled:         true,
		LocalGate:       rpc.TradingLocalGatePaper,
		BrokerGate:      rpc.BrokerTradingGateUnknown,
		Endpoint:        "127.0.0.1:4002",
		Account:         "DU1234567",
		AccountOrigin:   "pinned",
		ClientID:        15,
		ClientIDOrigin:  "default",
		MCPTrading:      rpc.TradingMCPDisabled,
		PreviewRequired: true,
		CanPreview:      false,
		Blocked:         true,
		Blockers: []rpc.TradingBlocker{{
			Code:    "gateway_client_id_unpinned",
			Message: "order submission requires a pinned client ID",
			Action:  "Set [gateway].client_id.",
		}},
	})
	got := stdout.String()
	for _, want := range []string{
		"IBKR Trading  BLOCKED",
		"Local gate     paper blocked",
		"Capabilities   preview=false transmit=false modify=false cancel=false",
		"gateway_client_id_unpinned: order submission requires a pinned client ID",
		"action: Set [gateway].client_id.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trading status missing %q:\n%s", want, got)
		}
	}
}
