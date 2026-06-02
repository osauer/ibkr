# Gamma zero-cache persistence

**Status:** Implemented and live since v1.0.0. Scope-keyed cache files are the current daemon architecture.
**Created:** 2026-05-22 07:30 CEST
**Last update:** 2026-05-25 07:43 CEST
**Owner:** osauer
**Related:** [internal/daemon/gamma_zero_cache.go](../../internal/daemon/gamma_zero_cache.go), [internal/daemon/gamma_zero_store.go](../../internal/daemon/gamma_zero_store.go), [docs/concepts.md](../concepts.md#gamma)

## Why this exists

The gamma-zero compute is intentionally heavy: it fans out across SPY and SPX option chains, filters out post-settlement 0DTE expiries, fits sticky-moneyness skew curves, and produces per-index plus combined regime diagnostics. After the slot-anchored expiry picker landed, a cold combined run could cross the five-minute mark. Restarting the daemon during a trading day should not force every dashboard or CLI caller through that full compute again.

The result's natural TTL is one New York trading session, so the cache can persist safely as long as stale sessions, stale methodology, and wrong-scope data are rejected on load.

## Design

Gamma cache persistence is deliberately per-domain, not a shared generic cache layer. It mirrors the breadth store's conventions where they are useful, but keeps gamma's lifecycle and validation local to the gamma compute:

- Atomic temp-file plus rename writes.
- Pretty-printed JSON for local debugging.
- A schema `version` field.
- A `method` token gate tied to the current gamma methodology.
- Missing, stale, or incompatible cache files collapse to a cold cache, not a daemon startup error.
- Persistence write failures are warnings; a successful in-memory compute still serves callers.
- Snapshot-time `quality.rankability` separates "served for context" from
  "rankable market evidence" so stale/degraded cache cannot confirm regime or
  canary state.

The default directory is `$XDG_CACHE_HOME/ibkr/gamma-zero`, falling back to `$HOME/.cache/ibkr/gamma-zero`.

## Scope Isolation

The daemon supports three cache scopes:

- `spy+spx` for dashboard, regime, and default `ibkr gamma` calls.
- `spy` for `ibkr gamma --only=spy`.
- `spx` for `ibkr gamma --only=spx`.

Each scope has its own file:

```text
gamma-zero-spy+spx.json
gamma-zero-spy.json
gamma-zero-spx.json
```

Scope-keying is load-bearing. Without it, an SPY-only diagnostic call could poison the combined dashboard cache, and persistence would make that wrong-scope value survive a daemon restart.

## Persisted Shape

```json
{
  "version": 1,
  "session_key": "2026-05-22",
  "scope": "spy+spx",
  "method": "bs-gamma-profile-v3-stickymoneyness-0dte-split",
  "result": { "...": "rpc.GammaZeroComputed" }
}
```

Four independent gates turn a persisted file into a cold cache:

- `version != 1`
- `session_key` does not match today's New York session date
- envelope `scope` does not match the requested scope
- `method` does not match the current gamma method token

The store never migrates or coerces old results. A methodology bump should recompute from IBKR data, not reinterpret prior cache contents.

Closed-session stale fallback and failed-refresh fallback are serve paths, not
rankability paths. The payload remains inspectable, but `quality.rankability`
must be `context_only`, `blocked`, or `unavailable` until freshness and coverage
gates recover.

## Non-goals

- No append-only historical snapshot log. That belongs in a separate store if the project needs gamma history for backtesting.
- No background eviction. Stale session files are tiny and are replaced on the next successful write.
- No cross-domain cache abstraction. The shared logic is small, and gamma's invalidation semantics are easier to audit when they stay local.

## Test Coverage

The store tests cover round-trip persistence, scope isolation, missing files, version mismatch, session mismatch, scope mismatch, method mismatch, atomic replacement, XDG directory resolution, and constructor seeding from valid persisted scopes.
