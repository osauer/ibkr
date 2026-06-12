package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/discover"
	"github.com/osauer/ibkr/internal/rpc"
)

func TestThetaProposalNeverOpensRisk(t *testing.T) {
	theta := -0.08
	spread := 12.0
	row := rpc.PositionView{
		Symbol:     "AAPL",
		SecType:    "OPTION",
		Quantity:   2,
		Multiplier: 100,
		Mark:       1.25,
		Expiry:     "20260619",
		Right:      "C",
		Strike:     200,
		Theta:      &theta,
		SpreadPct:  &spread,
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	prop, ok := thetaProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("theta proposal missing")
	}
	if prop.Action != rpc.OrderActionSell || prop.PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatalf("proposal action/effect = %s/%s, want SELL/close", prop.Action, prop.PositionEffect)
	}
	if prop.Quantity != 2 || prop.MaxQuantity != 2 {
		t.Fatalf("proposal qty=%d max=%d, want 2/2", prop.Quantity, prop.MaxQuantity)
	}
}

func TestRiskReductionEmitsReduceOnly(t *testing.T) {
	pct := 40.0
	mv := 40000.0
	group := rpc.PositionGroup{
		Underlying:             "MSFT",
		GroupMarketValueBase:   &mv,
		GroupMarketValuePctNLV: &pct,
		GroupMarketValue:       40000,
		Stock:                  &rpc.PositionView{Symbol: "MSFT", SecType: "STOCK", Quantity: 100, Mark: 400, Multiplier: 1, Currency: "USD"},
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	prop, ok := riskReductionProposal(policy, status, group, rpc.TradeProposalSourceFingerprints{}, time.Now())
	if !ok {
		t.Fatal("risk proposal missing")
	}
	if prop.PositionEffect != rpc.OrderPositionEffectReduce && prop.PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatalf("position effect=%q, want reduce/close", prop.PositionEffect)
	}
	if prop.Action != rpc.OrderActionSell {
		t.Fatalf("action=%q, want SELL", prop.Action)
	}
	if prop.Quantity <= 0 || prop.Quantity > prop.MaxQuantity {
		t.Fatalf("quantity=%d max=%d", prop.Quantity, prop.MaxQuantity)
	}
	if prop.RiskExcessNotional != 15000 {
		t.Fatalf("risk excess notional=%v, want 15000", prop.RiskExcessNotional)
	}
	if prop.RiskExcessCurrency != "USD" {
		t.Fatalf("risk excess currency=%q, want USD", prop.RiskExcessCurrency)
	}
	counts := proposalCounts([]rpc.TradeProposal{prop})
	if counts.RiskReductionExcessNotional != prop.RiskExcessNotional {
		t.Fatalf("risk excess aggregate=%v, want %v", counts.RiskReductionExcessNotional, prop.RiskExcessNotional)
	}
	if counts.RiskReductionExcessCurrency != "USD" {
		t.Fatalf("risk excess aggregate currency=%q, want USD", counts.RiskReductionExcessCurrency)
	}
}

func TestRiskReductionSkipsUnsupportedSecurityTypes(t *testing.T) {
	pct := 40.0
	mv := 40000.0
	group := rpc.PositionGroup{
		Underlying:             "ES",
		GroupMarketValueBase:   &mv,
		GroupMarketValuePctNLV: &pct,
		GroupMarketValue:       40000,
		Stock:                  &rpc.PositionView{Symbol: "ES", SecType: "FUT", Quantity: 1, Mark: 5000, Multiplier: 50, Currency: "USD"},
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	if prop, ok := riskReductionProposal(policy, status, group, rpc.TradeProposalSourceFingerprints{}, time.Now()); ok {
		t.Fatalf("unsupported security emitted proposal: %+v", prop)
	}
}

func TestTrailingStopStockProposalUsesBidAskAndBlocksWideSpread(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	bid, ask, spreadPct := 100.0, 101.0, 3.0
	longRow := rpc.PositionView{
		Symbol:     "MSFT",
		SecType:    "STK",
		Quantity:   10,
		Bid:        &bid,
		Ask:        &ask,
		Mark:       106,
		SpreadPct:  &spreadPct,
		Multiplier: 1,
		Currency:   "USD",
	}
	prop, ok := trailingStopStockProposal(policy, status, longRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("stock trail proposal missing")
	}
	if prop.OrderType != rpc.OrderTypeTRAIL || prop.Trail == nil || prop.Trail.TrailingAmount == nil || *prop.Trail.TrailingAmount != 8 {
		t.Fatalf("trail = %+v orderType=%q, want bid-derived 8.00 TRAIL amount", prop.Trail, prop.OrderType)
	}
	if prop.Trail.TrailingPercent != nil || prop.Trail.OffsetType != rpc.OrderTrailOffsetAmount {
		t.Fatalf("trail = %+v, want amount offset without broker percent", prop.Trail)
	}
	if prop.Trail.InitialStopPrice != 92 {
		t.Fatalf("initial stop = %.2f, want bid-based 92.00", prop.Trail.InitialStopPrice)
	}
	if !hasBlocker(prop.Blockers, "wide_spread") {
		t.Fatalf("blockers = %+v, want wide_spread", prop.Blockers)
	}

	shortRow := longRow
	shortRow.Quantity = -5
	shortRow.SpreadPct = nil
	prop, ok = trailingStopStockProposal(policy, status, shortRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("short stock trail proposal missing")
	}
	if prop.Action != rpc.OrderActionBuy || prop.Trail.InitialStopPrice != 109.08 {
		t.Fatalf("short stock action/stop = %s/%.2f, want BUY ask-based 109.08", prop.Action, prop.Trail.InitialStopPrice)
	}

	offHoursRow := longRow
	offHoursRow.Bid = nil
	offHoursRow.Ask = nil
	offHoursRow.SpreadPct = nil
	prop, ok = trailingStopStockProposal(policy, status, offHoursRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("off-hours stock trail proposal missing")
	}
	if hasBlocker(prop.Blockers, "missing_reference_price") {
		t.Fatalf("blockers = %+v, did not want missing_reference_price for percent broker trail", prop.Blockers)
	}
	if prop.Trail == nil || prop.Trail.TrailingAmount == nil || *prop.Trail.TrailingAmount != 8.48 || prop.Trail.InitialStopPrice != 97.52 {
		t.Fatalf("off-hours trail = %+v, want amount trail seeded from portfolio mark", prop.Trail)
	}
}

func TestTrailingStopStockProposalRoutesXetraPositionForPreview(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	bid, ask := 156.0, 156.04
	row := rpc.PositionView{
		Symbol:       "SAP",
		SecType:      "STOCK",
		ConID:        14204,
		Exchange:     "IBIS",
		Currency:     "EUR",
		LocalSymbol:  "SAP",
		TradingClass: "XETRA",
		Quantity:     1,
		Multiplier:   1,
		Mark:         156.02,
		Bid:          &bid,
		Ask:          &ask,
	}
	prop, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("stock trail proposal missing")
	}
	if prop.Contract.Market != "de" || prop.Contract.Exchange != "SMART" || prop.Contract.PrimaryExch != "IBIS" {
		t.Fatalf("proposal contract route = market %q exchange %q primary %q, want de/SMART/IBIS", prop.Contract.Market, prop.Contract.Exchange, prop.Contract.PrimaryExch)
	}
	if prop.Trail == nil || prop.Trail.TrailingAmount == nil || *prop.Trail.TrailingAmount != 12.48 {
		t.Fatalf("trailing amount = %+v, want 12.48", prop.Trail)
	}
	if prop.Trail.TrailingPercent != nil {
		t.Fatalf("trailing percent = %+v, want no broker percent", prop.Trail)
	}
	if prop.Trail.InitialStopPrice != 143.52 {
		t.Fatalf("initial stop = %.4f, want cent-rounded 143.52", prop.Trail.InitialStopPrice)
	}
}

