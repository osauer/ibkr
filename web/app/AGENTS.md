# Canary SPA Agent Rules

These rules apply when editing files under `web/app`.

## Serving And Refreshing

The SPA is embedded in the `ibkr` binary and served by the long-running
`ibkr app` process. Source edits are not visible in the in-app Browser until a
new binary is installed and the app host is restarted.

For the shared local/phone host, prefer:

```sh
make app-refresh
```

This installs the binary, restarts `ibkr app`, and prints a local pairing URL
for `http://127.0.0.1:8765`. Keep the shared app host LAN-capable; do not start
it loopback-only unless deliberately testing an isolated local preview.

## Pairing And Browser Preview

The root URL is authenticated. In a fresh browser, create a local pairing URL:

```sh
ibkr app pair --public-url http://127.0.0.1:8765 --json
```

Open the returned `.url` in the in-app Browser. Successful pairing redirects to
`http://127.0.0.1:8765/` with title `Canary · IBKR`.

Use the in-app Browser for visible local app QA. Do not use macOS `open`.

## Browser Debugging

The in-app Browser can read rendered DOM state, click, and inspect console logs,
but its page-evaluate sandbox may not expose every browser global such as
`fetch`. When reconciling rendered values to live data:

- read visible UI state from the Browser DOM;
- read raw live data with CLI/API commands such as `ibkr account --json` and
  `ibkr positions --json`;
- compare concepts, not only text formatting.

If in-app Browser screenshots fail, a Playwright/WebKit screenshot is an
acceptable fallback for visual QA. State that fallback in the completion note.

## P/L Semantics

Be precise with financial labels:

- `Daily P/L` means start-of-trading-day account or position P/L.
- `Open P/L` and `Unrealized P/L` mean current open-position cost-basis P/L.
- `Realized P/L` is a separate concept and can make unrealized-only totals
  meaningless for daily attribution.
- Quote price move and quote percent move describe the underlying's market move;
  they are not position P/L.
- Do not use `total P/L` in UI copy unless the total is explicitly defined and
  reconciles to that definition.

For the Underlyings hero, daily winner/loser buckets should be daily P/L
attribution by underlying, not open/unrealized P/L and not a client-estimated
quote-marked value.

## Gates

Use the narrow loop while iterating:

```sh
make app-check
```

When rendered behavior matters, refresh the embedded app assets and smoke the
browser:

```sh
make app-refresh-smoke APP_SMOKE_BROWSER=webkit
```

Before finishing, run the repo gate required by the root `AGENTS.md`.

If live `make smoke` fails, report the exact assertion and artifact. Do not
hide the failure by retrying silently. A rerun is only useful when explicitly
diagnosing external gateway nondeterminism, and the first failure still belongs
in the completion note.

## Regime And Risk Posture

When regime label, tone, readiness, or indicator state conflict, ask a
trading/risk agent to judge the UI posture and diagnose missing data. Prefer
fixing the canonical backend posture or data-quality surface over CSS-only UI
overrides.
