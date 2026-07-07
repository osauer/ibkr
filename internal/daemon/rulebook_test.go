package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/marketcal"
	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

// Recorded api.nasdaq.com response shape (captured 2026-07-06; see design
// doc "endpoint spike"). The parser must be strict: ambiguity is a miss.
const nasdaqEarningsFixture = `{"data":{"reportText":"ServiceNow, Inc. Common Stock is expected* to report earnings on  07/22/2026 after market close.  The report will be for the fiscal Quarter ending Jun 2026.","heading":"NOW Earnings Date","announcement":"Earnings announcement* for NOW: Jul 22, 2026"},"status":{"rCode":200}}`

func TestParseNasdaqEarnings(t *testing.T) {
	now := time.Date(2026, 7, 7, 8, 0, 0, 0, time.UTC)
	entry, err := parseNasdaqEarnings([]byte(nasdaqEarningsFixture), now)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if entry.Date != "2026-07-22" || entry.TimeOfDay != "amc" || !entry.Estimated {
		t.Fatalf("entry = %+v, want 2026-07-22 amc estimated", entry)
	}

	if _, err := parseNasdaqEarnings([]byte(`{"data":{"announcement":"No date scheduled"}}`), now); err == nil {
		t.Fatal("missing date must be an error, never a guessed date")
	}
	if _, err := parseNasdaqEarnings([]byte(`not json`), now); err == nil {
		t.Fatal("malformed payload must be an error")
	}
	if _, err := parseNasdaqEarnings([]byte(`{"data":{"announcement":"Earnings announcement for X: Julember 40, 2026"}}`), now); err == nil {
		t.Fatal("unparseable date must be an error")
	}
}

