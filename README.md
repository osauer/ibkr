# ibkr - IBKR MCP server for TWS and IB Gateway

[![ci](https://github.com/osauer/ibkr/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/osauer/ibkr/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/osauer/ibkr?display_name=tag&sort=semver)](https://github.com/osauer/ibkr/releases/latest)
[![go.mod](https://img.shields.io/github/go-mod/go-version/osauer/ibkr)](go.mod)
[![Go reference](https://pkg.go.dev/badge/github.com/osauer/ibkr.svg)](https://pkg.go.dev/github.com/osauer/ibkr)
[![license](https://img.shields.io/github/license/osauer/ibkr)](LICENSE)

[MCP tools](docs/reference/mcp-tools.md) · [MCP resources](docs/reference/mcp-resources.md) · [Configuration](docs/reference/config.md) · [Agentic use](docs/guides/agentic-use.md) · [Mobile app](web/app/README.md)

**Agentic portfolio analysis and trading-research workflows for IBKR MCP, TWS, and IB Gateway.**

`ibkr` turns your local IB Gateway or TWS session into structured account and market context for the terminal, Claude Desktop, Claude Code, Cursor, Continue, Zed, and other MCP hosts. It is the local `ibkr mcp` TWS bridge for portfolio review, exposure mapping, options diagnostics, market-regime checks, scanner-driven research, watchlist monitoring, and position-sizing math.

For MCP users, `ibkr mcp` is a local IBKR workflow layer for semi-professional retail traders who want agentic portfolio and trading-research analysis on live broker data. The bundled MCP surface is deliberately read-side only: it can analyze and size plans, but it cannot place, modify, or cancel orders.

Use it from a shell:

```sh
ibkr status
ibkr positions --by underlying
ibkr regime
ibkr canary
ibkr watch IBM --add
ibkr watch
ibkr calendar --market us --date 2026-05-25
ibkr quote SPY --watch
ibkr size --symbol AAPL --entry 207.50 --stop 202.50 --risk-pct 1
```

Or connect it to Claude Desktop, Claude Code, Cursor, Continue, Zed, or any MCP host and ask:

> "What's in my IBKR account?"
>
> "Review my portfolio and rank the risks I should look at today."
>
> "Show my AAPL exposure, including option deltas."
>
> "How does the market regime look today?"
>
> "Should I hold, watch, de-lever, or liquidate risk?"
>
> "Is Xetra open on Whit Monday?"
>
> "If I buy 100 MSFT at 418 with a stop at 408, what's my EUR risk?"

Your account data stays on the machine running IB Gateway or TWS unless you choose to send it to an MCP host. The project ships as one Go binary with a CLI, a local MCP server, and a Go library. No Python runtime, Java runtime, or hosted service is required.

**Contents** — [Install](#install) · [What you get](#what-you-get) · [Pick your path](#pick-your-path) · [How it works](#how-it-works) · [Configure](#configure) · [Safety](#safety) · [Other install paths](#other-install-paths) · [Troubleshooting](#troubleshooting)

## Install

**Prerequisites.** A running [IB Gateway](https://www.interactivebrokers.com/en/trading/ibgateway-stable.php) 10.37+ or TWS (paper or live) on the same machine. Auto-discovered on the four standard ports. An **IBKR Pro** account (IBKR Lite cannot use the TWS API).

### Claude Desktop

Download the latest MCP Bundle:

<https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb>

Open the `.mcpb` file with Claude Desktop, drag it into Claude Desktop, or use Settings -> Extensions -> Advanced settings -> Install Extension. Quit Claude completely and relaunch it after installation.

The MCPB bundles the `ibkr` binary for macOS and Linux, runs it locally through stdio, and does not require a separate shell install. Windows Claude Desktop is not supported because `ibkr` has no native Windows daemon; WSL works through the shell install path below.

### Shell, Cursor, Continue, Zed, and generic MCP hosts

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh
ibkr setup claude-desktop
```

The installer downloads the release for your OS and architecture, verifies the checksum, installs `ibkr` in `~/.local/bin`, and adds that directory to your shell rc when needed. On macOS, it also clears Gatekeeper quarantine.

`ibkr setup claude-desktop` writes the legacy MCP server entry to Claude Desktop. Prefer the MCPB path above for Claude Desktop unless you specifically want one shared shell-managed binary. Skip the setup command if you only want the shell tool.

For v1.0.0+ releases, the installer, `ibkr update`, and the MCPB release asset are covered by the signed `SHA256SUMS` file. The MCP Registry metadata also carries the MCPB file SHA-256. [Other install paths.](#other-install-paths)

## What you get

- **Account and positions.** Net liquidation, buying power, cash, margin, daily P&L, positions, option Greeks, per-underlying grouping, and portfolio-level delta/theta/gamma/vega rollups. Multi-currency accounts include FX exposure.
- **Quotes and history.** Snapshot quotes, coalesced stock/ETF streaming, daily OHLCV bars, previous close, day change, and data freshness (`live`, `frozen`, `delayed`, `delayed-frozen`).
- **Official market calendars.** US cash equities, US listed options regular sessions, and German Xetra cash equities with holidays, early closes, next open/close, and quote `session_context` when calendar state explains stale or missing data.
- **Local watchlist.** Add/remove/clear symbols offline, list them as JSON, show an enriched quote monitor with price, currency, changes, ranges, volume, timestamps, and held-stock context, or poll the saved list with `ibkr watch --watch`.
- **Options.** Expiry lists with ATM IV and implied move, strike grids with call/put quotes, deltas, and open interest. Option snapshots are supported; option streaming is not exposed.
- **Scanners.** Built-in market scans for movers, losers, unusual volume, gaps, high IV rank, and option volume. Agents can also compose ad-hoc scans without writing config.
- **Position sizing.** Fixed-fractional sizing against live NLV, with optional target, R-multiple, and breakeven win rate. Pure math; never an order ticket.
- **Market breadth.** S&P 500 participation from constituent daily bars: percent above 50-DMA, percent above 200-DMA, and fresh 52-week highs/lows. A fresh cache is instant; first-ever cold start can take about an hour because of IBKR pacing.
- **Dealer gamma.** Production-ready SPX/SPXW-canonical zero-gamma and concentration view, with SPY used as corroborating context when its option surface is usable. A fresh, rankable SPX result is the stable headline signal; SPY-only is a labeled proxy. Treat the signed level as a regime hint, not a precise trading level.
- **Risk regime.** One call returns the eight-row dashboard: VIX term structure, VVIX, HYG/SPY divergence, HY/IG OAS, funding spread, USD/JPY weekly move, SPX-canonical dealer gamma, and S&P 500 breadth. Heavy rows report `computing` instead of pretending stale data is fresh.
- **Portfolio canary.** `ibkr canary` and MCP `ibkr_canary` produce a stateless `market regime × portfolio shape` monitor with `action`, `market_confirmation`, `portfolio_fit`, `input_health`, planner readiness, stable fingerprints, and supporting `signals[]`. Account-only risk stays evidence, not a canary DEFEND trigger; DEFEND requires confirmed market pressure, vulnerable portfolio fit, and clean enough inputs. Use `ibkr canary --details` for the full evidence rows.

Every data command supports `--json`. `ibkr restart --json` is also useful for scripts: it reports whether a daemon was already running, old/new PIDs, whether `--force` was used, and the post-start `status.health` snapshot. Lifecycle commands such as `setup`, `update`, `restart`, `mcp`, and `daemon` are for local operation and transport setup.

For schemas and edge cases, see the [agent skill schema notes](skills/ibkr/schemas.md), [MCP tools reference](docs/reference/mcp-tools.md), [MCP resources reference](docs/reference/mcp-resources.md), [configuration reference](docs/reference/config.md), and [concept docs](docs/concepts.md).

For ready-to-run prompts, see [examples/ibkr_portfolio_analysis_prompt.md](examples/ibkr_portfolio_analysis_prompt.md) for portfolio review and [examples/ibkr_portfolio_canary_prompt.md](examples/ibkr_portfolio_canary_prompt.md) for scheduled stress checks.

## Pick your path

### Claude Desktop, Cursor, Continue, Zed

`ibkr mcp` starts a local stdio MCP server. MCP hosts can call the same read-only account, watchlist, quote, calendar, position, scanner, sizing, regime, and canary tools that the CLI exposes as JSON. Watchlist access through MCP can return either the saved symbols or enriched quote rows; local lifecycle verbs such as `setup`, `update`, `restart`, `mcp`, `daemon`, and `version` stay outside the MCP tool set.

The server also exposes quotes for stocks and ETFs as an MCP resource:

- `ibkr://quote/{symbol}`

`resources/read` returns one snapshot for that URI; `resources/subscribe` delivers coalesced ticks via `notifications/resources/updated` until you `resources/unsubscribe` or close the stdio. The resource shape is documented in [docs/reference/mcp-resources.md](docs/reference/mcp-resources.md).

For Claude Desktop, the recommended install path is the `.mcpb` asset from the latest release. For other clients, paste this into the client's MCP config (path varies):

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

The `command` must be the absolute path. `~` is not expanded by `exec` and `$PATH` is not consulted. `which ibkr` gives you the right value. After upgrading the binary, fully quit and relaunch the client — it caches the spawned server process. MCPB installs carry their own embedded binary; reinstall the new `.mcpb` release to update that path.

`claude.ai` (web) accepts only remote MCP servers and cannot reach a local IB Gateway. Use Desktop.

Logs (macOS, Claude Desktop): `~/Library/Logs/Claude/mcp-server-ibkr.log`.

### Claude Code

Inside a standalone Claude Code session:

```
/plugin marketplace add osauer/ibkr
/plugin install ibkr@ibkr
```

Or — for **Claude for Mac**'s embedded Claude Code pane, which doesn't expose `/plugin` slash commands — from a regular terminal:

```sh
claude plugin marketplace add osauer/ibkr
claude plugin install ibkr@ibkr
```

The plugin carries a skill, a `PreToolUse` hook that hard-blocks trading verbs and shell command chaining (failing closed if `jq` is missing from PATH), and a `SessionStart` hint when the binary isn't installed. The skill's `allowed-tools` pre-allows the read-only patterns once the skill activates. For a global allowlist that fires *before* the skill activates, copy `settings/ibkr.settings.json` into `~/.claude/settings.json` by hand.

**The plugin doesn't ship the binary.** It only carries the skill, hooks, and manifest — you still need the `ibkr` binary on PATH from [Install](#install). The two have independent release cadences and independent update paths:

```sh
# Binary release (new MCP tool descriptions are baked into the binary):
curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh | sh

# Plugin release (new skill commands, settings, hooks):
claude plugin update ibkr@ibkr
```

Restart the host (Claude for Mac, standalone Claude Code session, Cursor, …) after either update so it respawns the MCP server subprocess with the new descriptions and reloads the skill at the next session start.

### The shell

```sh
$ ibkr account --json | jq '.net_liquidation, .base_currency'
$ ibkr watch IBM --add
$ ibkr watch --list --json
$ ibkr watch --json | jq '.rows[] | {sym: .symbol, price: .price, chg: .change_pct, as_of: .price_as_of}'
$ ibkr quote AAPL,MSFT --json | jq '.[] | {sym: .symbol, price: .price, chg: .change_pct}'
$ ibkr quote MBG --market de --json | jq '{sym: .symbol, ccy: .contract.currency, last: .last}'
$ ibkr calendar --market us-options --date 2026-11-27 --json | jq '.session'
$ ibkr positions --by underlying --json | jq '.portfolio.effective_delta'
$ ibkr canary --json | jq '.decision, .rows[] | {title, decision, action}'
$ ibkr chain NVDA --json | jq '.expiries[] | select(.iv > 0.6)'
$ ibkr size --symbol AAPL --entry 207.50 --stop 202.50 --risk-pct 1
```

`ibkr --help` lists subcommands; `ibkr <cmd> --help` lists flags. `ibkr status` first if anything looks off.

### Mobile app

`ibkr app` serves a paired PWA for iPhone-sized checks when you are away from the desk: daemon status, account and positions, market context, canary state, and debug-only tools. Start it on the Mac running TWS or IB Gateway, then run `ibkr app pair` and scan the QR code.

See [web/app/README.md](web/app/README.md) for the short operator notes and [docs/design/mobile-app-mvp.md](docs/design/mobile-app-mvp.md) for the MVP design.

### Go and other agent SDKs

`pkg/ibkr` speaks the TWS API protocol directly:

```go
import "github.com/osauer/ibkr/pkg/ibkr"

cfg := ibkr.DefaultConfig()    // 127.0.0.1:4001
cfg.Port = 4002                // paper

c := ibkr.NewConnector(&ibkr.ConnectorConfig{
    ServiceName: "myapp",
    PoolConfig:  &ibkr.PoolConfig{ClientIDs: []int{15}, BaseConfig: cfg},
})
if err := c.Start(ctx); err != nil { return err }

snap, _ := c.RequestAccountSummary(ctx, 5*time.Second)
fmt.Printf("NLV: %.2f %s\n", *snap.NetLiquidation, snap.Currency)
```

From Python, TypeScript, or Rust, shell out to the CLI: subprocess in, JSON out. Wrap each `ibkr <cmd> --json` invocation as a function and register it with your model's tool-call API.

## How it works

`ibkr` runs local commands against one background daemon.

When you run a CLI command or an MCP tool, it connects to the daemon over a Unix socket. The daemon keeps the IB Gateway or TWS connection open, caches contract details, manages quote subscriptions, and returns JSON responses. It starts on first use and exits after 15 minutes of inactivity unless you run it in the foreground.

```text
CLI or MCP host -> local ibkr daemon -> IB Gateway or TWS -> your account data
```

Use `ibkr restart` after upgrading, changing daemon-loaded config, or when you want to clear stale gateway connection state. It sends SIGTERM, waits for cleanup, starts a fresh daemon, and reports the new process. If no daemon was running, it starts one and says so. `ibkr restart --force` escalates to SIGKILL only after the graceful timeout; use it for a daemon that ignores SIGTERM. This restarts the shared daemon used by CLI and MCP tool calls; it does not restart the `ibkr mcp` stdio process itself, which is owned by the MCP host. Fully relaunch the host when you need it to respawn MCP from a new binary or bundle.

This means your shell, Claude Desktop, Claude Code, Cursor, and other MCP clients can share one IBKR connection and one client ID. Tool calls stay fast because the gateway session is already open.

`pkg/ibkr` is a clean-room Go implementation of the read-side TWS protocol. Full coverage details live in [docs/reference/protocol.md](docs/reference/protocol.md), and the public package docs live in [pkg/ibkr/doc.go](pkg/ibkr/doc.go).

## Configure

No config file is required. The daemon TCP-probes `4001` (Gateway live), `4002` (Gateway paper), `7496` (TWS live), `7497` (TWS paper), picks the first responder, and falls over to alternates if the first one accepts TCP but never completes the handshake. The account is auto-detected via `managedAccounts`. Default client ID is `15`.

Write a config to **pin** a dimension. Anything you write is binding; anything you omit stays auto. Default path: `$XDG_CONFIG_HOME/ibkr/config.toml`, falling back to `~/.config/ibkr/config.toml`.

```toml
[gateway]
host       = "127.0.0.1"
port       = 4001          # binding: skip the probe
client_id  = 15
account    = ""            # empty = auto-detect via managedAccounts
tls        = false         # binding: no TLS fallback

[daemon]
idle_timeout = "15m"
log_level    = "info"

[scans.top-movers]
type     = "TOP_PERC_GAIN"
exchange = "STK.US.MAJOR"
limit    = 20
```

`ibkr status` shows what the daemon ended up using and where each value came from (`pinned` or `discovered`).

**TLS semantics.** A pinned `tls` value (true or false) is strict. An omitted `tls` means "auto": plain first, TLS on no-handshake-data.

**Strict keys.** Unknown top-level keys or sections fail at startup with a message that names them — your config can't silently drop fields. Supported sections: `[gateway]`, `[daemon]`, `[spx]`, `[scans.<name>]`.

References:

- [Configuration reference](docs/reference/config.md) for TOML sections and `IBKR_*` environment variables.
- [Concepts](docs/concepts.md) for breadth, gamma, and regime interpretation.
- [Agentic use](docs/guides/agentic-use.md) for Claude and MCP workflows.
- [Marketplace readiness](docs/guides/marketplace-readiness.md) for packaging notes.
- [Privacy](PRIVACY.md) for data locality and local files.

### Adding scanners

Two paths, depending on who's calling:

**Humans — add a preset to `config.toml`.** Use this when you want a stable shorthand you'll call by name:

```toml
[scans.tech-gainers]
type     = "TOP_PERC_GAIN"
exchange = "STK.NASDAQ"
limit    = 25
```

Then `ibkr scan tech-gainers`. **Caveat:** writing **any** `[scans.*]` block makes the seven built-in defaults disappear — the `[scans]` table is replace-not-merge. Copy the defaults from [internal/config/config.go](internal/config/config.go) into your file if you want to keep them. Run `ibkr restart` for new presets to be visible.

**Agents — use the ad-hoc form, no config write needed:**

```
ibkr scan --type TOP_PERC_GAIN --exchange STK.NASDAQ --limit 25 --json
ibkr scan --type TOP_PERC_GAIN --exchange STK.EU.IBIS --instrument STOCK.EU --limit 25 --json
```

Ad-hoc rows are capped at 50 (vs. preset's user-set limit) to keep an agent from accidentally pulling thousands.

**Finding the right `scanCode` and `locationCode`.** The TWS Market Scanner UI hides these strings behind human labels. Dump your gateway's actual catalog with:

```
ibkr scan params --instrument STK [--json]
```

The catalog varies by gateway version and by your market-data subscriptions — `scanCode`s like `HIGH_OPT_IMP_VOLAT_OVER_HIST` require US options data, and European stock locations often require `--instrument STOCK.EU` instead of the US default `STK`. `--instrument STK` narrows to US stock scans; omit for everything. Add `--raw` to get the full XML (~200 KB–2 MB) if you need a less-common field. There's no need to memorize the values — the catalog is the source of truth.

## Safety

`ibkr` is the stable read-only line. Five independent layers refuse `order`, `trade`, `cancel`:

1. Default `pkg/ibkr` builds return `ErrTradingDisabled` from `Connection.PlaceOrder`, `Connection.CancelOrder`, `Connector.SubmitOrder`, and `Connector.CancelOrder` before any wire write. The raw encoder is available only to explicit downstream forks built with `-tags trading`.
2. The daemon's order-handler dispatch returns `ErrTradingDisabled` for both `MethodOrderPlace` and `MethodOrderCancel` ([internal/daemon/trading_disabled.go](internal/daemon/trading_disabled.go)).
3. The bundled [settings/ibkr.settings.json](settings/ibkr.settings.json) denies the verbs in `permissions.deny`.
4. The plugin's `PreToolUse` hook hard-blocks the verb patterns and fails closed if `jq` is missing from PATH.
5. A unit test in `internal/mcp` refuses to ship the MCP server with any tool whose name contains `order`, `trade`, `cancel`, `submit`, or `place`.

Per [semver](https://semver.org/), v1.x keeps the CLI, JSON, and MCP read-only interfaces stable except for documented minor additions and patch fixes.

## Other install paths

- **`go install`**: `go install github.com/osauer/ibkr/cmd/ibkr@latest`. Requires Go 1.26+.
- **Claude Desktop MCPB**: download `ibkr.mcpb` from the latest [release](https://github.com/osauer/ibkr/releases/latest/download/ibkr.mcpb) and open it with Claude Desktop. The release also publishes `ibkr-vX.Y.Z.mcpb` for registry integrity and reproducible manual verification.
- **Different install dir**: `IBKR_INSTALL_DIR=/usr/local/bin sh install.sh`. The installer won't touch your shell rc when you override; manage PATH yourself.
- **Inspect the installer first**: `curl -fsSL https://raw.githubusercontent.com/osauer/ibkr/main/install.sh -o install.sh && less install.sh && sh install.sh`.
- **Manual download**: pick a tarball from the latest [release](https://github.com/osauer/ibkr/releases/latest). Each contains `ibkr` plus `LICENSE` and `README.md`. Verify `SHA256SUMS.asc` against the release-signing key, then verify the tarball against `SHA256SUMS`; see [SECURITY.md](SECURITY.md#release-integrity-v100).
- **Local build**: `git clone … && make install`.
- **Self-update**: `ibkr update` fetches the next stable release, verifies the PGP signature on `SHA256SUMS`, SHA-verifies the tarball, and atomically replaces `~/.local/bin/ibkr` (prior binary stashed as `.bak` for one-step rollback). See [docs/guides/updating.md](docs/guides/updating.md) for headless flag matrix, daemon-restart semantics, `ibkr restart`, and how the runtime S&P-500 constituent refresh works.

Windows is not supported — the daemon uses Unix-only primitives (setsid, flock, AF_UNIX sockets). WSL works.

## Testing

```sh
make check      # gofmt + go vet + staticcheck + govulncheck + plugin/parity checks
make test       # check + unit tests + integration tests against a live gateway
```

`make check` is the binding gate. It fails on stdlib vulnerabilities, so an outdated Go toolchain is a build failure. The lint/vuln tools are pinned in `go.mod` and run via `go tool`, so CI and local checks use the same versions. The gate also checks that MCP tools, streaming resources, generated references, and plugin metadata stay aligned with the CLI commands.

Integration tests under `test/integration/` connect to the live IB Gateway on `127.0.0.1:4001` and skip cleanly when it isn't reachable, so `go test ./...` doesn't hang on a laptop with no gateway. Override the port with `IBKR_TEST_PORT=4002 make test`.

No mock daemons. `pkg/ibkr/protocoltest/` is a wire-level encoder/decoder spec used by unit tests. Behavioural verification runs against a real IB Gateway.

## Troubleshooting

**"gateway not responding to TWS handshake within 12s".** The gateway accepts your TCP connection but never replies to the v100 handshake. Almost always the API socket is disabled. Launch TWS once, accept "Enable ActiveX and Socket Clients", quit TWS, restart Gateway. The flag carries over via shared `~/Jts/<userdir>/ibg.xml`. It also silently un-ticks itself when more than one of TWS / IB Gateway / IBKR Desktop is launched against the same login — if it keeps coming back, run only one of them.

**"no IBKR listener found on 127.0.0.1 ports ...".** Auto-discovery probed all four standard ports and got nothing. The error message tells you which case you're in: if TWS / IB Gateway / IBKR Desktop is running, the API socket is closed (checkbox unchecked, login pending, or non-default socket port — pin it in `[gateway]`); if nothing is running, just start one and the daemon reconnects automatically. On a non-loopback host, set `host = "192.168.x.y"` explicitly — auto-discovery only probes loopback.

**"none of N discovered endpoint(s) completed TWS handshake".** Both Gateway and TWS are running, both accept TCP, but neither completes the API handshake. Usually a stale Gateway window from earlier in the day plus a freshly logged-in TWS. The status output names every endpoint that was tried. Quit the one you don't need.

**`daemon socket did not appear`.** The daemon crashed during startup. Check `~/.local/state/ibkr/ibkr-daemon.log`. Common causes: gateway not running, configured `client_id` already in use, wrong port. Orphaned sockets from crashed daemons are handled automatically.

**Quotes time out.** Strict live entitlements, market closed. The daemon defaults to `SetMarketDataType(2)` (frozen), which returns the last-known price; with `live` only, snapshots stay empty out of trading hours. Loosen the gateway's market-data permissions.

**`use of closed network connection` during handshake.** IB Gateway rate-limits fast handshake retries. Wait ~30 seconds before restarting.

**CLI vs daemon version skew warning.** Run `ibkr restart`. It stops the old daemon and starts a new one from the current binary.

**Capturing the wire protocol for diagnostics.** Set `IBKR_WIRE_INTERCEPTOR=1` to enable the in-process recorder; pair with `IBKR_WIRE_LOG_PATH=/path/to/wire.jsonl` to also persist every frame as JSON-lines. `IBKR_WIRE_RING_SIZE=N` sizes the in-memory ring (default 256). For raw bytes, `IBKR_PACKET_LOG_TEMPLATE=/path/to/packets.bin` enables the lower-level packet logger. All four are off by default. Captured frames carry account-sensitive data — see [SECURITY.md §Diagnostic data sensitivity](SECURITY.md#diagnostic-data-sensitivity) before sharing logs.

## Disclaimer & trademarks

This project is an **independent, third-party client** for Interactive Brokers' [publicly documented TWS API](https://interactivebrokers.github.io/). It is not built, endorsed, sponsored, or supported by Interactive Brokers Group, Inc., or any of its affiliates.

- "Interactive Brokers", "IBKR", "TWS", and "IB Gateway" are trademarks or registered trademarks of Interactive Brokers Group, Inc. or its affiliates. They are used here nominatively, solely to identify the brokerage and the API this project connects to.
- `pkg/ibkr` is a clean-room Go re-implementation of the TWS wire protocol. **No code, libraries, or jars distributed by Interactive Brokers are included or redistributed in this project.**
- This project does not store, cache, or redistribute IBKR market data. All data is read live from a gateway you run locally, against your own account, and never leaves your machine via this code.
- Connecting to IBKR via the TWS API requires an **IBKR Pro** account; IBKR Lite does not include API access.
- Nothing here is investment advice. Use at your own risk; the MIT license's AS IS clause applies in full.

If you are Interactive Brokers and have a concern with anything in this repository, please open a GitHub issue and we will respond promptly.

## License

MIT. See [LICENSE](LICENSE).
