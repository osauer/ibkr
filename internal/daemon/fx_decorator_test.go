package daemon

import (
	"math"
	"testing"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

// TestFillFXRatesAppliesToNonBase: a USD position in a EUR account
// should get FXRate + MarketValueCcy filled; an EUR position should
// be left alone.
func TestFillFXRatesAppliesToNonBase(t *testing.T) {
	rows := []rpc.PositionView{
		{Symbol: "AAPL", Currency: "USD", MarketValue: 10000},
		{Symbol: "SAP", Currency: "EUR", MarketValue: 7500},
	}
	ledger := map[string]ibkrlib.CurrencyLedger{
		"USD": {ExchangeRate: 0.9214},
		"EUR": {ExchangeRate: 1.0},
	}
	fillFXRates(rows, ledger, "EUR")

	if rows[0].FXRate == nil || math.Abs(*rows[0].FXRate-0.9214) > 1e-9 {
		t.Errorf("AAPL FXRate = %v, want 0.9214", rows[0].FXRate)
	}
	if rows[0].MarketValueCcy == nil || math.Abs(*rows[0].MarketValueCcy-10000) > 1e-9 {
		t.Errorf("AAPL MarketValueCcy = %v, want 10000", rows[0].MarketValueCcy)
	}
	if rows[1].FXRate != nil {
		t.Errorf("SAP FXRate should be nil (same-currency), got %v", *rows[1].FXRate)
	}
}

// TestFillFXRatesEmptyLedger: pre-handshake / single-currency book
// leaves every row's FXRate nil — never zero-substituted.
func TestFillFXRatesEmptyLedger(t *testing.T) {
	rows := []rpc.PositionView{
		{Symbol: "AAPL", Currency: "USD", MarketValue: 10000},
	}
	fillFXRates(rows, nil, "EUR")
	if rows[0].FXRate != nil {
		t.Errorf("FXRate should be nil with empty ledger, got %v", *rows[0].FXRate)
	}
}

// TestAddFXSensitivitySumsNonBase: the portfolio's 1%-move sensitivity
// is Σ (non-base NetLiq × FX × 0.01). Base-currency rows must not
// contribute.
func TestAddFXSensitivitySumsNonBase(t *testing.T) {
	p := &rpc.PositionsPortfolio{}
	ledger := map[string]ibkrlib.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: 5000, ExchangeRate: 1.0},      // base — excluded
		"USD": {NetLiquidationByCurrency: 100000, ExchangeRate: 0.9214}, // non-base
		"GBP": {NetLiquidationByCurrency: 2000, ExchangeRate: 1.1500},   // non-base
	}
	addFXSensitivity(p, ledger, "EUR")

	// 100000 * 0.9214 * 0.01 + 2000 * 1.15 * 0.01 = 921.4 + 23 = 944.4
	want := 944.4
	if p.FXSensitivityPerPct == nil || math.Abs(*p.FXSensitivityPerPct-want) > 0.01 {
		t.Errorf("FXSensitivityPerPct = %v, want %v", p.FXSensitivityPerPct, want)
	}
	if p.FXBaseCurrency != "EUR" {
		t.Errorf("FXBaseCurrency = %q, want EUR", p.FXBaseCurrency)
	}
}

// TestAddFXSensitivityNilWhenOnlyBase: a portfolio with only same-
// currency holdings must NOT emit a sensitivity (the answer is "0
// exposure"; we surface that as nil so callers know there's nothing
// to act on — distinct from a real zero, though in this case they
// happen to coincide).
func TestAddFXSensitivityNilWhenOnlyBase(t *testing.T) {
	p := &rpc.PositionsPortfolio{}
	ledger := map[string]ibkrlib.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: 5000, ExchangeRate: 1.0},
	}
	addFXSensitivity(p, ledger, "EUR")
	if p.FXSensitivityPerPct != nil {
		t.Errorf("FXSensitivityPerPct should be nil when no non-base exposure, got %v", *p.FXSensitivityPerPct)
	}
}

// TestBuildCurrencyExposureReconciles: the wire-shape rows must
// reconcile within ~0.5% — NetLiquidationCcy × ExchangeRate matches
// NetLiquidationBase. Renderers and JSON consumers depend on this
// invariant.
func TestBuildCurrencyExposureReconciles(t *testing.T) {
	ledger := map[string]ibkrlib.CurrencyLedger{
		"USD": {NetLiquidationByCurrency: 95000, ExchangeRate: 0.9214},
		"GBP": {NetLiquidationByCurrency: 1000, ExchangeRate: 1.1500},
	}
	got := buildCurrencyExposure(ledger, "EUR")
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	for _, row := range got {
		want := row.NetLiquidationCcy * row.ExchangeRate
		if math.Abs(row.NetLiquidationBase-want) > 0.01 {
			t.Errorf("%s: base = %v, want ~%v", row.Currency, row.NetLiquidationBase, want)
		}
	}
	// Stable sort by currency.
	if got[0].Currency != "GBP" || got[1].Currency != "USD" {
		t.Errorf("currency order = [%s, %s], want [GBP, USD]", got[0].Currency, got[1].Currency)
	}
}

