package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// runRecon renders the post-trade reconciliation report
// (docs/design/post-trade-truth.md): broker statement flows vs. the
// declared capital-event ledger. show is read-only; dismiss is a human-only
// governance write the daemon refuses from agent origins. The report id
// printed here is what `ibkr policy capital-event reconcile --report <id>`
// signs off.
func runRecon(ctx context.Context, env *Env, args []string) int {
	sub := "show"
	if idx := firstPositionalIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	switch sub {
	case "show":
		return runReconShow(ctx, env, args)
	case "dismiss":
		return runReconDismiss(ctx, env, args)
	default:
		return fail(env, "recon: unknown subcommand %q (try `ibkr recon show`)", sub)
	}
}

func runReconShow(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "recon show")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	refresh := fs.Bool("refresh", false, "kick one background Flex statement fetch before reporting")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.ReconResult
	if err := env.Conn.Call(ctx, rpc.MethodReconSnapshot, rpc.ReconSnapshotParams{Refresh: *refresh}, &res); err != nil {
		return fail(env, "recon: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}

	fmt.Fprintf(env.Stdout, "Reconciliation — %s  status %s\n", res.AsOf.Local().Format("2006-01-02 15:04 MST"), res.Status)
	if res.ReportID != "" {
		fmt.Fprintf(env.Stdout, "  report %s  statements as of %s  coverage %s → %s\n",
			res.ReportID, res.StatementAsOf.Local().Format("2006-01-02 15:04"),
			res.CoverageFrom.Format("2006-01-02"), res.CoverageTo.Format("2006-01-02"))
	}
	if res.Message != "" {
		fmt.Fprintf(env.Stdout, "  note: %s\n", res.Message)
	}
	if !res.Fetch.Configured {
		fmt.Fprintln(env.Stdout, "  fetch: [flex] is not configured (set enabled, query_id, and the token file)")
	} else if res.Fetch.LastError != "" {
		fmt.Fprintf(env.Stdout, "  fetch: last attempt %s failed: %s\n", res.Fetch.LastAttempt.Local().Format("2006-01-02 15:04"), res.Fetch.LastError)
	} else if !res.Fetch.LastSuccess.IsZero() {
		fmt.Fprintf(env.Stdout, "  fetch: last success %s\n", res.Fetch.LastSuccess.Local().Format("2006-01-02 15:04"))
	}
	for _, h := range res.InputHealth {
		if h.Status != "ok" {
			fmt.Fprintf(env.Stdout, "  input %-11s %s %s\n", h.Source, h.Status, strings.Join(h.Notes, "; "))
		}
	}

	if len(res.Counts) > 0 {
		fmt.Fprintf(env.Stdout, "\n  matched %d", res.Counts["matched"])
		for _, cat := range []string{rpc.ReconMissingFromLedger, rpc.ReconLedgerOnly, rpc.ReconAmountMismatch, rpc.ReconDateMismatch, rpc.ReconAmbiguous, rpc.ReconUncategorized} {
			if n := res.Counts[cat]; n > 0 {
				fmt.Fprintf(env.Stdout, "  %s %d", cat, n)
			}
		}
		fmt.Fprintf(env.Stdout, "  — %d unresolved\n", res.Unresolved)
	}
	for _, ex := range res.Exceptions {
		mark := "•"
		if ex.Dismissed {
			mark = "✓"
		}
		fmt.Fprintf(env.Stdout, "  %s %-19s %s", mark, ex.Category, ex.LineID)
		if ex.AmountBase != nil {
			fmt.Fprintf(env.Stdout, "  %.2f", *ex.AmountBase)
		}
		if !ex.ValueDate.IsZero() {
			fmt.Fprintf(env.Stdout, "  %s", ex.ValueDate.Format("2006-01-02"))
		}
		if ex.Description != "" {
			fmt.Fprintf(env.Stdout, "  %s", ex.Description)
		}
		fmt.Fprintln(env.Stdout)
		if ex.Note != "" {
			fmt.Fprintf(env.Stdout, "      %s\n", ex.Note)
		}
		if ex.Dismissed {
			fmt.Fprintf(env.Stdout, "      dismissed: %s\n", ex.DismissReason)
		}
	}
	if res.Equity != nil {
		fmt.Fprintf(env.Stdout, "\n  equity check: statement %s %.2f", res.Equity.StatementDate.Format("2006-01-02"), res.Equity.StatementTotalBase)
		if res.Equity.DivergencePct != nil {
			fmt.Fprintf(env.Stdout, "  vs runtime %+.2f%%", *res.Equity.DivergencePct)
		}
		fmt.Fprintln(env.Stdout)
	}
	if res.Status == rpc.ReconStatusActive && res.Unresolved == 0 && res.ReportID != "" {
		fmt.Fprintf(env.Stdout, "\nClean. Sign off with: ibkr policy capital-event reconcile --report %s\n", res.ReportID)
	}
	return 0
}

func runReconDismiss(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "recon dismiss")
	line := fs.String("line", "", "exception line id from `ibkr recon show`")
	reason := fs.String("reason", "", "why this line is deliberately not a ledger event (journaled verbatim)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	params := rpc.ReconDismissParams{LineID: *line, Reason: *reason, Origin: env.Origin}
	var res rpc.RiskPolicyWriteResult
	if err := env.Conn.Call(ctx, rpc.MethodReconDismiss, params, &res); err != nil {
		return fail(env, "recon dismiss: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	fmt.Fprintln(env.Stdout, res.Message)
	return 0
}
