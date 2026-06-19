# Platform Settings

Platform settings are the narrow writeable preference surface for runtime UX
choices. They are not a second config system.

## Ownership

The daemon stores runtime preferences in
`$XDG_STATE_HOME/ibkr/platform-settings.json`, or
`~/.local/state/ibkr/platform-settings.json` when `XDG_STATE_HOME` is unset.
Only user preferences owned by `ibkr` belong in this file:

- `features.purge_restore.enabled`, default `true`
- `features.stock_protection.enabled`, default `true`
- optional experimental trading-limit overrides

TOML/config/build still own gateway endpoint, account, client ID, trading
enablement, trading mode, and whether write-capable trading code exists. MCP
broker writes are not exposed as a local setting.

## Contract

Every returned setting field carries:

- `access`: `read` or `write`
- `source`: `runtime`, `config`, `build`, or `observed`
- `reason` when a read-only value needs operator context

`settings.update` and `PATCH /api/settings` accept only writable fields. Unknown
fields and read-only writes return 400. `null` clears a runtime override and
reveals the underlying default/config value again.

## Policy

Purge/restore is enabled by default. Disabling it blocks purge/restore write
actions across CLI, RPC, API, and SPA, while `purge.status` remains readable.

Stock/ETF protection proposals are enabled by default. Disabling
`features.stock_protection.enabled` blocks stock/ETF protection proposal actions
with a `stock_protection_disabled` blocker, while proposal/status surfaces
remain readable. The setting cannot enable broker writes, option protection, or
policy-disabled buckets.

Trading mode is never writable here. Stable builds expose trading and limits as
read-only. Experimental trading builds may edit safety limits only after
`[trading].mode` is set to `paper` or `live` in TOML.

Market-data settings never store subscription entitlements. The settings surface
shows a compact observed-quality summary from live quote/status data; row-level
truth remains on quote, chain, position, and status responses.

## Surfaces

- Daemon RPC: `settings.get`, `settings.update`; both must work without gateway
  connectivity.
- CLI: `ibkr settings show [--json]`,
  `ibkr settings set features.purge_restore.enabled=true|false|null`, and
  `ibkr settings set features.stock_protection.enabled=true|false|null`.
- HTTP/app: `GET /api/settings`, `PATCH /api/settings`, `/api/bootstrap`, live
  snapshot, and SSE `settings` events.
- MCP: read-only `ibkr_settings`; no write tool in V1.
- SPA: Settings tab renders this contract directly and honors `access` before
  enabling controls.

When changing this surface, update daemon permissions/tests first, then adapters,
then docs. Run focused settings tests before `make check && make smoke`.
