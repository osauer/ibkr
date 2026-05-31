# Best IBKR MCP server for Claude Code

Updated: 2026-05-31 21:23 CEST

Use `ibkr mcp` when you want Claude Code to read Interactive Brokers account and market data without giving the agent order-entry authority. This page is for searches such as "best MCP server to connect IBKR API to Claude Code", "best IBKR MCP server for Claude Code", "Claude Code IBKR API", and "read-only IBKR MCP server".

The short answer: choose a local MCP server that connects to IB Gateway or Trader Workstation (TWS), exposes explicit read-only tools, and makes order placement impossible at the tool boundary. `ibkr` is designed around that boundary.

## Why read-only matters

Claude Code is powerful enough to inspect files, run commands, and use MCP tools. That is useful for portfolio analysis, but it is the wrong default for unsupervised brokerage order placement.

`ibkr mcp` exposes account, positions, quotes, calendars, options, scanners, breadth, gamma, broad-market regime lifecycle, portfolio canary lifecycle, and sizing tools. It does not expose tools for placing, modifying, or cancelling Interactive Brokers orders. If Claude asks to trade through this MCP server, the correct answer is that no order-entry interface exists.

## Best fit

- You want Claude Code to answer "what do I own?" and "what changed?" from live IBKR account context.
- You want local stdio MCP, not a hosted broker proxy.
- You already run IB Gateway or TWS with API socket access enabled.
- You want a single Go binary with no Python runtime, Java bridge, Docker stack, or remote custody layer.
- You want portfolio, options, scanner, regime-lifecycle, and canary-lifecycle analysis, not automated trade execution.

## Claude Code install path

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

Install the local `ibkr` binary separately:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
```

Restart Claude Code or Claude for Mac after installation so the host reloads plugins, skills, hooks, and MCP server processes.

## Generic Claude Code MCP config

If you do not want the plugin path, add the local stdio MCP server directly:

```sh
claude mcp add ibkr -- /ABSOLUTE/PATH/TO/ibkr mcp
```

The command path must be absolute. Use `which ibkr` after installing the binary.

## What Claude Code can ask

- "What's in my IBKR account?"
- "Show my positions by underlying and include option deltas."
- "What changed in my portfolio today?"
- "Which SPY expiries are listed and what is the implied move?"
- "How does the risk regime look today?"
- "If I enter AAPL at 180 with a stop at 175, how many shares fit 1% risk?"

## How to evaluate IBKR MCP servers

| Question | Why it matters |
| --- | --- |
| Does it expose order tools? | Read-only is the safer default for Claude Code and live brokerage data. |
| Does it run locally? | IB Gateway and TWS are local API sessions; local stdio avoids a remote custody or broker proxy layer. |
| Does it handle account and market context? | Useful portfolio analysis needs positions, quotes, options, calendars, and freshness metadata. |
| Does it have clear MCP tool descriptions? | Claude routes tool calls from descriptions, so vague schemas lead to bad tool selection. |
| Does it fail clearly when TWS or IB Gateway is unavailable? | Agents need actionable connection state before they can explain missing data. |

## Related pages

- [IBKR MCP server](../ibkr-mcp/) for the exact local MCP server overview.
- [Connect Claude to IBKR](../connect-claude-to-ibkr/) for Claude Desktop and Claude Code setup.
- [Read-only MCP server](../read-only-mcp-server/) for the no-order-entry boundary.
- [Analyze Interactive Brokers portfolio with AI](../analyze-interactive-brokers-portfolio-with-ai/) for portfolio analysis prompts.
- [MCP tools reference](../reference/mcp-tools.md) for the tool schema Claude routes against.
