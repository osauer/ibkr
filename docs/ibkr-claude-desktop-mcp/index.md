# IBKR Claude Desktop MCP setup

Updated: 2026-05-31 21:23 CEST

Use `ibkr mcp` to connect Claude Desktop to Interactive Brokers (IBKR) through a local read-only MCP server. This page is for searches such as "IBKR Claude Desktop MCP", "ibkr claude desktop", "Claude Desktop IBKR MCP", and "connect Claude Desktop to IBKR".

`ibkr` talks to the local IB Gateway or Trader Workstation (TWS) session that is already logged in on your machine. Claude Desktop receives account and market data through stdio MCP tools; it does not receive tools for placing, modifying, or cancelling Interactive Brokers orders.

## IBKR Desktop vs Claude Desktop

IBKR Desktop is Interactive Brokers' own trading application. Claude Desktop is Anthropic's local desktop AI client. This setup is about Claude Desktop reading IBKR data through a local MCP server, not about installing or automating the IBKR Desktop trading platform.

For best results, run IB Gateway or TWS as the local API source. IBKR Desktop is useful as a trading app, but the TWS API socket is the integration path that `ibkr mcp` uses.

## Fast install for Claude Desktop

Download the latest MCP Bundle:

```text
https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

Open the `.mcpb` file with Claude Desktop, install it from settings, then fully quit and relaunch Claude Desktop. Keep IB Gateway or TWS running locally before asking account, portfolio, quote, option, or regime questions.

## Prerequisites

- IB Gateway 10.37 or newer, or Trader Workstation (TWS), running locally.
- An IBKR Pro account with TWS API access.
- API socket access enabled in the local IBKR application.
- Claude Desktop on macOS or Linux for the MCPB path. Windows users can use WSL with the shell install path.

## What Claude Desktop can ask

- "What's in my IBKR account?"
- "Show my SPY exposure, including option deltas."
- "What changed in my portfolio today?"
- "What expiries are listed for NVDA and what is the implied move?"
- "How does the risk regime look today?"
- "If I enter AAPL at 180 with a stop at 175, how many shares fit 1% risk?"

Claude Desktop routes these questions to read-only MCP tools such as `ibkr_account`, `ibkr_positions`, `ibkr_quote`, `ibkr_chain`, `ibkr_regime`, and `ibkr_size`.

## Shell-managed alternative

If you want one shared binary for Claude Desktop, Claude Code, Cursor, Continue, Zed, and terminal use:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr setup claude-desktop
```

Restart Claude Desktop after upgrading the binary. MCP hosts cache the spawned server process until the host exits.

## Safety boundary

`ibkr mcp` is read-only. It exposes Interactive Brokers account, positions, quotes, calendars, options, scanners, breadth, gamma, broad-market regime lifecycle, portfolio canary lifecycle, and position-sizing context. It has no order-entry interface and cannot place, modify, or cancel trades.

## Related pages

- [Claude Desktop Interactive Brokers setup](../claude-desktop-interactive-brokers/) for the broader Claude Desktop setup path.
- [Connect Claude to IBKR](../connect-claude-to-ibkr/) for Claude Desktop and Claude Code setup.
- [IBKR MCP server](../ibkr-mcp/) for the exact local MCP server overview.
- [Interactive Brokers MCP server](../interactive-brokers-mcp-server/) for the full project setup page.
- [MCP tools reference](../reference/mcp-tools.md) for the tool schema Claude routes against.
