package cli

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runOrder(ctx context.Context, env *Env, args []string) int {
	if len(args) == 0 {
		return fail(env, "order: subcommand required (try `ibkr order preview ...`)")
	}
	subIdx := orderSubcommandIndex(args)
	if subIdx < 0 {
		return runOrderPreview(ctx, env, args)
	}
	sub := args[subIdx]
	args = append(append([]string{}, args[:subIdx]...), args[subIdx+1:]...)
	switch sub {
	case "preview":
		if orderPreviewFromPlanArgs(args) {
			return runOrderPreviewFromPlan(ctx, env, args)
		}
		return runOrderPreview(ctx, env, args)
	case "status":
		return runOrderStatus(ctx, env, args)
	case "place":
		return runOrderPlace(ctx, env, args)
	case "modify", "cancel":
		return fail(env, "order %s is not enabled; run `ibkr order preview` first and wait for the gated write path", sub)
	default:
		return fail(env, "order: unknown subcommand %q (try `ibkr order preview` or `ibkr order status`)", sub)
	}
}

func orderSubcommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "preview", "status", "place", "modify", "cancel":
			return i
		}
	}
	return -1
}

func orderPreviewFromPlanArgs(args []string) bool {
	for _, arg := range args {
		if arg == "--from-plan" || strings.HasPrefix(arg, "--from-plan=") || arg == "--candidate" || strings.HasPrefix(arg, "--candidate=") {
			return true
		}
	}
	return false
}

func runOrderPreview(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	limit := fs.Float64("limit", 0, "explicit LMT limit price")
	strategy := fs.String("strategy", "", "pricing strategy: patient-limit (default) or explicit-limit")
	tif := fs.String("tif", "", "time in force; DAY only")
	outsideRTH := fs.Bool("outside-rth", false, "allow outside regular trading hours when supported")
	timeout := fs.Duration("timeout", 5*time.Second, "quote snapshot timeout")
	market := fs.String("market", "", "stock market routing shortcut: us (default) or de")
	exchange := fs.String("exchange", "", "IBKR stock exchange/venue override (e.g. SMART, IBIS)")
	primary := fs.String("primary", "", "IBKR stock primary-exchange hint when routing through SMART")
	currency := fs.String("currency", "", "stock quote/order currency override (e.g. USD, EUR)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "preview" {
		rest = rest[1:]
	}
	if len(rest) != 3 {
		if len(rest) == 6 {
			return fail(env, "order preview: single-leg options are not enabled in this slice")
		}
		return fail(env, "order preview: usage is `ibkr order preview buy|sell SYMBOL QTY`")
	}
	qty, err := strconv.Atoi(rest[2])
	if err != nil || qty <= 0 {
		return fail(env, "order preview: quantity must be a positive integer")
	}
	var limitPtr *float64
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "limit" {
			v := *limit
			limitPtr = &v
		}
	})
	params := rpc.OrderPreviewParams{
		Action: strings.ToUpper(strings.TrimSpace(rest[0])),
		Contract: rpc.ContractParams{
			Symbol:      strings.ToUpper(strings.TrimSpace(rest[1])),
			SecType:     "STK",
			Market:      strings.TrimSpace(*market),
			Exchange:    strings.ToUpper(strings.TrimSpace(*exchange)),
			PrimaryExch: strings.ToUpper(strings.TrimSpace(*primary)),
			Currency:    strings.ToUpper(strings.TrimSpace(*currency)),
		},
		Quantity:   qty,
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: limitPtr,
		Strategy:   strings.TrimSpace(*strategy),
		TIF:        strings.ToUpper(strings.TrimSpace(*tif)),
		OutsideRTH: *outsideRTH,
		TimeoutMs:  int(timeout.Milliseconds()),
	}
	if params.Contract.Currency == "" && params.Contract.Market == "" && params.Contract.Exchange == "" && params.Contract.PrimaryExch == "" {
		params.Contract.Currency = "USD"
	}
	var res rpc.OrderPreviewResult
	if err := env.Conn.Call(ctx, rpc.MethodOrderPreview, params, &res); err != nil {
		return fail(env, "order preview: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderOrderPreviewText(env, &res)
	return 0
}

func runOrderPlace(_ context.Context, env *Env, args []string) int {
	fs := flagSet(env, "order place")
	token := fs.String("preview-token", "", "submit-capable preview token")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return fail(env, "order place: takes no positional args")
	}
	if strings.TrimSpace(*token) == "" {
		return fail(env, "order place: --preview-token is required")
	}
	return fail(env, "order place: broker write path disabled in the default build; preview tokens cannot be redeemed here")
}

func renderOrderPreviewText(env *Env, res *rpc.OrderPreviewResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Order Preview  %s\n", env.statusBadge(statusConcern{Text: "TOKEN", Level: statusConcernNotice}))
	statusRow(env, out, "Mode", res.Mode)
	statusRow(env, out, "Account", res.Account)
	statusRow(env, out, "Endpoint", fmt.Sprintf("%s client %d", res.Endpoint, res.ClientID))
	statusRow(env, out, "Draft", fmt.Sprintf("%s %d %s %s %.4f %s outside_rth=%v",
		res.Draft.Action, res.Draft.Quantity, res.Draft.Contract.Symbol, res.Draft.OrderType, res.Draft.LimitPrice, res.Draft.TIF, res.Draft.OutsideRTH))
	statusRow(env, out, "Strategy", res.Draft.Strategy)
	statusRow(env, out, "Notional", fmt.Sprintf("%.2f", res.Notional))
	statusRow(env, out, "Position", fmt.Sprintf("%.4g -> %.4g (%s)", res.Position.Before, res.Position.After, res.Position.Effect))
	statusRow(env, out, "Quote", formatOrderPreviewQuote(res.Quote))
	statusRow(env, out, "WhatIf", fmt.Sprintf("%s (required=%v)", res.WhatIf.Status, res.WhatIf.RequiredForSubmit))
	statusRow(env, out, "Token minted", fmt.Sprint(res.TokenMinted))
	statusRow(env, out, "Submit eligible", fmt.Sprint(res.SubmitEligible))
	statusRow(env, out, "Token ID", res.PreviewTokenID)
	if !res.PreviewTokenExpiresAt.IsZero() {
		statusRow(env, out, "Expires", res.PreviewTokenExpiresAt.Format(time.RFC3339))
	}
	if res.PreviewToken != "" {
		statusRow(env, out, "Token", res.PreviewToken)
	}
	if len(res.Warnings) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Warnings:")
		for _, w := range res.Warnings {
			fmt.Fprintf(out, "  - %s: %s\n", w.Code, w.Message)
			if w.Action != "" {
				fmt.Fprintf(out, "    action: %s\n", w.Action)
			}
		}
	}
	fmt.Fprintln(out)
}

func formatOrderPreviewQuote(q rpc.OrderQuoteSnapshot) string {
	parts := []string{q.Symbol}
	if q.Bid != nil {
		parts = append(parts, fmt.Sprintf("bid %.4f", *q.Bid))
	}
	if q.Ask != nil {
		parts = append(parts, fmt.Sprintf("ask %.4f", *q.Ask))
	}
	if q.Midpoint != nil {
		parts = append(parts, fmt.Sprintf("mid %.4f", *q.Midpoint))
	}
	if q.DataType != "" {
		parts = append(parts, "data "+q.DataType)
	}
	if q.QuoteQuality != "" {
		parts = append(parts, "quality "+q.QuoteQuality)
	}
	return strings.Join(parts, " | ")
}
