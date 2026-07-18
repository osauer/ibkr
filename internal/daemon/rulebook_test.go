package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/marketcal"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
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
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "EUR")
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

// TestMapRuleNamesCostBasisJoinAndCompleteness covers the v2 input assembly:
// multiplier-inclusive cost basis with the same-currency FX path (a
// zero-marked line must keep its cost basis — that is rule 13's −100% case),
// the stock-leg underlying join with its quality gate, and the exposure
// completeness signal that guards rule 1's lower bound.
func TestMapRuleNamesCostBasisJoinAndCompleteness(t *testing.T) {
	pos := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{
			{
				Underlying:           "AAA",
				GroupDollarDeltaBase: new(50000.0),
				Stock:                &rpc.PositionView{Symbol: "AAA", Quantity: 100, Mark: 100, Currency: "USD"},
				Options: []rpc.PositionView{
					// FXRate present: cost = 250 × 2 × 0.9, no ×multiplier.
					{Symbol: "AAA", Quantity: 2, Multiplier: 100, Expiry: "20260821", Strike: 110, Right: "C",
						AvgCost: 250, Mark: 3, Currency: "USD", FXRate: new(0.9),
						MarketValue: 600, MarketValueBase: new(540.0)},
					// No greeks-tick spot: joins the stock mark, disclosed —
					// and a delta WITHOUT a spot marks the sum incomplete.
					{Symbol: "AAA", Quantity: 1, Multiplier: 100, Expiry: "20260918", Strike: 120, Right: "C",
						AvgCost: 100, Mark: 1, Currency: "USD", FXRate: new(0.9), Delta: new(0.3),
						MarketValue: 100, MarketValueBase: new(90.0)},
				},
			},
			{
				Underlying:           "BBB",
				GroupDollarDeltaBase: new(0.0),
				Options: []rpc.PositionView{
					// Same-currency book, marked to zero: the MV ratio is
					// undefined but positionBaseRate resolves fx=1, so the
					// −100% line keeps its cost basis for rule 13.
					{Symbol: "BBB", Quantity: 1, Multiplier: 100, Expiry: "20260821", Strike: 10, Right: "C",
						AvgCost: 500, Mark: 0, Currency: "EUR", MarketValue: 0},
				},
			},
			{
				Underlying:           "STALE",
				GroupDollarDeltaBase: new(1000.0),
				Stock:                &rpc.PositionView{Symbol: "STALE", Quantity: 10, Mark: 50, Stale: true, Currency: "EUR"},
				Options: []rpc.PositionView{
					{Symbol: "STALE", Quantity: 1, Multiplier: 100, Expiry: "20260821", Strike: 60, Right: "C",
						AvgCost: 100, Mark: 1, Currency: "EUR", MarketValue: 100},
				},
			},
		},
	}
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "EUR")
	bysym := map[string]risk.NameInput{}
	for _, n := range names {
		bysym[n.Symbol] = n
	}

	aaa := bysym["AAA"]
	if got := aaa.Legs[0].CostBasisBase; got == nil || *got != 450 {
		t.Errorf("FXRate leg cost basis = %v, want 450 (AvgCost×|qty|×fx, no multiplier)", got)
	}
	if aaa.Legs[1].Underlying == nil || *aaa.Legs[1].Underlying != 100 ||
		aaa.Legs[1].UnderlyingSource != risk.UnderlyingSourceStockLegMark {
		t.Errorf("spotless leg must join the stock mark with disclosure, got %+v/%q",
			aaa.Legs[1].Underlying, aaa.Legs[1].UnderlyingSource)
	}
	if aaa.ExposureBaseComplete {
		t.Error("delta-without-spot leg must mark the exposure sum incomplete")
	}

	bbb := bysym["BBB"]
	if got := bbb.Legs[0].CostBasisBase; got == nil || *got != 500 {
		t.Errorf("same-currency zero-marked leg cost basis = %v, want 500 — the -100%% line must stay visible to rule 13", got)
	}

	if got := bysym["STALE"].Legs[0].Underlying; got != nil {
		t.Errorf("stale stock mark must not join as underlying, got %v", *got)
	}
}

