# Architecture

`ibkr` is a local-first adapter and risk-harness layer around one selected
Interactive Brokers TWS or IB Gateway session. The central ownership rule is:

> For broker-connected and runtime-state capabilities, the daemon owns the
> connection, observed state, schedulers, journals, and policy execution. Other
> processes adapt typed contracts for humans, AI hosts, and the Canary app.

That rule has deliberate local-only exceptions: watchlist metadata, setup,
update, restart/process management, and offline research/backtests can operate
without daemon RPC.

## System overview

[![ibkr canary runtime architecture](diagrams/system-architecture.svg)](diagrams/system-architecture.svg)

[PNG fallback](diagrams/system-architecture.png) ·
[SVG source generator](diagrams/render-architecture.mjs) ·
[Tabler Icons license](diagrams/ICON-LICENSE.txt)

The numbered columns are architectural layers: consumers, surface adapters,
the shared typed contract, daemon authority and domain semantics, integration
clients, and external providers or data. They are not six independently
deployed services.

The default deployment has one shared daemon. `ibkr mcp` is an adapter process
owned by each MCP host, not a second daemon. `ibkr app` is a separate HTTP/PWA
process. Within the daemon, a primary Interactive Brokers connection serves
interactive/account/gamma work and a dedicated client connection serves the
S&P 500 breadth historical-bar fan-out.

## Runtime processes

All local modes ship in the same `ibkr` binary, but they have different
lifetimes and authorities.

| Process or surface | Lifetime | Responsibility |
|---|---|---|
| CLI / TUI | One command | Validate input, call the daemon for broker/runtime work, render human or JSON output, and own a few explicit local-only workflows. |
| `ibkr daemon` | Demand-driven background process or foreground service | Own broker connections, runtime state, caches, schedulers, journals, external-source ingestion, risk-policy execution, proposals, opportunities, and reconciliation. |
| `ibkr mcp` | Long-lived child of each MCP host | Speak MCP JSON-RPC 2.0 over stdio and translate tools/resources to short-lived daemon calls. The MCP surface is read/research/preview-oriented and exposes no broker-write tools. |
| `ibkr app` | Independently run or supervised HTTP process | Serve the embedded Canary PWA, own pairing/auth/app state, maintain the live snapshot and quote streams, handle request-driven actions, emit SSE, and optionally connect to the remote relay and Web Push. |
| Canary paired PWA | Browser or iOS Home Screen app | Render authenticated snapshots, receive SSE and push notifications, and keep device-side credentials and recovery state. It is a PWA, not an Android Trusted Web Activity. |
| TWS / IB Gateway | Interactive Brokers process outside this repo | Terminate the local TWS API socket and maintain the broker-managed session. |

The normal daemon is auto-spawned by CLI, MCP, or app clients when the socket is
absent. Its default idle timeout is 15 minutes; foreground mode can disable idle
shutdown. Durable caches and journals outlive that process lifecycle.

## Code ownership layers

- `pkg/ibkr` is the clean-room low-level TWS wire client. Keep protocol,
  socket/TLS, request IDs, broker callbacks, contract resolution, and order-wire
  details here.
- `internal/daemon` is the long-running authority. It owns the primary and
  breadth broker connectors, external-source clients, caches, schedulers,
  journals, XDG state, risk-capital runtime state, daily Flex ingestion, and
  post-trade reconciliation.
- `internal/risk` is the pure evaluation library behind advisory verdicts:
  thresholds and fingerprints, canary signal types, option math, the daily
  trading rulebook, and risk-constitution evaluation. It performs no I/O and
  owns no broker state.
- `internal/rpc` defines the typed method names and request/response structs
  shared by daemon, CLI, app, and MCP adapters. Add fields here before teaching
  surfaces to render them.
- `internal/cli` adapts commands to daemon methods or one of the explicitly
  local-only workflows. Broker policy and state do not belong in the renderer.
- `internal/mcp` adapts MCP tools and resources to daemon contracts. Tool
  descriptions are part of the product surface and must match the actual
  authority and data-quality semantics.