func TestProposalEnginePreservesSnapshotOnTransientRefreshFailure(t *testing.T) {
	t.Parallel()
	oldAt := time.Date(2026, 6, 9, 14, 0, 0, 0, time.UTC)
	now := oldAt.Add(10 * time.Minute)
	policyFP := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	prop := rpc.TradeProposal{
		Key:               "trailing_stop:abc",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFP,
		CreatedAt:         oldAt,
	}
	srv := &Server{now: func() time.Time { return now }}
	engine := &proposalEngine{
		server: srv,
		store: &proposalStore{
			currentPath: filepath.Join(t.TempDir(), "trade-proposals-current.json"),
			eventsPath:  filepath.Join(t.TempDir(), "trade-proposals.jsonl"),
		},
		now: func() time.Time { return now },
		scope: func() brokerStateScope {
			return brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
		},
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              oldAt,
			Revision:          "sha256:rev",
			AccountID:         "DU1234567",
			AccountMode:       rpc.AccountModePaper,
			PolicyID:          "protection-mvp",
			PolicyVersion:     1,
			PolicyFingerprint: policyFP,
			PolicyStatus: rpc.ProtectionPolicyStatus{
				Status:      rpc.ProtectionPolicyStatusDefault,
				PolicyID:    "protection-mvp",
				Fingerprint: policyFP,
			},
			Proposals: []rpc.TradeProposal{prop},
			Counts:    proposalCounts([]rpc.TradeProposal{prop}),
		},
		ignored: map[string]struct{}{},
	}

	snap, ok := engine.preserveSnapshotOnRefreshFailure(
		brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper},
		rpc.AutoTradeStatus{Trading: rpc.TradingStatus{Mode: "paper", CanPreview: true}},
		rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault, PolicyID: "protection-mvp", PolicyVersion: 1, Fingerprint: policyFP},
		[]rpc.TradingBlocker{{Code: "account_unavailable", Message: "ibkr connection unavailable"}},
		false,
	)
	if !ok {
		t.Fatal("preserveSnapshotOnRefreshFailure ok=false, want preserved snapshot")
	}
	if !snap.AsOf.Equal(oldAt) {
		t.Fatalf("preserved AsOf = %s, want last healthy %s", snap.AsOf, oldAt)
	}
	if len(snap.Proposals) != 1 || snap.Proposals[0].Key != prop.Key {
		t.Fatalf("preserved proposals = %+v, want prior proposal", snap.Proposals)
	}
	if !hasBlocker(snap.Blockers, "account_unavailable") {
		t.Fatalf("preserved blockers = %+v, want account_unavailable", snap.Blockers)
	}
	current := engine.Snapshot(false)
	if len(current.Proposals) != 1 || !hasBlocker(current.Blockers, "account_unavailable") {
		t.Fatalf("installed snapshot = %+v, want proposal plus transient blocker", current)
	}
}

func TestTrailingStopOptionProposalRequiresOptInAndBlocksUnsafeShapes(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	if policy.Buckets.TrailingStop.Options.Enabled {
		t.Fatal("option trailing stop default enabled, want disabled")
	}
	policy.Buckets.TrailingStop.Options.Enabled = true
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	bid, ask, spreadPct := 2.00, 2.10, 4.9
	open := &rpc.MarketSession{Market: "us-options", IsOpen: true}
	row := rpc.PositionView{
		Symbol:         "SPY",
		SecType:        "OPT",
		Quantity:       1,
		Multiplier:     100,
		Currency:       "USD",
		Expiry:         "20260619",
		Right:          "C",
		Strike:         520,
		OptionBid:      &bid,
		OptionAsk:      &ask,
		SpreadPct:      &spreadPct,
		SessionContext: open,
	}
	prop, ok := trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), false, 0)
	if !ok {
		t.Fatal("option trail proposal missing")
	}
	if prop.State == rpc.TradeProposalStateBlocked {
		t.Fatalf("long option proposal blocked: %+v", prop.Blockers)
	}
	if prop.OrderType != rpc.OrderTypeTRAILLIMIT || prop.Trail == nil || prop.Trail.LimitOffset == nil || *prop.Trail.LimitOffset != 0.05 || prop.Trail.TrailingAmount == nil || *prop.Trail.TrailingAmount != 0.6 {
		t.Fatalf("option trail = %+v orderType=%q, want 0.60 TRAIL LIMIT amount offset 0.05", prop.Trail, prop.OrderType)
	}
	if prop.Trail.TrailingPercent != nil {
		t.Fatalf("option trail = %+v, want amount offset without broker percent", prop.Trail)
	}
	if math.Abs(prop.Trail.InitialStopPrice-1.40) > 0.0001 {
		t.Fatalf("long option stop = %.4f, want bid-premium 1.4000", prop.Trail.InitialStopPrice)
	}

	row.Quantity = -1
	prop, ok = trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), false, 0)
	if !ok {
		t.Fatal("short option trail proposal missing")
	}
	if !hasBlocker(prop.Blockers, "short_option_trail_disabled") {
		t.Fatalf("short-option blockers = %+v, want short_option_trail_disabled", prop.Blockers)
	}
	if math.Abs(prop.Trail.InitialStopPrice-2.73) > 0.0001 {
		t.Fatalf("short option stop = %.4f, want ask-premium 2.7300", prop.Trail.InitialStopPrice)
	}

	row.Quantity = 1
	row.SessionContext = nil
	prop, ok = trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("nil-session option trail proposal missing")
	}
	if !hasBlocker(prop.Blockers, "option_rth_closed") || !hasBlocker(prop.Blockers, "multi_leg_option_trail_unsupported") {
		t.Fatalf("blockers = %+v, want RTH and multi-leg blockers", prop.Blockers)
	}
}

func TestProposalPreviewParamsCarryOrderSource(t *testing.T) {
	prop := rpc.TradeProposal{
		Action:   rpc.OrderActionSell,
		Contract: rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
	}
	params := proposalOrderPreviewParams(prop, 3, 5000)
	if params.Source != proposalOrderSource {
		t.Fatalf("proposal preview source=%q, want %q", params.Source, proposalOrderSource)
	}
}

func TestProposalPreviewSafetyBlocksOpenEffect(t *testing.T) {
	prop := rpc.TradeProposal{
		Action:         rpc.OrderActionSell,
		MaxQuantity:    1,
		PositionEffect: rpc.OrderPositionEffectClose,
		SecType:        "STK",
	}
	preview := &rpc.OrderPreviewResult{
		Mode: "paper",
		Draft: rpc.OrderDraft{
			Action:    rpc.OrderActionSell,
			Contract:  rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:  1,
			OrderType: rpc.OrderTypeLMT,
			TIF:       rpc.OrderTIFDay,
			Source:    proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectOpen},
	}
	blockers := proposalPreviewSafetyBlockers(prop, preview)
	if !hasBlocker(blockers, "preview_effect_not_close_reduce") {
		t.Fatalf("blockers = %+v, want preview_effect_not_close_reduce", blockers)
	}
}

func TestProposalPreviewSafetyDoesNotOwnExecutionRoute(t *testing.T) {
	prop := rpc.TradeProposal{
		Action:         rpc.OrderActionBuy,
		MaxQuantity:    1,
		PositionEffect: rpc.OrderPositionEffectClose,
		SecType:        "OPT",
	}
	preview := &rpc.OrderPreviewResult{
		Mode: "live",
		Draft: rpc.OrderDraft{
			Action:     rpc.OrderActionBuy,
			Contract:   rpc.ContractParams{Symbol: "SPY", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20260619", Right: "C", Strike: 520, Multiplier: 100},
			Quantity:   1,
			OrderType:  rpc.OrderTypeLMT,
			TIF:        rpc.OrderTIFDay,
			OutsideRTH: false,
			Source:     proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectClose},
	}
	blockers := proposalPreviewSafetyBlockers(prop, preview)
	if hasBlocker(blockers, "proposal_not_paper") {
		t.Fatalf("blockers = %+v, proposal safety should not own paper/live routing", blockers)
	}
	if len(blockers) != 0 {
		t.Fatalf("blockers = %+v, want route-neutral proposal safety", blockers)
	}
}

func TestProposalPreviewSafetyBlocksTrailDrift(t *testing.T) {
	t.Parallel()
	propPct, previewPct, limitOffset := 8.0, 5.0, 0.05
	prop := rpc.TradeProposal{
		Action:         rpc.OrderActionSell,
		MaxQuantity:    1,
		PositionEffect: rpc.OrderPositionEffectClose,
		SecType:        "STK",
		OrderType:      rpc.OrderTypeTRAIL,
		Trail:          &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &propPct, InitialStopPrice: 92},
	}
	preview := &rpc.OrderPreviewResult{
		Mode: "paper",
		Draft: rpc.OrderDraft{
			Action:    rpc.OrderActionSell,
			Contract:  rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:  1,
			OrderType: rpc.OrderTypeTRAIL,
			TIF:       rpc.OrderTIFDay,
			Trail:     &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &previewPct, LimitOffset: &limitOffset, InitialStopPrice: 95},
			Source:    proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectClose},
	}
	blockers := proposalPreviewSafetyBlockers(prop, preview)
	if !hasBlocker(blockers, "trail_percent_drift") || !hasBlocker(blockers, "trail_limit_offset_drift") || !hasBlocker(blockers, "trail_initial_stop_drift") {
		t.Fatalf("blockers = %+v, want trail percent, limit offset, and initial stop drift", blockers)
	}
}

func TestProposalOrderPreviewParamsPreserveTrailStopPrice(t *testing.T) {
	t.Parallel()
	amount := 3.84
	prop := rpc.TradeProposal{
		Action:    rpc.OrderActionSell,
		Quantity:  1,
		OrderType: rpc.OrderTypeTRAIL,
		Contract:  rpc.ContractParams{Symbol: "MBG", SecType: "STK", Exchange: "SMART", Currency: "EUR"},
		Trail:     &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetAmount, TrailingAmount: &amount, InitialStopPrice: 44.04},
	}
	params := proposalOrderPreviewParams(prop, 1, 5000)
	if params.Trail == nil || params.Trail.InitialStopPrice != 44.04 {
		t.Fatalf("preview params trail = %+v, want initial stop preserved", params.Trail)
	}
	if params.Trail.TrailingAmount == nil || *params.Trail.TrailingAmount != amount {
		t.Fatalf("preview params trail = %+v, want trailing amount preserved", params.Trail)
	}
}

