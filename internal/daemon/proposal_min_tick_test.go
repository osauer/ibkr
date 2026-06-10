package daemon

import (
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

// Xetra trades MiFID-banded grids: a €155 name ticks at 0.02, where the old
// hardcoded 0.01 produced broker-rejected prices (error 110). With the
// broker-resolved MinTick threaded through, both the trail amount and the
// initial stop land on the venue grid.
func TestTrailingStopStockProposalRoundsOnResolvedMinTick(t *testing.T) {
	t.Parallel()
	bid, ask := 154.80, 154.82
	row := rpc.PositionView{
		Symbol: "SAP", SecType: "STK", ConID: 14204, Quantity: 1,
		Bid: &bid, Ask: &ask, Mark: 154.81, Multiplier: 1, Currency: "EUR",
		Exchange: "IBIS",
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())

	prop, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0.02)
	if !ok || prop.Trail == nil || prop.Trail.TrailingAmount == nil {
		t.Fatalf("proposal = %+v ok=%v, want trail proposal", prop, ok)
	}
	// 8% of bid 154.80 = 12.384 → ceil on 0.02 grid = 12.40 (wider trail is
	// the conservative direction); stop = 154.80 − 12.40 = 142.40 on-grid.
	if *prop.Trail.TrailingAmount != 12.40 {
		t.Fatalf("trailing amount = %.4f, want 12.40 on the 0.02 grid", *prop.Trail.TrailingAmount)
	}
	if prop.Trail.InitialStopPrice != 142.40 {
		t.Fatalf("initial stop = %.4f, want 142.40 on the 0.02 grid", prop.Trail.InitialStopPrice)
	}
	if prop.Contract.MinTick != 0.02 {
		t.Fatalf("contract min tick = %v, want 0.02 carried for preview-side rounding", prop.Contract.MinTick)
	}
}
