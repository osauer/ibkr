package rpc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOrderPreviewResultJSONSeparatesTokenMintedFromSubmitEligible(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(OrderPreviewResult{
		TokenMinted:    true,
		SubmitEligible: false,
		Executable:     false,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`"token_minted":true`,
		`"submit_eligible":false`,
		`"executable":false`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("OrderPreviewResult JSON missing %s: %s", want, got)
		}
	}
}
