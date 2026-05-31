# IBKR MCP TWS setup

Updated: 2026-05-31 21:23 CEST

`ibkr mcp` is a local IBKR MCP TWS bridge: Claude Desktop, Claude Code, Cursor, Zed, Continue, or another stdio MCP host talks to `ibkr mcp`, and `ibkr` reads account and market data from the Trader Workstation (TWS) API socket on the same machine.

Use this page when you searched for "ibkr mcp tws", "IBKR MCP TWS", "IBKR TWS MCP server", or "MCP server for IBKR TWS". The same binary also works with IB Gateway; choose TWS when you already keep Trader Workstation open for charting, manual supervision, or daily trading context.

## Why this one ranks differently from simple Python bridges

Many IBKR MCP search results are small wrappers around a few TWS API calls. `ibkr` is broader:

- A single Go binary with a local daemon, CLI, stdio MCP server, Claude Desktop MCPB, Claude Code plugin, and Go TWS protocol library.
- Persistent local connection management instead of reconnecting for every assistant request.
- Account, positions, quotes, calendars, options, scanners, sizing, breadth, dealer gamma, broad-market stress-lifecycle regime, and portfolio-aware canary lifecycle tools.
- Read-only MCP boundary in the published server: no order placement, order modification, or order cancellation tools.
- JSON-friendly CLI output for non-MCP agent SDKs and shell automation.

## TWS connection model

```text
Claude / Cursor / Zed / other MCP host
  -> ibkr mcp
  -> local ibkr daemon
  -> Trader Workstation API socket
  -> Interactive Brokers account and market data
```

`ibkr` auto-discovers the standard local API ports, including TWS paper and live ports. You can pin host, port, client ID, account, and TLS settings in local config when auto-discovery is not right for your workstation.

## Install

Claude Desktop users can install the MCP Bundle:

```sh
open https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

Shell, Cursor, Continue, Zed, and generic MCP hosts can install the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr status
ibkr setup claude-desktop
```

Generic MCP config:

```json
{
  "mcpServers": {
    "ibkr": {
      "command": "/ABSOLUTE/PATH/TO/ibkr",
      "args": ["mcp"]
    }
  }
}
```

The `command` must be an absolute path because MCP hosts usually do not expand `~` or read your interactive shell's `PATH`.

## TWS prerequisites

In Trader Workstation, enable API socket access:

- Enable ActiveX and Socket Clients.
- Use the configured TWS API port: commonly `7497` for paper and `7496` for live.
- Keep TWS running on the same machine as `ibkr`.

The published `ibkr mcp` server does not need TWS order entry permissions because it exposes no order-entry tool surface.

## What to ask through MCP

- "What's in my IBKR account?"
- "Show my SPY exposure, including option deltas."
- "What expiries are listed for NVDA and what is the implied move?"
- "Quote AAPL and MSFT and warn me if the data is delayed or stale."
- "How does the market regime look today?"
- "Run the portfolio canary and tell me whether this is quiet, watch, act, rebalance, opportunity, or blocked on data quality."
- "If I enter AAPL at 207.50 with a stop at 202.50 and risk 1%, what size fits?"

## TWS or IB Gateway?

Use TWS when the desktop platform is already part of your workflow. Use IB Gateway when you want a lighter headless API process. The `ibkr mcp` tools are the same in both cases; only the local IBKR process behind the API socket changes.

## Related pages

- [IBKR MCP server](../ibkr-mcp/) for the main MCP setup page.
- [TWS MCP server](../tws-mcp-server/) for Trader Workstation-specific setup notes.
- [Interactive Brokers MCP server](../interactive-brokers-mcp-server/) for the broader comparison and setup guide.
- [IB Gateway MCP server](../ib-gateway-mcp/) for the headless Gateway path.
- [MCP tools reference](../reference/mcp-tools.md) for exact tool names, parameters, and descriptions.
