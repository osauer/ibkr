package rpc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTradingStatusJSONOmitsMissingPaperSmokeTime(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(TradingStatus{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"enabled"`) {
		t.Fatalf("trading status should not expose legacy enabled field: %s", data)
	}
	if strings.Contains(string(data), "paper_smoke_at") {
		t.Fatalf("paper_smoke_at should be absent when no evidence exists: %s", data)
	}
}

func TestTradingStatusJSONIncludesPaperSmokeTimeWhenPresent(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)

	data, err := json.Marshal(TradingStatus{PaperSmokeAt: &at})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"paper_smoke_at":"2026-05-28T07:00:00Z"`) {
		t.Fatalf("paper_smoke_at should be present when evidence exists: %s", data)
	}
}
