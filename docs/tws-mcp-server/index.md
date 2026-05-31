# TWS MCP server for Interactive Brokers

Updated: 2026-05-31 21:23 CEST

`ibkr` can act as a TWS MCP server by connecting a local MCP host to the Interactive Brokers Trader Workstation socket API. Claude Desktop, Claude Code, Cursor, Zed, Continue, and other MCP clients talk to `ibkr mcp` over stdio; `ibkr` talks to TWS through the local TWS API port. If your exact search was "ibkr mcp tws", the [IBKR MCP TWS setup](../ibkr-mcp-tws/) page is the shortest path.

This is useful when you keep TWS open for trading, charting, or manual supervision and want an assistant to read account and market context from the same local session.

## How the connection works

```text
MCP host -> ibkr mcp -> local ibkr daemon -> TWS API socket -> Interactive Brokers account data
```

`ibkr` auto-discovers the standard local ports, including TWS paper and live ports. You can also pin host, port, client ID, account, and TLS settings in the local config file when auto-discovery is not what you want.

## TWS versus IB Gateway

Use TWS when you already want the desktop platform open. Use IB Gateway when you want a lighter background process for API access. The MCP surface is the same either way: read-only account, positions, quotes, calendars, option chains, scanners, sizing, breadth, gamma, broad-market regime lifecycle, and portfolio canary lifecycle tools.

## Install for an MCP host

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr status
ibkr setup claude-desktop
```

For a generic MCP host:

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

The command must be an absolute path because MCP hosts usually do not expand `~` or consult your interactive shell's `PATH`.

## What to ask

- "What's in my IBKR account?"
- "Show my SPY exposure, including option deltas."
- "What expiries are listed for NVDA and what is the implied move?"
- "Why does this quote look stale after the market close?"
- "How does the risk regime look today?"

For exact tool names and parameters, see the [MCP tools reference](../reference/mcp-tools.md).

## Read-only by design

The TWS connection can expose sensitive brokerage state. `ibkr mcp` keeps the interface narrow: it has no order-entry, order-modification, or order-cancellation tool.
