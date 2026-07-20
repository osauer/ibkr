# Architecture

`ibkr` runs on your machine and connects to one Interactive Brokers TWS or IB
Gateway session. It adapts that session for humans, AI hosts, and the Canary
app, and it adds a risk harness. One ownership rule shapes the design:

> The daemon owns all broker-connected and runtime-state capability: the
> connection, observed state, schedulers, journals, and policy execution.
> Every other process adapts typed contracts for its audience, and none of
> them owns broker state.

A few local workflows are deliberate exceptions: watchlist edits, setup,
updates, process management, and offline research or backtests all run
without daemon RPC.

## System Overview

[![ibkr canary runtime architecture](diagrams/system-architecture.svg)](diagrams/system-architecture.svg)

[PNG fallback](diagrams/system-architecture.png) ·
[SVG source generator](diagrams/render-architecture.mjs) ·
[Tabler Icons license](diagrams/ICON-LICENSE.txt)

The numbered columns are layers of one system, not six separate services:
consumers, surface adapters, the shared typed contract, daemon authority and
domain logic, integration clients, and external providers or data.

The default deployment has one shared daemon. `ibkr mcp` is an adapter process
owned by each MCP host, not a second daemon, and `ibkr app` is a separate
HTTP/PWA process. Inside the daemon, a primary broker connection serves
interactive, account, and gamma work, while a second client connection carries
the S&P 500 breadth history fan-out.

## Runtime Processes

All local modes ship in the same `ibkr` binary, but they differ in lifetime
and authority.

| Process or Surface | Lifetime | Responsibility |
|---|---|---|
| CLI / TUI | One command | Validates input, calls the daemon for broker and runtime work, and renders human or JSON output. A few local-only workflows run without the daemon. |
| `ibkr daemon` | Demand-driven background process or foreground service | Owns the broker connections and all runtime state: caches, schedulers, journals, source ingestion, risk-policy execution, proposals, opportunities, and reconciliation. |
| `ibkr mcp` | Long-lived child of each MCP host | Speaks MCP JSON-RPC 2.0 over stdio and translates tools and resources into short daemon calls. The surface serves read, research, and preview work; it exposes no broker-write tools. |
| `ibkr app` | Independently run or supervised HTTP process | Serves the embedded Canary PWA and owns pairing, auth, and app state. Maintains the live snapshot and quote streams, emits SSE, and can connect to the remote relay and Web Push. |
| Canary Paired PWA | Browser or iOS Home Screen app | Renders authenticated snapshots, receives SSE and push notifications, and keeps device-side credentials and recovery state. It is a plain PWA, not an Android Trusted Web Activity. |
| TWS / IB Gateway | Interactive Brokers process outside this repo | Terminates the local TWS API socket and maintains the broker-managed session. |

Clients auto-spawn the daemon when the socket is absent, and it exits after
15 idle minutes unless foreground mode disables the timeout. Durable caches
and journals survive the process.

## Code Ownership Layers

- `pkg/ibkr` is the clean-room TWS wire client. Protocol framing, sockets and
  TLS, request IDs, broker callbacks, contract resolution, and order-wire
  details live here.
- `internal/daemon` is the long-running authority. It owns both broker
  connectors, the external-source clients, caches, schedulers, journals, XDG
  state, risk-capital runtime state, daily Flex ingestion, and post-trade
  reconciliation.
- `internal/risk` is the pure evaluation library behind advisory verdicts:
  thresholds and fingerprints, canary signal types, option math, the daily
  trading rulebook, and risk-constitution evaluation. It does no I/O and owns
  no broker state.
- `internal/rpc` defines the typed method names and request/response structs
  that daemon, CLI, app, and MCP adapters share. Add fields here first; teach
  surfaces to render them second.
- `internal/cli` adapts commands to daemon methods or to one of the local-only
  workflows. Broker policy and state do not belong in the renderer.
- `internal/mcp` adapts MCP tools and resources to daemon contracts. Tool
  descriptions are product surface and must match the real authority and data
  quality.
- `internal/app` owns the HTTP host, device auth, app-local persistence, live
  polling and streaming, SSE fanout, Web Push, and the optional outbound
  relay. The live cache is the normal read path. Settings, reviews, and
  order, proposal, or opportunity actions make direct typed daemon calls
  instead.
- `cloudflare/remote-relay` is a Worker plus a Durable Object, used as
  transport. It forwards framed HTTP and SSE traffic over the app's outbound
  connector. It owns no device grants, pairing sessions, browser sessions,
  daemon access, or broker credentials.
- `web/app` is the embedded no-build Canary SPA and its service worker.
  Global account, market, and sync state stays outside individual tab
  content.

## Data Flows and Protocols

The daemon protocol and the MCP protocol are different on purpose; only the
MCP side is JSON-RPC 2.0.

