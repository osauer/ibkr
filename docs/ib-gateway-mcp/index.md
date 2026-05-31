# IB Gateway MCP server

Updated: 2026-05-31 21:23 CEST

`ibkr mcp` is an IB Gateway MCP server for users who want a local, headless Interactive Brokers data bridge for AI assistants. It uses the same local socket API that IB Gateway exposes for ordinary TWS API clients, then presents the read-only data surface as MCP tools.

Use this setup for Claude Desktop, Claude Code, Cursor, Zed, Continue, and other MCP hosts when IB Gateway is the process you keep running for account and market-data access.

## Gateway prerequisites

- IB Gateway 10.37 or newer, running on the same machine.
- An IBKR Pro account with TWS API access.
- API socket access enabled in IB Gateway.
- The paper or live Gateway port reachable on loopback.

`ibkr` probes the standard Gateway and TWS ports by default. If your setup uses a custom port, write a local config file and pin only the fields you need.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr status
```

Claude Desktop users can install the MCP Bundle from the latest release:

```text
https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

Other MCP hosts can launch:

```json
{
  "mcpServers": {
    "ibkr": {
      "command": "/Users/you/.local/bin/ibkr",
      "args": ["mcp"]
    }
  }
}
```

## What Gateway-backed MCP tools can read

- Account summary, positions, daily P&L, buying power, cash, and margin.
- Stock and ETF quotes, quote freshness, and daily history.
- Official market calendars and stale-quote session context.
- Option chains, expiries, expected moves, deltas, and open interest.
- Market scanners, sizing math, S&P 500 breadth, dealer gamma, broad-market regime lifecycle, and portfolio canary lifecycle.

See the [agentic use guide](../guides/agentic-use.md) for example questions and [MCP tools reference](../reference/mcp-tools.md) for the exact schema.

## Why local Gateway matters

Brokerage data should not need a hosted middle layer just to answer account and market questions. `ibkr` keeps the broker connection local, shares one daemon between shell and MCP clients, and does not expose order-entry tools.
