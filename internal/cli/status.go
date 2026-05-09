package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/osauer/ibkr/internal/rpc"
)

func runStatus(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.HealthResult
	if err := env.Conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
		return fail(env, "status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	out := env.Stdout
	state := "ok"
	if !res.Connected {
		state = "degraded — gateway not connected"
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "ibkrd %s  ·  uptime %s  ·  %s\n", res.DaemonVersion, time.Duration(res.UptimeSeconds)*time.Second, state)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Profile:        %s\n", res.Profile)
	fmt.Fprintf(out, "  Account:        %s\n", nonEmpty(res.Account, "auto-detect"))
	fmt.Fprintf(out, "  Gateway:        %s:%d %s\n", res.GatewayHost, res.GatewayPort, formatTLSField(res))
	fmt.Fprintf(out, "  Client ID:      %d\n", res.ClientID)
	fmt.Fprintf(out, "  Connected:      %v\n", res.Connected)
	if res.Connected {
		fmt.Fprintf(out, "  Server version: %d\n", res.ServerVersion)
		fmt.Fprintf(out, "  Data type:      %s\n", nonEmpty(res.DataType, "live"))
	} else {
		if res.LastError != "" {
			fmt.Fprintf(out, "  Reason:         %s\n", res.LastError)
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  Troubleshooting:")
		fmt.Fprintln(out, "    1. IB Gateway running on the configured host/port?")
		fmt.Fprintln(out, "       (paper = 4002, live = 4001 by IBKR default)")
		fmt.Fprintln(out, "    2. Gateway 10.37+ ships with the API socket disabled. Launch TWS once,")
		fmt.Fprintln(out, "       accept the 'Enable ActiveX and Socket Clients' prompt, restart Gateway.")
		fmt.Fprintln(out, "    3. Daemon log: ~/.local/state/ibkr/ibkrd.log")
	}
	fmt.Fprintln(out)
	return 0
}

// formatTLSField renders the configured + negotiated TLS state, calling out
// fallback when the negotiated mode differs from what config requested.
func formatTLSField(res rpc.HealthResult) string {
	if !res.Connected {
		return fmt.Sprintf("(tls=%v)", res.GatewayTLS)
	}
	if res.GatewayTLS == res.NegotiatedTLS {
		return fmt.Sprintf("(tls=%v)", res.NegotiatedTLS)
	}
	return fmt.Sprintf("(tls=%v, configured=%v ⚠ fallback)", res.NegotiatedTLS, res.GatewayTLS)
}
