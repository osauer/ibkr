# Trading order workflow

**Status:** Ephemeral proposal for maintainer review. Do not implement until approved; delete this file after successful implementation and migrate durable facts into README, reference docs, skills, and release notes.
**Created:** 2026-05-25 21:08 CEST
**Owner:** osauer
**Related:** [README.md](../../README.md), [docs/reference/protocol.md](../reference/protocol.md), [skills/ibkr/SKILL.md](../../skills/ibkr/SKILL.md), [internal/rpc/rpc.go](../../internal/rpc/rpc.go), [pkg/ibkr/orders.go](../../pkg/ibkr/orders.go)

## Decision

Add optional order entry for equities / ETFs and single-leg listed options,
including live-capable execution in v1, but keep a fresh install non-trading.
Live trading is not a hidden default. It requires explicit local configuration,
a pinned account and endpoint, a recent successful paper trading gate, and a
separate live acknowledgement. The final trading permission belongs to TWS / IB
Gateway: if the gateway is in Read-Only API mode, or the account does not have
permissions, `ibkr` surfaces the broker rejection. `ibkr` still keeps a local
intent gate so a new install, MCP host, or shell agent cannot silently cross
from research into execution.

This changes the public safety contract. Even if default behavior remains
non-trading, the first release containing order execution should be treated as
a major user-facing release because README, site metadata, MCP discovery files,
security copy, and the Claude skill currently promise no order-entry surface.

## Scope

First release:

- Paper and live execution modes. Both use the same preview, `WhatIf`,
  journaling, submit, modify, cancel, and reconciliation path. Live mode adds
  stricter startup and confirmation gates.
- Stocks and ETFs (`STK`) routed through the same contract-resolution paths
  used by `quote`, `positions`, and `watch`.
- Single-leg listed options (`OPT`) using explicit expiry / right / strike.
- Order actions: buy, reduce, and close. Every preview classifies position
  effect as `open`, `reduce`, `close`, or `flip`. Stock shorts / flips and
  option sell-to-open are out of scope for the first release.
- Order types: `LMT` only. `MKT`, `STP`, and `STP LMT` remain blocked until a
  later paper gate covers them.
- Time-in-force: `DAY` only. `GTC` waits until modify / cancel reconciliation
  is reliable across daemon restarts.
- Lifecycle: preview, place, open orders, status, modify, cancel, reconcile.

Non-goals:

- Multi-leg option combos, brackets, OCO, trailing stops, scale orders,
  conditional orders, portfolio allocation, global cancel, auto-trading loops,
  and FA allocation.
- Client ID 0 auto-binding of manual TWS orders.
- Strategy-performance testing from paper fills. Paper smoke proves protocol
  and lifecycle handling, not execution quality.
- Any claim that the app makes a live order safe to trade. The app can enforce
  intent, scope, and observability gates; market risk remains the user's and
  broker permissions remain authoritative.

## User story

The intended workflow reuses today's read-side commands before any order
surface appears.

### 0. First install and enablement

A fresh install remains read-only. There should be no startup dialog that asks
new users to enable trading before they have asked for an order command. The
first deliberate trading moment is either:

```sh
ibkr trading configure
```

or an attempted order command that fails closed and points at the configure
flow. The configure flow is the startup dialog for trading:

1. Pick `paper` or `live`.
2. Pin host, port, client ID, and account. Auto-discovered endpoints cannot
   submit orders.
3. Confirm that TWS / Gateway owns the final API permission, including Read-Only
   API mode and account trading permissions.
4. For live mode, require an already successful paper smoke for the same
   installed version and require a live acknowledgement tied to the account and
   endpoint.
5. For MCP write tools, require a separate MCP write enablement and an
   out-of-band human nonce flow.

This keeps research usage frictionless while making execution opt-in and
auditable.

### 1. Check connectivity, account, and trading readiness

```sh
ibkr status
ibkr account
ibkr positions --by underlying
ibkr regime
```

`ibkr status` becomes the trust dashboard. It must include:

```text
Trading
  local gate: disabled | paper | live
  broker gate: unknown | read-only-rejected | paper-smoke-passed | last-submit-accepted
  endpoint: 127.0.0.1:4002 paper pinned
  account: DU1234567 pinned
  client id: 15 pinned
  MCP trading: disabled | preview-only | paper-write | live-write
  preview required: true
  open orders: 2
  last order event: AAPL BUY 10 LMT 248.80 Submitted at 2026-05-25 15:32 CEST
  paper smoke: pass at 2026-05-25 13:50 CEST
  live override: blocked | ready
```

