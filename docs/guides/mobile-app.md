# Mobile App

Updated: 2026-06-03 18:19 CEST

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

## Restart

```sh
ibkr restart --app
```

This sends SIGTERM to the existing `ibkr app` process so HyperServe can shut
down gently. It preserves the old app flags such as `--addr`, `--public-url`,
and `--state-dir`. If launchd respawns the app, the command reports the
supervised PID and does not start a duplicate.

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
