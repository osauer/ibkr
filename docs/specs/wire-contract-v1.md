# Wire contract v1 — planning

**Status**: planning (no implementation yet)
**Created**: 2026-05-22 06:04 CEST
**Owner**: osauer
**Drives**: decoupling internal compute types from the JSON wire format, and the path to a network-exposable contract for a future SPA / mobile client.

## Why this exists

Today the daemon's JSON-RPC wire types (`internal/rpc/rpc.go`, 1,531 LOC) double as both the wire format and the daemon's compute output structures. The CLI, MCP server, and `cmd/_preview` all consume the same Go types directly. There is no mapper layer; what the daemon computes is what goes on the wire.

This works for the current consumer mix — three Go consumers that ship in lockstep with the daemon — but it has two costs that will start to bite:

1. **Internal compute changes are wire changes.** Refactoring how regime or gamma compute things almost always requires touching `rpc.go`, even when the consumer-visible effect is identical. The CHANGELOG cannot cleanly distinguish "internal change" from "contract change."
2. **No safe path to remote consumers.** A browser SPA or mobile app would not ship in lockstep with the daemon — clients can be stuck on old versions for weeks (app-store update lag). Without a versioned, stable contract and a DTO layer, every internal refactor risks breaking deployed clients.

A SPA / mobile client is a **possible direction** (not yet committed), with mobile remote access as the primary use case. This doc captures the plan so that:

- The DTO split (Phase A) can be executed when the in-flight features land — without re-deriving the analysis.
- The network contract design (Phase B) has a starting point when the SPA decision is firmed up.

## Current state of `internal/rpc/rpc.go`

1,531 LOC, ~50 types, ~10 constant blocks. Categorising the types by their role:

| Category | Types | Notes |
|---|---|---|
| Pure envelope / protocol | `Request`, `Response`, `Error`, `Frame`, `FrameError`; method-name consts; error-code consts | Clean. Already DTO-shaped. No leak. |
| Pure data DTOs | `Quote`, `ChainStrike`, `ChainExpiry`, `HistoryBar`, `HistoryDailyResult`, `ScanRow`, `ScanResult`, `ScanListResult`, `ScanPresetSummary`, `AccountResult`, `CurrencyExposure`, `BackgroundTaskStatus`, `HealthResult` | Clean. Just shape. |
| Mostly clean, lightly leaky | `PositionView` (~40 fields incl. Greeks/option-specific), `PositionsPortfolio`, `PositionGroup`, `BreadthSPXResult` (carries a `History` array with daemon-internal constituent counts) | Well-documented. Big payloads on mobile; could be slimmed via a `view=summary` variant later. Not conceptually wrong. |
| Compute-shape leaks | `GammaZeroComputed` (Profile sweep, TopStrikes, near/term mirrored fields, `SkewFitInfo` per-expiry diagnostics, `DerivedIVLegs`, `Warnings`); per-indicator structs in `RegimeSnapshotResult` (per-scalar `Quality` provenance, `FieldsMissing []string` daemon-internal list) | These reflect how the daemon computes, not what the consumer needs to know. Primary targets for the DTO split. |
| Helpers (functions) | `IsLiveDataType(dt string) bool`, `IsOptionRTH(now time.Time) bool` | Pure. Cohere with the contract. Move with the wire types. |

**Daemon-side coupling**: ~104 distinct `rpc.*{}` construction sites in non-test daemon code. Heaviest concentrations: `GammaZeroComputed` (55), `PositionView` (51), `RegimeSnapshotResult` (16). The mapper layer would be ~15–20 functions, one per top-level result envelope.

## Phase A — DTO split (preconditional, mechanical)

**Goal**: decouple the wire types from internal compute types so the daemon can refactor freely without touching the contract. **No new transport. No new wire format. No behaviour change.**

### A.1 — File reorganisation within `internal/rpc/`

Split `rpc.go` into multiple files in the same package, by topic. Compiles to identical bits.