If trading is enabled but the endpoint or account is not pinned, status is
blocked:

```text
Trading: blocked
reason: order submission requires a pinned endpoint and pinned account; current endpoint was auto-discovered
```

If live trading is configured but the live override is incomplete, status is
also blocked:

```text
Trading: blocked
reason: live trading requires allow_live=true, a matching live account acknowledgement, and a recent successful paper smoke
```

The broker gate cannot be perfectly known from a passive read. It starts
`unknown`, then updates from concrete evidence: successful paper smoke,
successful accepted submit, or a TWS Read-Only API / permission rejection.
Evidence is bound to account, endpoint, client ID, gateway session, and
timestamp; any mismatch invalidates it.

### 2. Identify a symbol and current price context

```sh
ibkr watch --quotes
ibkr scan --type TOP_PERC_GAIN --exchange STK.US.MAJOR --limit 25 --json
ibkr quote AAPL --json
ibkr chain AAPL
ibkr chain AAPL --expiry 2026-06-19 --width 5 --json
ibkr quote SPY 20260619 C 520 --json
```

`order preview` must echo the quote or chain row it used: bid, ask, midpoint,
data type, timestamp / freshness, session context, and spread quality. A
midpoint-derived default is allowed only when bid/ask are fresh enough and the
spread passes the strategy's limits.

### 3. Check the current risk profile

```sh
ibkr positions --symbol AAPL --by underlying --json
ibkr account --json
ibkr size --symbol AAPL --entry 248.80 --stop 240 --risk-pct 0.5 --json
```

`order preview` should reuse the sizing/account/positions logic rather than
fork a second risk engine. Optional `--stop` and `--target` arguments are
analysis inputs only unless a later bracket-order feature explicitly creates
attached child orders.

### 4. Preview the order

Equity:

```sh
ibkr order preview buy AAPL 10 --strategy patient-limit
ibkr order preview buy AAPL 10 --limit 248.80 --tif DAY --stop 240
```

Option:

```sh
ibkr order preview buy SPY 20260619 C 520 1 --limit 2.10
ibkr order preview sell SPY 20260619 C 520 1 --limit 2.20 --close-only
```

Preview output includes:

- Canonical contract and resolved conid when available.
- Side, quantity, order type, limit price, TIF, outside-RTH flag.
- Strategy used and the exact quote inputs behind computed price.
- Notional / premium, max cash at risk for long options, local position-effect
  classification, position-after summary, and broker-backed margin impact.
- Warnings: stale or frozen data, wide option spread, market closed,
  live endpoint, mode / account mismatch, missing stop/risk input, partial
  position coverage for close-only sells.
- A short-lived preview token binding account, endpoint, client ID, order
  draft, quote observation, strategy, current order version for modify/cancel,
  IBKR `WhatIf` margin state, and expiry.

`WhatIf` is required before any place/modify token can execute. Local account,
position, and buying-power math is a disclosure aid, not the acceptance gate.
The daemon must submit the same order draft with IBKR's `WhatIf` flag, capture
the returned `OrderState` margin fields, and bind the preview token to that
broker response. If `WhatIf` is unavailable, times out, or returns a broker
rejection, the preview can render diagnostics but cannot produce an executable
token.

### 5. Place, modify, and cancel through explicit confirmation

New order:

```sh
ibkr order place --preview-token TOKEN --yes
ibkr orders open
ibkr order status 12345
```

Modify:

```sh
ibkr order modify 12345 --limit 248.20 --preview
ibkr order modify 12345 --preview-token TOKEN --yes
```

Cancel:

```sh
ibkr order cancel 12345 --preview
ibkr order cancel 12345 --preview-token TOKEN --yes
```

`modify` is a first-class operation, not an afterthought. V1 native modify is
restricted to limit price and total quantity on orders created by the same
username / client ID / order ID. IBKR's same-order-id modification goes through
`placeOrder` when the existing order is active and TWS accepts the changed
fields. The preview must refuse TIF, order-type, account, route, contract, and
outside-RTH changes; those require a future explicit cancel-replace workflow.
Queue-priority implications are surfaced in the preview, not hidden.

When modifying quantity, `--qty` means desired total order quantity, not
additional shares/contracts. If the order is partially filled, the preview must
show filled, remaining, requested total, and reject any total below filled.

