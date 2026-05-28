# Claude Desktop Interactive Brokers setup

Updated: 2026-05-28 04:47 CEST

Claude Desktop can read Interactive Brokers account and market context through the local `ibkr` MCP server. The recommended path is the MCP Bundle (`.mcpb`) release asset, which packages the `ibkr` binary for Claude Desktop and runs it locally through stdio.

This page is for searches such as "Claude Desktop Interactive Brokers", "Claude Desktop IBKR MCP", "IBKR Claude Desktop MCP", and "connect Claude to IBKR".

If search results show Interactive Brokers' IBKR Desktop trading platform, that is a different product. This page is about Claude Desktop using the local `ibkr mcp` server through IB Gateway or TWS.

## Fast path: MCP Bundle

Download the latest bundle:

```text
https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb
```

Open the `.mcpb` file with Claude Desktop, drag it into Claude Desktop, or install it from Claude Desktop settings. Fully quit and relaunch Claude Desktop after installation so it starts the bundled server process.

## Prerequisites

- IB Gateway 10.37 or newer, or Trader Workstation (TWS), running locally.
- An IBKR Pro account with TWS API access.
- API socket access enabled in the local IBKR application.
- macOS or Linux for the bundled binary. Windows users can use WSL with the shell install path.

## What Claude can ask through ibkr

- "What's in my IBKR account?"
- "Show my AAPL exposure, including option deltas."
- "What expiries are listed for SPY and what is the implied move?"
- "Is Xetra open on Whit Monday?"
- "Are dealers supporting or amplifying today's SPY move?"
- "If I enter MSFT at 418 with a stop at 408, what is my EUR risk?"

Claude receives structured account and market data from `ibkr mcp`; it does not receive an order-entry tool.

## Shell-managed alternative

If you want one shared shell-managed binary for Claude Desktop, Cursor, Continue, Zed, and terminal use:

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr setup claude-desktop
```

Restart Claude Desktop after upgrading the binary. MCP hosts cache the spawned server process until the host exits.

## References

- [IBKR MCP server](https://osauer.dev/ibkr/ibkr-mcp/) for the exact local MCP server overview.
- [IBKR Claude Desktop MCP setup](https://osauer.dev/ibkr/ibkr-claude-desktop-mcp/) for the exact IBKR Claude Desktop MCP query.
- [Interactive Brokers MCP server](https://osauer.dev/ibkr/interactive-brokers-mcp-server/) for the full project setup page.
- [MCP tools reference](../reference/mcp-tools.md) for the tool schema Claude routes against.
