package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runProposals(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	subIdx := max(proposalsSubcommandIndex(args), 0)
	sub := args[subIdx]
	args = append(append([]string{}, args[:subIdx]...), args[subIdx+1:]...)
	switch sub {
	case "status":
		return runProposalsStatus(ctx, env, args)
	case "refresh":
		return runProposalsRefresh(ctx, env, args)
	case "list":
		return runProposalsList(ctx, env, args)
	case "preview":
		return runProposalsPreview(ctx, env, args)
	case "submit":
		return runProposalsSubmit(ctx, env, args)
	case "ignore":
		return runProposalsIgnore(ctx, env, args)
	default:
		return fail(env, "proposals: unknown subcommand %q", sub)
	}
}

func proposalsSubcommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "status", "refresh", "list", "preview", "submit", "ignore":
			return i
		}
	}
	return -1
}

func runProposalsStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.AutoTradeStatus
	if err := env.Conn.Call(ctx, rpc.MethodAutoTradeStatus, nil, &res); err != nil {
		return fail(env, "proposals status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalStatusText(env, &res)
	return 0
}

func runProposalsRefresh(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals refresh")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.TradeProposalSnapshot
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsRefresh, rpc.TradeProposalRefreshParams{Show: true}, &res); err != nil {
		return fail(env, "proposals refresh: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalsText(env, &res)
	return 0
}

func runProposalsList(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals list")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.TradeProposalSnapshot
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsSnapshot, rpc.TradeProposalSnapshotParams{Show: true}, &res); err != nil {
		return fail(env, "proposals list: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalsText(env, &res)
	return 0
}

func runProposalsPreview(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals preview")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	qty := fs.Int("quantity", 0, "selected quantity; defaults to proposal quantity")
	timeout := fs.Duration("timeout", 5*time.Second, "quote/WhatIf timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "proposals preview: usage is `ibkr proposals preview KEY REVISION`")
	}
	var res rpc.TradeProposalPreviewResult
	params := rpc.TradeProposalPreviewParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, TimeoutMs: int(timeout.Milliseconds())}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsPreview, params, &res); err != nil {
		return fail(env, "proposals preview: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalPreviewText(env, &res)
	return 0
}

func runProposalsSubmit(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals submit")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	qty := fs.Int("quantity", 0, "selected quantity; defaults to proposal quantity")
	fastPath := fs.Bool("fast-path", true, "perform one-confirm preview+submit")
	timeout := fs.Duration("timeout", 5*time.Second, "quote/WhatIf timeout")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "proposals submit: usage is `ibkr proposals submit KEY REVISION`")
	}
	var res rpc.TradeProposalSubmitResult
	params := rpc.TradeProposalSubmitParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, FastPath: *fastPath, TimeoutMs: int(timeout.Milliseconds())}
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsSubmit, params, &res); err != nil {
		return fail(env, "proposals submit: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderProposalSubmitText(env, &res)
	return 0
}

func runProposalsIgnore(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "proposals ignore")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	reason := fs.String("reason", "", "ignore reason")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fail(env, "proposals ignore: usage is `ibkr proposals ignore KEY [REVISION]`")
	}
	params := rpc.TradeProposalIgnoreParams{Key: fs.Arg(0), Reason: strings.TrimSpace(*reason)}
	if fs.NArg() == 2 {
		params.Revision = fs.Arg(1)
	}
	var res rpc.TradeProposalIgnoreResult
	if err := env.Conn.Call(ctx, rpc.MethodTradeProposalsIgnore, params, &res); err != nil {
		return fail(env, "proposals ignore: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	fmt.Fprintf(env.Stdout, "Ignored %s (%s)\n", res.Key, res.Message)
	return 0
}

func renderProposalStatusText(env *Env, st *rpc.AutoTradeStatus) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Protection Proposals  %s\n", env.statusBadge(statusConcern{Text: strings.ToUpper(nonEmpty(st.Policy.Status, "unknown")), Level: statusConcernNotice}))
	statusRow(env, out, "Proposals", fmt.Sprint(st.ProposalsEnabled))
	statusRow(env, out, "Auto submit", fmt.Sprint(st.AutoSubmit))
	statusRow(env, out, "Fast path", fmt.Sprint(st.FastPathEnabled))
	statusRow(env, out, "Policy", fmt.Sprintf("%s v%d %s", st.Policy.PolicyID, st.Policy.PolicyVersion, st.Policy.Fingerprint.Key))
	if len(st.Blockers) > 0 {
		fmt.Fprintln(out, "Blockers:")
		for _, b := range st.Blockers {
			fmt.Fprintf(out, "  - %s: %s\n", b.Code, b.Message)
		}
	}
	fmt.Fprintln(out)
}

func renderProposalsText(env *Env, snap *rpc.TradeProposalSnapshot) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Protection Proposals  %d actionable / %d total\n", snap.Counts.Actionable, snap.Counts.Total)
	statusRow(env, out, "Revision", snap.Revision)
	statusRow(env, out, "Policy", fmt.Sprintf("%s v%d", snap.PolicyID, snap.PolicyVersion))
	statusRow(env, out, "Theta/day", fmt.Sprintf("%.2f", snap.Counts.ThetaPerDay))
	for _, b := range snap.Blockers {
		fmt.Fprintf(out, "  blocker: %s: %s\n", b.Code, b.Message)
	}
	for _, p := range snap.Proposals {
		state := "ready"
		if len(p.Blockers) > 0 {
			state = "blocked"
		}
		fmt.Fprintf(out, "  %s  %s  %s %d %s  %s  [%s]\n", p.Key, p.Bucket, p.Action, p.Quantity, p.Symbol, p.Reason, state)
	}
	fmt.Fprintln(out)
}

func renderProposalPreviewText(env *Env, res *rpc.TradeProposalPreviewResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Proposal Preview  accepted=%v submit_eligible=%v\n", res.Accepted, res.SubmitEligible)
	statusRow(env, out, "Proposal", res.Proposal.Key)
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	for _, b := range res.Blockers {
		fmt.Fprintf(out, "  blocker: %s: %s\n", b.Code, b.Message)
	}
	fmt.Fprintln(out)
}

func renderProposalSubmitText(env *Env, res *rpc.TradeProposalSubmitResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Proposal Submit  accepted=%v\n", res.Accepted)
	statusRow(env, out, "Proposal", res.Proposal.Key)
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	statusRow(env, out, "Order ref", res.OrderRef)
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	for _, b := range res.Blockers {
		fmt.Fprintf(out, "  blocker: %s: %s\n", b.Code, b.Message)
	}
	fmt.Fprintln(out)
}
