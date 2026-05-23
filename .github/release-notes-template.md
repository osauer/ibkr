**The data flows out. Orders don't go in.**

`ibkr` is a read-only interface to your Interactive Brokers account, reachable from a Go library, a shell CLI, a stdio MCP server (Claude Desktop, Cursor, Continue, Zed), and a Claude Code plugin. Hand it to an assistant, a cron job, a notebook, or your own service.

## What's new in __VERSION__

__HIGHLIGHTS__

## Install in two commands

~~~sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr setup claude-desktop
~~~

The first command picks the right binary for your platform, verifies its SHA-256, installs it to `~/.local/bin/ibkr`, and adds that directory to your `PATH` if needed. On macOS it also clears the Gatekeeper quarantine flag. The second command writes the MCP server entry into Claude Desktop's config; fully quit Claude Desktop (⌘Q on macOS) and reopen.

If you only want the shell tool, stop after the first command and try:

~~~sh
ibkr account
ibkr quote AAPL
ibkr positions --by underlying
~~~

**Prerequisite**: a running [IB Gateway 10.37+](https://www.interactivebrokers.com/en/trading/ib-gateway.php) or TWS (paper or live) on the same machine. The daemon auto-discovers it across the four standard ports.

See the [README](https://github.com/osauer/ibkr#readme) for the full feature menu and the troubleshooting matrix. Read-only by construction; the [Safety](https://github.com/osauer/ibkr#safety) section walks through the four guards.

---

### Paranoid? Inspect the installer before running it

~~~sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh -o install.sh
less install.sh
sh install.sh
~~~

### Doing something custom?

- **`go install`**: `go install github.com/osauer/ibkr/cmd/ibkr@__VERSION__` (or `@latest`).
- **Different install dir**: `IBKR_INSTALL_DIR=/usr/local/bin sh install.sh`. The installer won't touch your shell rc when you override; manage PATH yourself.
- **Manual download**: pick a tarball from the Assets section below. Verify against `SHA256SUMS`.
- **Cursor / Continue / Zed / other local MCP clients**: see [Pick your path](https://github.com/osauer/ibkr#claude-desktop-cursor-continue-zed) in the README for the JSON snippet (config file path differs per client).
- **Claude Code**: `/plugin marketplace add osauer/ibkr` then `/plugin install ibkr@ibkr` inside any session.

Windows isn't supported. The daemon uses Unix-only primitives (`setsid`, `flock`, AF_UNIX sockets). WSL works.

---
