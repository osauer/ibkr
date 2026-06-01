package ibkr

import "testing"

func TestOptionDetailMatchesRequestRejectsTradingClassMismatch(t *testing.T) {
	t.Parallel()
	requested := Contract{Symbol: "SPX", TradingClass: "SPX", Expiry: "20260619", Strike: 5400, Right: "C"}

	if optionDetailMatchesRequest(ContractDetailsLite{ConID: 123, TradingClass: "SPXW"}, requested) {
		t.Fatalf("SPX request must not accept SPXW contract details")
	}
	if !optionDetailMatchesRequest(ContractDetailsLite{ConID: 123, TradingClass: "SPX"}, requested) {
		t.Fatalf("matching SPX contract details should be accepted")
	}
	if optionDetailMatchesRequest(ContractDetailsLite{TradingClass: "SPX"}, requested) {
		t.Fatalf("zero ConID contract details should be rejected")
	}
}