// TestNonBaseExposure pins rule 14's corroboration: an empty currency report
// is only trusted as base-only when a healthy positions snapshot shows no
// non-base holdings — an unprimed or absent snapshot must stay unknown
// (never-false-pass; a $LEDGER flake must not pass a 90%-USD book).
func TestNonBaseExposure(t *testing.T) {
	acct := func(rows ...rpc.CurrencyExposure) *rpc.AccountResult {
		return &rpc.AccountResult{BaseCurrency: "EUR", CurrencyExposure: rows}
	}
	posWith := func(ccy string) *rpc.PositionsResult {
		return &rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{{
			Underlying: "AAA",
			Stock:      &rpc.PositionView{Symbol: "AAA", Quantity: 1, Currency: ccy},
		}}}
	}

	if got, _ := nonBaseExposure(acct(), nil); got != nil {
		t.Errorf("empty report with no corroborating snapshot = %v, want nil (unknown)", *got)
	}
	if got, _ := nonBaseExposure(acct(), posWith("USD")); got != nil {
		t.Errorf("empty report with a USD leg = %v, want nil (report contradicted)", *got)
	}
	if got, _ := nonBaseExposure(acct(), posWith("EUR")); got == nil || *got != 0 {
		t.Errorf("empty report corroborated base-only = %v, want 0 (legitimate pass)", got)
	}
	got, ccys := nonBaseExposure(acct(
		rpc.CurrencyExposure{Currency: "USD", NetLiquidationCcy: 100000, ExchangeRate: 0.9, NetLiquidationBase: 90000},
		rpc.CurrencyExposure{Currency: "EUR", NetLiquidationCcy: 5000, ExchangeRate: 1, NetLiquidationBase: 5000},
	), posWith("USD"))
	if got == nil || *got != 90000 || len(ccys) != 1 || ccys[0] != "USD" {
		t.Errorf("non-base sum = %v/%v, want 90000/[USD] (base row excluded)", got, ccys)
	}
	if got, _ := nonBaseExposure(acct(
		rpc.CurrencyExposure{Currency: "USD", NetLiquidationCcy: 100000, ExchangeRate: 0},
	), posWith("USD")); got != nil {
		t.Errorf("missing exchange rate = %v, want nil (conversion unavailable)", *got)
	}
}

func TestBucketRegimeStage(t *testing.T) {
	cases := map[string]string{
		rpc.LifecycleQuiet:           risk.RegimeBucketCalm,
		rpc.LifecycleOpportunity:     risk.RegimeBucketCalm,
		rpc.LifecycleEarlyWarning:    risk.RegimeBucketEarlyWarning,
		rpc.LifecycleStabilization:   risk.RegimeBucketEarlyWarning,
		rpc.LifecycleConfirmedStress: risk.RegimeBucketConfirmed,
		rpc.LifecyclePanic:           risk.RegimeBucketConfirmed,
		rpc.LifecycleForcedDefense:   risk.RegimeBucketConfirmed,
		rpc.LifecycleDataQuality:     "", // hold the previous latch
		"":                           "",
		"some_future_stage":          risk.RegimeBucketEarlyWarning, // middle, never silently calm
	}
	for stage, want := range cases {
		if got := bucketRegimeStage(stage); got != want {
			t.Errorf("bucketRegimeStage(%q) = %q, want %q", stage, got, want)
		}
	}
}

