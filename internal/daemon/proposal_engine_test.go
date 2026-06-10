package daemon

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/internal/config"
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
	prop, ok := trailingStopStockProposal(policy, status, longRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true)
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
	prop, ok = trailingStopStockProposal(policy, status, shortRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true)
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
	prop, ok = trailingStopStockProposal(policy, status, offHoursRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true)
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
	prop, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true)
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
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              oldAt,
			Revision:          "sha256:rev",
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
	prop, ok := trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), false)
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
	prop, ok = trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), false)
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
	prop, ok = trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true)
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
	a := proposalRevision(policy, sources, proposals)
	sources.Regime = &rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime-b"}
	b := proposalRevision(policy, sources, proposals)
	if a != b {
		t.Fatalf("revision changed on regime-only churn: %s != %s", a, b)
	}
	sources.Positions = &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions-b"}
	c := proposalRevision(policy, sources, proposals)
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
	a := proposalRevision(policy, sources, proposals)
	sources.MarketEvents = &rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-b"}
	if b := proposalRevision(policy, sources, proposals); b != a {
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
