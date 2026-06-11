package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
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
	fastPath := fs.Bool("fast-path", false, "use current snapshot for supported fast previews such as trailing stops")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 {
		return fail(env, "proposals preview: usage is `ibkr proposals preview KEY REVISION`")
	}
	var res rpc.TradeProposalPreviewResult
	params := rpc.TradeProposalPreviewParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, TimeoutMs: int(timeout.Milliseconds()), FastPath: *fastPath}
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
	params := rpc.TradeProposalSubmitParams{Key: fs.Arg(0), Revision: fs.Arg(1), Quantity: *qty, FastPath: *fastPath, TimeoutMs: int(timeout.Milliseconds()), Origin: env.Origin}
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
		printTradingBlockers(out, "  ", st.Blockers)
	}
	fmt.Fprintln(out)
}

// printTradingBlockers renders blockers with their remediation action: under
// market stress the action text is the difference between a dead end and the
// next command to run.
func printTradingBlockers(out io.Writer, indent string, blockers []rpc.TradingBlocker) {
	for _, b := range blockers {
		fmt.Fprintf(out, "%s- %s: %s\n", indent, b.Code, b.Message)
		if b.Action != "" {
			fmt.Fprintf(out, "%s  action: %s\n", indent, b.Action)
		}
	}
}

func renderProposalsText(env *Env, snap *rpc.TradeProposalSnapshot) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Protection Proposals  %d actionable / %d total\n", snap.Counts.Actionable, snap.Counts.Total)
	statusRow(env, out, "Revision", snap.Revision)
	statusRow(env, out, "Policy", fmt.Sprintf("%s v%d", snap.PolicyID, snap.PolicyVersion))
	statusRow(env, out, "Theta/day", fmt.Sprintf("%.2f", snap.Counts.ThetaPerDay))
	printTradingBlockers(out, "  ", snap.Blockers)
	for _, p := range snap.Proposals {
		state := "ready"
		if len(p.Blockers) > 0 {
			state = "blocked"
		}
		head := fmt.Sprintf("%s  %s  %s %d %s", p.Key, p.Bucket, p.Action, p.Quantity, p.Symbol)
		if p.OrderType != "" {
			head += "  " + p.OrderType
		}
		if p.Trail != nil {
			head += " " + formatOrderTrail(p.Trail)
		}
		fmt.Fprintf(out, "  %s  %s  [%s]\n", head, p.Reason, state)
		for _, d := range p.Details {
			fmt.Fprintf(out, "      %s\n", d)
		}
		printTradingBlockers(out, "      ", p.Blockers)
	}
	fmt.Fprintln(out)
}

func renderProposalPreviewText(env *Env, res *rpc.TradeProposalPreviewResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Proposal Preview  accepted=%v submit_eligible=%v\n", res.Accepted, res.SubmitEligible)
	statusRow(env, out, "Proposal", res.Proposal.Key)
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	renderProposalOrderPreview(env, out, res.Preview)
	printTradingBlockers(out, "  ", res.Blockers)
	fmt.Fprintln(out)
}

// renderProposalOrderPreview shows what the user is one submit away from
// placing: the bound draft (incl. trail spec), live quote, position impact,
// and the broker WhatIf verdict with its failure detail when present.
func renderProposalOrderPreview(env *Env, out io.Writer, p *rpc.TradeProposalOrderPreview) {
	if p == nil {
		return
	}
	statusRow(env, out, "Draft", formatOrderDraftSummary(p.Draft))
	statusRow(env, out, "Notional", fmt.Sprintf("%.2f", p.Notional))
	statusRow(env, out, "Position", fmt.Sprintf("%.4g -> %.4g (%s)", p.Position.Before, p.Position.After, p.Position.Effect))
	statusRow(env, out, "Quote", formatOrderPreviewQuote(p.Quote))
	statusRow(env, out, "WhatIf", fmt.Sprintf("%s (required=%v)", p.WhatIf.Status, p.WhatIf.RequiredForSubmit))
	if p.WhatIf.Message != "" {
		statusRow(env, out, "WhatIf detail", p.WhatIf.Message)
	}
	if p.WhatIf.Action != "" {
		statusRow(env, out, "WhatIf action", p.WhatIf.Action)
	}
	if len(p.Warnings) > 0 {
		fmt.Fprintln(out, "Warnings:")
		for _, w := range p.Warnings {
			fmt.Fprintf(out, "  - %s: %s\n", w.Code, w.Message)
			if w.Action != "" {
				fmt.Fprintf(out, "    action: %s\n", w.Action)
			}
		}
	}
}

func renderProposalSubmitText(env *Env, res *rpc.TradeProposalSubmitResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Proposal Submit  accepted=%v\n", res.Accepted)
	statusRow(env, out, "Proposal", res.Proposal.Key)
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	statusRow(env, out, "Order ref", res.OrderRef)
	if res.Place != nil {
		statusRow(env, out, "Broker ID", strconv.Itoa(res.Place.ReservedOrderID))
		statusRow(env, out, "Lifecycle", nonEmpty(res.Place.LifecycleStatus, res.Place.SendState))
		if res.Place.Status != "" {
			statusRow(env, out, "Status", res.Place.Status)
		}
		if res.Place.Message != "" && res.Place.Message != res.Message {
			statusRow(env, out, "Broker message", res.Place.Message)
		}
	}
	if res.Message != "" {
		statusRow(env, out, "Message", res.Message)
	}
	printTradingBlockers(out, "  ", res.Blockers)
	fmt.Fprintln(out)
}
