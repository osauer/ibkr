package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