- `internal/app` owns the HTTP host, device auth, app-local persistence, live
  polling/streaming, SSE fanout, Web Push, and optional outbound relay. Its live
  cache is the normal read path, while settings, reviews, order/proposal/
  opportunity actions, and other request-driven routes can make direct typed
  daemon calls.
- `cloudflare/remote-relay` is a Worker plus Durable Object transport. It routes
  framed HTTP/SSE traffic over the app's outbound connector; it does not own
  device grants, pairing sessions, browser sessions, daemon access, or broker
  credentials.
- `web/app` is the embedded no-build Canary SPA and service worker. Global
  account/market/sync state stays outside individual tab content.

## Data flows and protocols

The daemon protocol and MCP protocol are intentionally different and should not
both be called JSON-RPC 2.0.

| Flow | Protocol and payload | Notes |
|---|---|---|
| Human → CLI | `argv` / stdin; human text or JSON on stdout | One-shot local process. |
| AI host → `ibkr mcp` | MCP JSON-RPC 2.0, newline-delimited over stdio | The host owns the process lifetime. |
| CLI / MCP / app → daemon | Custom typed newline-delimited JSON request/response frames over a Unix domain socket | The envelope uses project fields such as `ok`, `frame`, `stream`, and `end`; it is not JSON-RPC 2.0. |
| App live service → daemon | Periodic typed calls plus long-lived quote streams | Populates one app snapshot/cache and change fanout. |
| App request routes → daemon | Request-driven typed calls over the same Unix socket | Used where a fresh action/review/settings response is required instead of the cached snapshot. |
| Daemon → TWS / Gateway | Clean-room TWS wire protocol over TCP, optionally TLS | Primary interactive connection plus a dedicated breadth connection with separate client ID and rate-limit budget. |
| Browser / PWA ↔ app | Public static assets and pairing/auth endpoints, then authenticated HTTP(S) JSON and `/api/events` Server-Sent Events | Local/LAN access reaches the app directly. Pairing-session creation is loopback-only. |
| PWA ↔ remote relay | Public HTTPS carrying allowed HTTP/SSE traffic | Optional remote path. |
| App ↔ remote relay | HTTPS registration, then authenticated outbound WSS | Requests and streaming responses are framed over the WebSocket. The local connector enforces the forwarded-path allowlist. |
| App → browser push service → PWA | VAPID-authenticated Web Push over HTTPS | Push payloads are redacted; relay transport is not in the delivery path. |
| Daemon → external observed-data sources | Scheduled or on-demand HTTPS/FTP; JSON, CSV, XML, RSS, and text | Source health and stale/unknown states remain explicit in typed results. |

## Broker and external data sources

Not all market context arrives through TWS or Gateway.

| Source | Runtime path | Data |
|---|---|---|
| TWS / IB Gateway API socket | TWS wire protocol over TCP/TLS | Account, positions, quotes, option chains/Greeks/OI, historical bars, scanners, order lifecycle, shortable-share observations, and broker WhatIf/eligibility. |
| IBKR Flex Web Service | HTTPS POST and polling | Daily raw Flex XML statements used as broker statement truth for reconciliation. |
| IBKR short-stock availability | FTP | Borrow availability and fee-rate evidence. |
| Nasdaq | HTTPS JSON, pipe-delimited text, and RSS/XML | Earnings dates, Reg SHO threshold securities, LULD/trade-halt context. |
| FRED, CBOE, Federal Reserve, US Treasury | HTTPS CSV/XML | Public regime and rates series. |
| Wikipedia S&P 500 list | Scheduled HTTPS refresh | Breadth constituent membership, with a validated cache and embedded fallback. |
| Official exchange calendars | Embedded Go data | Build-time schedules; no runtime calendar network call. |

The market-event source cache is currently memory-only. Other source families
have explicit disk caches described below.

## Data and persistence

[![ibkr canary state ownership and lifecycle](diagrams/data-and-persistence.svg)](diagrams/data-and-persistence.svg)

[PNG fallback](diagrams/data-and-persistence.png) ·
[SVG source generator](diagrams/render-architecture.mjs) ·
[Tabler Icons license](diagrams/ICON-LICENSE.txt)

