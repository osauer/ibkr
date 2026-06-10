package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

func runTrading(ctx context.Context, env *Env, args []string) int {
	sub := "status"
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		return runTradingStatus(ctx, env, args)
	}
	if len(args) > 0 && args[0] != "status" {
		sub = args[0]
		args = args[1:]
	} else if len(args) > 0 {
		args = args[1:]
	}
	switch sub {
	case "status":
		return runTradingStatus(ctx, env, args)
	default:
		return fail(env, "trading: unknown subcommand %q (try `ibkr trading status`)", sub)
	}
}

func runTradingStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "trading status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.TradingStatus
	if err := env.Conn.Call(ctx, rpc.MethodTradingStatus, nil, &res); err != nil {
		return fail(env, "trading status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderTradingStatusText(env, &res)
	if res.Blocked {
		return 1
	}
	return 0
}

func renderTradingStatusText(env *Env, st *rpc.TradingStatus) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Trading  %s\n", env.statusBadge(tradingStatusVerdict(*st)))
	fmt.Fprintln(out)
	statusRow(env, out, "Mode", formatTradingMode(env, *st))
	statusRow(env, out, "Endpoint", nonEmpty(st.Endpoint, "auto-detect"))
	statusRow(env, out, "Account", nonEmpty(st.Account, "auto-detect")+" ("+nonEmpty(st.AccountOrigin, "auto")+")")
	statusRow(env, out, "Client ID", fmt.Sprintf("%d (%s)", st.ClientID, nonEmpty(st.ClientIDOrigin, "default")))
	statusRow(env, out, "MCP trading", nonEmpty(st.MCPTrading, rpc.TradingMCPDisabled))
	statusRow(env, out, "Capabilities", formatTradingCapabilities(*st))
	statusRow(env, out, "Open orders", fmt.Sprint(st.OpenOrders))
	if st.LastOrderEvent != "" {
		statusRow(env, out, "Last event", st.LastOrderEvent)
	}
	if st.Mode == config.TradingModeLive {
		statusRow(env, out, "Live override", nonEmpty(st.LiveOverride, rpc.TradingLiveOverrideBlocked))
		if st.PaperSmoke != "" {
			statusRow(env, out, "Paper smoke", formatPaperSmokeValue(*st))
		}
	}
	if len(st.Blockers) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Blockers:")
		for _, b := range st.Blockers {
			fmt.Fprintf(out, "  - %s: %s\n", b.Code, b.Message)
			if b.Action != "" {
				fmt.Fprintf(out, "    action: %s\n", b.Action)
			}
		}
	}
	if len(st.WriteBlockers) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Write blockers:")
		for _, b := range st.WriteBlockers {
			fmt.Fprintf(out, "  - %s: %s\n", b.Code, b.Message)
			if b.Action != "" {
				fmt.Fprintf(out, "    action: %s\n", b.Action)
			}
		}
	}
	fmt.Fprintln(out)
}

func formatTradingCapabilities(st rpc.TradingStatus) string {
	return fmt.Sprintf("preview=%v write=%v", st.CanPreview, st.CanWrite)
}

func formatPaperSmokeValue(st rpc.TradingStatus) string {
	value := st.PaperSmoke
	if st.PaperSmokeAt != nil && !st.PaperSmokeAt.IsZero() {
		value += " at " + st.PaperSmokeAt.Format(time.RFC3339)
	}
	if st.PaperSmokeAccount != "" || st.PaperSmokeEndpoint != "" || st.PaperSmokeClientID != 0 {
		value += fmt.Sprintf(" (%s via %s, client %d)", nonEmpty(st.PaperSmokeAccount, "unknown-account"), nonEmpty(st.PaperSmokeEndpoint, "unknown-endpoint"), st.PaperSmokeClientID)
	}
	if st.PaperSmokeMaxAge != "" {
		value += "; max age " + st.PaperSmokeMaxAge
	}
	return value
}

func tradingStatusVerdict(st rpc.TradingStatus) statusConcern {
	switch {
	case st.Mode == config.TradingModeDisabled:
		return statusConcern{Text: "DISABLED", Level: statusConcernNotice}
	case st.Blocked:
		return statusConcern{Text: "BLOCKED", Level: statusConcernWarn}
	default:
		return statusConcern{Text: "READY", Level: statusConcernNone}
	}
}

func formatTradingMode(env *Env, st rpc.TradingStatus) string {
	if st.Mode == config.TradingModeDisabled {
		return env.dim(config.TradingModeDisabled)
	}
	if st.Blocked {
		return env.yellow(st.Mode + " blocked")
	}
	if st.Mode == config.TradingModeLive {
		return env.yellow(st.Mode + " ready")
	}
	return env.green(nonEmpty(st.Mode, "unknown") + " ready")
}

func formatTradingStatusValue(env *Env, st rpc.TradingStatus) string {
	if st.Mode == "" {
		return env.dim("unknown")
	}
	if st.Mode == config.TradingModeDisabled {
		return env.dim("disabled")
	}
	if st.Blocked {
		msg := "blocked"
		if len(st.Blockers) > 0 {
			msg += ": " + st.Blockers[0].Message
		}
		return env.yellow(st.Mode + " " + msg)
	}
	if st.Mode == config.TradingModeLive {
		return env.yellow("live ready")
	}
	return env.green(st.Mode + " ready")
}