```
internal/rpc/
├── doc.go              package docs (moved from rpc.go header)
├── methods.go          method-name consts (MethodAccountSummary, …)
├── envelope.go         Request, Response, Error, Frame, FrameError, error codes
├── helpers.go          IsLiveDataType, IsOptionRTH, MarketData* consts
├── account.go          AccountResult, CurrencyExposure
├── positions.go        PositionView, PositionsResult, PositionsPortfolio, PositionGroup,
│                       PositionsListParams, ContractParams, SecType* consts
├── quote.go            Quote, QuoteSnapshotParams, QuoteSubscribeParams, CancelParams
├── chain.go            ChainStrike, ChainExpiry, ChainExpiriesParams, ChainExpiriesResult,
│                       ChainFetchParams, ChainResult
├── scan.go             ScanRow, ScanResult, ScanListResult, ScanPresetSummary,
│                       ScanRunParams, ScanParamsParams, ScanParamsResult, ScanParam*
├── history.go          HistoryBar, HistoryDailyResult, HistoryDailyParams
├── breadth.go          BreadthSPXResult, BreadthDailyValue, BreadthState, BreadthSPXParams
├── gamma.go            GammaZeroSPXResult, GammaZeroComputed, GammaProfilePoint,
│                       StrikeConcentration, SkewFitInfo, GammaZeroSPXParams,
│                       GammaZeroParams, GammaZeroStatus* + Scope* consts
├── regime.go           RegimeSnapshotResult, RegimeSnapshotParams, RegimeVIXTerm,
│                       RegimeHYGSPYDivergence, RegimeUSDJPY, RegimeGammaZero, RegimeBreadth,
│                       Quality, StreakInfo, RegimeStatus* + freshness/confidence consts
└── health.go           HealthResult, BackgroundTaskStatus
```

Same package, same import path (`github.com/osauer/ibkr/internal/rpc`). No call-site changes. This is purely for navigability.

**Effort**: a few hours.
**Risk**: near-zero. `make check` catches any mis-import; the file split is mechanical.

### A.2 — Mapper layer at daemon exit boundary

Introduce daemon-internal compute types for the three biggest leaks. Each handler ends with a single `toRPC(...)` call that maps internal → wire.

#### A.2.1 — `internal/daemon/regime`

Today: `internal/daemon/regime.go` fetchers (`fetchRegimeVIXTerm`, etc.) return `rpc.RegimeVIXTerm` etc. directly. `handleRegimeSnapshot` assembles `*rpc.RegimeSnapshotResult` field by field.

Phase A:
- Introduce `internal/daemon/regime/types.go` with `VIXTermResult`, `HYGSPYResult`, `USDJPYResult`, `GammaZeroResult`, `BreadthResult`, and `SnapshotResult` — daemon-internal types that may carry more or different fields than the wire (e.g. `FieldsMissing` stays internal).
- `internal/daemon/regime/wire.go` contains `(SnapshotResult) ToRPC() *rpc.RegimeSnapshotResult` + per-indicator mappers.
- Fetchers return the internal type; `handleRegimeSnapshot` calls `.ToRPC()` once at the end.

#### A.2.2 — `internal/daemon/gamma`

Today: `gamma_zero_compute.go` builds `rpc.GammaZeroComputed` field by field across 55 sites (the leaky compute-shape type).

Phase A:
- Introduce `internal/daemon/gamma/types.go` with `Computed` carrying the same fields, but free to evolve.
- `internal/daemon/gamma/wire.go` provides `(Computed) ToRPC() *rpc.GammaZeroComputed`.
- The mapper is identity at v1 — boilerplate, but it's the hook for any future trimming (e.g. drop `SkewFitInfo` from the public wire while keeping it for daemon logs).

#### A.2.3 — `internal/daemon/positions`

Today: `PositionView` is constructed across ~51 sites in `handlers.go` / `positions.go` / aggregators.

Phase A:
- Introduce `internal/daemon/positions/types.go` with `View` and `Portfolio`.
- `internal/daemon/positions/wire.go` provides mappers.
- Identity at v1.

#### A.2.4 — Identity for the rest

For the 12 other result types (account, quote, chain, scan, history, breadth, health), introduce identity mappers. They are boilerplate today, but having the seam in place means Phase B doesn't have to add it in a hurry.

### A.3 — Verification

- `make check && make smoke` must pass.
- `make smoke` exercises account / chain / regime / gamma — output must be byte-identical to pre-refactor for the same input.
- Test files in `internal/daemon/` may need import path updates if they reach into compute internals; otherwise no changes.

