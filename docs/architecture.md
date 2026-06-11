# Architecture

This repo is layered around one rule: the daemon owns broker connectivity and
state; every other surface is an adapter over typed RPC contracts.

## Layers

- `pkg/ibkr` is the low-level Interactive Brokers client. Keep protocol, socket,
  and TWS/Gateway details here.
- `internal/daemon` is the long-running owner of the gateway connection, caches,
  schedulers, journals, market-event source caches, and XDG state files. It
  serves newline-delimited JSON-RPC over a local Unix socket and must stay useful
  when the gateway is disconnected whenever a method is state/config-only.
- `internal/rpc` is the contract layer: method names plus request/response
  structs shared by daemon, CLI, app, and MCP. Add fields here before teaching
  surfaces to render them.
- `internal/cli` is a thin command adapter. It validates command shape, calls the
  daemon, and renders human or JSON output; it should not duplicate daemon policy.
- `internal/mcp` is an LLM-facing adapter over daemon RPC. Tool descriptions are
  part of the product surface and must explain when to invoke or avoid a tool.
- `internal/app` serves the paired PWA, owns app auth/pairing/push state, polls
  daemon RPC into a live snapshot, streams updates to the browser, and owns the
  optional outbound remote relay connector.
- `cloudflare/remote-relay` is the hosted transport relay for remote app access.
  It is transport only: pairing/auth/session validation remain in `internal/app`.
- `web/app` is the embedded no-build SPA. Keep global account/market/sync state
  outside tab content; use existing inline SVG and CSS patterns.

## State

Configuration in TOML/env/build flags is operator-owned and should remain the
source of truth for gateway identity, trading mode, and build capability. Daemon XDG state is runtime-owned and may hold caches,
journals, observed facts, and user preferences. Never persist broker
entitlements; expose only observed quality on read surfaces.

## HTTP App

The app uses HyperServe as a small `net/http`-shaped server with method-aware
routes, security hardening, and SSE formatting. See HyperServe's own
[architecture](https://github.com/osauer/hyperserve/blob/v1.2.0/ARCHITECTURE.md)
and [SSE ADR](https://github.com/osauer/hyperserve/blob/v1.2.0/docs/0010-server-sent-events-support.md).
In this repo, route registration lives in `internal/app/http/routes.go`; business
state still comes from app stores or daemon RPC, not from HTTP handlers.

Remote app access uses an outbound connector from `internal/app/relay` to the
Cloudflare Worker. The Worker must not create pairing sessions, hold device
grants, or talk to the daemon; it only routes allowed HTTP/SSE traffic back to
the local app process.

## Change Flow

For a new capability, start with ownership: config, daemon state, observed
snapshot, or build flag. Then update `internal/rpc`, daemon behavior, tests, CLI,
HTTP/app live snapshot, MCP if LLM-visible, SPA if user-visible, and generated
docs when MCP/config references change.

Market-event flags are daemon-owned observed context. Adapters render or filter
the typed `market_events.snapshot` result; they do not refetch Reg SHO, halt,
borrow-inventory, or borrow-fee sources or duplicate proposal-blocking policy.
