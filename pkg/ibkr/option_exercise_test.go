package ibkr

import (
	"strings"
	"testing"
)

func TestEncodeExerciseOptionsMessage(t *testing.T) {
	t.Parallel()
	conn := &Connection{serverVersion: 99}
	req := OptionExerciseRequest{
		TickerID: 7,
		Contract: &Contract{
			ConID:        12345,
			Symbol:       "aapl",
			SecType:      "OPT",
			Expiry:       "20260619",
			Strike:       100,
			Right:        "c",
			Multiplier:   100,
			Exchange:     "",
			Currency:     "",
			LocalSymbol:  "AAPL  260619C00100000",
			TradingClass: "AAPL",
		},
		ExerciseAction:   OptionExerciseActionExercise,
		ExerciseQuantity: 2,
		Account:          "DU123",
		Override:         0,
	}

	msg, err := conn.encodeExerciseOptionsMessage(req)
	if err != nil {
		t.Fatalf("encodeExerciseOptionsMessage: %v", err)
	}
	fields := strings.Split(string(msg), "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	want := []string{
		"21",
		"2",
		"7",
		"12345",
		"AAPL",
		"OPT",
		"20260619",
		"100",
		"C",
		"100",
		"SMART",
		"USD",
		"AAPL  260619C00100000",
		"AAPL",
		"1",
		"2",
		"DU123",
		"0",
	}
	if strings.Join(fields, "|") != strings.Join(want, "|") {
		t.Fatalf("fields=%q, want %q", fields, want)
	}
}

func TestValidateOptionExerciseRequest(t *testing.T) {
	t.Parallel()
	valid := OptionExerciseRequest{
		TickerID: 1,
		Contract: &Contract{
			Symbol:   "AAPL",
			SecType:  "OPT",
			Expiry:   "20260619",
			Strike:   100,
			Right:    "C",
			Currency: "USD",
		},
		ExerciseAction:   OptionExerciseActionExercise,
		ExerciseQuantity: 1,
		Account:          "DU123",
	}
	if err := validateOptionExerciseRequest(valid); err != nil {
		t.Fatalf("valid exercise request failed: %v", err)
	}
	invalid := valid
	invalid.Override = 2
	if err := validateOptionExerciseRequest(invalid); err == nil || !strings.Contains(err.Error(), "override") {
		t.Fatalf("invalid override err=%v, want override", err)
	}
	invalid = valid
	invalid.ExerciseAction = 9
	if err := validateOptionExerciseRequest(invalid); err == nil || !strings.Contains(err.Error(), "action") {
		t.Fatalf("invalid action err=%v, want action", err)
	}
}