### A.4 — What Phase A buys

- The daemon can refactor regime / gamma / positions compute internals without touching `rpc.go`.
- The CHANGELOG can cleanly distinguish "internal compute change" from "wire contract change."
- Phase B becomes much smaller: the mapper-layer hook is already there.

### A.5 — What Phase A does NOT do

- No new transport. Unix socket only.
- No new wire format. JSON-RPC as-is.
- No version negotiation. Wire is still implicitly v1.
- No payload slimming for mobile.

### A.6 — Effort estimate

| Step | Effort |
|---|---|
| A.1 file reorg | ~3 hours |
| A.2.1 regime mapper | ~4 hours |
| A.2.2 gamma mapper | ~4 hours |
| A.2.3 positions mapper | ~4 hours |
| A.2.4 identity mappers (12 types) | ~3 hours |
| Test fixes, `make check`, `make smoke`, `ibkr` artefact runs | ~4 hours |
| **Total** | **~22 hours, ~3 focused days** |

Can be done as one worktree agent task. Bounded, mechanical, no behaviour change.

## Phase B — network contract for remote / mobile (SPA-driven)

**Trigger**: SPA / mobile client decision firms up. Not before.

### B.1 — Transport

For mobile specifically:

- **HTTP for unary calls** (account.summary, positions.list, quote.snapshot, chain.fetch, scan.run, history.daily, regime.snapshot, gamma.zero_spx, breadth.spx, status.health) — request/response with no streaming. REST-style works fine.
- **Server-Sent Events (SSE) for streams** (quote.subscribe) — battery-friendlier than WebSocket on mobile, simpler to implement, and the existing "frame" pattern maps cleanly to SSE messages.
- **Avoid WebSocket** — bidirectional is not needed today.
- **Keep Unix socket for local CLI / MCP** — same handlers, two transports. The local consumers do not pay the network cost.

### B.2 — Versioning

URL prefix: `/v1/regime/snapshot`. Standard, browser-friendly, simple. A future breaking change ships behind `/v2/...` while `/v1/...` keeps working for old clients. No envelope-level version field; the path is the version.

### B.3 — Authentication

Read-only daemon, so the scope is read-only.

- **Bearer tokens**, long-lived, generated locally on the daemon, stored in the iOS/Android keychain on the client.
- **Pairing flow**: user runs `ibkr token create` on the desktop, gets a token string (or a QR code). Mobile app scans / pastes, stores in keychain. Daemon maintains a token store at `~/.config/ibkr/tokens.toml` keyed by token-id; revoke via `ibkr token revoke <id>`.
- **TLS required end-to-end** for any non-localhost binding. Self-signed cert for LAN-only access; reverse-proxy via Tailscale / Caddy for internet exposure.

### B.4 — Payload slimming for mobile

Some current payloads are heavy:

- `RegimeSnapshotResult` includes per-scalar `Quality` provenance (~12 fields per indicator) and `StreakInfo` and `FieldsMissing`. Useful for `--explain`; mobile-overhead.
- `GammaZeroComputed` includes the 60-point `Profile` sweep + per-expiry `SkewFitInfo`. Useful for a desktop chart; mobile may want a summary-only variant.

**Do NOT trim v1.** Ship the existing payloads first; a `?view=summary` query param or a sibling `/v1/regime/summary` endpoint can slim things later without breaking the full endpoint.

The mapper layer from Phase A is exactly where view-variants live.

### B.5 — Discoverability / code-gen

If we go with HTTP + JSON, OpenAPI is nearly free and earns its keep:

- Define the schema once. `oapi-codegen` generates Go server-side stubs.
- `openapi-typescript` generates TypeScript types for a SPA.
- `openapi-generator` produces Swift / Kotlin types for native mobile.
- Swagger UI for free, served by the daemon when binding non-localhost.

The OpenAPI schema becomes the **versioned contract**. The mapper layer from Phase A is where Go-side types meet the schema-driven shape.

Alternative — gRPC + grpc-web. Stronger typing, less browser-friendly, more setup, no good story for mobile native. **Reject** for this use case.

### B.6 — Effort estimate