func TestProposalOrderPreviewParamsCarriesProposalTIF(t *testing.T) {
	t.Parallel()
	prop := rpc.TradeProposal{Action: rpc.OrderActionSell, Quantity: 1, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC}
	if params := proposalOrderPreviewParams(prop, 1, 5000); params.TIF != rpc.OrderTIFGTC {
		t.Fatalf("params TIF = %q, want GTC", params.TIF)
	}
	// Proposals persisted before the TIF field existed mean DAY.
	prop.TIF = ""
	if params := proposalOrderPreviewParams(prop, 1, 5000); params.TIF != rpc.OrderTIFDay {
		t.Fatalf("legacy params TIF = %q, want DAY", params.TIF)
	}
}

func TestProposalPreviewSafetyBlocksTIFDrift(t *testing.T) {
	t.Parallel()
	pct := 8.0
	mkTrail := func() *rpc.OrderTrailSpec {
		return &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &pct}
	}
	prop := rpc.TradeProposal{
		Action: rpc.OrderActionSell, MaxQuantity: 1, PositionEffect: rpc.OrderPositionEffectClose,
		SecType: "STK", OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC, Trail: mkTrail(),
	}
	preview := &rpc.OrderPreviewResult{
		Mode: "paper",
		Draft: rpc.OrderDraft{
			Action:    rpc.OrderActionSell,
			Contract:  rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:  1,
			OrderType: rpc.OrderTypeTRAIL,
			TIF:       rpc.OrderTIFDay,
			Trail:     mkTrail(),
			Source:    proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectClose},
	}
	if blockers := proposalPreviewSafetyBlockers(prop, preview); !hasBlocker(blockers, "tif_drift") {
		t.Fatalf("blockers = %+v, want tif_drift", blockers)
	}
	preview.Draft.TIF = rpc.OrderTIFGTC
	if blockers := proposalPreviewSafetyBlockers(prop, preview); len(blockers) != 0 {
		t.Fatalf("matched GTC blockers = %+v, want none", blockers)
	}
	preview.Draft.TIF = "IOC"
	if blockers := proposalPreviewSafetyBlockers(prop, preview); !hasBlocker(blockers, "unsupported_tif") {
		t.Fatalf("blockers = %+v, want unsupported_tif", blockers)
	}
}

func TestTrailingStopProposalTIFFromPolicy(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusActive, "test", "", now)
	bid, ask := 99.0, 100.0
	row := rpc.PositionView{Symbol: "MBG", SecType: "STK", ConID: 29622935, Exchange: "IBIS", Currency: "EUR", Quantity: 10, Mark: 99.5, MarketValue: 995, Multiplier: 1, Bid: &bid, Ask: &ask}

	day, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, now, true, 0.01)
	if !ok {
		t.Fatal("expected stock trailing proposal")
	}
	if day.TIF != rpc.OrderTIFDay {
		t.Fatalf("default policy proposal TIF = %q, want DAY", day.TIF)
	}
	if !hasDetailContaining(day.Details, "tif=DAY") || !hasDetailContaining(day.Details, "overnight gaps") {
		t.Fatalf("details = %+v, want DAY session-close caveat", day.Details)
	}

	policy.Buckets.TrailingStop.TIF = rpc.OrderTIFGTC
	gtc, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, now, true, 0.01)
	if !ok {
		t.Fatal("expected GTC stock trailing proposal")
	}
	if gtc.TIF != rpc.OrderTIFGTC {
		t.Fatalf("GTC policy proposal TIF = %q, want GTC", gtc.TIF)
	}
	if !hasDetailContaining(gtc.Details, "tif=GTC") || hasDetailContaining(gtc.Details, "overnight gaps") {
		t.Fatalf("details = %+v, want GTC persistence note without the DAY caveat", gtc.Details)
	}
}

func hasDetailContaining(details []string, substr string) bool {
	for _, d := range details {
		if strings.Contains(d, substr) {
			return true
		}
	}
	return false
}

func TestTrailingStopFastPathPreviewUsesCurrentSnapshot(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	policyFingerprint := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	trailPercent := 8.0
	prop := rpc.TradeProposal{
		Key:               "trailing_stop:sap",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionQuantity:  1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		Trail:             &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &trailPercent, InitialStopPrice: 92},
		TIF:               rpc.OrderTIFDay,
		Contract:          rpc.ContractParams{Symbol: "SAP", SecType: "STK", Exchange: "SMART", Currency: "EUR", Multiplier: 1},
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFingerprint,
		CreatedAt:         now,
	}
	srv.tradeProposals = &proposalEngine{
		server: srv,
		store: &proposalStore{
			currentPath: filepath.Join(t.TempDir(), "trade-proposals-current.json"),
			eventsPath:  filepath.Join(t.TempDir(), "trade-proposals.jsonl"),
		},
		now:     func() time.Time { return now },
		ignored: map[string]struct{}{},
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              now,
			Revision:          "sha256:rev",
			AccountID:         "DU1234567",
			AccountMode:       rpc.AccountModePaper,
			PolicyID:          "protection-mvp",
			PolicyVersion:     1,
			PolicyFingerprint: policyFingerprint,
			PolicyStatus: rpc.ProtectionPolicyStatus{
				Status:      rpc.ProtectionPolicyStatusDefault,
				PolicyID:    "protection-mvp",
				Fingerprint: policyFingerprint,
			},
			AutoTrade: rpc.AutoTradeStatus{Trading: srv.tradingStatus(srv.endpoint), ProposalsEnabled: true, FastPathEnabled: true},
			Trading:   srv.tradingStatus(srv.endpoint),
			Proposals: []rpc.TradeProposal{prop},
		},
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{
		Key:       prop.Key,
		Revision:  prop.Revision,
		Quantity:  1,
		TimeoutMs: 20,
		FastPath:  true,
	})
	if err != nil {
		t.Fatalf("fast preview err = %v", err)
	}
	if !res.Accepted || !res.SubmitEligible || res.Preview == nil {
		t.Fatalf("fast preview = %+v, want accepted submit-eligible preview", res)
	}
	if res.Proposal.Key != prop.Key || res.Preview.Draft.OrderType != rpc.OrderTypeTRAIL {
		t.Fatalf("preview proposal/draft = %s/%s, want %s/TRAIL", res.Proposal.Key, res.Preview.Draft.OrderType, prop.Key)
	}
}

// A GTC trailing-stop proposal must clear the whole preview chain — params,
// daemon preview validator, WhatIf, and the proposal-vs-preview drift gate —
// with zero blockers; each unit gate passing individually does not prove a
// missed DAY assumption isn't hiding between them.
func TestTrailingStopFastPathPreviewGTCEndToEnd(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	now := time.Date(2026, 6, 10, 13, 0, 0, 0, time.UTC)
	policyFingerprint := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	trailPercent := 8.0
	prop := rpc.TradeProposal{
		Key:               "trailing_stop:sap",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionQuantity:  1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		Trail:             &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &trailPercent, InitialStopPrice: 92},
		TIF:               rpc.OrderTIFGTC,
		Contract:          rpc.ContractParams{Symbol: "SAP", SecType: "STK", Exchange: "SMART", Currency: "EUR", Multiplier: 1},
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFingerprint,
		CreatedAt:         now,
	}
	srv.tradeProposals = &proposalEngine{
		server:  srv,
		store:   testProposalStore(t),
		now:     func() time.Time { return now },
		ignored: map[string]struct{}{},
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              now,
			Revision:          "sha256:rev",
			AccountID:         "DU1234567",
			AccountMode:       rpc.AccountModePaper,
			PolicyID:          "protection-mvp",
			PolicyVersion:     1,
			PolicyFingerprint: policyFingerprint,
			PolicyStatus:      rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault, PolicyID: "protection-mvp", Fingerprint: policyFingerprint},
			AutoTrade:         rpc.AutoTradeStatus{Trading: srv.tradingStatus(srv.endpoint), ProposalsEnabled: true, FastPathEnabled: true},
			Trading:           srv.tradingStatus(srv.endpoint),
			Proposals:         []rpc.TradeProposal{prop},
		},
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{
		Key:       prop.Key,
		Revision:  prop.Revision,
		Quantity:  1,
		TimeoutMs: 20,
		FastPath:  true,
	})
	if err != nil {
		t.Fatalf("GTC fast preview err = %v", err)
	}
	if !res.Accepted || !res.SubmitEligible || res.Preview == nil || len(res.Blockers) != 0 {
		t.Fatalf("GTC fast preview = %+v, want accepted submit-eligible with no blockers", res)
	}
	if res.Preview.Draft.TIF != rpc.OrderTIFGTC {
		t.Fatalf("GTC preview draft TIF = %q, want GTC end-to-end", res.Preview.Draft.TIF)
	}
}