// TestRulesRegimeStagePersistence pins restart-mid-stress: a latched stage
// survives into a fresh Server via the state file, and a skewed stored
// bucket is re-derived from the stage rather than trusted.
func TestRulesRegimeStagePersistence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := &Server{}
	s.latchRulesRegimeStage(&rpc.RegimeSnapshotResult{Lifecycle: rpc.LifecycleState{Stage: rpc.LifecycleConfirmedStress}})
	if st := s.rulesRegimeStageSnapshot(); st.Bucket != risk.RegimeBucketConfirmed {
		t.Fatalf("latched bucket = %q, want confirmed", st.Bucket)
	}

	fresh := &Server{}
	st := fresh.rulesRegimeStageSnapshot()
	if st.Bucket != risk.RegimeBucketConfirmed || st.Stage != rpc.LifecycleConfirmedStress {
		t.Fatalf("restart lost the stage: %+v", st)
	}

	// data_quality must hold the previous latch, not clear it.
	s.latchRulesRegimeStage(&rpc.RegimeSnapshotResult{Lifecycle: rpc.LifecycleState{Stage: rpc.LifecycleDataQuality}})
	if st := s.rulesRegimeStageSnapshot(); st.Bucket != risk.RegimeBucketConfirmed {
		t.Errorf("data_quality stage cleared the latch: %+v", st)
	}

	// A skewed stored bucket is re-derived from the stage on load.
	path, err := defaultTradingStatePath(rulesRegimeStageFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateStateAtomic(path, []byte(`{"version":1,"bucket":"calm","stage":"panic","as_of":"2026-07-08T10:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	skewed := &Server{}
	if st := skewed.rulesRegimeStageSnapshot(); st.Bucket != risk.RegimeBucketConfirmed {
		t.Errorf("stored bucket was trusted over the stage: %+v", st)
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

	// Rule 13: only averaging down into the SAME flagged line warns — a
	// different strike (a roll) must not.
	res.Rules = append(res.Rules, risk.RuleRow{ID: risk.RuleExitDiscipline, Number: 13, Status: risk.RuleStatusWatch,
		Offenders: []risk.RuleOffender{{Symbol: "NOW", Leg: "NOW 20260821 C 115"}}})
	sameLeg := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{
		Symbol: "NOW", SecType: "OPT", Expiry: "2026-08-21", Right: "c", Strike: 115}}
	if codes := warningCodes(rulebookPreviewWarnings(res, sameLeg, rpc.OrderPositionImpact{Effect: "increase"})); !codes["rule_exit_discipline"] {
		t.Errorf("averaging down into a flagged line must warn (dash expiry + lowercase right normalized), got %v", codes)
	}
	otherStrike := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{
		Symbol: "NOW", SecType: "OPT", Expiry: "20260821", Right: "C", Strike: 120}}
	if codes := warningCodes(rulebookPreviewWarnings(res, otherStrike, rpc.OrderPositionImpact{Effect: "increase"})); codes["rule_exit_discipline"] {
		t.Error("rolling to a different strike must not inherit the exit-discipline warning")
	}
}

func TestJournalRuleTransitionsCarriesPolicyFingerprint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	pol := risk.DefaultRulebookPolicy()
	key := pol.FingerprintKey()
	server := &Server{}
	server.journalRuleTransitions(&rpc.RulesResult{
		AsOf:          time.Now(),
		PolicyID:      pol.ID,
		PolicyVersion: pol.Version,
		PolicyFingerprint: &rpc.Fingerprint{
			Version: rpc.RulebookPolicyFingerprintVersion,
			Key:     key,
		},
		Rules: []risk.RuleRow{{ID: risk.RuleSingleNameExposure, Status: risk.RuleStatusWatch}},
	})
	(&Server{}).journalRuleTransitions(&rpc.RulesResult{
		AsOf:          time.Now(),
		PolicyID:      pol.ID,
		PolicyVersion: pol.Version,
		Rules:         []risk.RuleRow{{ID: risk.RuleCashSellOnly, Status: risk.RuleStatusAct}},
	})

	path, err := defaultTradingStatePath("rules-decisions.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal entries = %d, want 2", len(lines))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("journal entry is not JSON: %v", err)
	}
	if got, _ := entry["policy_fingerprint"].(string); got == "" || got != key {
		t.Fatalf("policy_fingerprint = %q, want %q", got, key)
	}
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatalf("nil-fingerprint journal entry is not JSON: %v", err)
	}
	if got, ok := entry["policy_fingerprint"].(string); !ok || got != "" {
		t.Fatalf("nil policy_fingerprint = %#v, want empty string", entry["policy_fingerprint"])
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
	applyFeatureSettingsPatch := func(next *platformSettingsData, featurePatch json.RawMessage) error {
		patch := map[string]json.RawMessage{"features": featurePatch}
		flat, err := flattenSettingsPatch(patch)
		if err != nil {
			return err
		}
		for key, raw := range flat {
			if err := applySettingsKey(next, key, raw); err != nil {
				return err
			}
		}
		return nil
	}
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
	if got := next.Features.Rulebook.EarningsOverrides["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("failed patch must leave prior overrides intact, got %q", got)
	}

	// Patches merge per symbol: touching AMD must not drop NOW, a null AMD
	// clears only AMD, and a null map clears everything.
	upsert := json.RawMessage(`{"rulebook":{"earnings_overrides":{"amd":"2026-08-04Tbmo"}}}`)
	if err := applyFeatureSettingsPatch(next, upsert); err != nil {
		t.Fatalf("upsert patch: %v", err)
	}
	if got := next.Features.Rulebook.EarningsOverrides["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("unmentioned symbol must survive a per-symbol patch, got %q", got)
	}
	if got := next.Features.Rulebook.EarningsOverrides["AMD"]; got != "2026-08-04Tbmo" {
		t.Fatalf("AMD override = %q, want normalized upsert", got)
	}
	clearOne := json.RawMessage(`{"rulebook":{"earnings_overrides":{"AMD":null}}}`)
	if err := applyFeatureSettingsPatch(next, clearOne); err != nil {
		t.Fatalf("clear-one patch: %v", err)
	}
	if _, exists := next.Features.Rulebook.EarningsOverrides["AMD"]; exists {
		t.Fatal("per-symbol null must clear only that symbol")
	}
	if got := next.Features.Rulebook.EarningsOverrides["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("clearing one symbol must not touch the others, got %q", got)
	}
	clearAll := json.RawMessage(`{"rulebook":{"earnings_overrides":null}}`)
	if err := applyFeatureSettingsPatch(next, clearAll); err != nil {
		t.Fatalf("clear-all patch: %v", err)
	}
	if next.Features.Rulebook.EarningsOverrides != nil {
		t.Fatal("null map must clear all overrides")
	}
}
