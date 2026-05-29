# IBKR MCP server

Updated: 2026-05-29 12:28 CEST

`ibkr mcp` is a local MCP server for agentic Interactive Brokers portfolio analysis and trading research. It connects Claude Desktop, Claude Code, Cursor, Zed, Continue, and other stdio MCP hosts to the machine already running IB Gateway or Trader Workstation (TWS).

Use this page when you searched for "IBKR MCP", "ibkr mcp server", or "Interactive Brokers MCP". The short version: install the `ibkr` binary, run it beside your local IBKR session, and expose account, positions, quotes, calendars, options, scanners, breadth, gamma, risk-regime, portfolio-canary, and sizing context to an AI workflow. The bundled MCP surface is read-side only, so agents can review and size plans without receiving order-entry tools.

## Best fit

- You want an IBKR MCP server that works locally through stdio.
- You already run IB Gateway or TWS and want Claude or another agent to analyze account and market context.
- You want a single Go binary instead of a Python bridge, Java jar, hosted broker proxy, or remote custody layer.
- You want portfolio intelligence, market research, risk checks, and controlled trading-workflow analysis before any separate execution step.

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
- Market scanners, fixed-fractional position sizing, S&P 500 breadth, SPY+SPX dealer gamma, an eight-row risk-regime dashboard, and the `ibkr_canary` scheduled stress-check tool.
- Streaming stock and ETF quote resources through `ibkr://quote/{symbol}`.

The full schema is in the [MCP tools reference](../reference/mcp-tools.md) and [MCP resources reference](../reference/mcp-resources.md).

For a scheduled canary workflow that returns only concrete stress-check action rows, use [examples/ibkr_portfolio_canary_prompt.md](https://github.com/osauer/ibkr/blob/main/examples/ibkr_portfolio_canary_prompt.md).

## Current execution boundary

The current bundled CLI and MCP server expose analysis and sizing tools. They do not expose tools for placing, modifying, or cancelling Interactive Brokers orders. If a host asks to buy or sell securities through this MCP server, the correct answer is that the current public server has no order-entry interface.

## Related pages

- [Interactive Brokers MCP server](https://osauer.dev/ibkr/interactive-brokers-mcp-server/) for the broader setup guide.
- [TWS MCP server](https://osauer.dev/ibkr/tws-mcp-server/) for Trader Workstation-specific setup notes.
- [IB Gateway MCP server](https://osauer.dev/ibkr/ib-gateway-mcp/) for headless Gateway-oriented setup.
- [Claude Desktop Interactive Brokers setup](https://osauer.dev/ibkr/claude-desktop-interactive-brokers/) for the Claude MCPB path.
- [IBKR Claude Desktop MCP setup](https://osauer.dev/ibkr/ibkr-claude-desktop-mcp/) for the exact IBKR Claude Desktop MCP query.
- [Connect Claude to IBKR](https://osauer.dev/ibkr/connect-claude-to-ibkr/) for Claude Desktop and Claude Code setup.
- [Best IBKR MCP server for Claude Code](https://osauer.dev/ibkr/best-ibkr-mcp-server-claude-code/) for the Claude Code comparison and safety query.
- [Analyze Interactive Brokers portfolio with AI](https://osauer.dev/ibkr/analyze-interactive-brokers-portfolio-with-ai/) for agent-first portfolio questions.
- [Portfolio review with Claude and IBKR](https://osauer.dev/ibkr/portfolio-review-with-claude-ibkr/) for a prompt-driven portfolio review workflow.
- [Read-only MCP server](https://osauer.dev/ibkr/read-only-mcp-server/) for the current safety boundary.
