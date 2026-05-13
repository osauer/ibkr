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
	base := nonEmpty(a.BaseCurrency, "USD")
	fmt.Fprintf(out, "Account  %s · base=%s%s\n",
		nonEmpty(a.AccountID, "—"), base, env.suffixBadge(a.DataType))
	fmt.Fprintln(out, env.rule(44))
	fmt.Fprintln(out)
	// Width 14 fits the widest typical NLV with a thousands-grouped
	// currency symbol ("€ 992,841.68" is 12; -€ 999,999.99 is 13).
	const accountValWidth = 14
	fmt.Fprintf(out, "  Net liquidation         %s\n", env.bold(env.formatMoneyNegCcyRight(a.NetLiquidation, base, accountValWidth)))
	fmt.Fprintf(out, "  Buying power            %s\n", env.formatMoneyNegCcyRight(a.BuyingPower, base, accountValWidth))
	fmt.Fprintf(out, "  Available funds         %s\n", env.formatMoneyNegCcyRight(a.AvailableFunds, base, accountValWidth))
	fmt.Fprintf(out, "  Excess liquidity        %s\n", env.formatMoneyNegCcyRight(a.ExcessLiquidity, base, accountValWidth))
	fmt.Fprintf(out, "  Total cash              %s\n", env.formatMoneyNegCcyRight(a.TotalCash, base, accountValWidth))
	fmt.Fprintf(out, "  Maintenance margin      %s\n", env.formatMoneyNegCcyRight(a.MaintenanceMargin, base, accountValWidth))
	fmt.Fprintf(out, "  Initial margin          %s\n", env.formatMoneyNegCcyRight(a.InitialMargin, base, accountValWidth))
	fmt.Fprintln(out)
	renderCurrencyExposure(env, a)
	fmt.Fprintf(out, "  as of %s\n", formatTimeShort(a.AsOf))
	return 0
}

// renderCurrencyExposure prints one row per non-base currency holding.
// Empty when the account is single-currency or the daemon hasn't
// received the $LEDGER:ALL response yet (pre-handshake). The row's
// CCY column shows the contract currency; the per-row amount is
// rendered with that currency's symbol; the base column uses the
// account base currency.
func renderCurrencyExposure(env *Env, a *rpc.AccountResult) {
	if len(a.CurrencyExposure) == 0 {
		return
	}
	out := env.Stdout
	base := nonEmpty(a.BaseCurrency, "USD")
	fmt.Fprintf(out, "Currency exposure  (base=%s)\n", base)
	fmt.Fprintln(out, env.rule(60))
	fmt.Fprintf(out, "  %-4s   %16s   %12s   %16s\n",
		"CCY", "NET LIQ (CCY)", "FX→BASE", "NET LIQ (BASE)")
	for _, ex := range a.CurrencyExposure {
		fmt.Fprintf(out, "  %-4s   %16s   %12s   %16s\n",
			ex.Currency,
			formatMoneyCcy(ex.NetLiquidationCcy, ex.Currency),
			fmtFX(ex.ExchangeRate),
			formatMoneyCcy(ex.NetLiquidationBase, base))
	}
	fmt.Fprintln(out)
}

// fmtFX renders an exchange rate to 4 decimals. Em-dash when zero — the
// gateway never sends a real FX of zero, but the field IS zero on
// pre-handshake rows we filter out elsewhere, defense-in-depth.
func fmtFX(v float64) string {
	if v <= 0 {
		return padDash(12)
	}
	return fmt.Sprintf("%12.4f", v)
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
