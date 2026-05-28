# Connect Claude to IBKR

Updated: 2026-05-28 04:47 CEST

Use `ibkr mcp` to connect Claude Desktop or Claude Code to Interactive Brokers without giving Claude order access. The server runs locally, talks to IB Gateway or Trader Workstation (TWS), and exposes read-only account and market-data tools over MCP.

This page is for searches such as "connect Claude to IBKR", "connect Claude to Interactive Brokers", "Claude Code IBKR MCP", "IBKR Claude Desktop MCP", and "Claude Desktop IBKR portfolio analysis".

## Claude Desktop

The shortest path is the MCP Bundle from the latest release:

```text
https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

Open the `.mcpb` file with Claude Desktop, install it from settings, then fully quit and relaunch Claude Desktop. Keep IB Gateway or TWS running locally before asking account or market questions.

## Claude Code

Install the Claude Code plugin from the self-hosted marketplace:

```text
/plugin marketplace add osauer/ibkr
/plugin install ibkr@ibkr
```

For Claude for Mac's embedded Claude Code pane, run the equivalent commands from a regular terminal:

```sh
claude plugin marketplace add osauer/ibkr
claude plugin install ibkr@ibkr
```

The Claude Code plugin carries the skill, hooks, and manifest. It does not ship the `ibkr` binary, so install the binary separately:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
```

## What Claude can ask

- "What's in my IBKR account?"
- "Show my SPY exposure, including option deltas."
- "What changed in my portfolio today?"
- "What expiries are listed for NVDA and what is the implied move?"
- "How does the risk regime look today?"
- "If I enter AAPL at 180 with a stop at 175, how many shares fit 1% risk?"

Claude will choose tools such as `ibkr_account`, `ibkr_positions`, `ibkr_quote`, `ibkr_chain`, `ibkr_regime`, and `ibkr_size` from the MCP descriptions.

## Safety boundary

`ibkr mcp` has no order-entry tools. Claude can read account and market context, size a hypothetical plan, and explain stale data; it cannot place, modify, or cancel Interactive Brokers orders through this server.

## Related pages

- [Claude Desktop Interactive Brokers setup](../claude-desktop-interactive-brokers/) for the MCPB path.
- [IBKR Claude Desktop MCP setup](../ibkr-claude-desktop-mcp/) for the exact IBKR Claude Desktop MCP query.
- [Agentic use guide](../guides/agentic-use.md) for more example conversations.
- [MCP tools reference](../reference/mcp-tools.md) for exact tool names and schemas.