func TestTrailingStopPreviewBlocksWhenWhatIfNotSubmitEligible(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusUnavailable, Available: false, Message: "timeout waiting for broker WhatIf response"}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	policyFingerprint := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	trailAmount := 8.0
	prop := rpc.TradeProposal{
		Key:               "trailing_stop:sap",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionQuantity:  1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		Trail:             &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetAmount, TrailingAmount: &trailAmount, InitialStopPrice: 92},
		TIF:               rpc.OrderTIFDay,
		Contract:          rpc.ContractParams{Symbol: "SAP", SecType: "STK", Exchange: "SMART", Currency: "EUR", Multiplier: 1},
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFingerprint,
		CreatedAt:         now,
	}
	srv.tradeProposals = &proposalEngine{
		server: srv,
		store:  testProposalStore(t),
		now:    func() time.Time { return now },
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              now,
			Revision:          "sha256:rev",
			AccountID:         "DU1234567",
			AccountMode:       rpc.AccountModePaper,
			PolicyID:          "protection-mvp",
			PolicyVersion:     1,
			PolicyFingerprint: policyFingerprint,
			PolicyStatus:      rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault, PolicyID: "protection-mvp", Fingerprint: policyFingerprint},
			AutoTrade:         rpc.AutoTradeStatus{Trading: srv.tradingStatus(srv.endpoint), ProposalsEnabled: true, FastPathEnabled: true},
			Trading:           srv.tradingStatus(srv.endpoint),
			Proposals:         []rpc.TradeProposal{prop},
		},
		ignored: map[string]struct{}{},
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: prop.Revision, Quantity: 1, TimeoutMs: 20, FastPath: true})
	if err != nil {
		t.Fatalf("fast preview err = %v", err)
	}
	if res.Accepted || res.SubmitEligible || !hasBlocker(res.Blockers, "preview_not_submit_eligible") {
		t.Fatalf("fast preview = %+v, want blocked not-submit-eligible result", res)
	}
	if res.Preview == nil || res.Preview.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable {
		t.Fatalf("preview = %+v, want unavailable WhatIf context", res.Preview)
	}
}

func preservedTrailingStopProposal(now time.Time) rpc.TradeProposal {
	trailPercent := 8.0
	policyFingerprint := fingerprintProtectionPolicy(defaultProtectionPolicy())
	return rpc.TradeProposal{
		Key:               "trailing_stop:sap",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionQuantity:  1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		Trail:             &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &trailPercent, InitialStopPrice: 92},
		TIF:               rpc.OrderTIFDay,
		Contract:          rpc.ContractParams{Symbol: "SAP", SecType: "STK", Exchange: "SMART", Currency: "EUR", Multiplier: 1},
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFingerprint,
		CreatedAt:         now,
	}
}

func preservedProposalSnapshot(now time.Time, prop rpc.TradeProposal, blockers []rpc.TradingBlocker) rpc.TradeProposalSnapshot {
	return rpc.TradeProposalSnapshot{
		Kind:              rpc.TradeProposalSnapshotKind,
		SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:              now,
		Revision:          prop.Revision,
		AccountID:         "DU1234567",
		AccountMode:       rpc.AccountModePaper,
		PolicyID:          prop.PolicyID,
		PolicyVersion:     prop.PolicyVersion,
		PolicyFingerprint: prop.PolicyFingerprint,
		PolicyStatus: rpc.ProtectionPolicyStatus{
			Status:        rpc.ProtectionPolicyStatusDefault,
			PolicyID:      prop.PolicyID,
			PolicyVersion: prop.PolicyVersion,
			Fingerprint:   prop.PolicyFingerprint,
		},
		Proposals: []rpc.TradeProposal{prop},
		Counts:    proposalCounts([]rpc.TradeProposal{prop}),
		Blockers:  append([]rpc.TradingBlocker(nil), blockers...),
	}
}

func testProposalStore(t *testing.T) *proposalStore {
	t.Helper()
	return &proposalStore{
		currentPath: filepath.Join(t.TempDir(), "trade-proposals-current.json"),
		eventsPath:  filepath.Join(t.TempDir(), "trade-proposals.jsonl"),
	}
}

func TestTrailingStopFastPathPreviewBlocksPreservedSnapshotBlockers(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("preview must not fetch quotes while preserved snapshot has blockers")
		return rpc.OrderQuoteSnapshot{}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	prop := preservedTrailingStopProposal(now)
	srv.tradeProposals = &proposalEngine{
		server:  srv,
		store:   testProposalStore(t),
		now:     func() time.Time { return now },
		ignored: map[string]struct{}{},
		snapshot: preservedProposalSnapshot(now, prop, []rpc.TradingBlocker{
			{Code: "account_unavailable", Message: "account snapshot failed"},
		}),
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{
		Key:      prop.Key,
		Revision: prop.Revision,
		Quantity: 1,
		FastPath: true,
	})
	if err != nil {
		t.Fatalf("preview err = %v", err)
	}
	if res.Accepted || res.Preview != nil || !hasBlocker(res.Blockers, "account_unavailable") {
		t.Fatalf("preview = %+v, want blocked by preserved snapshot blocker", res)
	}
}

func TestTrailingStopSubmitBlocksPreservedRefreshFailureBeforePreview(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("submit must not preview while refresh preserved stale snapshot with blockers")
		return rpc.OrderQuoteSnapshot{}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	prop := preservedTrailingStopProposal(now)
	srv.tradeProposals = &proposalEngine{
		server:   srv,
		store:    testProposalStore(t),
		now:      func() time.Time { return now },
		ignored:  map[string]struct{}{},
		snapshot: preservedProposalSnapshot(now.Add(-time.Minute), prop, nil),
	}

	res, err := srv.tradeProposals.Submit(context.Background(), rpc.TradeProposalSubmitParams{
		Key:      prop.Key,
		Revision: prop.Revision,
		Quantity: 1,
		FastPath: true,
	})
	if err != nil {
		t.Fatalf("submit err = %v", err)
	}
	if res.Accepted || res.Preview != nil || !hasBlocker(res.Blockers, "account_unavailable") {
		t.Fatalf("submit = %+v, want blocked by preserved refresh failure", res)
	}
}

func TestProposalRevisionIgnoresRegimeLifecycleChurn(t *testing.T) {
	policy := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	sources := rpc.TradeProposalSourceFingerprints{
		Account:   &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
		Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
		Regime:    &rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime-a"},
	}
	proposals := []rpc.TradeProposal{{Key: "theta_hygiene:abc", Quantity: 1, PositionEffect: rpc.OrderPositionEffectClose}}
	scope := brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}
	a := proposalRevision(policy, sources, scope, proposals)
	sources.Regime = &rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime-b"}
	b := proposalRevision(policy, sources, scope, proposals)
	if a != b {
		t.Fatalf("revision changed on regime-only churn: %s != %s", a, b)
	}
	sources.Positions = &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions-b"}
	c := proposalRevision(policy, sources, scope, proposals)
	if c == a {
		t.Fatalf("revision did not change on positions fingerprint change: %s", c)
	}
}

func TestProposalRevisionIgnoresMarketEventSourceChurn(t *testing.T) {
	policy := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	sources := rpc.TradeProposalSourceFingerprints{
		Account:      &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
		Positions:    &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
		MarketEvents: &rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-a"},
	}
	proposals := []rpc.TradeProposal{{Key: "risk_reduction:abc", Quantity: 1, PositionEffect: rpc.OrderPositionEffectReduce}}
	scope := brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}
	a := proposalRevision(policy, sources, scope, proposals)
	sources.MarketEvents = &rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-b"}
	if b := proposalRevision(policy, sources, scope, proposals); b != a {
		t.Fatalf("revision changed on market-event-only churn: %s != %s", a, b)
	}
}

func TestMarketEventHardBlockerBlocksProposal(t *testing.T) {
	prop := rpc.TradeProposal{
		Symbol:         "CRWV",
		Action:         rpc.OrderActionSell,
		PositionEffect: rpc.OrderPositionEffectReduce,
	}
	events := &rpc.MarketEventsResult{BySymbol: map[string][]rpc.MarketEventFlag{
		"CRWV": {{
			ID:       rpc.MarketEventHaltRegulatoryOrNews,
			Symbol:   "CRWV",
			Label:    "Halt",
			Status:   rpc.MarketEventStatusActive,
			Severity: rpc.MarketEventSeverityBlock,
			Role:     rpc.MarketEventRoleHardBlocker,
			Source:   "Nasdaq trade halt RSS",
		}},
	}}
	applyMarketEventFlagsToProposal(&prop, events)
	if prop.State != rpc.TradeProposalStateBlocked {
		t.Fatalf("state=%q, want blocked", prop.State)
	}
	if !hasBlocker(prop.Blockers, "market_event_"+rpc.MarketEventHaltRegulatoryOrNews) {
		t.Fatalf("blockers=%+v, want market-event blocker", prop.Blockers)
	}
}

