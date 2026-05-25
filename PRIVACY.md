# Privacy

Last reviewed: 2026-05-25 10:13 CEST

`ibkr` is a local, read-only Interactive Brokers client. It does not run a hosted service, collect telemetry, or send account data to the project maintainer.

## What data the tool can access

When you run `ibkr`, it talks to the IB Gateway or TWS instance that you run locally. Depending on the command, the local daemon can read:

- account identifiers, balances, cash, margin, buying power, and P&L
- positions, market values, option Greeks, option chains, open interest, and quotes
- historical daily bars and scanner results returned by your IBKR gateway
- local configuration paths and daemon health information

The tool is read-only: it does not expose order placement, order cancellation, or trade-modification commands.

## Where data goes

By default, data stays on your machine:

- CLI output is written to your terminal.
- MCP tool results are sent over stdio to the local MCP client that launched `ibkr mcp`.
- The daemon listens on a local Unix-domain socket, not a public TCP port.
- The daemon talks to IB Gateway or TWS over the configured gateway host, normally loopback.

If you paste output into a chat, connect the MCP server through a remote tunnel, or use a third-party MCP host, that host may receive the account and market data returned by the local tool. Review that host's privacy and retention policy before enabling access.

## Local files

`ibkr` may write local operational files:

- daemon logs under `~/.local/state/ibkr/` by default
- caches under `~/.cache/ibkr/`, including contract details, S&P 500 constituent data, breadth history, and gamma snapshots
- local watchlists under `$XDG_DATA_HOME/ibkr/watchlist.json`, falling back to `~/.local/share/ibkr/watchlist.json`
- optional user configuration under `~/.config/ibkr/config.toml`
- optional user-requested regime JSONL logs at paths passed to `ibkr regime --log`
- optional diagnostic wire logs only when explicitly enabled with the diagnostic environment variables documented in [SECURITY.md](./SECURITY.md#diagnostic-data-sensitivity)

These files can contain account-sensitive information. Protect your local user account and avoid sharing logs without redaction.

## Network access

Normal operation requires access to your local IB Gateway or TWS API socket. Additional network access is used only for explicit update or refresh paths:

- `install.sh` and `ibkr update` contact GitHub releases to download release artifacts.
- the S&P 500 constituent refresher fetches Wikipedia's public S&P 500 company list unless disabled in config.

## Deleting data

To remove local runtime state, quit MCP clients, stop the daemon, and delete the relevant local directories:

```sh
pkill -f 'ibkr daemon'
rm -rf ~/.cache/ibkr
rm -rf ~/.local/state/ibkr
```

Delete `~/.config/ibkr/config.toml` if you also want to remove local configuration.

## Contact

Report security issues privately through GitHub Private Vulnerability Reporting as described in [SECURITY.md](./SECURITY.md). For privacy or data-handling questions, open a GitHub issue without account-specific details.