The preview token for modify/cancel binds to the observed order status,
remaining quantity, limit price, and update timestamp. If the order fills,
partially fills, rejects, or otherwise changes before confirmation, the token
is invalid and the user must preview again.

## CLI surface

Configuration:

```sh
ibkr trading status [--json]
ibkr trading configure --mode paper|live --host 127.0.0.1 --port PORT --account ACCOUNT --client-id ID
ibkr trading disable
ibkr trading paper-smoke [--json]
ibkr trading live-check [--json]
ibkr trading mcp status [--json]
ibkr trading mcp enable --mode preview|paper-write|live-write
ibkr trading mcp disable
ibkr trading mcp nonce --scope place|modify|cancel --mode paper|live --ttl 5m
```

Order lifecycle:

```sh
ibkr orders open [--account ACCOUNT] [--json]
ibkr order status <order-id|perm-id> [--json]
ibkr order preview buy|sell SYMBOL QTY [--limit PRICE|--strategy NAME] [--tif DAY] [--stop PRICE] [--target PRICE] [--json]
ibkr order preview buy|sell SYMBOL YYYYMMDD C|P STRIKE CONTRACTS [--limit PRICE|--strategy NAME] [--close-only] [--json]
ibkr order place --preview-token TOKEN [--yes] [--live-ack ACCOUNT] [--json]
ibkr order modify <order-id> [--limit PRICE] [--qty QTY] --preview [--json]
ibkr order modify <order-id> --preview-token TOKEN [--yes] [--live-ack ACCOUNT] [--json]
ibkr order cancel <order-id> --preview [--json]
ibkr order cancel <order-id> --preview-token TOKEN [--yes] [--live-ack ACCOUNT] [--json]
```

No direct `ibkr order place buy AAPL ...` path in the first release. Every
write goes through preview token confirmation, even for a human in a shell.
For live-mode CLI writes, non-interactive commands must include
`--live-ack ACCOUNT`; interactive commands prompt the user to type the account
before sending.

## Strategy layer

The CLI should expose strategy names, not raw broker algo fields as the main
interface. Strategies compile into a concrete order draft. The first strategy
can be plain limit-order logic; the abstraction leaves room for IBKR broker
algos later.

Initial strategies:

- `patient-limit` (default for stocks / ETFs only): compute a side-aware
  midpoint limit from fresh bid/ask. For buys, round midpoint down to a valid
  tick unless that would cross below bid; for sells, round midpoint up unless
  that would cross above ask. Reject if midpoint is unavailable.
- `join` (stocks / ETFs only): buy at bid or sell at ask. Useful when the user
  wants price priority.
- `cross-small`: improve toward the opposite side within `--max-slippage`.
  Disabled for options until option spread handling is battle-tested.
- `explicit-limit`: set by `--limit`; skips strategy price computation but
  still runs quote freshness, spread, risk, and session warnings.

Blocked initially:

- `market` for options.
- Strategy-computed prices for options. V1 options require explicit `--limit`
  until min tick, quote freshness, entitlement, multiplier, and spread gates are
  proven in paper.
- Any strategy that slices time or repeatedly modifies orders.
- Broker algo strategies such as Adaptive or MidPrice until the wire encoder,
  docs, and paper gate cover their fields.

Spread and freshness defaults:

- Strategy-computed prices require live or recent data. Frozen/delayed data is
  allowed only for explicit-limit previews and must require an acknowledgement
  before submit.
- Option midpoint is rejected when bid/ask are missing, bid is zero, ask <= bid,
  or spread exceeds a configured threshold. A conservative first threshold is
  max($0.10, 10% of midpoint), and live mode should expose that threshold in
  status and config.
- Outside regular trading hours is false by default. `--outside-rth` is an
  explicit user choice and only applies where IBKR / venue supports it.

## Configuration

Add a `[trading]` section:

```toml
[trading]
enabled = false
mode = "paper"                  # paper | live
require_preview = true
max_notional = 10000
max_option_contracts = 5
allow_stock_short = false
allow_option_sell_to_open = false
allow_option_market_orders = false
allow_live = false
live_ack_account = ""
live_ack_endpoint = ""
paper_smoke_required_for_live = true
paper_smoke_max_age = "168h"
last_paper_smoke_at = "2026-05-25T13:50:00+02:00"
last_paper_smoke_version = "vX.Y.Z"
mcp_enabled = false
mcp_mode = "preview"            # preview | paper-write | live-write
mcp_nonce_ttl = "5m"
```

