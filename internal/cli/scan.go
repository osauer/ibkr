package cli

import (
	"context"
	"fmt"

	"github.com/osauer/ibkr/internal/rpc"
)

func runScan(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "scan")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	limit := fs.Int("limit", 0, "max rows (0 = preset default)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fail(env, "scan: usage: ibkr scan <preset> | ibkr scan list")
	}
	if rest[0] == "list" {
		return runScanList(ctx, env, *jsonOut)
	}
	preset := rest[0]
	params := rpc.ScanRunParams{Preset: preset, Limit: *limit}
	var res rpc.ScanResult
	if err := env.Conn.Call(ctx, rpc.MethodScanRun, params, &res); err != nil {
		return fail(env, "scan: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderScanText(env, &res)
}

func runScanList(ctx context.Context, env *Env, jsonOut bool) int {
	var res rpc.ScanListResult
	if err := env.Conn.Call(ctx, rpc.MethodScanList, nil, &res); err != nil {
		return fail(env, "scan list: %v", err)
	}
	if jsonOut {
		return printJSON(env, res)
	}
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  PRESET                TYPE                    EXCHANGE             LIMIT")
	for _, p := range res.Presets {
		fmt.Fprintf(out, "  %-20s  %-22s  %-18s   %d\n", p.Name, p.Type, p.Exchange, p.Limit)
	}
	fmt.Fprintln(out)
	return 0
}

func renderScanText(env *Env, r *rpc.ScanResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Scan: %s (%s)  ·  %d rows  ·  as of %s\n",
		r.Preset, r.Type, len(r.Rows), formatTimeShort(r.AsOf))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  RANK  SYMBOL    NOTE")
	for _, row := range r.Rows {
		fmt.Fprintf(out, "  %-4d  %-9s %s\n", row.Rank, row.Symbol, row.Comment)
	}
	fmt.Fprintln(out)
	return 0
}
