# Analyze an Interactive Brokers portfolio with AI

Updated: 2026-05-29 12:28 CEST

`ibkr` lets an AI assistant analyze an Interactive Brokers portfolio from live local account and market data. Claude Desktop, Claude Code, Cursor, Zed, Continue, or another MCP host calls `ibkr mcp`; `ibkr` reads IB Gateway or TWS; the assistant receives structured responses for portfolio review, exposure mapping, options diagnostics, market-regime checks, and next-review workflows.

This page is for searches such as "analyze Interactive Brokers portfolio with AI", "AI assistant for IBKR portfolio analysis", "Claude IBKR positions options analysis", and "natural language TWS API portfolio analysis".

## Portfolio questions it handles

- "Summarize my account: cash, buying power, margin, and today's P&L."
- "Show my largest single-name exposures."
- "Group my positions by underlying and include option deltas."
- "Which holdings on my watchlist have stale prices?"
- "What is my SPY exposure after options?"
- "What are the next option expiries and implied moves for my top holdings?"
- "How does today's risk regime affect this portfolio?"
- "Should I hold, watch, de-lever, or liquidate risk if stress is evident?"

The assistant does not need screenshots or copied tables. It can call MCP tools and receive JSON for account, positions, quotes, calendars, option chains, history, scanners, breadth, gamma, risk-regime context, and the portfolio canary. The current bundled MCP surface is read-side only, which makes it suitable for analysis, review, stress checks, and plan sizing without exposing order-entry tools.

## Why this is better than copy-paste analysis

Manual portfolio analysis usually means pasting account balances, positions, and option chains into a chat. That is slow, stale, and easy to redact incorrectly. `ibkr mcp` keeps the broker connection local and returns only the data requested for the current question.

The host still receives account-sensitive data when you ask it to analyze your account. Use a host and model policy you trust. The `ibkr` side of the broker connection stays local, and the current public MCP surface is limited to analysis and sizing.

## Example assistant flow

User:

```text
How does my IBKR portfolio look today? Call out option delta and any stale prices.
```

Likely MCP calls:

```text
ibkr_account
ibkr_positions
ibkr_watch
ibkr_regime
ibkr_canary
```

Expected answer shape:

- account-level cash, buying power, margin, and daily P&L
- largest position and underlying exposures
- option Greeks rolled up by underlying where available
- stale, frozen, delayed, or closed-market quote warnings
- current risk-regime context when relevant

For a fuller semi-professional portfolio-review workflow, use the prompt in [examples/ibkr_portfolio_analysis_prompt.md](https://github.com/osauer/ibkr/blob/main/examples/ibkr_portfolio_analysis_prompt.md).

For scheduled stress monitoring, use [examples/ibkr_portfolio_canary_prompt.md](https://github.com/osauer/ibkr/blob/main/examples/ibkr_portfolio_canary_prompt.md).

For the Claude-specific workflow, including a sanitized sample review and a shorter paste-ready prompt, see [Portfolio review with Claude and IBKR](../portfolio-review-with-claude-ibkr/).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr status
```

Claude Desktop users can use the MCPB:

```text
https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

For other MCP hosts:

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

## Controlled execution boundary

The bundled CLI and MCP server do not expose order placement, order modification, or order cancellation. The sizing tool performs math against net liquidation value; it does not submit an order ticket.

## References

- [Agentic use guide](../guides/agentic-use.md) for natural-language examples.
- [MCP tools reference](../reference/mcp-tools.md) for exact schemas.
- [Concepts](../concepts.md) for regime, dealer gamma, and breadth interpretation.
