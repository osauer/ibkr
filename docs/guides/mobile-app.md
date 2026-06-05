# Mobile App

Updated: 2026-06-05 17:42 CEST

The mobile app layer is served by `ibkr app`. It is a HyperServe process that
serves the PWA, owns pairing, streams `/api/events`, and sends opt-in canary
push notifications.

## Local Run

```sh
ibkr app
```

In another shell:

```sh
ibkr app pair
```

Scan the QR code or open the printed pairing URL. The URL contains a short-lived
pairing id plus nonce, not a durable secret.

The default app bind is LAN-capable (`0.0.0.0:8765`), so the same app process
can serve a local browser preview at `http://127.0.0.1:8765` and a phone at the
Mac's LAN URL. For a local preview, override only the pairing URL:

```sh
ibkr app pair --public-url http://127.0.0.1:8765 --json
```

Use plain `ibkr app pair` for the phone so the running app host's configured
public URL is used. Do not run the shared app host with `--addr 127.0.0.1:8765`
when a phone needs to pair too.

## Restart

```sh
ibkr restart --app
```

This sends SIGTERM to the existing `ibkr app` process so HyperServe can shut
down gently. It preserves the old app flags such as `--addr`, `--public-url`,
and `--state-dir`. If launchd respawns the app, the command reports the
supervised PID and does not start a duplicate.

To switch an old local-only app host back to the shared local-preview plus
phone mode:

```sh
ibkr restart --app --addr 0.0.0.0:8765
```

When `--addr` is overridden without `--public-url`, restart clears any preserved
app `--public-url` flag so the app can derive the Mac's LAN URL again.

For SPA development, use the refresh target so embedded assets and the running
app host stay in sync:

```sh
make app-refresh
```

## Browser Smoke

To avoid QR-driven manual testing for every frontend issue, run a browser smoke
against an already-running app:

```sh
make app-smoke
```

Override the target when the app is on a different address:

```sh
make app-smoke APP_SMOKE_URL=http://127.0.0.1:18765
```

To test app restart recovery without touching the default app state:

```sh
make app-lifecycle-smoke
```

The smoke script:

- creates a pairing session through `/api/pairing/sessions`;
- opens the pairing URL in Playwright;
- optionally removes `globalThis.Notification` before page load;
- requires a real `/api/events` SSE snapshot;
- waits for the dashboard;
- clicks the `Snapshot` debug tool;
- fails on browser page errors or console errors.

The lifecycle smoke starts an isolated app on `127.0.0.1:18765`, pairs without
QR, disables WebCrypto to exercise the local HTTP fallback credential, runs
`ibkr restart --app`, and verifies that the same browser context reauthenticates
and returns to `Live` with an SSE subscriber.

The script first tries the local Node `playwright` package, then the Codex
bundled runtime when available. If Playwright's managed Chromium browser is not
installed, it falls back to installed Chrome. Outside Codex, install Playwright
for the local Node environment before running the smoke.

The Go test gate also includes static compatibility tests for browser globals in
`web/app/app_compat_test.go`, so direct unguarded `Notification` references fail
under `go test ./...`.

For the full SPA development and debugging playbook, including P/L semantics and
Browser plugin caveats, see [Canary SPA Development](canary-spa-dev.md).