Submission blockers:

- `[trading].enabled` is not true.
- `[gateway].account` is absent.
- `[gateway].port` is absent.
- `[gateway].client_id` is absent.
- `mode` is anything other than `paper` or `live`.
- `mode=paper` but port/account do not look paper-like (`4002`, `7497`, or
  account prefix / explicit override).
- `mode=live` but `[trading].allow_live` is not true.
- `mode=live` but `live_ack_account` and `live_ack_endpoint` do not match the
  current pinned account and endpoint.
- `mode=live` but no recent successful paper smoke is recorded for the current
  installed version, pinned account family, endpoint class, and client ID.
- Live CLI confirmation is non-interactive and missing `--live-ack ACCOUNT`.
- Preview token is missing, expired, mismatched, or not generated by this
  daemon key.
- Preview token is not bound to a successful broker `WhatIf` result for the
  exact order draft or modify draft being sent.
- MCP submit/modify/cancel is attempted while `mcp_enabled=false`, while
  `mcp_mode` lacks the required write scope, or without a fresh out-of-band
  human nonce minted by the CLI.

Runtime library change:

- Replace the package build-tag-only guard with a runtime config flag.
  `DefaultConfig()` remains non-trading, so direct Go callers still get
  `ErrTradingDisabled` unless they opt in. The daemon passes the runtime
  opt-in only after config and readiness checks pass.
- Keep the old `ErrTradingDisabled` sentinel for compatibility.

## RPC and wire surface

Add daemon RPC methods:

- `trading.status`
- `orders.open`
- `order.status`
- `order.preview`
- `order.place`
- `order.modify.preview`
- `order.modify`
- `order.cancel.preview`
- `order.cancel`

Add typed payloads in `internal/rpc`:

- `TradingStatus`, `TradingBlocker`, `TradingMode`, `BrokerTradingGate`
- `OrderContract`, `OrderDraft`, `OrderStrategy`, `OrderPreviewResult`
- `OrderWhatIfResult`, `OrderMarginImpact`
- `OrderTokenRef`, `OrderPlaceResult`, `OrderModifyResult`,
  `OrderCancelResult`
- `OrderView`, `OrderLifecycleStatus`, `OrderEvent`

The typed lifecycle status is the critical state surface. Consumers should not
infer from missing orders or raw IBKR strings alone. Map IBKR states into:

- `previewed`
- `pending_submit`
- `pre_submitted`
- `submitted`
- `partially_filled`
- `filled`
- `pending_cancel`
- `cancelled`
- `rejected`
- `inactive`
- `unknown_reconcile_required`

Wire protocol coverage to add in `pkg/ibkr`:

- `reqIds` / `nextValidId` as the allocator source.
- `placeOrder` for new and native modify.
- `cancelOrder`.
- `reqOpenOrders` for same-client active orders.
- `reqAllOpenOrders` for read-only display only; do not modify orders not
  owned by this client.
- `openOrder`, `openOrderEnd`, `orderStatus`, error message mapping.
- `execDetails` in v1 reconciliation. IBKR documents duplicate and missing
  `orderStatus` callbacks, so fills and terminal exposure cannot rely on
  status alone. `commissionReport` can be nullable in v1 but the execution
  stream is required.
- Later: complete commission reporting once fills are in scope for P&L
  attribution.

Avoid `reqAutoOpenOrders(true)` and client ID 0. IBKR documents that client 0
can bind manual TWS orders to the API client; that is too broad for this
product.

## State, idempotency, and audit

Order execution needs a durable local journal. This is not a permission layer;
it is the source of truth for what this daemon attempted and how it reconciled
broker events.

Recommended state:

- `$XDG_STATE_HOME/ibkr/order-journal.jsonl`, falling back to
  `$HOME/.local/state/ibkr/order-journal.jsonl`.
- Directory mode `0700`, file mode `0600`, same sensitivity posture as wire
  logs.
- Append-only events: previewed, token-confirmed, send-attempted, send-error,
  broker-acknowledged, status-updated, modify-requested, cancel-requested,
  reconciled-unknown.
- `order_ref` set on every API order, for example
  `ibkr-20260525-153210-<short-token>`.