| Flow | Protocol and Payload | Notes |
|---|---|---|
| Human → CLI | `argv` / stdin; human text or JSON on stdout | One-shot local process. |
| AI host → `ibkr mcp` | MCP JSON-RPC 2.0, newline-delimited over stdio | The host owns the process lifetime. |
| CLI / MCP / app → daemon | Custom typed newline-delimited JSON request/response frames over a Unix domain socket | The envelope uses project fields such as `ok`, `frame`, `stream`, and `end`. It is not JSON-RPC 2.0. |
| App live service → daemon | Periodic typed calls plus long-lived quote streams | Feeds one app snapshot/cache and its change fanout. |
| App request routes → daemon | Request-driven typed calls over the same Unix socket | Used where a route needs a fresh action, review, or settings response instead of the cached snapshot. |
| Daemon → TWS / Gateway | Clean-room TWS wire protocol over TCP, optionally TLS | A primary interactive connection, plus a breadth connection with its own client ID and rate budget. |
| Browser / PWA ↔ app | Public static assets and pairing/auth endpoints, then authenticated HTTP(S) JSON and `/api/events` Server-Sent Events | Local and LAN access reaches the app directly. Pairing-session creation is loopback-only. |
| PWA ↔ remote relay | Public HTTPS carrying allowed HTTP/SSE traffic | Optional remote path. |
| App ↔ remote relay | HTTPS registration, then authenticated outbound WSS | Requests and streaming responses travel as frames over the WebSocket. The local connector enforces the forwarded-path allowlist. |
| App → browser push service → PWA | VAPID-authenticated Web Push over HTTPS | Push payloads are redacted, and the relay is not in the delivery path. |
| Daemon → external observed-data sources | Scheduled or on-demand HTTPS/FTP; JSON, CSV, XML, RSS, and text | Source health and stale or unknown states stay explicit in typed results. |

## Broker and External Data Sources

Not all market context arrives through TWS or Gateway.

| Source | Runtime Path | Data |
|---|---|---|
| TWS / IB Gateway API socket | TWS wire protocol over TCP/TLS | Account, positions, quotes, option chains/Greeks/OI, historical bars, scanners, order lifecycle, shortable-share observations, and broker WhatIf/eligibility. |
| IBKR Flex Web Service | HTTPS POST and polling | Daily raw Flex XML statements used as broker statement truth for reconciliation. |
| IBKR short-stock availability | FTP | Borrow availability and fee-rate evidence. |
| Nasdaq | HTTPS JSON, pipe-delimited text, and RSS/XML | Earnings dates, Reg SHO threshold securities, LULD/trade-halt context. |
| FRED, CBOE, Federal Reserve, US Treasury | HTTPS CSV/XML | Public regime and rates series. |
| Wikipedia S&P 500 list | Scheduled HTTPS refresh | Breadth constituent membership, with a validated cache and embedded fallback. |
| Official exchange calendars | Embedded Go data | Handwritten build-time tables for US equities, US options, and Xetra, covering 2026 through 2028; a date outside coverage reports an explicit unknown state. There is no runtime calendar network call. |

The market-event source cache is memory-only today; the other source families
have the disk caches described below.

## Data and Persistence

[![ibkr canary state ownership and lifecycle](diagrams/data-and-persistence.svg)](diagrams/data-and-persistence.svg)

[PNG fallback](diagrams/data-and-persistence.png) ·
[SVG source generator](diagrams/render-architecture.mjs) ·
[Tabler Icons license](diagrams/ICON-LICENSE.txt)

Configuration is operator-owned, and daemon state and app state are separate
authorities. Caches are rebuildable observations; journals and broker
statements are durable evidence and can contain sensitive account or trading
data. The journals and raw statements are the evidence of record; the history
index below is only a derived view of them.

| Class | Default Location | Owner and Representative Contents |
|---|---|---|
| Operator configuration | `$XDG_CONFIG_HOME/ibkr/config.toml`, falling back to `~/.config/ibkr/config.toml`; policy defaults under `~/.config/ibkr/policies/` | Gateway/account/client pins, daemon/trading settings, protection/opportunity policy, the operator-authored `risk-policy.toml`, and the separate `flex-token` secret. The risk policy has no embedded default: missing approval stays unapproved. |
| Daemon durable state | `$XDG_STATE_HOME/ibkr`, falling back to `~/.local/state/ibkr` | `platform-settings.json`, order preview/readiness material, `order-journal.jsonl`, purge ledger, proposal/opportunity snapshots and journals, risk-capital state and event journals, governance/brief/rule/regime/canary journals, and `statements/flex-*.xml`. |
| Rotated evidence archives | `$XDG_STATE_HOME/ibkr/rotated/` | Monthly gzip archives of the regime, rules, and canary decision journals, written only after the bytes are fully absorbed into the history index. Immutable evidence, kept forever: rotation compresses and relocates, it never deletes. The raw keep window is `history.rotation.keep_raw_months` (default 2). |
| Derived history index | `$XDG_STATE_HOME/ibkr/history.db` (SQLite, WAL) | Daemon-only writer and opener. A rebuildable query index over the evidence journals and retained statements, kept current by automatic backfill and tail ingest. Deleting the file is safe; it rebuilds from archives, journals, and statements at daemon start. |
| App durable state | `$XDG_STATE_HOME/ibkr/app`, falling back to `~/.local/state/ibkr/app` | Private `state.json` with device grants, push subscriptions, VAPID material, alerts, governance evidence, and relay credentials; `app.lock` enforces one app process per state directory. |
| Rebuildable cache | `$XDG_CACHE_HOME/ibkr`, falling back to `~/.cache/ibkr` | Contract cache, FX and earnings caches, breadth state and S&P membership, regime series/history/streaks, gamma results/OI/expiry grids, and updater scratch space. |
| User data | `$XDG_DATA_HOME/ibkr`, falling back to `~/.local/share/ibkr` | `watchlist.json`; explicit research exports are separate operator-created files. |
| Runtime IPC and logs | `$XDG_RUNTIME_DIR/ibkr/ibkr.sock`, falling back to `~/.cache/ibkr/ibkr.sock`; daemon log defaults to `~/.local/state/ibkr/ibkr-daemon.log` | Unix socket, sibling lock/PID file, rotated daemon text log, and optional macOS LaunchAgent/app logs under `~/Library`. |
| Browser / PWA state | Browser cookie jar, IndexedDB, and `localStorage` | Short-lived session, durable HttpOnly device continuity, P-256 device key, local recovery material, preferences, and a non-authorizing relay route identifier. |
| Hosted relay state | Cloudflare Durable Object | Connector token and expiry for the optional route. It stores no device grants or broker state. |

