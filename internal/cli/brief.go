package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func runBrief(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "brief")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON (never stamps)")
	kind := fs.String("kind", "", "stamp morning or eod instead of the first incomplete daily artefact")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	*kind = strings.ToLower(strings.TrimSpace(*kind))
	if *kind != "" && *kind != rpc.BriefKindMorning && *kind != rpc.BriefKindEOD {
		return fail(env, "brief: --kind must be morning or eod")
	}

	var res rpc.BriefResult
	if err := env.Conn.Call(ctx, rpc.MethodBriefSnapshot, rpc.BriefSnapshotParams{}, &res); err != nil {
		return fail(env, "brief: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderBrief(env, res)

	if !briefHumanOrigin(env.Origin) {
		fmt.Fprintln(env.Stdout, "\nagent-origin render — not stamped")
		return 0
	}
	target := res.StampTarget
	if *kind != "" {
		target = *kind
	}
	if target == "" {
		fmt.Fprintf(env.Stdout, "\nnot stamped — %s\n", nonEmpty(res.StampTargetReason, "no daily artefact target"))
		return 0
	}
	var ack rpc.BriefAckResult
	err := env.Conn.Call(ctx, rpc.MethodBriefAck, rpc.BriefAckParams{
		Kind: target, BriefFingerprint: res.BriefFingerprint, Origin: env.Origin,
	}, &ack)
	if err != nil {
		// Rendering succeeded. Stamping is advisory, but the failure must be
		// conspicuous and must not turn a useful brief into a failing command.
		fmt.Fprintf(env.Stderr, "ibkr: brief rendered but stamp failed: %v\n", err)
		return 0
	}
	if ack.AlreadyStamped {
		fmt.Fprintf(env.Stdout, "\nstamp: %s artefact for %s — already stamped\n", ack.Kind, ack.Day)
	} else {
		fmt.Fprintf(env.Stdout, "\nstamp: %s artefact for %s\n", ack.Kind, ack.Day)
	}
	return 0
}

func briefHumanOrigin(origin string) bool {
	return origin == rpc.OrderOriginHumanTTY || origin == rpc.OrderOriginPairedDevice
}

func renderBrief(env *Env, res rpc.BriefResult) {
	fmt.Fprintf(env.Stdout, "Daily brief — %s  %s\n", res.AsOf.Local().Format("2006-01-02 15:04 MST"), shortFingerprint(res.BriefFingerprint))

	fmt.Fprintln(env.Stdout, "\nA  Market")
	briefLine(env, "regime", res.Market.Regime.BriefRowState,
		briefJoin(res.Market.Regime.Stage, res.Market.Regime.Verdict))
	breadth := "—"
	if res.Market.Breadth.PctAbove50DMA != nil {
		breadth = fmt.Sprintf("50-DMA %.1f%%", *res.Market.Breadth.PctAbove50DMA)
		if res.Market.Breadth.PctAbove200DMA != nil {
			breadth += fmt.Sprintf(" · 200-DMA %.1f%%", *res.Market.Breadth.PctAbove200DMA)
		}
	}
	briefLine(env, "breadth", res.Market.Breadth.BriefRowState, breadth)
	gamma := "—"
	if res.Market.Gamma.Spot != nil {
		gamma = fmt.Sprintf("spot %.2f", *res.Market.Gamma.Spot)
		if res.Market.Gamma.ZeroGamma != nil {
			gamma += fmt.Sprintf(" · zero %.2f", *res.Market.Gamma.ZeroGamma)
		}
		if res.Market.Gamma.GapPct != nil {
			gamma += fmt.Sprintf(" · gap %+.1f%%", *res.Market.Gamma.GapPct)
		}
	}
	briefLine(env, "dealer gamma", res.Market.Gamma.BriefRowState, gamma)
	briefLine(env, "canary", res.Market.Canary.BriefRowState,
		briefJoin(res.Market.Canary.Action, res.Market.Canary.Severity, res.Market.Canary.Summary))

	fmt.Fprintln(env.Stdout, "\nB  Calendar")
	session := briefJoin(res.Calendar.Session.Market, res.Calendar.Session.State)
	briefLine(env, "session", res.Calendar.Session.BriefRowState, session)
	for _, event := range res.Calendar.MarketEvents {
		value := fmt.Sprintf("%d", event.Count)
		if len(event.Symbols) > 0 {
			value += " · " + strings.Join(event.Symbols, ", ")
		}
		briefLine(env, event.Kind, event.BriefRowState, value)
	}

	fmt.Fprintln(env.Stdout, "\nC  Portfolio")
	acct := "—"
	if res.Portfolio.Account.EquityBase != nil {
		acct = formatMoneyCcy(*res.Portfolio.Account.EquityBase, res.Portfolio.Account.BaseCurrency)
	}
	if res.Portfolio.Account.DailyPnLBase != nil {
		acct += " · day " + formatMoneyCcy(*res.Portfolio.Account.DailyPnLBase, res.Portfolio.Account.BaseCurrency)
	}
	briefLine(env, "account", res.Portfolio.Account.BriefRowState, acct)
	movers := make([]string, 0, len(res.Portfolio.Movers.Rows))
	for _, mover := range res.Portfolio.Movers.Rows {
		movers = append(movers, fmt.Sprintf("%s %s", mover.Symbol, formatMoneyCcy(mover.DailyPnLBase, res.Portfolio.Account.BaseCurrency)))
	}
	briefLine(env, "movers", res.Portfolio.Movers.BriefRowState, strings.Join(movers, " · "))
	briefLine(env, "premium at risk", res.Portfolio.PremiumAtRisk.BriefRowState, briefMoney(res.Portfolio.PremiumAtRisk))
	briefLine(env, "hedge cost / day", res.Portfolio.HedgeCost.BriefRowState, briefMoney(res.Portfolio.HedgeCost))
	orders := "—"
	if res.Portfolio.WorkingOrders.Count != nil {
		orders = fmt.Sprintf("%d", *res.Portfolio.WorkingOrders.Count)
	}
	briefLine(env, "working orders", res.Portfolio.WorkingOrders.BriefRowState, orders)

	fmt.Fprintln(env.Stdout, "\nD  Risk & limits")
	capital := briefJoin(res.RiskLimits.Capital.Tier, res.RiskLimits.Capital.Enforcement)
	if res.RiskLimits.Capital.ConsumedPct != nil {
		capital = briefJoin(capital, fmt.Sprintf("%.1f%% consumed", *res.RiskLimits.Capital.ConsumedPct))
	}
	briefLine(env, "capital", res.RiskLimits.Capital.BriefRowState, capital)
	latch := "open"
	if res.RiskLimits.Latch.Latched {
		latch = "LATCHED"
		if res.RiskLimits.Latch.AgeDays != nil {
			latch += fmt.Sprintf(" · %d day(s)", *res.RiskLimits.Latch.AgeDays)
		}
	}
	briefLine(env, "drawdown latch", res.RiskLimits.Latch.BriefRowState, latch)
	briefLine(env, "active overrides", res.RiskLimits.Overrides.BriefRowState, fmt.Sprintf("%d", len(res.RiskLimits.Overrides.Rows)))
	briefLine(env, "policy drift", res.RiskLimits.PolicyDrift.BriefRowState, fmt.Sprintf("%d", len(res.RiskLimits.PolicyDrift.Rows)))

	fmt.Fprintln(env.Stdout, "\nE  Process")
	reconcile := "never"
	if !res.Process.Reconcile.LastReconciledAt.IsZero() {
		reconcile = res.Process.Reconcile.LastReconciledAt.Local().Format("2006-01-02 15:04")
		if res.Process.Reconcile.Source != "" {
			reconcile += " · " + res.Process.Reconcile.Source
		}
	}
	if !res.Process.Reconcile.Deadline.IsZero() {
		reconcile += " · due " + res.Process.Reconcile.Deadline.Local().Format("2006-01-02")
		if res.Process.Reconcile.DaysRemaining != nil {
			reconcile += fmt.Sprintf(" (%d day(s))", *res.Process.Reconcile.DaysRemaining)
		}
	}
	briefLine(env, "reconcile", res.Process.Reconcile.BriefRowState, reconcile)
	briefLine(env, "auto-extend", res.Process.AutoExtend.BriefRowState, res.Process.AutoExtend.ReportID)
	oneTap := "blocked"
	if res.Process.OneTap.Signable {
		oneTap = "signable · ibkr policy capital-event reconcile"
	} else if len(res.Process.OneTap.Blockers) > 0 {
		oneTap += " · " + strings.Join(res.Process.OneTap.Blockers, "; ")
	}
	briefLine(env, "one-tap sign-off", res.Process.OneTap.BriefRowState, oneTap)
	delta := fmt.Sprintf("%d transition(s), %d added, %d removed", len(res.Process.RulesDelta.Transitions), len(res.Process.RulesDelta.Added), len(res.Process.RulesDelta.Removed))
	if res.Process.RulesDelta.RulebookFingerprintChanged {
		delta += " · fingerprint changed"
	}
	briefLine(env, "rules delta", res.Process.RulesDelta.BriefRowState, delta)
	for _, artefact := range res.Process.Artefacts.Rows {
		state := "not declared"
		if artefact.Declared {
			state = "not completed"
		}
		if artefact.Completed {
			state = "completed"
		}
		briefLine(env, "artefact "+artefact.Kind, artefact.BriefRowState, state)
	}
}

func briefLine(env *Env, label string, state rpc.BriefRowState, value string) {
	if strings.TrimSpace(value) == "" {
		value = "—"
	}
	fmt.Fprintf(env.Stdout, "  %-18s %-11s %s\n", label, state.Status, value)
	fmt.Fprintf(env.Stdout, "    %s\n", state.Detail)
}

func briefJoin(values ...string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " · ")
}

func briefMoney(row rpc.BriefMoneyCoverageRow) string {
	if row.AmountBase == nil {
		return "—"
	}
	return formatMoneyCcy(*row.AmountBase, row.BaseCurrency)
}
