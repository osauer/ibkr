package daemon

import (
	"context"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

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

// TestRepairCurrencyLedgerFXRatesFixesSuspiciousUnitRates covers the live
// regression where the gateway reports ExchangeRate=1 (or omits it) for
// held non-base currencies in a EUR account. Repaired rates must feed both
// account exposure and positions FX sensitivity.
func TestRepairCurrencyLedgerFXRatesFixesSuspiciousUnitRates(t *testing.T) {
	ledger := map[string]ibkrlib.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: 5000, ExchangeRate: 1.0},
		"USD": {NetLiquidationByCurrency: 95000, ExchangeRate: 1.0},
		"CHF": {NetLiquidationByCurrency: 1000, ExchangeRate: 0},
	}
	calls := map[string]int{}
	var mu sync.Mutex
	got := repairCurrencyLedgerFXRatesWithResolver(context.Background(), ledger, "EUR", time.Millisecond, func(_ context.Context, baseCcy, ccy string, _ time.Duration) (float64, bool) {
		if baseCcy != "EUR" {
			return 0, false
		}
		mu.Lock()
		calls[ccy]++
		mu.Unlock()
		switch ccy {
		case "USD":
			return 0.86, true
		case "CHF":
			return 1.05, true
		default:
			return 0, false
		}
	})

	if got["EUR"].ExchangeRate != 1.0 {
		t.Errorf("EUR ExchangeRate = %v, want 1", got["EUR"].ExchangeRate)
	}
	if math.Abs(got["USD"].ExchangeRate-0.86) > 1e-9 {
		t.Errorf("USD ExchangeRate = %v, want 0.86", got["USD"].ExchangeRate)
	}
	if math.Abs(got["CHF"].ExchangeRate-1.05) > 1e-9 {
		t.Errorf("CHF ExchangeRate = %v, want 1.05", got["CHF"].ExchangeRate)
	}
	if calls["USD"] != 1 || calls["CHF"] != 1 || calls["EUR"] != 0 {
		t.Errorf("resolver calls = %#v, want USD=1 CHF=1 EUR=0", calls)
	}

	exposure := buildCurrencyExposure(got, "EUR")
	if len(exposure) != 2 {
		t.Fatalf("got %d exposure rows, want 2", len(exposure))
	}
	for _, row := range exposure {
		want := row.NetLiquidationCcy * row.ExchangeRate
		if math.Abs(row.NetLiquidationBase-want) > 0.01 {
			t.Errorf("%s NetLiquidationBase = %v, want %v", row.Currency, row.NetLiquidationBase, want)
		}
	}

	p := &rpc.PositionsPortfolio{}
	addFXSensitivity(p, got, "EUR")
	wantSens := (95000*0.86 + 1000*1.05) * 0.01
	if p.FXSensitivityPerPct == nil || math.Abs(*p.FXSensitivityPerPct-wantSens) > 0.01 {
		t.Errorf("FXSensitivityPerPct = %v, want %v", p.FXSensitivityPerPct, wantSens)
	}
}

func TestRepairCurrencyLedgerFXRatesKeepsValidRates(t *testing.T) {
	ledger := map[string]ibkrlib.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: 5000, ExchangeRate: 1.0},
		"USD": {NetLiquidationByCurrency: 95000, ExchangeRate: 0.9214},
	}
	called := false
	got := repairCurrencyLedgerFXRatesWithResolver(context.Background(), ledger, "EUR", time.Millisecond, func(context.Context, string, string, time.Duration) (float64, bool) {
		called = true
		return 0, false
	})
	if called {
		t.Fatal("resolver should not be called for an existing non-unit FX rate")
	}
	if math.Abs(got["USD"].ExchangeRate-0.9214) > 1e-9 {
		t.Errorf("USD ExchangeRate = %v, want 0.9214", got["USD"].ExchangeRate)
	}
}

