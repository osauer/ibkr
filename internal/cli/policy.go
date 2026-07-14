package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// runPolicy renders and operates the risk constitution
// (docs/design/risk-policy.md). show is read-only; the write verbs are
// governance acts (capital events, one-shot overrides, drawdown reset,
// artefact completions) — the daemon accepts them from human origins only,
// so agent sessions can read this surface but never operate it. No verb
// here touches broker writes, freeze, or trading limits.
func runPolicy(ctx context.Context, env *Env, args []string) int {
	sub := "show"
	if idx := firstPositionalIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	switch sub {
	case "show":
		return runPolicyShow(ctx, env, args)
	case "default":
		return runPolicyDefault(ctx, env, args)
	case "capital-event":
		return runPolicyCapitalEvent(ctx, env, args)
	case "override":
		return runPolicyOverride(ctx, env, args)
	case "reset-drawdown":
		return runPolicyResetDrawdown(ctx, env, args)
	case "artefact":
		return runPolicyArtefact(ctx, env, args)
	default:
		return fail(env, "policy: unknown subcommand %q (try `ibkr policy show --explain`)", sub)
	}
}

// firstPositionalIndex finds the first non-flag token, skipping the values
// of catalog-known value flags. Needed because Run() hoists flags before
// positionals, so a subcommand can arrive after its own flags
// (settingsSubcommandIndex precedent).
func firstPositionalIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if isValueFlag(arg) && !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		return i
	}
	return -1
}