- `reserved_order_id`, `client_id`, `perm_id` when known, and current
  `send_state` (`reserved`, `send_attempted`, `broker_acknowledged`,
  `uncertain_send`, `terminal`).
- `preview_token_id` recorded without storing the full token.

Idempotency rules:

- A confirmed token can execute at most once.
- Reserve and journal the IBKR order ID before calling `placeOrder`. The ID
  must be greater than any order ID observed through `nextValidId`, `openOrder`,
  `orderStatus`, or `reqAllOpenOrders`.
- If the CLI loses its socket after confirmation, retrying with the same token
  returns the existing order result when the journal proves the send happened.
- If the socket dies after `send_attempted` but before broker acknowledgement,
  mark `uncertain_send`, reconnect, request open orders / executions, and
  reconcile before allowing another token to place the same order.
- On daemon restart, request open orders for this client ID and reconcile by
  order ID, permanent ID, and `order_ref`. Anything the journal says was sent
  but the gateway no longer reports becomes `unknown_reconcile_required` until
  executions or terminal status explain it.
- Never infer "filled" from disappearance from open orders alone. Cancelled,
  filled, rejected, expired, and disconnected states all need explicit broker
  evidence or an unknown state.

## MCP and skill contract

MCP tools in the first implementation:

- `ibkr_trading_status`
- `ibkr_orders_open`
- `ibkr_order_status`
- `ibkr_order_preview`
- `ibkr_order_place`
- `ibkr_order_modify_preview`
- `ibkr_order_modify`
- `ibkr_order_cancel_preview`
- `ibkr_order_cancel`

MCP write tools exist in v1, including live-capable tools, but are unusable
until both local config and per-action human intent are present. A write tool
requires:

- `[trading].mcp_enabled=true`.
- `mcp_mode=paper-write` for paper orders or `mcp_mode=live-write` for live
  orders.
- A preview token produced by `ibkr_order_preview` or the matching
  modify/cancel preview.
- A human nonce minted outside MCP by `ibkr trading mcp nonce`. The nonce binds
  scope (`place`, `modify`, or `cancel`), mode, account, endpoint, client ID,
  preview token or order ID, issue time, expiry, and single-use state.

The MCP server must not expose a tool that mints executable nonces. This keeps
MCP write support real while preventing a model from chaining preview -> submit
inside the same tool surface without human participation.

Tool descriptions must say when to invoke and when not to invoke. Preview and
write descriptions must say:

- Requires explicit user request.
- Preview tools do not execute an order.
- Place, modify, and cancel tools do execute broker API writes when all gates
  pass, including live writes when live mode is configured.
- Write tools require a preview token and an out-of-band human nonce; they
  cannot mint the nonce themselves.
- Does not override TWS / Gateway Read-Only API or broker permissions.
- Not for market data, option discovery, or portfolio analysis; use quote,
  chain, positions, account, and regime tools first.

Skill policy:

- Allow read, status, open orders, order status, order preview, and MCP write
  tools only when the user has explicitly asked for that specific place,
  modify, or cancel action.
- Do not allow the skill to infer execution intent from analysis requests such
  as "what should I buy?" or "build a trade plan".
- Keep Bash hooks conservative: allow preview/status/open-order reads; block
  direct CLI place/modify/cancel and `ibkr trading mcp nonce` unless the user
  explicitly requested the exact action in the current turn.
- Skill examples must show the read -> preview -> human nonce -> write chain
  and label live-mode examples as live.

## Risk review

