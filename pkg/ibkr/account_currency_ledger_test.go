package ibkr

import (
	"math"
	"testing"
)

// TestExtractCurrencyLedgerHappyPath simulates a captured $LEDGER:ALL
// response for a EUR base account holding USD positions. The parser
// must produce one row per non-BASE currency, with the right values
// in each field.
func TestExtractCurrencyLedgerHappyPath(t *testing.T) {
	raw := map[string]string{
		// BASE pseudo-currency entries (should be dropped — they
		// duplicate the unsuffixed totals).
		"CashBalance_BASE":              "1234.56",
		"NetLiquidationByCurrency_BASE": "100000",
		"ExchangeRate_BASE":             "1.0",
		// EUR row — the account's actual base.
		"CashBalance_EUR":              "1500.00",
		"NetLiquidationByCurrency_EUR": "5000.00",
		"StockMarketValue_EUR":         "3500.00",
		"OptionMarketValue_EUR":        "0.00",
		"UnrealizedPnL_EUR":            "120.50",
		"RealizedPnL_EUR":              "-15.25",
		"ExchangeRate_EUR":             "1.0",
		// USD row — non-base exposure, the case the user actually cares about.
		"CashBalance_USD":              "10500.75",
		"NetLiquidationByCurrency_USD": "95000.00",
		"StockMarketValue_USD":         "60000.00",
		"OptionMarketValue_USD":        "24500.00",
		"UnrealizedPnL_USD":            "-300.10",
		"RealizedPnL_USD":              "1250.00",
		"ExchangeRate_USD":             "0.9214",
		// Unrelated non-LEDGER fields must be ignored.
		"NetLiquidation":      "100000.00",
		"AccountReady":        "true",
		"AccountType":         "INDIVIDUAL",
		"AccruedDividend_USD": "12.34", // not in the canonical set; ignored
	}

	ledger := extractCurrencyLedger(raw)

	if _, ok := ledger["BASE"]; ok {
		t.Errorf("BASE row should be dropped, got %+v", ledger["BASE"])
	}
	usd, ok := ledger["USD"]
	if !ok {
		t.Fatalf("expected USD row, got keys=%v", keysOf(ledger))
	}
	if math.Abs(usd.NetLiquidationByCurrency-95000) > 0.01 {
		t.Errorf("USD NetLiq = %v, want 95000", usd.NetLiquidationByCurrency)
	}
	if math.Abs(usd.CashBalance-10500.75) > 0.01 {
		t.Errorf("USD CashBalance = %v, want 10500.75", usd.CashBalance)
	}
	if math.Abs(usd.StockMarketValue-60000) > 0.01 {
		t.Errorf("USD StockMarketValue = %v, want 60000", usd.StockMarketValue)
	}
	if math.Abs(usd.OptionMarketValue-24500) > 0.01 {
		t.Errorf("USD OptionMarketValue = %v, want 24500", usd.OptionMarketValue)
	}
	if math.Abs(usd.ExchangeRate-0.9214) > 0.0001 {
		t.Errorf("USD ExchangeRate = %v, want 0.9214", usd.ExchangeRate)
	}

	// Reconciliation: NetLiq(USD) × FX ≈ contribution to base NetLiq.
	gotBase := usd.NetLiquidationByCurrency * usd.ExchangeRate
	if gotBase < 80000 || gotBase > 95000 {
		t.Errorf("USD→base = %v, expected 87.5k–88k for 95k * 0.9214", gotBase)
	}
}

// TestExtractCurrencyLedgerSkipsZeroBalance: currencies the gateway
// happens to mention but where the user holds nothing must drop out.
func TestExtractCurrencyLedgerSkipsZeroBalance(t *testing.T) {
	raw := map[string]string{
		"NetLiquidationByCurrency_HKD": "0",
		"CashBalance_HKD":              "0",
		"StockMarketValue_HKD":         "0",
		"OptionMarketValue_HKD":        "0",
		"ExchangeRate_HKD":             "0.12",
		// Real USD position.
		"NetLiquidationByCurrency_USD": "1000",
		"ExchangeRate_USD":             "0.92",
	}
	ledger := extractCurrencyLedger(raw)
	if _, ok := ledger["HKD"]; ok {
		t.Errorf("HKD with all-zero balance should be dropped")
	}
	if _, ok := ledger["USD"]; !ok {
		t.Errorf("USD with NetLiq should be retained")
	}
}

// TestExtractCurrencyLedgerMalformedValue: a non-parseable value for
// one field must not corrupt the rest of the row or panic.
func TestExtractCurrencyLedgerMalformedValue(t *testing.T) {
	raw := map[string]string{
		"NetLiquidationByCurrency_USD": "not-a-number",
		"CashBalance_USD":              "500",
		"ExchangeRate_USD":             "0.92",
	}
	ledger := extractCurrencyLedger(raw)
	usd, ok := ledger["USD"]
	if !ok {
		t.Fatalf("USD row should still be present from valid fields")
	}
	if usd.NetLiquidationByCurrency != 0 {
		t.Errorf("malformed NetLiq should leave field zero, got %v", usd.NetLiquidationByCurrency)
	}
	if math.Abs(usd.CashBalance-500) > 0.01 {
		t.Errorf("CashBalance = %v, want 500", usd.CashBalance)
	}
}

func keysOf(m map[string]CurrencyLedger) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
