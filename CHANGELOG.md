# Changelog

## v1.0.0 — 2026-05-09

Initial release. Standalone, read-only IBKR command-line tool with Claude Code
integration.

### Added

- `pkg/ibkr` shared library extracted from `regime/internal/components/ibkr`.
  Strips regime-specific orchestrator and logging dependencies; exposes a
  clean public API (`Connector`, `Position`, `Order`, `AccountSummary`,
  `MarketSnapshot`, `Subscription`, `RunScannerSubscription`,
  `SubscribeOption`, `SetLogger`, `SetLogLevel`).
- `cmd/ibkrd` long-lived daemon owning the IB Gateway connection. Listens on
  a Unix socket (`$XDG_RUNTIME_DIR/ibkr/ibkrd.sock`), serves newline-delimited
  JSON-RPC, idle-shuts-down after 30 minutes of no client activity.
- `cmd/ibkr` thin CLI client. Auto-spawns the daemon on first invocation.
  Subcommands: `account`, `positions`, `quote` (snapshot + `--watch`),
  `chain`, `history`, `scan` (run + list), `status`, `version`. `--json` on
  every command for parseable output; `--text` (default) prints ANSI tables.
- `quote` snapshots include bid/ask sizes and cumulative day volume; the
  `--watch` stream emits sizes alongside price ticks. JSON consumers see
  `bid_size` / `ask_size` / `volume` fields (omitted when the gateway
  didn't deliver the corresponding tick).
- `positions` renders a conditional `REAL P&L` column when at least one row
  carries a non-zero realized value, and supports `--by underlying` to
  group stock + option legs per underlying with summed P&L. The JSON
  response always includes a `by_underlying` array regardless of the flag.
- `history SYM [--days 90]` returns daily OHLCV bars via IBKR HMDS.
  Calendar-driven lookback; daily granularity only in v1.0 (intraday is
  v1.1).
- `internal/config` TOML config loader with profile selection
  (live / paper / custom) and scanner presets.
- `internal/cache` JSON-file contract cache and inactive-symbol store.
- `internal/dial` Unix-socket client with deadline-driven calls and
  subscription streaming.
- `internal/daemon` server, JSON-RPC dispatch, request handlers, lifecycle
  (signal handling + idle timer).
- `internal/rpc` shared wire types so CLI and daemon agree on the schema.
- Build-tag trading guard (`!trading`) that compiles `order place` /
  `order cancel` to stubs returning `ErrTradingDisabled`.
- Skill bundle (`skill/SKILL.md` + `skill/schemas.md`) describing every
  subcommand, JSON field, and refusal contract.
- Settings snippet (`settings/ibkr.settings.json`) pre-allowing read-only
  `Bash(ibkr …)` patterns and adding a `PreToolUse` hook that hard-blocks
  trading verbs.
- `install.sh` building binaries, copying the skill, and merging settings
  via `jq`.
- `Makefile` with `build`, `install`, `install-skill`, `test`,
  `test-pkg`, `test-daemon`, `clean`.
- Integration tests under `test/integration/` exercising the full
  daemon + CLI stack against the live IB Gateway. Shared-daemon model
  avoids handshake storms; tests skip cleanly when the gateway is offline.
- Live position support via streaming `reqAccountUpdates` so position rows
  carry mark / market-value / unrealized P&L.

### Notes vs. baseline (regime)

This is a fresh project, not an iteration on regime. The relevant baseline
is the `regime/internal/components/ibkr` package, ~19,700 LoC, lifted into
`pkg/ibkr/` and de-coupled from `regime/internal/core` and
`regime/internal/logging`. The orchestrator-specific `ProcessTick` method
and the AI-assisted wire analyzer were removed; the wire interceptor now
records frames without trying to fix them at runtime. The inherited test
suite passes against the live IB Gateway.

### Known limitations

- Per-leg option chain pricing is best-effort: the v1 implementation falls
  back gracefully when IBKR cannot resolve the option contract from
  symbol+expiry+strike+right alone, leaving cells blank rather than
  fabricating a value. v1.1 adds full conID-resolved chain pricing.
- Self-update is deferred to v1.1; `ibkr version` is the only metadata
  command in this release.
- `quote` subscriptions are throttled at the CLI render layer only; the
  gateway-side subscription always runs at full rate.