func TestBorrowMarketFlagOnlyAppliesToShortBuyToCover(t *testing.T) {
	for _, flag := range []rpc.MarketEventFlag{
		{
			ID:       rpc.MarketEventBorrowInventoryTight,
			Symbol:   "CRWV",
			Label:    "Borrow tight",
			Status:   rpc.MarketEventStatusActive,
			Severity: rpc.MarketEventSeverityWatch,
			Role:     rpc.MarketEventRoleProposalModifier,
		},
		{
			ID:       rpc.MarketEventBorrowFeeExtreme,
			Symbol:   "CRWV",
			Label:    "Fee extreme",
			Status:   rpc.MarketEventStatusActive,
			Severity: rpc.MarketEventSeverityAct,
			Role:     rpc.MarketEventRoleProposalModifier,
		},
	} {
		events := &rpc.MarketEventsResult{BySymbol: map[string][]rpc.MarketEventFlag{"CRWV": {flag}}}
		longSell := rpc.TradeProposal{
			Symbol:           "CRWV",
			Action:           rpc.OrderActionSell,
			PositionQuantity: 100,
			PositionEffect:   rpc.OrderPositionEffectReduce,
		}
		if got := proposalMarketEventFlags(longSell, events); len(got) != 0 {
			t.Fatalf("long sell %s flags=%+v, want none", flag.ID, got)
		}
		shortCover := rpc.TradeProposal{
			Symbol:           "CRWV",
			Action:           rpc.OrderActionBuy,
			PositionQuantity: -100,
			PositionEffect:   rpc.OrderPositionEffectReduce,
		}
		if got := proposalMarketEventFlags(shortCover, events); len(got) != 1 || got[0].ID != flag.ID {
			t.Fatalf("short cover %s flags=%+v, want borrow flag", flag.ID, got)
		}
	}
}

func TestProposalOutcomeMarksAreIdempotentPerProposalDate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trade-proposal-outcomes.jsonl")
	store := newProposalOutcomeStore(path)
	mark := proposalOutcomeMark{
		At:                time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC),
		MarkDate:          "2026-06-06",
		State:             proposalOutcomeStateMarked,
		ProposalKey:       "theta_hygiene:abc",
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:test"},
		MarkPrice:         1.23,
	}
	if err := store.AppendMark(mark); err != nil {
		t.Fatalf("append first outcome: %v", err)
	}
	if err := store.AppendMark(mark); err != nil {
		t.Fatalf("append duplicate outcome: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read outcomes: %v", err)
	}
	if got := strings.Count(string(raw), "\n"); got != 1 {
		t.Fatalf("outcome rows=%d, want 1; file=%s", got, raw)
	}
}

func TestProposalDailyMarkCarriesPolicyIdentity(t *testing.T) {
	price := 1.25
	prop := rpc.TradeProposal{
		Key:               "theta_hygiene:abc",
		Revision:          "sha256:rev",
		Bucket:            rpc.TradeProposalBucketThetaHygiene,
		Symbol:            "AAPL",
		SecType:           "OPT",
		Action:            rpc.OrderActionSell,
		Quantity:          2,
		LimitPrice:        &price,
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"},
		SourceFingerprints: rpc.TradeProposalSourceFingerprints{
			Account:   &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
			Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
		},
	}
	mark := proposalOutcomeMarked(prop, time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC))
	if mark.State != proposalOutcomeStateMarked || mark.MarkDate != "2026-06-06" {
		t.Fatalf("daily mark state/date = %q/%q", mark.State, mark.MarkDate)
	}
	if mark.PolicyID != prop.PolicyID || mark.PolicyFingerprint.Key != prop.PolicyFingerprint.Key {
		t.Fatalf("daily mark missing policy identity: %+v", mark)
	}
	if mark.MarkPrice != price || mark.BaselinePrice != price {
		t.Fatalf("daily mark price/baseline = %.2f/%.2f, want %.2f", mark.MarkPrice, mark.BaselinePrice, price)
	}
}

func TestProposalFillOutcomeCarriesPolicyIdentity(t *testing.T) {
	submitted := proposalEvent{
		Type:              "submitted",
		Key:               "risk_reduction:def",
		Revision:          "sha256:rev",
		Bucket:            rpc.TradeProposalBucketRiskReduction,
		PolicyID:          "protection-mvp",
		PolicyVersion:     2,
		PolicyFingerprint: rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"},
		SourceFingerprints: rpc.TradeProposalSourceFingerprints{
			Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
		},
	}
	ev := orderJournalEvent{
		Source:         proposalOrderSource,
		OrderRef:       "ibkr-20260606-100000",
		PreviewTokenID: "ptok_123",
		ExecID:         "exec-1",
		Symbol:         "MSFT",
		SecType:        "STK",
		Action:         rpc.OrderActionSell,
		Quantity:       5,
		Filled:         5,
		LimitPrice:     100,
		AvgFillPrice:   101,
		Multiplier:     1,
	}
	mark := proposalOutcomeFilledFromJournal(ev, submitted, time.Date(2026, 6, 6, 10, 1, 0, 0, time.UTC))
	if mark.ProposalKey != submitted.Key || mark.PolicyID != submitted.PolicyID || mark.PolicyVersion != submitted.PolicyVersion || mark.PolicyFingerprint.Key != submitted.PolicyFingerprint.Key {
		t.Fatalf("fill outcome missing submitted policy identity: %+v", mark)
	}
	if mark.ExecutionPnL != 5 {
		t.Fatalf("execution pnl=%.2f, want 5.00", mark.ExecutionPnL)
	}
}

// newProposalScopeTestServer builds the minimal Server a proposalEngine
// Refresh needs before it touches the gateway: resolved config (trading mode
// defaults to disabled, so tradingStatus never reaches the order journal),
// an embedded-default protection policy, and a discovery endpoint that
// currentBrokerStateScope falls back to while no connector is attached.
func newProposalScopeTestServer(t *testing.T, ep discover.Endpoint, now time.Time) *Server {
	t.Helper()
	pm := newProtectionPolicyManager("", false, time.Second, func() time.Time { return now })
	pm.reload()
	return &Server{
		cfg:                &config.Resolved{},
		protectionPolicies: pm,
		endpoint:           ep,
		now:                func() time.Time { return now },
	}
}

func newProposalScopeTestEngine(t *testing.T, srv *Server) *proposalEngine {
	t.Helper()
	return &proposalEngine{
		server:  srv,
		store:   testProposalStore(t),
		now:     srv.now,
		ignored: map[string]struct{}{},
	}
}

func scopedTestSnapshot(account, mode string, asOf time.Time) rpc.TradeProposalSnapshot {
	return rpc.TradeProposalSnapshot{
		Kind:          rpc.TradeProposalSnapshotKind,
		SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:          asOf,
		Revision:      "sha256:test",
		AccountID:     account,
		AccountMode:   mode,
		Proposals: []rpc.TradeProposal{{
			Key:            "theta_hygiene:abc",
			Revision:       "sha256:test",
			State:          rpc.TradeProposalStateGenerated,
			Bucket:         rpc.TradeProposalBucketThetaHygiene,
			Symbol:         "SAP",
			SecType:        "STK",
			Action:         rpc.OrderActionSell,
			Quantity:       1,
			MaxQuantity:    1,
			PositionEffect: rpc.OrderPositionEffectClose,
		}},
	}
}

func TestBrokerScopeConcreteRejectsAggregateIdentities(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		scope brokerStateScope
		want  bool
	}{
		{"paper account", brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, true},
		{"live account", brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}, true},
		{"aggregate All", brokerStateScope{Account: "All", Mode: rpc.AccountModeLive}, false},
		{"aggregate All padded", brokerStateScope{Account: " All ", Mode: rpc.AccountModeLive}, false},
		{"empty account", brokerStateScope{Account: "", Mode: rpc.AccountModeLive}, false},
		{"multi-account list", brokerStateScope{Account: "DU7654321,U1234567", Mode: rpc.AccountModeLive}, false},
		{"unknown mode", brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeUnknown}, false},
		{"empty mode", brokerStateScope{Account: "U1234567", Mode: ""}, false},
	}
	for _, tc := range cases {
		if got := brokerScopeConcrete(tc.scope); got != tc.want {
			t.Errorf("%s: brokerScopeConcrete(%+v) = %v, want %v", tc.name, tc.scope, got, tc.want)
		}
	}
}