| Risk | Protection |
|---|---|
| Fresh install unexpectedly trades | Trading disabled by default; no write command works without config + preview token. |
| Auto-discovery lands on live Gateway | Submit requires pinned port and account; status blocks discovered endpoints. |
| User enables live by accident | Live mode requires `mode=live`, `allow_live=true`, account / endpoint acknowledgement, recent paper smoke, and per-order live acknowledgement in non-interactive CLI writes. |
| TWS Read-Only API is still enabled | Broker rejection is surfaced as typed `trading_rejected`; status records last evidence. |
| Agent submits without human intent | MCP writes require explicit enablement, preview token, and out-of-band human nonce; Bash hook blocks direct CLI writes and nonce minting without exact user intent. |
| Human nonce leaks into logs or chat | Nonces are scoped, short-lived, single-use, redacted in logs, and bound to account, endpoint, client ID, mode, and preview token / order ID. |
| Duplicate submit after retry | Order refs are deterministic and persisted before send; confirmation is idempotent by token/ref. |
| Broker margin/risk differs from local math | Executable preview requires IBKR `WhatIf`; local math is never the acceptance gate. |
| `WhatIf` accepts but real submit rejects | Submit result is still broker-authoritative; preview describes `WhatIf` as margin/risk preflight, not execution acceptance. |
| `orderStatus` skips a fill transition | V1 reconciliation consumes `execDetails`; disappearance from open orders is never enough. |
| User modifies an order that just filled | Modify token binds order version/status/remaining quantity; stale tokens reject. |
| Modify changes queue priority or economics | Preview calls out native modify semantics and restricts v1 modify to limit price and total quantity. |
| Cancel pending still fills | Cancel result reports `pending_cancel` until `orderStatus` confirms cancelled; never claims finality early. |
| Midpoint price is stale or fake | Strategy prices require fresh bid/ask and valid spread; explicit-limit path carries warnings. |
| Option spread is too wide | Strategy refuses wide spreads; user must provide explicit limit and acknowledge. |
| Sell/short/flip risk | First release blocks stock shorts/flips and option sell-to-open unless a later risk model explicitly supports them. |
| Manual TWS orders get captured | Avoid client ID 0 and auto-bind. Display all-open-orders separately from owned mutable orders. |
| Public docs still say read-only | Shipping checklist updates README, landing page, llms.txt, metadata, SECURITY, PRIVACY, skill, MCP docs. |
| Paper tests pass but live behaves differently | Release gate proves paper lifecycle; live enablement requires explicit override and status labels live behavior as broker-dependent. Optional live check is `WhatIf` only and never places a live order. |

## Testing and gates

Unit tests:

- Config parse / defaults / blocker derivation.
- CLI parsing and flag hoisting for order commands.
- Strategy price computation and tick rounding.
- Position-effect classification (`open`, `reduce`, `close`, `flip`) for stock
  and option positions.
- `WhatIf` preview request/response handling and broker rejection mapping.
- Preview token generation, expiry, mismatch, and order-version binding.
- Lifecycle mapping from raw IBKR statuses.
- No direct write path without preview token.
- MCP schemas for preview and write tools, including disabled-by-default
  behavior, scope checks, and human nonce validation.
- Live-mode blocker derivation: missing `allow_live`, stale paper smoke,
  acknowledgement mismatch, and non-interactive missing `--live-ack`.
- Skill / hook regex parity.
- Docs regeneration.

Hermetic wire tests:

- `placeOrder` field order for equity and option limit orders.
- `placeOrder` with same order ID for native modify.
- `cancelOrder` payload.
- `openOrder` and `orderStatus` parsing.
- `execDetails` parsing and reconciliation.
- Error mapping for read-only / permission / price-increment / not-cancellable
  broker responses.

Live paper gate:

```sh
IBKR_TEST_PORT=4002 make trading-smoke
```

`trading-smoke` must:

1. Fail unless endpoint is pinned to paper mode.
2. Fail unless account looks paper or the test env explicitly acknowledges a
   custom paper account.
3. Start an isolated daemon, not the user's normal daemon.
4. Use a dedicated paper account, pinned client ID, and known liquid instrument
   (`AAPL` or `SPY`) with max notional below the configured cap.
5. Preview a small equity limit order priced inside IBKR precaution bands but
   away from the touch. Too-far prices can be rejected before lifecycle code is
   exercised; too-close prices can fill before modify/cancel.
6. Place it, wait for `openOrder` / `orderStatus`.
7. Modify only limit price away from market, wait for updated status.
8. Cancel it, wait for confirmed `cancelled`; if it fills, the test fails
   unless it can prove the fill happened before the cancel request was sent.
9. If option permissions are available, repeat preview/place/modify/cancel for
   a small long option limit order; otherwise skip only the option subcase with
   an explicit entitlement message.
10. Assert wire frames include the expected outbound and inbound lifecycle,
    including `WhatIf`, `openOrder`, `orderStatus`, and `execDetails`.

Release gate:

- A release containing trading must run strict paper smoke. No gateway should
  fail the release path, not skip.
- Existing `make smoke` can continue to skip on no gateway for read-only flows.
- `make check` remains static; `make trading-smoke` is live-paper only.
- Live smoke must never place a real order. If provided, `ibkr trading
  live-check` is limited to configuration validation and broker `WhatIf`
  preflight against the pinned live account.

## Impacted files

