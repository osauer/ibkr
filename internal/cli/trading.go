package cli

import (
	"context"
	"fmt"
	"time"
)

type tradingStatusResult struct {
	Enabled         bool      `json:"enabled"`
	Mode            string    `json:"mode"`
	LocalGate       string    `json:"local_gate"`
	BrokerGate      string    `json:"broker_gate"`
	PreviewRequired bool      `json:"preview_required"`
	Blocked         bool      `json:"blocked"`
	Blockers        []string  `json:"blockers"`
	AsOf            time.Time `json:"as_of"`
}

func runTrading(_ context.Context, env *Env, args []string) int {
	sub := "status"
	if subIdx := tradingSubcommandIndex(args); subIdx >= 0 {
		sub = args[subIdx]
		args = append(append([]string{}, args[:subIdx]...), args[subIdx+1:]...)
	}
	if sub != "status" {
		return fail(env, "trading: unknown subcommand %q (try `ibkr trading status`)", sub)
	}
	return runTradingStatus(env, args)
}

func tradingSubcommandIndex(args []string) int {
	for i, arg := range args {
		if arg == "status" {
			return i
		}
	}
	return -1
}

func runTradingStatus(env *Env, args []string) int {
	fs := flagSet(env, "trading status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	res := tradingStatusResult{
		Enabled:         false,
		Mode:            "read-only-default",
		LocalGate:       "disabled",
		BrokerGate:      "whatif_unavailable",
		PreviewRequired: true,
		Blocked:         true,
		Blockers: []string{
			"default build exposes risk-plan candidate preview diagnostics only",
			"broker WhatIf and order writes require an explicitly enabled trading build",
			"MCP order submission is not exposed",
		},
		AsOf: time.Now(),
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderTradingStatus(env, res)
	return 1
}

func renderTradingStatus(env *Env, res tradingStatusResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "IBKR Trading  %s\n", env.statusBadge(statusConcern{Text: "DISABLED", Level: statusConcernNotice}))
	statusRow(env, out, "Local gate", res.LocalGate)
	statusRow(env, out, "Broker gate", res.BrokerGate)
	statusRow(env, out, "Preview req", fmt.Sprint(res.PreviewRequired))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Blockers:")
	for _, blocker := range res.Blockers {
		fmt.Fprintf(out, "  - %s\n", blocker)
	}
	fmt.Fprintln(out)
}