func TestNasdaqSymbolMapping(t *testing.T) {
	cases := map[string]string{
		"NOW":   "NOW",
		"BRK B": "BRK.B",
		"brk b": "BRK.B",
		"":      "",
		"BAD/S": "",
	}
	for in, want := range cases {
		if got := nasdaqSymbol(in); got != want {
			t.Errorf("nasdaqSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEarningsOverride(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	e, ok := parseEarningsOverride("2026-07-22Tamc", loc)
	if !ok || e.Date.Format("2006-01-02") != "2026-07-22" || e.TimeOfDay != "amc" {
		t.Fatalf("override parse = %+v ok=%v", e, ok)
	}
	if _, ok := parseEarningsOverride("July 22", loc); ok {
		t.Fatal("bad override must not parse")
	}
	if e, ok := parseEarningsOverride("2026-07-22", loc); !ok || e.TimeOfDay != "" {
		t.Fatalf("date-only override = %+v ok=%v, want empty time_of_day", e, ok)
	}
}

func TestSessionsUntilCountsTradingDays(t *testing.T) {
	cal := marketcal.New()
	loc, _ := time.LoadLocation("America/New_York")
	// Tue 2026-07-07 → Thu 2026-07-09: Tue+Wed+Thu = 3 sessions.
	got := sessionsUntil(cal, time.Date(2026, 7, 7, 9, 0, 0, 0, loc), time.Date(2026, 7, 9, 0, 0, 0, 0, loc))
	if got == nil || *got != 3 {
		t.Fatalf("sessionsUntil Tue→Thu = %v, want 3", got)
	}
	// Fri 2026-07-10 → Mon 2026-07-13 skips the weekend: 2 sessions.
	got = sessionsUntil(cal, time.Date(2026, 7, 10, 9, 0, 0, 0, loc), time.Date(2026, 7, 13, 0, 0, 0, 0, loc))
	if got == nil || *got != 2 {
		t.Fatalf("sessionsUntil Fri→Mon = %v, want 2", got)
	}
	if got := sessionsUntil(cal, time.Date(2026, 7, 10, 9, 0, 0, 0, loc), time.Date(2026, 7, 1, 0, 0, 0, 0, loc)); got != nil {
		t.Fatalf("past target = %v, want nil", got)
	}
}

// TestMapRuleNamesExposureMatchesGroups is the aggregation-consistency
// gate: rule 1 must read the identical delta-dollar exposure the canary's
// concentration check reads (GroupDollarDeltaBase). Bars may differ across
// surfaces; observations may not.
func TestMapRuleNamesExposureMatchesGroups(t *testing.T) {
	pos := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{
			{
				Underlying:           "NOW",
				GroupDollarDeltaBase: new(380000.0),
				GroupMarketValueBase: new(120000.0),
				Stock:                &rpc.PositionView{Symbol: "NOW", Quantity: 500, DayChangePct: new(1.6)},
				Options: []rpc.PositionView{{
					Symbol: "NOW", SecType: "OPTION", Quantity: 50, Multiplier: 100,
					Expiry: "20260821", Strike: 115, Right: "C", Mark: 7.86,
					Underlying: new(108.0), Delta: new(0.46),
					MarketValue: 39300, MarketValueBase: new(36000.0),
				}},
			},
			{
				Underlying:           "GAPPY",
				GroupDollarDeltaBase: new(10000.0),
				Options: []rpc.PositionView{{
					Symbol: "GAPPY", Quantity: 10, Multiplier: 100, Expiry: "20260821",
					Strike: 10, Right: "C", Mark: 2, MarketValue: 2000, MarketValueBase: new(1800.0),
				}},
			},
		},
	}
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy())
	if len(names) != 2 {
		t.Fatalf("names = %d, want 2", len(names))
	}
	bysym := map[string]risk.NameInput{}
	for _, n := range names {
		bysym[n.Symbol] = n
	}
	if bysym["NOW"].ExposureBase != 380000 {
		t.Fatalf("NOW exposure = %v, want the group's GroupDollarDeltaBase 380000", bysym["NOW"].ExposureBase)
	}
	if bysym["NOW"].Legs[0].ExtrinsicBase == nil {
		t.Fatal("computable leg extrinsic must be set")
	}
	// The GAPPY leg has no delta: its notional must land in the greeks gap,
	// never silently shrink the exposure.
	if bysym["GAPPY"].GreeksGapNotionalBase != 1800 {
		t.Fatalf("GAPPY greeks gap = %v, want 1800", bysym["GAPPY"].GreeksGapNotionalBase)
	}
	if bysym["NOW"].Legs[0].DTE <= 0 {
		t.Fatalf("DTE = %d, want positive", bysym["NOW"].Legs[0].DTE)
	}
}

func TestRulebookPreviewWarnings(t *testing.T) {
	res := &rpc.RulesResult{
		Enabled: true,
		AsOf:    time.Now(),
		Rules: []risk.RuleRow{
			{ID: risk.RuleSingleNameExposure, Number: 1, Status: risk.RuleStatusAct,
				Offenders: []risk.RuleOffender{{Symbol: "NOW"}}},
			{ID: risk.RuleCashSellOnly, Number: 3, Status: risk.RuleStatusAct},
			{ID: risk.RuleExtrinsicBudget, Number: 4, Status: risk.RuleStatusWatch},
		},
	}
	buyStock := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "NOW", SecType: "STK"}}
	buyOpt := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "NOW", SecType: "OPT"}}
	sellOther := rpc.OrderDraft{Action: "SELL", Contract: rpc.ContractParams{Symbol: "IBM", SecType: "STK"}}

	warns := rulebookPreviewWarnings(res, buyOpt, rpc.OrderPositionImpact{Effect: "increase"})
	codes := warningCodes(warns)
	for _, want := range []string{"rule_single_name_exposure", "rule_cash_sell_only", "rule_extrinsic_budget"} {
		if !codes[want] {
			t.Errorf("buy option on offender: missing %s (got %v)", want, codes)
		}
	}
	for _, w := range warns {
		if w.Scope != "rulebook" || (w.Severity != risk.RuleStatusAct && w.Severity != risk.RuleStatusWatch) {
			t.Errorf("warning %s carries scope=%q severity=%q; wants rulebook scope and the rule's own severity", w.Code, w.Scope, w.Severity)
		}
	}

	if warns := rulebookPreviewWarnings(res, buyStock, rpc.OrderPositionImpact{Effect: "reduce"}); len(warns) != 0 {
		t.Errorf("reduce effect must never warn, got %v", warningCodes(warns))
	}
	warns = rulebookPreviewWarnings(res, sellOther, rpc.OrderPositionImpact{Effect: "open_short"})
	if codes := warningCodes(warns); codes["rule_single_name_exposure"] || codes["rule_cash_sell_only"] {
		t.Errorf("sell on a non-offender must not inherit NOW/cash warnings, got %v", codes)
	}
	if warns := rulebookPreviewWarnings(nil, buyStock, rpc.OrderPositionImpact{Effect: "increase"}); warns != nil {
		t.Error("nil rules result must be silent")
	}
}

func warningCodes(warns []rpc.DataWarning) map[string]bool {
	out := map[string]bool{}
	for _, w := range warns {
		out[w.Code] = true
	}
	return out
}

func TestRulebookSettingsPatch(t *testing.T) {
	next := &platformSettingsData{Version: 1}
	patch := json.RawMessage(`{"rulebook":{"enabled":false,"earnings_overrides":{"now":"2026-07-22Tamc","BB":null}}}`)
	if err := applyFeatureSettingsPatch(next, patch); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if next.Features.Rulebook.Enabled == nil || *next.Features.Rulebook.Enabled {
		t.Fatal("enabled=false not applied")
	}
	if got := next.Features.Rulebook.EarningsOverrides["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("override = %q, want normalized NOW key", got)
	}
	if _, exists := next.Features.Rulebook.EarningsOverrides["BB"]; exists {
		t.Fatal("null override value must clear the symbol")
	}
	bad := json.RawMessage(`{"rulebook":{"earnings_overrides":{"NOW":"soon"}}}`)
	if err := applyFeatureSettingsPatch(next, bad); err == nil || !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Fatalf("bad override date must fail loudly, got %v", err)
	}
}