func TestProposalRefreshRejectsUnscopedAccountIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	// Aggregate "All" account on a live port: the exact identity the
	// leaked snapshot was persisted under. The nil connector means a
	// pass-through gate would fail with account_unavailable instead —
	// asserting on the blocker code proves the scope gate runs first.
	srv := newProposalScopeTestServer(t, discover.Endpoint{Host: "127.0.0.1", Port: 7496, Account: "All"}, now)
	e := newProposalScopeTestEngine(t, srv)

	snap, err := e.Refresh(context.Background(), false)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(snap.Proposals) != 0 {
		t.Fatalf("proposals = %+v, want none", snap.Proposals)
	}
	if !hasBlocker(snap.Blockers, "account_identity_unscoped") {
		t.Fatalf("blockers = %+v, want account_identity_unscoped", snap.Blockers)
	}
	if hasBlocker(snap.Blockers, "account_unavailable") {
		t.Fatalf("blockers = %+v, scope gate must run before the account summary", snap.Blockers)
	}
	if snap.AccountID != "" || snap.AccountMode != "" {
		t.Fatalf("unscoped shell stamped with identity %q/%q, want empty", snap.AccountID, snap.AccountMode)
	}
}

func TestProposalSnapshotServeRefusesScopeMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	e.snapshot = scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive} }

	got := e.Snapshot(true)
	if len(got.Proposals) != 0 {
		t.Fatalf("served %d paper proposals into a live session", len(got.Proposals))
	}
	if !hasBlocker(got.Blockers, "proposal_scope_mismatch") {
		t.Fatalf("blockers = %+v, want proposal_scope_mismatch", got.Blockers)
	}
	if got.AccountID != "U1234567" || got.AccountMode != rpc.AccountModeLive {
		t.Fatalf("refusal shell identity %q/%q, want connected session", got.AccountID, got.AccountMode)
	}
	// Refusal must not mark the stored proposals as shown nor overwrite
	// the stored snapshot/persisted file with the shell.
	if raw, err := os.ReadFile(e.store.eventsPath); err == nil && strings.Contains(string(raw), `"shown"`) {
		t.Fatalf("refused serve appended shown events: %s", raw)
	}
	if _, err := os.Stat(e.store.currentPath); !os.IsNotExist(err) {
		t.Fatalf("refused serve persisted a snapshot: stat err=%v", err)
	}
	if e.snapshot.AccountID != "DU7654321" || len(e.snapshot.Proposals) != 1 {
		t.Fatalf("refused serve mutated stored snapshot: %+v", e.snapshot)
	}

	// Matching session serves the stored proposals (case-insensitively).
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "du7654321", Mode: rpc.AccountModePaper} }
	got = e.Snapshot(false)
	if len(got.Proposals) != 1 || len(got.Blockers) != 0 {
		t.Fatalf("matching scope refused: proposals=%d blockers=%+v", len(got.Proposals), got.Blockers)
	}
}

func TestProposalSnapshotServeRefusesWhenCurrentScopeUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	e.snapshot = scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)
	e.scope = func() brokerStateScope { return brokerStateScope{} }

	got := e.Snapshot(false)
	if len(got.Proposals) != 0 {
		t.Fatalf("served proposals while session identity is unknown: %+v", got.Proposals)
	}
	if !hasBlocker(got.Blockers, "account_identity_unscoped") {
		t.Fatalf("blockers = %+v, want account_identity_unscoped (not a fabricated mismatch)", got.Blockers)
	}
}

func TestProposalServeGuardPassesBlockerShells(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	shell := emptyProposalSnapshot(now)
	shell.Blockers = []rpc.TradingBlocker{{Code: "proposals_disabled", Message: "manual protection proposals are disabled by config"}}
	e.snapshot = shell
	e.scope = func() brokerStateScope { return brokerStateScope{} }

	got := e.Snapshot(false)
	if !hasBlocker(got.Blockers, "proposals_disabled") {
		t.Fatalf("blockers = %+v, want session-independent shell served as-is", got.Blockers)
	}
}

func TestProposalInstallScopedFailsClosedOnScopeChange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	// Session switched between refresh-start (paper scope the data was
	// fetched under) and install: the generated snapshot must never be
	// installed or persisted.
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive} }
	snap := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)

	got := e.installScoped(snap, brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, false)
	if len(got.Proposals) != 0 || !hasBlocker(got.Blockers, "proposal_scope_mismatch") {
		t.Fatalf("installScoped result = %+v, want proposal_scope_mismatch shell", got)
	}
	// The wrong-scope generated snapshot must never reach disk. Shells
	// serve in-memory only (see replaceSnapshot), so the fail-closed
	// install writes nothing and a fresh store stays empty.
	if raw, err := os.ReadFile(e.store.currentPath); err == nil {
		if strings.Contains(string(raw), "theta_hygiene:abc") {
			t.Fatalf("persisted snapshot carries stale-scope proposals: %s", raw)
		}
		t.Fatalf("fail-closed install must not persist anything, got: %s", raw)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read persisted snapshot: %v", err)
	}

	// Stable scope installs the generated snapshot unchanged — and that
	// one IS persisted for warm-start adoption.
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper} }
	got = e.installScoped(snap, brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, false)
	if len(got.Proposals) != 1 || len(got.Blockers) != 0 {
		t.Fatalf("stable scope install = %+v, want generated snapshot", got)
	}
	raw, err := os.ReadFile(e.store.currentPath)
	if err != nil {
		t.Fatalf("stable-scope install should persist the generated snapshot: %v", err)
	}
	if !strings.Contains(string(raw), "theta_hygiene:abc") {
		t.Fatalf("persisted snapshot missing the generated proposal: %s", raw)
	}
}

func TestInstallProposalEngineFailsClosedOnLegacySnapshot(t *testing.T) {
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	writeCurrent := func(t *testing.T, body string) *Server {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("XDG_STATE_HOME", dir)
		path := filepath.Join(dir, "ibkr", "trade-proposals-current.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
		srv.installProposalEngine()
		return srv
	}

	// The exact unscoped shape from the originating incident: schema v1,
	// account_id "All", no account_mode.
	legacy := `{"kind":"ibkr.trade_proposal_snapshot","schema_version":"trade-proposal-snapshot-v1","as_of":"2026-06-10T12:54:00Z","revision":"sha256:legacy","account_id":"All","policy_id":"protection-mvp","policy_status":{"status":"default"},"auto_trade":{"trading":{},"proposals_enabled":true,"enabled":false,"auto_submit":false,"fast_path_enabled":true,"hot_reload":true,"blocked":false,"policy":{"status":"default"}},"trading":{},"proposals":[{"key":"theta_hygiene:abc","revision":"sha256:legacy","state":"generated","bucket":"theta_hygiene","rank":1,"symbol":"SAP","sec_type":"STK","action":"SELL","quantity":1,"max_quantity":1,"position_quantity":1,"position_effect":"close","order_type":"LMT","tif":"DAY","outside_rth":false,"contract":{"symbol":"SAP"},"reason":"test"}],"counts":{"total":1,"actionable":1,"theta_hygiene":1,"risk_reduction":0}}`
	srv := writeCurrent(t, legacy)
	if srv.tradeProposals == nil {
		t.Fatal("proposal engine not installed")
	}
	if got := srv.tradeProposals.snapshot; got.Kind != "" {
		t.Fatalf("legacy unscoped snapshot adopted at load: %+v", got)
	}

	scoped, err := json.Marshal(scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now))
	if err != nil {
		t.Fatal(err)
	}
	srv = writeCurrent(t, string(scoped))
	if got := srv.tradeProposals.snapshot; !got.LoadedFromState || got.AccountID != "DU7654321" || got.AccountMode != rpc.AccountModePaper {
		t.Fatalf("scoped v2 snapshot not adopted: %+v", got)
	}

	shell, err := json.Marshal(emptyProposalSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	srv = writeCurrent(t, string(shell))
	if got := srv.tradeProposals.snapshot; got.Kind != "" {
		t.Fatalf("identity-less v2 shell adopted at load: %+v", got)
	}
}

func TestProposalSubmitBlockedOnUnscopedAccountIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{Host: "127.0.0.1", Port: 7496, Account: "All"}, now)
	e := newProposalScopeTestEngine(t, srv)

	res, err := e.Submit(context.Background(), rpc.TradeProposalSubmitParams{Key: "theta_hygiene:abc", Revision: "sha256:test", FastPath: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Accepted {
		t.Fatal("submit accepted under unscoped account identity")
	}
	if !hasBlocker(res.Blockers, "account_identity_unscoped") {
		t.Fatalf("blockers = %+v, want account_identity_unscoped", res.Blockers)
	}
	if res.Preview != nil || res.Place != nil {
		t.Fatalf("submit reached preview/place despite unscoped identity: %+v", res)
	}
}

func TestProposalFastPathPreviewRefusesScopeMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	e.snapshot = scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive} }

	prop, blockers, ok := e.fastPathPreviewProposal("theta_hygiene:abc", "sha256:test")
	if !ok {
		t.Fatal("fast path fell through to revalidation; scope mismatch must fail closed in the fast path")
	}
	if prop.Key != "" {
		t.Fatalf("fast path returned a foreign-session proposal: %+v", prop)
	}
	if !hasBlocker(blockers, "proposal_scope_mismatch") {
		t.Fatalf("blockers = %+v, want proposal_scope_mismatch", blockers)
	}
}

