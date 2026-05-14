package cli

import (
	"context"
	"fmt"
	"strings"

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
	header := fmt.Sprintf("Account  %s · base=%s", nonEmpty(a.AccountID, "—"), base)
	if a.AccountType != "" {
		header += " · type=" + env.dim(a.AccountType)
	}
	header += env.suffixBadge(a.DataType)
	fmt.Fprintln(out, header)
	fmt.Fprintln(out, env.rule(44))
	fmt.Fprintln(out)
	// Width 14 fits the widest typical NLV with a thousands-grouped
	// currency symbol ("€ 992,841.68" is 12; -€ 999,999.99 is 13).
	const accountValWidth = 14
	// Hero number first — bolded — so the answer to "how much do I have?"
	// lands on the first row regardless of section structure beneath.
	fmt.Fprintf(out, "  Net liquidation         %s\n",
		env.bold(env.formatMoneyNegCcyRight(a.NetLiquidation, base, accountValWidth)))
	fmt.Fprintln(out)

	fmt.Fprintln(out, env.dim("  Balances"))
	fmt.Fprintf(out, "    Buying power            %s\n", env.formatMoneyNegCcyRight(a.BuyingPower, base, accountValWidth))
	fmt.Fprintf(out, "    Available funds         %s\n", env.formatMoneyNegCcyRight(a.AvailableFunds, base, accountValWidth))
	fmt.Fprintf(out, "    Excess liquidity        %s\n", env.formatMoneyNegCcyRight(a.ExcessLiquidity, base, accountValWidth))
	fmt.Fprintf(out, "    Total cash              %s\n", env.formatMoneyNegCcyRight(a.TotalCash, base, accountValWidth))
	fmt.Fprintf(out, "    Gross position value    %s\n", env.formatMoneyNegCcyRight(a.GrossPositionValue, base, accountValWidth))
	fmt.Fprintf(out, "    Cushion                 %s\n", env.formatCushion(a.Cushion, accountValWidth))
	fmt.Fprintln(out)

	// P&L block sign-colours both directions — Unrealized/Realized are
	// performance numbers, distinct from balances above.
	fmt.Fprintln(out, env.dim("  P&L (today)"))
	fmt.Fprintf(out, "    Unrealized              %s\n", env.formatSignedMoneyRight(a.UnrealizedPnL, base, accountValWidth))
	fmt.Fprintf(out, "    Realized                %s\n", env.formatSignedMoneyRight(a.RealizedPnL, base, accountValWidth))
	fmt.Fprintln(out)

	fmt.Fprintln(out, env.dim("  Margin"))
	fmt.Fprintf(out, "    Initial                 %s\n", env.formatMoneyNegCcyRight(a.InitialMargin, base, accountValWidth))
	fmt.Fprintf(out, "    Maintenance             %s\n", env.formatMoneyNegCcyRight(a.MaintenanceMargin, base, accountValWidth))
	fmt.Fprintln(out)

	// Look-ahead block: only when at least one value lands. Cash accounts
	// (and stub gateways pre-handshake) leave the whole quad zero, which
	// would otherwise render as four em-dashes adding noise.
	if a.LookAheadInitMargin != 0 || a.LookAheadMaintMargin != 0 || a.LookAheadAvailable != 0 || a.LookAheadExcess != 0 {
		fmt.Fprintln(out, env.dim("  Look-ahead margin"))
		fmt.Fprintf(out, "    Initial                 %s\n", env.formatMoneyNegCcyRight(a.LookAheadInitMargin, base, accountValWidth))
		fmt.Fprintf(out, "    Maintenance             %s\n", env.formatMoneyNegCcyRight(a.LookAheadMaintMargin, base, accountValWidth))
		fmt.Fprintf(out, "    Available funds         %s\n", env.formatMoneyNegCcyRight(a.LookAheadAvailable, base, accountValWidth))
		fmt.Fprintf(out, "    Excess liquidity        %s\n", env.formatMoneyNegCcyRight(a.LookAheadExcess, base, accountValWidth))
		fmt.Fprintln(out)
	}

	renderCurrencyExposure(env, a)
	fmt.Fprintf(out, "  as of %s\n", formatTimeShort(a.AsOf))
	return 0
}

// formatCushion renders the margin cushion as a 2-decimal ratio with a
// severity badge. Banding follows IBKR's convention: < 0.10 is the
// margin-warning zone (red ⚠), 0.10–0.30 is yellow (watch), > 0.30 is
// comfortable (green ✓). Zero renders as the dim em-dash placeholder so
// pre-handshake state stays distinct from "real cushion of exactly 0",
// which would itself land in the red zone.
func (e *Env) formatCushion(v float64, w int) string {
	if v == 0 {
		return e.dim(padDash(w))
	}
	s := fmt.Sprintf("%.2f", v)
	switch {
	case v < 0.10:
		s += " ⚠"
		s = padLeftVisible(s, w)
		return e.red(s)
	case v < 0.30:
		s = padLeftVisible(s, w)
		return e.yellow(s)
	default:
		s += " ✓"
		s = padLeftVisible(s, w)
		return e.green(s)
	}
}

// formatSignedMoneyRight renders v as money with the supplied currency
// prefix, right-aligned to w visible cells, and colours it green/red/dim
// by sign (positive green, negative red, zero dim) using the signPnL
// rule shared by P&L renderers. Positive values explicitly carry a leading
// "+" so a trader scanning the column never has to deduce direction from
// colour alone (colour-blindness, dim terminals, NO_COLOR).
func (e *Env) formatSignedMoneyRight(v float64, ccy string, w int) string {
	if v == 0 {
		return e.dim(padDash(w))
	}
	s := formatMoneyCcy(v, ccy)
	if v > 0 {
		s = "+" + s
	}
	if pad := w - len(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return e.colorBySign(v, s, signPnL)
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