Implementation:

- `internal/config/config.go` - `[trading]` config, docgen comments if env
  overrides are added.
- `internal/rpc/rpc.go` - methods and typed order/trading payloads.
- `internal/daemon/server.go` - status, connector config, order dispatcher.
- `internal/daemon/orders.go` - new order preview/place/modify/cancel logic.
- `internal/daemon/order_journal.go` - durable append-only order audit and
  reconciliation state.
- `internal/daemon/handlers.go` - quote, account, position helpers reused by
  preview.
- `internal/cli/cli.go` - command registry and value flags.
- `internal/cli/order.go`, `internal/cli/orders.go`, `internal/cli/trading.go`
  - new command handlers.
- `internal/cli/status.go` - render trading readiness.
- `internal/mcp/tools.go` - trading/order tool registry and schemas.
- `pkg/ibkr/trading_disabled.go`, `pkg/ibkr/trading_enabled.go` - replace or
  narrow build-tag guard.
- `pkg/ibkr/connection.go` - order lifecycle parsing and runtime write guard.
- `pkg/ibkr/connector.go` - submit/modify/cancel/open-order methods.
- `pkg/ibkr/orders.go`, `pkg/ibkr/types.go` - order model validation.
- `cmd/wire-assert/main.go` - order lifecycle wire assertions.
- `scripts/trading-smoke.sh`, `Makefile` - paper trading gate.

Tests:

- `internal/config/config_test.go`
- `internal/rpc/rpc_test.go`
- `internal/daemon/*order*_test.go`
- `internal/daemon/*journal*_test.go`
- `internal/cli/*order*_test.go`
- `internal/mcp/tools_test.go`
- `pkg/ibkr/*order*_test.go`
- `cmd/wire-assert/main_test.go`
- `test/integration/*trading*_test.go` only if kept skip-safe; release smoke
  should be the binding paper gate.

Docs and public metadata:

- `README.md`
- `SECURITY.md`
- `PRIVACY.md`
- `docs/index.html`
- `docs/interactive-brokers-mcp-server/index.html`
- `docs/llms.txt`
- `docs/mcp-server.json`
- `docs/.well-known/mcp/server.json`
- `docs/reference/protocol.md`
- `docs/reference/config.md`
- `docs/reference/mcp-tools.md` via `make docs-regen`
- `docs/guides/agentic-use.md`
- `docs/guides/marketplace-readiness.md`
- `.github/release-notes-template.md`
- `.claude-plugin/plugin.json`
- `settings/ibkr.settings.json`
- `hooks/hooks.json`
- `skills/ibkr/SKILL.md`
- `skills/ibkr/schemas.md`
- `scripts/discovery-check/main.go` because safety metadata can no longer
  require `can_place_orders=false` unconditionally.

## Review questions

1. Should the first execution release be `v2.0.0` because the public contract
   changes from read-only to optionally trading-capable?
2. Confirm live-capable execution belongs in v1 behind `allow_live`, account /
   endpoint acknowledgement, recent paper smoke, and per-action live
   acknowledgement.
3. Confirm MCP v1 should include place, modify, and cancel write tools guarded
   by config plus an out-of-band CLI-minted human nonce.
4. Confirm `patient-limit` is the default for stocks only, with options
   requiring explicit `--limit` in v1.
5. Do we want `ibkr order place --preview-token` or a generic
   `ibkr order confirm --preview-token`? `place` is clearer for new orders;
   `confirm` is cleaner across place/modify/cancel.

## External references

- IBKR order submission docs: next valid ID, `placeOrder`, `openOrder`, and
  `orderStatus`: https://interactivebrokers.github.io/tws-api/order_submission.html
- IBKR order modification docs: same-order-id modification behavior and queue
  priority caveats: https://interactivebrokers.github.io/tws-api/modifying_orders.html
- IBKR margin / `WhatIf` docs: order-state margin preflight:
  https://interactivebrokers.github.io/tws-api/margin.html
- IBKR Campus order lifecycle and active-order retrieval docs: https://www.interactivebrokers.com/campus/ibkr-api-page/twsapi-doc/
- IBKR active orders docs, including same-client order ownership and client ID
  0 auto-binding caveat: https://interactivebrokers.github.io/tws-api/open_orders.html
- TWS API setup docs mentioning Read-Only API: https://www.interactivebrokers.com/campus/ibkr-api-page/twsapi-doc/