func TestProposalPreserveOnFailureDropsForeignScopeSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	snap := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)
	snap.PolicyStatus = rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault}
	e.snapshot = snap

	// Paper→live switch with a transient account fetch failure: the old
	// paper snapshot must not be preserved into the live session.
	_, ok := e.preserveSnapshotOnRefreshFailure(
		brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive},
		rpc.AutoTradeStatus{},
		rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault},
		[]rpc.TradingBlocker{{Code: "account_unavailable", Message: "transient"}},
		false,
	)
	if ok {
		t.Fatal("foreign-scope snapshot preserved across a session switch")
	}

	// Same session: preservation still works.
	preserved, ok := e.preserveSnapshotOnRefreshFailure(
		brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper},
		rpc.AutoTradeStatus{},
		rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault},
		[]rpc.TradingBlocker{{Code: "account_unavailable", Message: "transient"}},
		false,
	)
	if !ok || len(preserved.Proposals) != 1 {
		t.Fatalf("same-scope snapshot not preserved: ok=%v %+v", ok, preserved)
	}
}

func TestProposalIgnoreIsScopedPerAccountMode(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	paper := brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}
	live := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}

	e.scope = func() brokerStateScope { return paper }
	e.Ignore(rpc.TradeProposalIgnoreParams{Key: "theta_hygiene:abc", Reason: "test"})
	if !e.isIgnored(paper, "theta_hygiene:abc") {
		t.Fatal("ignore not effective in its own scope")
	}
	if e.isIgnored(live, "theta_hygiene:abc") {
		t.Fatal("paper ignore suppressed the same contract on the live session")
	}

	// Ignores recorded while the session identity is unknown must never
	// suppress proposals in a concrete session.
	e.scope = func() brokerStateScope { return brokerStateScope{} }
	e.Ignore(rpc.TradeProposalIgnoreParams{Key: "theta_hygiene:def"})
	if e.isIgnored(paper, "theta_hygiene:def") || e.isIgnored(live, "theta_hygiene:def") {
		t.Fatal("unscoped ignore leaked into a concrete session")
	}
}

func TestProposalRevisionChangesWithScope(t *testing.T) {
	t.Parallel()
	policy := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	sources := rpc.TradeProposalSourceFingerprints{
		Account:   &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
		Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
	}
	proposals := []rpc.TradeProposal{{Key: "theta_hygiene:abc", Quantity: 1, PositionEffect: rpc.OrderPositionEffectClose}}
	paper := proposalRevision(policy, sources, brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, proposals)
	live := proposalRevision(policy, sources, brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}, proposals)
	if paper == live {
		t.Fatalf("revision identical across sessions with bucket-equal sources: %s", paper)
	}
	again := proposalRevision(policy, sources, brokerStateScope{Account: "du7654321", Mode: rpc.AccountModePaper}, proposals)
	if paper != again {
		t.Fatalf("revision not case-stable for the same scope: %s != %s", paper, again)
	}
}

// TestReplaceSnapshotDoesNotPersistShells pins the restart-survival rule:
// a transient error/unscoped shell installed by the startup refresh
// (which races the gateway connect) must not overwrite the persisted
// last-good snapshot. That overwrite made installProposalEngine warn
// "ignoring persisted snapshot without a concrete account/mode scope"
// on every start, so warm-start adoption never happened.
func TestReplaceSnapshotDoesNotPersistShells(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 11, 13, 39, 0, 0, time.UTC)
	store := &proposalStore{
		currentPath: filepath.Join(t.TempDir(), "trade-proposals-current.json"),
		eventsPath:  filepath.Join(t.TempDir(), "trade-proposals.jsonl"),
	}
	engine := &proposalEngine{
		store:   store,
		now:     func() time.Time { return now },
		ignored: map[string]struct{}{},
	}

	good := rpc.TradeProposalSnapshot{
		Kind:          rpc.TradeProposalSnapshotKind,
		SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:          now,
		Revision:      "sha256:good",
		AccountID:     "U1234567",
		AccountMode:   rpc.AccountModeLive,
		Proposals:     []rpc.TradeProposal{},
	}
	engine.replaceSnapshot(good)

	shell := emptyProposalSnapshot(now.Add(time.Minute))
	shell.Blockers = []rpc.TradingBlocker{{Code: "account_unavailable", Message: "ibkr connection unavailable"}}
	engine.replaceSnapshot(shell)

	persisted, err := store.LoadCurrent()
	if err != nil {
		t.Fatalf("LoadCurrent: %v", err)
	}
	if persisted.Revision != "sha256:good" {
		t.Fatalf("persisted revision = %q, want the generated snapshot to survive the shell install", persisted.Revision)
	}
	if got := engine.Snapshot(false).Revision; got != "empty" {
		t.Fatalf("in-memory snapshot revision = %q, want the shell to keep serving in-memory", got)
	}
}

// TestProposalRefreshWaitBacksOffTransientFailures pins the Run-loop
// retry schedule: a clean refresh waits the full cadence, transient
// failures retry at 30s doubling up to the cadence cap. Without the
// quick retry, a daemon restart that races the gateway connect serves
// the "ibkr connection unavailable" blocker for a full 15-minute cadence
// (observed 2026-06-11 in the SPA protection panel).
func TestProposalRefreshWaitBacksOffTransientFailures(t *testing.T) {
	t.Parallel()
	cadence := 15 * time.Minute
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, cadence},
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, cadence},   // 16m, capped
		{200, cadence}, // shift-overflow guard
	}
	for _, tc := range cases {
		if got := proposalRefreshWait(cadence, tc.failures); got != tc.want {
			t.Errorf("proposalRefreshWait(%v, %d) = %v, want %v", cadence, tc.failures, got, tc.want)
		}
	}
	if got := proposalRefreshWait(10*time.Second, 3); got != 10*time.Second {
		t.Errorf("a cadence below the retry base must win: got %v", got)
	}
}

func TestProposalRefreshTransientClassifiesBlockers(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"account_identity_unscoped", "account_unavailable", "positions_unavailable", "proposal_scope_mismatch"} {
		snap := rpc.TradeProposalSnapshot{Blockers: []rpc.TradingBlocker{{Code: code}}}
		if !proposalRefreshTransient(snap) {
			t.Errorf("blocker %q should classify as transient", code)
		}
	}
	for _, snap := range []rpc.TradeProposalSnapshot{
		{},
		{Blockers: []rpc.TradingBlocker{{Code: "proposals_disabled"}}},
		{Blockers: []rpc.TradingBlocker{{Code: "policy_drift"}}},
	} {
		if proposalRefreshTransient(snap) {
			t.Errorf("snapshot with blockers %+v should not classify as transient", snap.Blockers)
		}
	}
}

// failedRefreshSnapshot mimics the preserve path's output: last-good
// proposals with the transient blocker merged in and as_of frozen at the
// original generation time.
func failedRefreshSnapshot(asOf time.Time, code string) rpc.TradeProposalSnapshot {
	snap := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, asOf)
	snap.Blockers = []rpc.TradingBlocker{{Code: code, Message: "fetch failed"}}
	return snap
}

// Not parallel: NewLogger redirects the global pkg/ibkr logger, so a
// concurrent test's library output could land in this buffer.
func TestNoteRefreshOutcomeWarnsAfterStreakAndLogsRecovery(t *testing.T) {
	start := time.Date(2026, 6, 11, 18, 23, 41, 0, time.UTC)
	now := start
	var buf bytes.Buffer
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, start)
	srv.logger = NewLogger(&buf, "info")
	e := newProposalScopeTestEngine(t, srv)
	e.now = func() time.Time { return now }
	good := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, start)

	// Failures below proposalRefreshWarnStreak stay quiet: startup
	// refreshes race the gateway connect by design.
	for range proposalRefreshWarnStreak - 1 {
		e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
		now = now.Add(time.Minute)
	}
	if got := buf.String(); strings.Contains(got, "refresh blocked") {
		t.Fatalf("warned before the streak threshold: %s", got)
	}

	// The threshold failure warns with the streak, blocker codes, and the
	// age of the snapshot still being served.
	e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
	if got := buf.String(); !strings.Contains(got, "refresh blocked 3 consecutive times over 2m0s") ||
		!strings.Contains(got, "codes: account_unavailable") ||
		!strings.Contains(got, "(2m0s old)") {
		t.Fatalf("threshold warn missing streak/codes/age: %s", got)
	}

	// Every further failed attempt warns again — Run's backoff paces these
	// at the escalation/cadence rate, so this is one line per escalation.
	now = now.Add(time.Minute)
	e.noteRefreshOutcome(failedRefreshSnapshot(start, "positions_unavailable"), nil)
	if got := buf.String(); strings.Count(got, "refresh blocked") != 2 ||
		!strings.Contains(got, "codes: positions_unavailable") {
		t.Fatalf("escalation warn missing: %s", got)
	}

	// Recovery closes the streak with one info line and resets the state.
	now = now.Add(time.Minute)
	e.noteRefreshOutcome(good, nil)
	if got := buf.String(); !strings.Contains(got, "refresh recovered after 4 blocked attempts over 4m0s") {
		t.Fatalf("recovery info missing: %s", got)
	}
	if h := e.RefreshHealth(); h.Streak != 0 || len(h.Codes) != 0 || !h.Since.IsZero() {
		t.Fatalf("streak not reset on recovery: %+v", h)
	}

	// A short blip that never crossed the threshold recovers silently.
	buf.Reset()
	e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
	e.noteRefreshOutcome(good, nil)
	if got := buf.String(); strings.Contains(got, "refresh blocked") || strings.Contains(got, "refresh recovered") {
		t.Fatalf("short blip should stay quiet: %s", got)
	}
}