Configuration is operator-owned. Daemon and app state are separate authorities.
Caches are rebuildable observations; journals and broker statements are durable
evidence and can contain sensitive account or trading information.

| Class | Default location | Owner and representative contents |
|---|---|---|
| Operator configuration | `$XDG_CONFIG_HOME/ibkr/config.toml`, falling back to `~/.config/ibkr/config.toml`; policy defaults under `~/.config/ibkr/policies/` | Gateway/account/client pins, daemon/trading settings, protection/opportunity policy, the operator-authored `risk-policy.toml`, and the separate `flex-token` secret. The risk policy has no embedded default: missing approval stays unapproved. |
| Daemon durable state | `$XDG_STATE_HOME/ibkr`, falling back to `~/.local/state/ibkr` | `platform-settings.json`, order preview/readiness material, `order-journal.jsonl`, purge ledger, proposal/opportunity snapshots and journals, risk-capital state and event journals, governance/brief/rule/regime journals, and `statements/flex-*.xml`. |
| App durable state | `$XDG_STATE_HOME/ibkr/app`, falling back to `~/.local/state/ibkr/app` | Private `state.json` with device grants, push subscriptions, VAPID material, alerts, governance evidence, and relay credentials; `app.lock` enforces one app process per state directory. |
| Rebuildable cache | `$XDG_CACHE_HOME/ibkr`, falling back to `~/.cache/ibkr` | Contract cache, FX and earnings caches, breadth state and S&P membership, regime series/history/streaks, gamma results/OI/expiry grids, and updater scratch space. |
| User data | `$XDG_DATA_HOME/ibkr`, falling back to `~/.local/share/ibkr` | `watchlist.json`; explicit research exports are separate operator-created files. |
| Runtime IPC and logs | `$XDG_RUNTIME_DIR/ibkr/ibkr.sock`, falling back to `~/.cache/ibkr/ibkr.sock`; daemon log defaults to `~/.local/state/ibkr/ibkr-daemon.log` | Unix socket, sibling lock/PID file, rotated daemon text log, and optional macOS LaunchAgent/app logs under `~/Library`. |
| Browser / PWA state | Browser cookie jar, IndexedDB, and `localStorage` | Short-lived session, durable HttpOnly device continuity, P-256 device key, local recovery material, preferences, and a non-authorizing relay route identifier. |
| Hosted relay state | Cloudflare Durable Object | Connector token and expiry for the optional route. It stores no device grants or broker state. |

Never persist broker market-data entitlements. Expose observed data type, quality,
freshness, and warnings on typed read surfaces instead.

## Deployment scopes and multiple instances

The default topology is one daemon shared by CLI, all local MCP adapter
processes, and the app through the canonical Unix socket. A flock-backed lock
enforces one daemon per socket directory.

Additional daemon/gateway/account scopes are technically possible, but they are
not an automatic multiplexing feature. A genuinely isolated stack needs its own
config, socket, log, account/client pins, and process-wide XDG state/cache/data
roots. Most durable paths are XDG-global rather than derived from the socket, so
changing only `IBKR_SOCKET` does not isolate persistence safely.

## HTTP app and remote access

The app uses HyperServe as a small `net/http`-shaped server with method-aware
routes, security hardening, and SSE formatting. Route registration lives in
`internal/app/http/routes.go`; business state comes from app stores, the live
snapshot, or typed daemon calls rather than from HTTP handlers inventing policy.

Remote access keeps pairing, auth, session validation, forwarding allowlists,
and daemon access on the local Mac. The Worker and Durable Object carry only
route transport state. Browser HTTP/SSE requests are serialized into frames over
the app's authenticated outbound WebSocket and streamed back through the Worker.

## Change flow

For a new capability, start with ownership: operator config, daemon runtime
state, app-local state, observed snapshot, cache, or build flag. Then update the
typed contract, owning behavior, tests, adapters, rendered surfaces, and
generated references in that order. Adapters must not refetch daemon-owned
sources or recreate risk/trading verdicts.

Market-event flags are daemon-owned observed context. Adapters render or filter
the typed `market_events.snapshot`; they do not refetch Reg SHO, halt,
borrow-inventory, borrow-fee, or earnings sources or duplicate proposal-blocking
policy.
