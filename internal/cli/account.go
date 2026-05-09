package cli

import (
	"context"
	"fmt"

	"github.com/osauer/ibkr/internal/rpc"
)

func runAccount(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "account")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	var res rpc.AccountResult
	if err := env.Conn.Call(ctx, rpc.MethodAccountSummary, nil, &res); err != nil {
		return fail(env, "account: %v", err)
	}

	if *jsonOut {
		return printJSON(env, res)
	}
	return renderAccountText(env, &res)
}

func renderAccountText(env *Env, a *rpc.AccountResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Account  %s · profile=%s · base=%s%s\n",
		nonEmpty(a.AccountID, "—"), a.Profile, nonEmpty(a.BaseCurrency, "USD"), suffixBadge(a.DataType))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Net liquidation         %s\n", formatMoney(a.NetLiquidation))
	fmt.Fprintf(out, "  Buying power            %s\n", formatMoney(a.BuyingPower))
	fmt.Fprintf(out, "  Available funds         %s\n", formatMoney(a.AvailableFunds))
	fmt.Fprintf(out, "  Excess liquidity        %s\n", formatMoney(a.ExcessLiquidity))
	fmt.Fprintf(out, "  Total cash              %s\n", formatMoney(a.TotalCash))
	fmt.Fprintf(out, "  Maintenance margin      %s\n", formatMoney(a.MaintenanceMargin))
	fmt.Fprintf(out, "  Initial margin          %s\n", formatMoney(a.InitialMargin))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  as of %s\n", formatTimeShort(a.AsOf))
	return 0
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