func runPolicyShow(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy show")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	explain := fs.Bool("explain", false, "show every limit with its plain-English meaning, source, and enforcement class")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.RiskPolicyResult
	if err := env.Conn.Call(ctx, rpc.MethodRiskPolicySnapshot, struct{}{}, &res); err != nil {
		return fail(env, "policy: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}

	fmt.Fprintf(env.Stdout, "Risk constitution — %s  status %s\n", res.AsOf.Local().Format("2006-01-02 15:04 MST"), res.Status)
	if res.PolicyID != "" {
		fp := ""
		if res.PolicyFingerprint != nil {
			fp = "  " + shortFingerprint(res.PolicyFingerprint.Key)
		}
		fmt.Fprintf(env.Stdout, "  policy %s v%d%s  (%s)\n", res.PolicyID, res.PolicyVersion, fp, res.Path)
	} else {
		fmt.Fprintf(env.Stdout, "  no policy file at %s\n", res.Path)
	}
	if res.Message != "" {
		fmt.Fprintf(env.Stdout, "  note: %s\n", res.Message)
	}
	for _, h := range res.InputHealth {
		if h.Status != "ok" {
			fmt.Fprintf(env.Stdout, "  input %-11s %s %s\n", h.Source, h.Status, strings.Join(h.Notes, "; "))
		}
	}

	c := res.Capital
	fmt.Fprintf(env.Stdout, "\nCapital tier: %s (block enforcement: %s)\n", strings.ToUpper(c.Tier), c.Enforcement)
	cur := c.BaseCurrency
	if cur == "" {
		cur = "base"
	}
	if c.EquityBase != nil {
		fmt.Fprintf(env.Stdout, "  equity            %14.2f %s  as of %s%s\n", *c.EquityBase, cur, c.EquityAsOf.Local().Format("2006-01-02 15:04"), staleTag(c.EquityStale))
	}
	if c.EffectiveRiskCapitalBase != nil {
		fmt.Fprintf(env.Stdout, "  effective risk    %14.2f %s  = min(declared, equity − floor)\n", *c.EffectiveRiskCapitalBase, cur)
	}
	if c.AdjustedPeakBase != nil {
		fmt.Fprintf(env.Stdout, "  adjusted peak     %14.2f %s  (external flows %+.2f)\n", *c.AdjustedPeakBase, cur, deref(c.CumExternalFlowsBase))
	}
	if c.ConsumedPct != nil {
		fmt.Fprintf(env.Stdout, "  drawdown          %14.2f %s  = %.1f%% of declared risk capital consumed\n", deref(c.DrawdownBase), cur, *c.ConsumedPct)
	}
	if c.BlockLatched {
		fmt.Fprintf(env.Stdout, "  LATCHED since %s — clearing requires `ibkr policy reset-drawdown --reason ...` (human, journaled)\n", c.LatchedAt.Local().Format("2006-01-02 15:04"))
	}
	if c.LastReconciledAt.IsZero() {
		fmt.Fprintln(env.Stdout, "  reconciled        never — review `ibkr recon`, resolve exceptions, then sign off with `ibkr policy capital-event reconcile --report <id>`")
	} else {
		fmt.Fprintf(env.Stdout, "  reconciled        %s%s\n", c.LastReconciledAt.Local().Format("2006-01-02 15:04"), staleTag(c.ReconcileStale))
	}
	for _, r := range c.Reasons {
		fmt.Fprintf(env.Stdout, "  (%s)\n", r)
	}

	if len(res.Unapproved) > 0 {
		fmt.Fprintf(env.Stdout, "\nUnapproved (no number exists until you write it in the policy file):\n")
		for _, k := range res.Unapproved {
			fmt.Fprintf(env.Stdout, "  • %s\n", k)
		}
	}

	if *explain {
		activeOverride := map[string]rpc.OverrideRecord{}
		for _, o := range res.Overrides {
			if o.Active {
				activeOverride[o.Control] = o
			}
		}
		fmt.Fprintln(env.Stdout, "\nEffective limits:")
		for _, l := range res.Limits {
			mark := ""
			if o, ok := activeOverride[l.Key]; ok {
				mark = fmt.Sprintf("  [override until %s: %s]", o.ExpiresAt.Local().Format("15:04"), o.Reason)
			}
			fmt.Fprintf(env.Stdout, "  %-34s %-14s %-10s %-9s%s\n", l.Key, l.Value, l.Source, l.Enforcement, mark)
			fmt.Fprintf(env.Stdout, "      %s\n", l.Meaning)
		}
	}

	if len(res.Overrides) > 0 {
		fmt.Fprintln(env.Stdout, "\nOverrides:")
		for _, o := range res.Overrides {
			state := "expired"
			if o.Active {
				state = "active until " + o.ExpiresAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(env.Stdout, "  %s  %s (%s) — %s\n", o.ID, o.Control, state, o.Reason)
		}
	}
	if len(res.Cadence) > 0 {
		fmt.Fprintln(env.Stdout, "\nCadence:")
		for _, a := range res.Cadence {
			fmt.Fprintf(env.Stdout, "  %-8s last completed %s %s\n", a.Artefact, a.CompletedAt.Local().Format("2006-01-02 15:04"), a.Note)
		}
	}
	if len(res.Inventory) > 0 {
		fmt.Fprintln(env.Stdout, "\nSibling policy pins:")
		for _, p := range res.Inventory {
			switch p.Status {
			case "match":
				fmt.Fprintf(env.Stdout, "  %-10s match (%s v%s)\n", p.Policy, p.LiveID, p.LiveVersion)
			case "drift":
				fmt.Fprintf(env.Stdout, "  %-10s DRIFT pinned %s v%s, live %s v%s — re-approve the pin or investigate\n", p.Policy, p.PinnedID, p.PinnedVersion, p.LiveID, p.LiveVersion)
			case "unpinned":
				fmt.Fprintf(env.Stdout, "  %-10s unpinned (live %s v%s)\n", p.Policy, p.LiveID, p.LiveVersion)
			default:
				fmt.Fprintf(env.Stdout, "  %-10s %s\n", p.Policy, p.Status)
			}
		}
	}
	return 0
}

func runPolicyCapitalEvent(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy capital-event")
	amount := fs.Float64("amount", 0, "amount in the policy base currency (deposit/withdrawal)")
	effectiveAt := fs.String("effective-at", "", "when the flow hit the account (YYYY-MM-DD or RFC3339; default now)")
	note := fs.String("note", "", "free-text note for the journal")
	report := fs.String("report", "", "recon report id being signed off (required for reconcile; from `ibkr recon`)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "policy capital-event: exactly one of deposit|withdrawal|reconcile is required")
	}
	params := rpc.CapitalEventParams{Type: fs.Arg(0), AmountBase: *amount, Note: *note, Report: *report, Origin: env.Origin}
	if *effectiveAt != "" {
		t, err := parseFlexibleTime(*effectiveAt)
		if err != nil {
			return fail(env, "policy capital-event: %v", err)
		}
		params.EffectiveAt = t
	}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyCapitalEvent, params, *jsonOut)
}

func runPolicyOverride(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy override")
	control := fs.String("control", "", "constitution key being excepted (see `ibkr policy show --explain`)")
	reason := fs.String("reason", "", "why this exception is justified (journaled verbatim)")
	hours := fs.Int("hours", 0, "override lifetime; capped by override.max_duration_hours")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	params := rpc.OverrideParams{Control: *control, Reason: *reason, Hours: *hours, Origin: env.Origin}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyOverride, params, *jsonOut)
}

func runPolicyResetDrawdown(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy reset-drawdown")
	reason := fs.String("reason", "", "why risk resumes (journaled verbatim; the reset re-bases the peak)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	params := rpc.ResetDrawdownParams{Reason: *reason, Origin: env.Origin}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyResetDrawdown, params, *jsonOut)
}

func runPolicyArtefact(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy artefact")
	note := fs.String("note", "", "free-text note for the journal")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "policy artefact: exactly one of morning|eod|weekly is required")
	}
	params := rpc.ArtefactParams{Artefact: fs.Arg(0), Note: *note, Origin: env.Origin}
	return callPolicyWrite(ctx, env, rpc.MethodRiskPolicyArtefact, params, *jsonOut)
}

func callPolicyWrite(ctx context.Context, env *Env, method string, params any, jsonOut bool) int {
	var res rpc.RiskPolicyWriteResult
	if err := env.Conn.Call(ctx, method, params, &res); err != nil {
		return fail(env, "policy: %v", err)
	}
	if jsonOut {
		return printJSON(env, res)
	}
	fmt.Fprintln(env.Stdout, res.Message)
	if res.Override != nil {
		fmt.Fprintf(env.Stdout, "  %s  %s expires %s\n", res.Override.ID, res.Override.Control, res.Override.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	return 0
}

func parseFlexibleTime(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (use YYYY-MM-DD or RFC3339)", v)
}

func staleTag(stale bool) string {
	if stale {
		return "  [STALE]"
	}
	return ""
}

func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func shortFingerprint(key string) string {
	if len(key) > 19 {
		return key[:19] + "…"
	}
	return key
}
