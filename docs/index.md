# Read-only Interactive Brokers MCP server and Go TWS API

Updated: 2026-05-24 18:32 CEST

`ibkr` is a local, read-only Interactive Brokers MCP server, terminal CLI,
and Go implementation of the TWS API. It lets Claude Desktop, Claude Code,
Cursor, Continue, Zed, shell scripts, and Go programs inspect IBKR accounts,
positions, quotes, option chains, market scanners, S&P 500 breadth, SPY+SPX
dealer gamma, and risk regime through a running IB Gateway or TWS session.

The data flows out. Orders do not go in. The bundled CLI, daemon, and MCP
server cannot place, modify, or cancel trades.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr setup claude-desktop
```

Prerequisites: a running IB Gateway 10.37+ or TWS session on the same machine,
and an IBKR Pro account with TWS API access.

## Use It From

- Claude Desktop, Claude Code, Cursor, Continue, Zed, or any MCP host via
  `ibkr mcp`
- A shell via commands such as `ibkr status`, `ibkr positions`, `ibkr quote`,
  `ibkr chain`, `ibkr scan`, `ibkr gamma`, and `ibkr regime`
- Go code through `github.com/osauer/ibkr/pkg/ibkr`

## Start Here

- [GitHub repository](https://github.com/osauer/ibkr)
- [README](https://github.com/osauer/ibkr#readme)
- [Agentic use guide](guides/agentic-use.md)
- [MCP tools reference](reference/mcp-tools.md)
- [MCP resources reference](reference/mcp-resources.md)
- [Configuration reference](reference/config.md)
- [Privacy](https://github.com/osauer/ibkr/blob/main/PRIVACY.md)
- [Security](https://github.com/osauer/ibkr/blob/main/SECURITY.md)

## Example Questions

- "What's in my IBKR account?"
- "Show my SPY exposure, including option deltas."
- "What is AAPL trading at?"
- "Are SPY dealers supporting or amplifying today's move?"
- "How does the market regime look today?"
- "If I buy 100 MSFT at 418 with a stop at 408, what's my EUR risk?"

## Safety Boundary

`ibkr` is an independent, third-party client for Interactive Brokers' public
TWS API. It is not built, endorsed, sponsored, or supported by Interactive
Brokers Group, Inc. or its affiliates.
