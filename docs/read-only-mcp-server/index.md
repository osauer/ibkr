# Read-only MCP server for Interactive Brokers

Updated: 2026-05-27 22:39 CEST

`ibkr mcp` is intentionally a read-only MCP server for Interactive Brokers. It gives AI assistants account and market context from IB Gateway or TWS, but it does not expose a place-order, modify-order, or cancel-order tool.

This page is for searches such as "read-only IBKR MCP server", "safe AI trading assistant Interactive Brokers", "Interactive Brokers MCP no order access", and "portfolio analysis MCP server".

## What read-only means here

Allowed surfaces:

- account summary, cash, margin, buying power, daily P&L
- positions, option legs, marks, and Greek coverage
- quotes, quote freshness, daily bars, and official market calendars
- option chains, expiries, expected moves, scanners, breadth, dealer gamma, and risk regime
- fixed-fractional position sizing as math only

Not exposed:

- no order placement
- no order modification
- no order cancellation
- no hosted broker credential store
- no remote service sitting between the assistant and IBKR

## Why this matters for agents

Agentic workflows are useful for analysis: "what do I own?", "what changed today?", "which positions carry option delta?", "is the market regime stressed?", and "how large is this hypothetical trade?". Those questions need account and market data, not order-entry authority.

`ibkr` keeps that boundary explicit. The MCP tool list is generated from the same source as the CLI documentation, and the public references call out that lifecycle and trading verbs stay outside the MCP surface.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr status
```

For Claude Desktop, install:

```text
https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

For a generic local MCP host:

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

## Good assistant prompts

- "Summarize my IBKR account and positions."
- "Show positions by underlying and include option Greeks."
- "Are any watchlist quotes stale because the market is closed?"
- "How does today's risk regime look?"
- "For this hypothetical entry and stop, what position size risks 1%?"

If the assistant tries to trade through `ibkr mcp`, it has reached outside the tool's capability boundary.

## References

- [IBKR MCP server](../ibkr-mcp/) for the main server overview.
- [Agentic use guide](../guides/agentic-use.md) for example conversations.
- [MCP tools reference](../reference/mcp-tools.md) for the read-only tool inventory.