func TestRepairCurrencyLedgerFXRatesZerosUnresolvedUnitRates(t *testing.T) {
	ledger := map[string]ibkrlib.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: 5000, ExchangeRate: 1.0},
		"USD": {NetLiquidationByCurrency: 95000, ExchangeRate: 1.0},
	}
	got := repairCurrencyLedgerFXRatesWithResolver(context.Background(), ledger, "EUR", time.Millisecond, func(context.Context, string, string, time.Duration) (float64, bool) {
		return 0, false
	})
	if got["USD"].ExchangeRate != 0 {
		t.Errorf("USD ExchangeRate = %v, want 0 for unresolved suspicious rate", got["USD"].ExchangeRate)
	}
	if exposure := buildCurrencyExposure(got, "EUR"); len(exposure) != 0 {
		t.Fatalf("got %d exposure rows, want 0 when FX rate is unresolved", len(exposure))
	}
	rows := []rpc.PositionView{{Symbol: "AAPL", Currency: "USD", MarketValue: 10000}}
	fillFXRates(rows, got, "EUR")
	if rows[0].FXRate != nil {
		t.Errorf("FXRate should be nil for unresolved suspicious rate, got %v", *rows[0].FXRate)
	}
}

func TestAddFXSensitivitySkipsUnknownBase(t *testing.T) {
	p := &rpc.PositionsPortfolio{}
	ledger := map[string]ibkrlib.CurrencyLedger{
		"USD": {NetLiquidationByCurrency: 95000, ExchangeRate: 0.9214},
	}
	addFXSensitivity(p, ledger, "")
	if p.FXSensitivityPerPct != nil {
		t.Errorf("FXSensitivityPerPct should be nil with unknown base currency, got %v", *p.FXSensitivityPerPct)
	}
}

func TestMissingPositionFXCurrenciesDetectsIncompleteLedger(t *testing.T) {
	stocks := []rpc.PositionView{
		{Symbol: "AAPL", Currency: "USD"},
		{Symbol: "SAP", Currency: "EUR"},
	}
	options := []rpc.PositionView{
		{Symbol: "VOD", Currency: "GBP"},
	}
	ledger := map[string]ibkrlib.CurrencyLedger{
		"USD": {ExchangeRate: 0},
		"GBP": {ExchangeRate: 1.15},
	}
	got := missingPositionFXCurrencies(stocks, options, ledger, "EUR")
	want := []string{"USD"}
	if !slices.Equal(got, want) {
		t.Errorf("missingPositionFXCurrencies = %v, want %v", got, want)
	}
}

func TestMergeCurrencyLedgersPrefersFreshPrimary(t *testing.T) {
	primary := map[string]ibkrlib.CurrencyLedger{
		"USD": {NetLiquidationByCurrency: 95000, ExchangeRate: 0.86},
	}
	fallback := map[string]ibkrlib.CurrencyLedger{
		"USD": {NetLiquidationByCurrency: 94000, ExchangeRate: 1.0},
		"CHF": {NetLiquidationByCurrency: 825, ExchangeRate: 1.09},
	}
	got := mergeCurrencyLedgers(primary, fallback)
	if math.Abs(got["USD"].ExchangeRate-0.86) > 1e-9 {
		t.Errorf("USD ExchangeRate = %v, want primary 0.86", got["USD"].ExchangeRate)
	}
	if math.Abs(got["CHF"].ExchangeRate-1.09) > 1e-9 {
		t.Errorf("CHF ExchangeRate = %v, want fallback 1.09", got["CHF"].ExchangeRate)
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

// TestBaseCurrencyFromRaw_PrefersAccountValueSuffix pins the live-account
// regression where several $LEDGER exchange-rate rows were 1.0 and the
// old map iteration fallback randomly labelled the portfolio base CHF/USD.
func TestBaseCurrencyFromRaw_PrefersAccountValueSuffix(t *testing.T) {
	raw := map[string]string{
		"Currency":           "BASE",
		"NetLiquidation_EUR": "181000",
		"ExchangeRate_CHF":   "1.0",
		"ExchangeRate_USD":   "1.0",
	}
	if got := baseCurrencyFromRaw(raw); got != "EUR" {
		t.Errorf("baseCurrencyFromRaw = %q, want EUR", got)
	}
}

// TestBaseCurrencyFromRaw_AmbiguousUnitRates: if all we know is that
// multiple currencies have unit exchange rates, returning any one of them
// is worse than returning unknown.
func TestBaseCurrencyFromRaw_AmbiguousUnitRates(t *testing.T) {
	raw := map[string]string{
		"ExchangeRate_CHF": "1.0",
		"ExchangeRate_USD": "1.0",
	}
	if got := baseCurrencyFromRaw(raw); got != "" {
		t.Errorf("baseCurrencyFromRaw = %q, want empty for ambiguous unit rates", got)
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