The daemon is the history index's only writer and its only opener. CLI, MCP,
and the app reach it through typed RPC; the app never opens the database
file. Archives plus the live journal always reconstruct the original byte
stream, so nothing is ever lost to rotation. Order reads serve from the
index only while it provably matches the journal; anything else falls back
to the journal scan automatically, and the journal stays authoritative.

Never persist broker market-data entitlements. Expose observed data type,
quality, freshness, and warnings on typed read surfaces instead.

## Deployment Scopes and Multiple Instances

The default topology is one daemon shared by the CLI, all local MCP adapter
processes, and the app, through the canonical Unix socket. A flock-backed
lock enforces one daemon per socket directory.

You can run more daemon, gateway, or account scopes, but nothing multiplexes
them for you. An isolated stack needs its own config, socket, log, account
and client pins, and its own XDG state, cache, and data roots. Most durable
paths are XDG-global rather than derived from the socket, so changing only
`IBKR_SOCKET` does not isolate persistence.

## HTTP App and Remote Access

The app is the only HTTP process; it serves through HyperServe
(`github.com/osauer/hyperserve`), the project's companion `net/http`-shaped
server, with method-aware routes, hardened defaults, and SSE formatting. The
daemon and the MCP adapter do not serve HTTP at all: the daemon listens on
the Unix socket, and MCP speaks stdio.

Routes register in `internal/app/http/routes.go` in four families: static
PWA assets, pairing and auth, the authenticated app API, and the
`/api/events` SSE stream. Handlers read app stores, the live snapshot, or
typed daemon calls; they do not invent policy. There is no separate health
route; `/api/bootstrap` and `/api/snapshot` carry relay and liveness state.

Remote access keeps pairing, auth, session validation, forwarding allowlists,
and daemon access on the local machine. The Worker and the Durable Object
carry route transport state only. Browser HTTP and SSE requests travel as
frames over the app's authenticated outbound WebSocket and stream back
through the Worker.

## Observability

The observability layer is thin on purpose: text logs, one health surface,
and evidence journals. There are no metrics and no tracing.

- The daemon writes structured text logs through `log/slog`. The `log_level`
  config key sets the level; the default is `info`. The log lives at
  `~/.local/state/ibkr/ibkr-daemon.log` and rotates at boot once it passes
  64 MiB, keeping one older generation. The app logs to `ibkr-app.log`.
- `ibkr status` renders the daemon's `status.health` report: gateway,
  session, and TLS state, uptime, background tasks, subsystem health, data
  quality, data-farm notices, and trading state. It ends in one verdict:
  ready, attention, offline, or starting.
- Typed read surfaces carry their own source health. Regime clusters, gamma,
  the market calendar, and governance report stale, partial, degraded, or
  unknown instead of guessing.
- Append-only JSONL journals are the evidence trail: orders, regime
  decisions, rule decisions, proposal outcomes, risk-capital events, and the
  purge ledger. `ibkr brief` is the human-readable aggregator over them,
  and `ibkr regime history`, `ibkr rules history`, `ibkr canary history`,
  and `ibkr recon equity` query them through the derived index.

## Change Flow

Start every new capability with an ownership decision: operator config,
daemon runtime state, app-local state, observed snapshot, cache, derived
history index, or build flag. Then work outward in order: typed contract, owning behavior, tests,
adapters, rendered surfaces, and generated references. Adapters never refetch
daemon-owned sources and never recreate risk or trading verdicts.

Market-event flags are daemon-owned observed context. Adapters render or
filter the typed `market_events.snapshot`; they do not refetch Reg SHO, halt,
borrow-inventory, borrow-fee, or earnings sources, and they do not duplicate
proposal-blocking policy.
