# IBKR MCP server

Updated: 2026-05-27 22:28 CEST

`ibkr mcp` is a local, read-only MCP server for Interactive Brokers account and market data. It connects Claude Desktop, Claude Code, Cursor, Zed, Continue, and other stdio MCP hosts to the machine already running IB Gateway or Trader Workstation (TWS).

Use this page when you searched for "IBKR MCP", "ibkr mcp server", or "Interactive Brokers MCP". The short version: install the `ibkr` binary, run it beside your local IBKR session, and expose account, positions, quotes, calendars, options, scanners, breadth, gamma, and risk-regime tools without exposing order entry.

## Best fit

- You want an IBKR MCP server that works locally through stdio.
- You already run IB Gateway or TWS and want Claude or another agent to read account and market context.
- You want a single Go binary instead of a Python bridge, Java jar, hosted broker proxy, or remote custody layer.
- You want account and market analysis, not automated order placement.

## Install

Claude Desktop users can install the MCP Bundle:

```sh
open https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

Shell, Cursor, Continue, Zed, and generic MCP users can install the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr setup claude-desktop
```

Generic MCP configuration:

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

## What the MCP server exposes

- Account summary, buying power, cash, margin, daily P&L, positions, option legs, and Greek coverage.
- Snapshot stock and ETF quotes, daily history, previous close, and quote freshness.
- Official market calendars for US equities, US listed options, and Xetra.
- Option expiries, implied volatility, expected moves, strike grids, deltas, and open interest.
- Market scanners, fixed-fractional position sizing, S&P 500 breadth, SPY+SPX dealer gamma, and an eight-row risk-regime dashboard.
- Streaming stock and ETF quote resources through `ibkr://quote/{symbol}`.

The full schema is in the [MCP tools reference](../reference/mcp-tools.md) and [MCP resources reference](../reference/mcp-resources.md).

## Safety boundary

The bundled CLI and MCP server are read-only. They do not expose tools for placing, modifying, or cancelling Interactive Brokers orders. If a host asks to buy or sell securities through this MCP server, the correct answer is that the server has no order-entry interface.

## Related pages

- [Interactive Brokers MCP server](https://osauer.dev/ibkr/interactive-brokers-mcp-server/) for the broader setup guide.
- [TWS MCP server](https://osauer.dev/ibkr/tws-mcp-server/) for Trader Workstation-specific setup notes.
- [IB Gateway MCP server](https://osauer.dev/ibkr/ib-gateway-mcp/) for headless Gateway-oriented setup.
- [Claude Desktop Interactive Brokers setup](https://osauer.dev/ibkr/claude-desktop-interactive-brokers/) for the Claude MCPB path.
- [Connect Claude to IBKR](https://osauer.dev/ibkr/connect-claude-to-ibkr/) for Claude Desktop and Claude Code setup.
- [Analyze Interactive Brokers portfolio with AI](https://osauer.dev/ibkr/analyze-interactive-brokers-portfolio-with-ai/) for agent-first portfolio questions.
- [Read-only MCP server](https://osauer.dev/ibkr/read-only-mcp-server/) for the safety boundary.
