# Experimental trading config

Updated: 2026-06-05 08:18 CEST

Stable `ibkr` is read-only. It can read account and market data, compute risk context, size positions, and preview stock/ETF LMT order drafts without broker submission. It does not place, modify, cancel, or transmit broker orders.

Trading builds are a separate experimental path. They are provided as-is for explicit operator testing, not as the default product and not as an unattended automation surface.

## Read-Only Default

For read-only use, use no config file. With no `config.toml`, the daemon probes the standard local TWS and IB Gateway ports, auto-detects the account from `managedAccounts`, and uses client ID `15`.

Only create `~/.config/ibkr/config.toml` when you want active local overrides. Any value in that file is binding. Anything omitted stays auto-detected.

## Inactive Trading Config

Keep trading configuration inactive as:

```text
~/.config/ibkr/config.toml.trading
```

That file is documentation and staging only. The daemon does not load it.

To activate it, remove the `.trading` suffix:

```sh
mv ~/.config/ibkr/config.toml.trading ~/.config/ibkr/config.toml
ibkr restart
ibkr trading status
```

Before doing that, verify the pinned account, endpoint, and client ID. Trading config must not rely on account auto-detection.

## Required Pins

When trading is enabled, the daemon expects these values to be pinned:

- `[gateway].port`
- `[gateway].account`
- `[gateway].client_id`
- `[trading].enabled = true`
- `[trading].mode = "paper"` or `"live"`
- `[trading].require_preview = true`

Paper mode should use a paper endpoint or account, such as TWS paper on `7497` or a `DU...` account.

Live mode is intentionally heavier. It requires a live-looking endpoint and account, a live override, matching live acknowledgement fields, and fresh paper-smoke evidence from the paired paper setup. Do not enable live trading from a copied template; fill the fields deliberately for the account and endpoint in front of you.

## Release Channel

Stable release artifacts remain read-only. A professional trading preview should be published as a separate experimental channel with distinct asset names, clear as-is language, its own smoke tests, and no automatic path through `ibkr update`.

Do not attach experimental trading tarballs to the stable release namespace until the updater matches stable assets exactly. The stable updater currently expects the normal `ibkr-vX.Y.Z-<os>-<arch>.tar.gz` shape.

MCP order writes should stay out of the preview channel unless they have their own explicit review, nonce, audit, and human-confirmation model. CLI/operator testing comes first.
