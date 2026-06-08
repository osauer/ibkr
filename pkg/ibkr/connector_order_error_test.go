package ibkr

import "testing"

func TestOrderBrokerErrorStatusRejectsMinimumTickError(t *testing.T) {
	t.Parallel()

	if got := orderBrokerErrorStatus(110, "The price does not conform to the minimum price variation for this contract."); got != "Rejected" {
		t.Fatalf("orderBrokerErrorStatus(110) = %q, want Rejected", got)
	}
}