func TestProposalBlockerCodesFlattensAndFallsBackToError(t *testing.T) {
	t.Parallel()
	snap := rpc.TradeProposalSnapshot{Blockers: []rpc.TradingBlocker{
		{Code: "account_unavailable"}, {Code: "account_unavailable"}, {Code: "wide_spread"}, {Code: ""},
	}}
	if got := strings.Join(proposalBlockerCodes(snap, nil), ","); got != "account_unavailable,wide_spread" {
		t.Fatalf("codes = %q, want deduped account_unavailable,wide_spread", got)
	}
	if got := proposalBlockerCodes(rpc.TradeProposalSnapshot{}, errors.New("dial tcp: refused")); len(got) != 1 || got[0] != "dial tcp: refused" {
		t.Fatalf("blocker-less failure should fall back to the error: %v", got)
	}
}

func TestProposalSubsystemHealthReportsRefreshStreak(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 6, 11, 18, 23, 41, 0, time.UTC)
	now := start
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, start)
	e := newProposalScopeTestEngine(t, srv)
	e.now = func() time.Time { return now }
	srv.tradeProposals = e
	e.replaceSnapshot(scopedTestSnapshot("DU7654321", rpc.AccountModePaper, start))

	find := func() (rpc.SubsystemHealth, bool) {
		for _, sub := range srv.subsystemHealth(true) {
			if sub.Name == "proposals" {
				return sub, true
			}
		}
		return rpc.SubsystemHealth{}, false
	}

	sub, ok := find()
	if !ok || sub.Status != "ready" || sub.Message != "" {
		t.Fatalf("clean engine should report ready: %+v ok=%v", sub, ok)
	}

	for range proposalRefreshWarnStreak - 1 {
		e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
		now = now.Add(time.Minute)
	}
	if sub, _ := find(); sub.Status != "ready" {
		t.Fatalf("sub-threshold streak should stay ready: %+v", sub)
	}

	e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
	sub, _ = find()
	if sub.Status != "degraded" {
		t.Fatalf("threshold streak should degrade: %+v", sub)
	}
	if !strings.Contains(sub.Message, "blocked 3 consecutive times") ||
		!strings.Contains(sub.Message, start.Format(time.RFC3339)) {
		t.Fatalf("degraded message missing streak/as_of: %q", sub.Message)
	}
	if sub.LastError != "account_unavailable" || !sub.LastErrorAt.Equal(start) {
		t.Fatalf("degraded row missing codes/since: %+v", sub)
	}

	e.noteRefreshOutcome(scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now), nil)
	if sub, _ := find(); sub.Status != "ready" || sub.LastError != "" {
		t.Fatalf("recovered engine should report ready: %+v", sub)
	}

	srv.cfg.AutoTrade.ProposalsEnabled = new(false)
	if sub, _ := find(); sub.Status != "disabled" {
		t.Fatalf("disabled config should report disabled: %+v", sub)
	}

	srv.tradeProposals = nil
	if _, ok := find(); ok {
		t.Fatal("nil engine must not report a proposals subsystem")
	}
}

func TestApplyNotionalCapToProposalsDisclosesPreviewGate(t *testing.T) {
	t.Parallel()
	proposals := []rpc.TradeProposal{
		{Key: "trailing_stop:over", State: rpc.TradeProposalStateGenerated, Notional: 72396},
		{Key: "trailing_stop:under", State: rpc.TradeProposalStateGenerated, Notional: 9500},
		{Key: "trailing_stop:atcap", State: rpc.TradeProposalStateGenerated, Notional: 10000},
		{Key: "trailing_stop:nomark", State: rpc.TradeProposalStateGenerated},
	}
	applyNotionalCapToProposals(proposals, 10000)

	over := proposals[0]
	if over.State != rpc.TradeProposalStateBlocked || !hasBlocker(over.Blockers, "order_notional_exceeds_max") {
		t.Fatalf("over-cap proposal = %+v, want blocked with order_notional_exceeds_max", over)
	}
	if over.MaxNotional != 10000 {
		t.Fatalf("over-cap MaxNotional = %.2f, want disclosed cap 10000", over.MaxNotional)
	}
	blocker := over.Blockers[0]
	if !strings.Contains(blocker.Message, "72396.00") || !strings.Contains(blocker.Message, "10000.00") {
		t.Fatalf("blocker message %q, want both sides of the comparison", blocker.Message)
	}
	if !strings.Contains(blocker.Action, "trading.limits.max_notional=72396") {
		t.Fatalf("blocker action %q, want runtime settings remediation with the needed value", blocker.Action)
	}
	for _, tc := range []struct {
		name string
		got  rpc.TradeProposal
	}{{"under-cap", proposals[1]}, {"at-cap", proposals[2]}, {"no-mark", proposals[3]}} {
		if tc.got.State != rpc.TradeProposalStateGenerated || len(tc.got.Blockers) != 0 {
			t.Fatalf("%s proposal = %+v, want generated without blockers (gate passes notional <= cap)", tc.name, tc.got)
		}
		if tc.got.MaxNotional != 10000 {
			t.Fatalf("%s MaxNotional = %.2f, want disclosed cap 10000", tc.name, tc.got.MaxNotional)
		}
	}

	unresolved := []rpc.TradeProposal{{Key: "trailing_stop:x", State: rpc.TradeProposalStateGenerated, Notional: 1e9}}
	applyNotionalCapToProposals(unresolved, 0)
	if unresolved[0].MaxNotional != 0 || len(unresolved[0].Blockers) != 0 {
		t.Fatalf("unresolved-cap proposal = %+v, want untouched (no honest cap to disclose)", unresolved[0])
	}
}

// TestGenerateDisclosesEffectiveMaxNotional proves generation discloses the
// same merged TOML+runtime cap the preview gate enforces. On trading-capable
// builds the runtime trading.limits.max_notional override (5000) must block
// an 8000-notional proposal; on stable builds runtime limit overrides are
// read-only and the TOML default (10000) lets the same proposal stay ready.
func TestGenerateDisclosesEffectiveMaxNotional(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, time.UTC)
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.now = func() time.Time { return now }
	_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"trading":{"limits":{"max_notional":5000}}}`)})
	if orderWritesAvailable && err != nil {
		t.Fatalf("set runtime max_notional override: %v", err)
	}
	if !orderWritesAvailable && err == nil {
		t.Fatal("stable build accepted a trading.limits write, want read-only refusal")
	}

	engine := &proposalEngine{
		server:  srv,
		now:     srv.now,
		ignored: map[string]struct{}{},
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", now)
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{
		Symbol:     "AMD",
		SecType:    "STK",
		Quantity:   80,
		Mark:       100,
		Multiplier: 1,
		Currency:   "USD",
	}}}
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	props := engine.generate(policy, status, pos, rpc.TradeProposalSourceFingerprints{}, nil, scope, now)
	if len(props) != 1 {
		t.Fatalf("generated %d proposals, want 1: %+v", len(props), props)
	}
	p := props[0]
	if p.Notional != 8000 {
		t.Fatalf("proposal notional = %.2f, want mark-based 8000", p.Notional)
	}
	gateCap := srv.effectiveTradingConfig().MaxNotional
	if p.MaxNotional != gateCap {
		t.Fatalf("disclosed MaxNotional = %.2f, want the gate's effective cap %.2f", p.MaxNotional, gateCap)
	}
	if orderWritesAvailable {
		if gateCap != 5000 {
			t.Fatalf("effective cap = %.2f, want runtime override 5000", gateCap)
		}
		if p.State != rpc.TradeProposalStateBlocked || !hasBlocker(p.Blockers, "order_notional_exceeds_max") {
			t.Fatalf("proposal = %+v, want blocked: 8000 estimated notional exceeds the 5000 runtime cap", p)
		}
	} else {
		if gateCap != 10000 {
			t.Fatalf("effective cap = %.2f, want TOML default 10000 on a stable build", gateCap)
		}
		if p.State != rpc.TradeProposalStateGenerated || len(p.Blockers) != 0 {
			t.Fatalf("proposal = %+v, want ready: 8000 estimated notional is under the 10000 default cap", p)
		}
	}
}