// TestBuildCurrencyExposureDropsBaseCurrency: a EUR base account that
// also has an "EUR" ledger entry must NOT surface it as exposure —
// it's the base by definition and would duplicate the top-level totals.
func TestBuildCurrencyExposureDropsBaseCurrency(t *testing.T) {
	ledger := map[string]ibkrlib.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: -50, ExchangeRate: 1.0},
		"USD": {NetLiquidationByCurrency: 95000, ExchangeRate: 0.9214},
	}
	got := buildCurrencyExposure(ledger, "EUR")
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (EUR row should be filtered out)", len(got))
	}
	if got[0].Currency != "USD" {
		t.Errorf("Currency = %q, want USD (EUR should be dropped as base)", got[0].Currency)
	}
}

// TestBuildCurrencyExposureFallbackByFXRate: when the caller doesn't
// know the base currency (early-handshake state), fall back to dropping
// the row whose FX rate is exactly 1.0 — that's the base by definition.
func TestBuildCurrencyExposureFallbackByFXRate(t *testing.T) {
	ledger := map[string]ibkrlib.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: 5000, ExchangeRate: 1.0},
		"USD": {NetLiquidationByCurrency: 95000, ExchangeRate: 0.9214},
	}
	got := buildCurrencyExposure(ledger, "")
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 (EUR with FX=1 should be dropped)", len(got))
	}
	if got[0].Currency != "USD" {
		t.Errorf("Currency = %q, want USD", got[0].Currency)
	}
}

// TestBaseCurrencyFromRaw_IgnoresLiteralBASE is the regression for the
// "FX sensitivity ... BASE per +1% FX" rendering: when IBKR emits a bare
// `Currency` tag whose value is the literal string "BASE" (the gateway's
// pseudo-currency name, not the actual base currency identity), we MUST
// fall through to the ExchangeRate_<ccy>=1.0 signal instead of returning
// "BASE" as if it were a real currency.
func TestBaseCurrencyFromRaw_IgnoresLiteralBASE(t *testing.T) {
	raw := map[string]string{
		"Currency":          "BASE",
		"ExchangeRate_EUR":  "1.0",
		"ExchangeRate_USD":  "0.9214",
		"ExchangeRate_BASE": "1.0",
	}
	if got := baseCurrencyFromRaw(raw); got != "EUR" {
		t.Errorf("baseCurrencyFromRaw = %q, want EUR (literal BASE must be ignored)", got)
	}
}

// TestBaseCurrencyFromRaw_RealCurrencyTag: if the gateway happens to
// emit a Currency tag whose value is an actual currency code (rare but
// not impossible), prefer it over the ledger scan.
func TestBaseCurrencyFromRaw_RealCurrencyTag(t *testing.T) {
	raw := map[string]string{
		"Currency":         "eur",
		"ExchangeRate_USD": "0.9214",
	}
	if got := baseCurrencyFromRaw(raw); got != "EUR" {
		t.Errorf("baseCurrencyFromRaw = %q, want EUR", got)
	}
}

// TestBaseCurrencyFromRaw_ExchangeRateOnly: no Currency tag at all,
// pure $LEDGER:ALL output — the ExchangeRate_<ccy>=1.0 row identifies
// the base.
func TestBaseCurrencyFromRaw_ExchangeRateOnly(t *testing.T) {
	raw := map[string]string{
		"ExchangeRate_EUR": "1.0",
		"ExchangeRate_USD": "0.9214",
		"ExchangeRate_GBP": "1.1500",
	}
	if got := baseCurrencyFromRaw(raw); got != "EUR" {
		t.Errorf("baseCurrencyFromRaw = %q, want EUR", got)
	}
}

// TestBaseCurrencyFromRaw_NoSignal: nothing usable in the raw map — we
// return empty, never invent a default.
func TestBaseCurrencyFromRaw_NoSignal(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation":   "100000",
		"AccountReady":     "true",
		"ExchangeRate_USD": "0.9214",
	}
	if got := baseCurrencyFromRaw(raw); got != "" {
		t.Errorf("baseCurrencyFromRaw = %q, want empty", got)
	}
}

// TestBaseCurrencyFromRaw_EmptyMap: pre-handshake state.
func TestBaseCurrencyFromRaw_EmptyMap(t *testing.T) {
	if got := baseCurrencyFromRaw(nil); got != "" {
		t.Errorf("baseCurrencyFromRaw(nil) = %q, want empty", got)
	}
	if got := baseCurrencyFromRaw(map[string]string{}); got != "" {
		t.Errorf("baseCurrencyFromRaw(empty) = %q, want empty", got)
	}
}