| Step | Effort |
|---|---|
| OpenAPI schema covering v1 surface | ~3 days |
| HTTP transport (handlers + routing) | ~3 days |
| SSE transport for quote.subscribe | ~2 days |
| Auth: token store, pairing CLI, TLS bootstrap | ~3 days |
| Generate TS/Swift/Kotlin types, smoke tests | ~2 days |
| **Total** | **~13 focused days, ~2-3 weeks calendar** |

This is the contract / transport / auth layer **only**. The SPA itself is a separate, much larger investment.

## Mobile-specific things worth surfacing

Not obvious until you think about a phone screen:

- **Long-running computes**: the gamma compute can take 30–60 seconds. The existing `Status: "computing" + EtaSeconds` pattern is well-shaped for this. Mobile UI shows progress estimate + re-polls; never block on a long request.
- **Background updates**: iOS / Android limit background work. Streams are foreground-only. Background changes rely on polling + (optionally) push notifications.
- **Offline state**: cellular drops happen. The existing `Quality` envelope with `freshness` + `at` timestamp is the right shape — the SPA shows "as of 3 minutes ago" and the user knows it's stale.
- **Token rotation**: token store on the daemon must support revoke-by-id. Compromise scenario: user lost phone; user `ibkr token revoke <phone-id>` on the desktop and the lost device is locked out immediately.

## Open questions

These need answers before Phase B starts; they do **not** block Phase A.

1. **Internet exposure or LAN-only?**
   LAN-only is dramatically simpler (mDNS discovery, self-signed cert, no port-forwarding). Internet exposure needs Tailscale / Caddy and a deployment story.
   - *LAN-only* fits a "use my phone at home" model.
   - *Internet exposure* fits a "check positions while travelling" model.
   - Mobile primary use case suggests internet-capable, but LAN-only could be v1 if Tailscale-everywhere is acceptable.

2. **Single user or multi-user?**
   Current daemon is single-account, single-user. If multi-user (e.g. shared family account, or different IBKR accounts), token store needs scope/account binding.

3. **Push notifications?**
   "Regime turned red" or "gamma compute finished" pushed to mobile is high-value but needs a relay (APNs / FCM) and a separate auth story. Defer to v1.1.

4. **Native mobile vs. PWA?**
   PWA is faster to ship, no app-store gating, works cross-platform. Native gives keychain integration, background fetch, push notifications. The contract is the same either way; only the client implementation differs.

5. **Trade execution?**
   Currently the daemon is strictly read-only. Adding write surface (place orders, cancel) is a category change — needs separate auth scope, audit logging, idempotency keys, and probably a confirmation flow. **Out of scope for this doc.**

## Rollout — when Phase A ships

1. Phase A lands as a single PATCH release (e.g. v0.31.0 if you want a MINOR signal for "internal restructure"; v0.30.3 if framed as pure refactor). CHANGELOG entry under Engineering notes; no user-visible change.
2. Old `internal/rpc` import path keeps working — the daemon's mapper layer is the only new addition.
3. CLI and MCP run unchanged. `make smoke` verifies byte-identical output.
4. The CHANGELOG header for subsequent releases can now meaningfully use the line "wire contract unchanged" when only internal types moved.

## Rollout — when Phase B ships

1. Phase B lands as a MINOR release (e.g. v0.31.0 or v0.32.0) because it adds a new transport surface — not a breaking change for existing consumers, but a non-trivial new surface.
2. The HTTP listener is **off by default**. Enable via `[network] enabled = true` in `~/.config/ibkr/config.toml` plus a bound address. Existing Unix-socket consumers see no change.
3. First token is generated via `ibkr token create --label "my phone"`; user copies / scans into the mobile client.
4. OpenAPI schema published in the repo (e.g. `docs/api/openapi.yaml`) and served at `/v1/openapi.yaml` by the daemon for tooling.

## References

- `internal/rpc/rpc.go` — current wire contract.
- `docs/specs/regime-v2-design.md` — regime indicator design.
- `docs/specs/risk-regime-dashboard.md` — methodology spec, referenced by `rpc.Quality` provenance.
- v0.30.0 CHANGELOG — first release to document `rpc.IsOptionRTH(now)` as a deliberate public helper, establishing precedent for the rpc package as a contract surface.
- v0.30.2 CHANGELOG — most recent internal refactor, illustrates the kind of change that Phase A makes invisible to consumers.
