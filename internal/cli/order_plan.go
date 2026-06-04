package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runOrderPreviewFromPlan(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order preview")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	planPath := fs.String("from-plan", "", "risk-plan JSON artifact to preview")
	candidateID := fs.String("candidate", "", "risk-plan candidate ID to preview")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "order preview: takes no positional args when using --from-plan")
	}
	if strings.TrimSpace(*planPath) == "" || strings.TrimSpace(*candidateID) == "" {
		return fail(env, "order preview: usage is `ibkr order preview --from-plan PLAN.json --candidate ID`")
	}
	plan, err := loadRiskPlanArtifact(strings.TrimSpace(*planPath))
	if err != nil {
		return fail(env, "order preview: %v", err)
	}
	res := previewRiskPlanCandidate(ctx, env, plan, strings.TrimSpace(*candidateID))
	if *jsonOut {
		code := printJSON(env, res)
		if code != 0 {
			return code
		}
		if len(res.Blockers) > 0 {
			return 1
		}
		return 0
	}
	renderRiskPlanOrderPreviewText(env, &res)
	if len(res.Blockers) > 0 {
		return 1
	}
	return 0
}

func loadRiskPlanArtifact(path string) (rpc.RiskPlanResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return rpc.RiskPlanResult{}, fmt.Errorf("read risk plan %s: %w", path, err)
	}
	var plan rpc.RiskPlanResult
	if err := json.Unmarshal(raw, &plan); err != nil {
		return rpc.RiskPlanResult{}, fmt.Errorf("decode risk plan %s: %w", path, err)
	}
	if plan.Kind != rpc.RiskPlanKind || plan.SchemaVersion != rpc.RiskPlanSchemaVersion {
		return rpc.RiskPlanResult{}, fmt.Errorf("artifact is %q/%q, want %q/%q", plan.Kind, plan.SchemaVersion, rpc.RiskPlanKind, rpc.RiskPlanSchemaVersion)
	}
	if plan.PlanID == "" {
		return rpc.RiskPlanResult{}, fmt.Errorf("risk plan artifact has no plan_id")
	}
	return plan, nil
}

func previewRiskPlanCandidate(ctx context.Context, env *Env, plan rpc.RiskPlanResult, candidateID string) rpc.RiskPlanOrderPreviewResult {
	now := time.Now()
	candidate, ok := findRiskPlanCandidate(plan, candidateID)
	res := rpc.RiskPlanOrderPreviewResult{
		Kind:              "ibkr.order_preview",
		SchemaVersion:     "order-preview-v1",
		AsOf:              now,
		PlanID:            plan.PlanID,
		CandidateID:       candidateID,
		PolicyFingerprint: plan.PolicyFingerprint,
		WhatIf: rpc.OrderWhatIfResult{
			Status:            rpc.OrderWhatIfStatusUnavailable,
			RequiredForSubmit: true,
			Available:         false,
			Message:           "broker WhatIf/order wire support is not enabled in the default read-only build; preview is diagnostic only",
			Action:            "Do not place; enable the gated trading build before expecting submit eligibility.",
		},
		NotExecution: "Read-only handoff preview; no order is submitted and no submit-capable token is minted.",
	}
	if !ok {
		res.Blockers = append(res.Blockers, "candidate_not_found")
		res.SourceValidation = "not_checked"
		return res
	}
	if candidate.Status != rpc.RiskPlanCandidatePreviewable {
		res.Blockers = append(res.Blockers, "candidate_not_previewable:"+candidate.Status)
	}
	res.SourceValidation = validatePlanSource(ctx, env, plan)
	if res.SourceValidation != "ok" {
		res.Blockers = append(res.Blockers, res.SourceValidation)
	}
	for i, leg := range candidate.Legs {
		if leg.EstimatedLimitPrice == nil || *leg.EstimatedLimitPrice <= 0 {
			res.Blockers = append(res.Blockers, fmt.Sprintf("leg_%d_missing_limit_price", i+1))
			continue
		}
		action, err := brokerActionForPlanLeg(leg.Action)
		if err != nil {
			res.Blockers = append(res.Blockers, fmt.Sprintf("leg_%d_%v", i+1, err))
			continue
		}
		res.Previews = append(res.Previews, rpc.RiskPlanOrderLegPreview{
			CandidateLeg: leg,
			Draft: rpc.OrderDraft{
				Action:     action,
				Contract:   leg.Contract,
				Quantity:   leg.Quantity,
				OrderType:  leg.OrderType,
				LimitPrice: *leg.EstimatedLimitPrice,
				TIF:        leg.TIF,
				OutsideRTH: leg.OutsideRTH,
				Strategy:   leg.LimitStrategy,
				OrderRef:   fmt.Sprintf("%s_%s_%02d", plan.PlanID, candidate.ID, i+1),
			},
		})
	}
	res.Blockers = append(res.Blockers, "broker_whatif_unavailable")
	return res
}

