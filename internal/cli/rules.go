package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/ibkr/internal/risk"
	"github.com/osauer/ibkr/internal/rpc"
)

// runRules renders the daemon's advisory trading-rulebook checklist
// (docs/design/trading-rulebook.md). Read-only; verdicts, ranking, and
// thresholds all come from the daemon — this renderer adds no policy.
func runRules(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "rules")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	symbol := fs.String("symbol", "", "narrow offender lists to one underlying")
	all := fs.Bool("all", false, "show passing rules too (default shows breaches first, passes compact)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	var res rpc.RulesResult
	if err := env.Conn.Call(ctx, rpc.MethodRulesSnapshot, rpc.RulesSnapshotParams{Symbol: *symbol}, &res); err != nil {
		return fail(env, "rules: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}

	if !res.Enabled {
		fmt.Fprintln(env.Stdout, "Trading rulebook is disabled (features.rulebook.enabled=false).")
		return 0
	}
	fmt.Fprintf(env.Stdout, "Trading rulebook — %s  policy %s v%d  status %s\n",
		res.AsOf.Local().Format("2006-01-02 15:04 MST"), res.PolicyID, res.PolicyVersion, res.Status)
	for _, h := range res.InputHealth {
		if h.Status != "ok" {
			fmt.Fprintf(env.Stdout, "  input %-9s %s %s\n", h.Source, h.Status, strings.Join(h.Notes, "; "))
		}
	}
	fmt.Fprintln(env.Stdout)

	order := res.Ranked
	if len(order) != len(res.Rules) {
		order = order[:0]
		for i := range res.Rules {
			order = append(order, i)
		}
	}
	shown := 0
	for _, ix := range order {
		r := res.Rules[ix]
		if r.Status == risk.RuleStatusPass && !*all {
			continue
		}
		shown++
		fmt.Fprintf(env.Stdout, "%s  %2d %-22s %s\n", ruleGlyph(r.Status), r.Number, r.ID, ruleHeadline(r))
		fmt.Fprintf(env.Stdout, "      %s\n", r.Evidence)
		for i, o := range r.Offenders {
			if i >= 5 {
				fmt.Fprintf(env.Stdout, "      … %d more\n", len(r.Offenders)-i)
				break
			}
			line := o.Symbol
			if o.Leg != "" {
				line = o.Leg
			}
			if o.Note != "" {
				line += " — " + o.Note
			}
			fmt.Fprintf(env.Stdout, "      • %s\n", line)
		}
		for _, note := range r.Notes {
			fmt.Fprintf(env.Stdout, "      (%s)\n", note)
		}
	}
	if shown == 0 {
		fmt.Fprintln(env.Stdout, "All 12 rules pass. Rerun with --all to see the full checklist.")
	}
	passes := res.BreachCounts[risk.RuleStatusPass]
	fmt.Fprintf(env.Stdout, "\n%d act, %d watch, %d unknown, %d info, %d pass",
		res.BreachCounts[risk.RuleStatusAct], res.BreachCounts[risk.RuleStatusWatch],
		res.BreachCounts[risk.RuleStatusUnknown], res.BreachCounts[risk.RuleStatusInfo], passes)
	if n := res.BreachCounts[risk.RuleStatusNotEvaluated]; n > 0 {
		fmt.Fprintf(env.Stdout, ", %d not evaluated", n)
	}
	fmt.Fprintln(env.Stdout)
	if len(res.Earnings) > 0 {
		var unknowns []string
		for _, e := range res.Earnings {
			if e.Source == "unknown" {
				unknowns = append(unknowns, e.Symbol)
			}
		}
		if len(unknowns) > 0 {
			fmt.Fprintf(env.Stdout, "No earnings date for: %s — set one with `ibkr settings set features.rulebook.earnings_overrides` (rules 6-8 stay unknown, never pass).\n",
				strings.Join(unknowns, ", "))
		}
	}
	return 0
}

func ruleGlyph(status string) string {
	switch status {
	case risk.RuleStatusAct:
		return "ACT "
	case risk.RuleStatusWatch:
		return "WARN"
	case risk.RuleStatusInfo:
		return "INFO"
	case risk.RuleStatusUnknown:
		return "?   "
	case risk.RuleStatusNotEvaluated:
		return "--  "
	default:
		return "ok  "
	}
}

func ruleHeadline(r risk.RuleRow) string {
	if r.Observed != nil && r.Threshold != nil {
		return fmt.Sprintf("%s (observed %.1f vs %.1f %s)", r.Title, *r.Observed, *r.Threshold, r.Unit)
	}
	if r.Reason != "" {
		return fmt.Sprintf("%s (%s)", r.Title, r.Reason)
	}
	return r.Title
}