func validatePlanSource(ctx context.Context, env *Env, plan rpc.RiskPlanResult) string {
	if env == nil || env.Conn == nil {
		return "source_not_checked_no_daemon"
	}
	current, err := FetchRiskPlan(ctx, env.Conn, rpc.RiskPlanModeAuto, nil)
	if err != nil {
		return "source_check_failed:" + err.Error()
	}
	if current.PolicyFingerprint != plan.PolicyFingerprint {
		return "policy_fingerprint_changed"
	}
	if fingerprintKey(current.SourceFingerprints.Account) != fingerprintKey(plan.SourceFingerprints.Account) {
		return "account_fingerprint_changed"
	}
	if fingerprintKey(current.SourceFingerprints.Positions) != fingerprintKey(plan.SourceFingerprints.Positions) {
		return "positions_fingerprint_changed"
	}
	if fingerprintKey(current.SourceFingerprints.Regime) != fingerprintKey(plan.SourceFingerprints.Regime) {
		return "regime_fingerprint_changed"
	}
	return "ok"
}

func fingerprintKey(fp *rpc.Fingerprint) string {
	if fp == nil {
		return ""
	}
	return fp.Version + " " + fp.Key
}

func findRiskPlanCandidate(plan rpc.RiskPlanResult, id string) (rpc.RiskPlanCandidate, bool) {
	for _, candidate := range plan.Candidates {
		if candidate.ID == id {
			return candidate, true
		}
	}
	return rpc.RiskPlanCandidate{}, false
}

func brokerActionForPlanLeg(action string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case "SELL", "SELL_TO_CLOSE":
		return rpc.OrderActionSell, nil
	case "BUY_TO_CLOSE":
		return rpc.OrderActionBuy, nil
	default:
		return "", fmt.Errorf("unsupported action %q", action)
	}
}

func renderRiskPlanOrderPreviewText(env *Env, res *rpc.RiskPlanOrderPreviewResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Order Preview  %s\n", env.statusBadge(statusConcern{Text: "BLOCKED", Level: statusConcernWarn}))
	statusRow(env, out, "Plan", res.PlanID)
	statusRow(env, out, "Candidate", res.CandidateID)
	statusRow(env, out, "Source", res.SourceValidation)
	statusRow(env, out, "Submit eligible", fmt.Sprint(res.SubmitEligible))
	statusRow(env, out, "WhatIf", res.WhatIf.Status+"; required=true")
	if len(res.Previews) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Drafts:")
		for _, preview := range res.Previews {
			d := preview.Draft
			fmt.Fprintf(out, "  - %s %d %s %s %.4f %s\n", d.Action, d.Quantity, d.Contract.Symbol, d.OrderType, d.LimitPrice, d.TIF)
		}
	}
	if len(res.Blockers) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Blockers:")
		for _, blocker := range res.Blockers {
			fmt.Fprintf(out, "  - %s\n", blocker)
		}
	}
	fmt.Fprintln(out)
}
