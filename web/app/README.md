# ibkr Mobile App

This is the small PWA served by `ibkr app`. It is meant for the thing you want
on a phone: is the local IBKR setup alive, what does the account look like, and
is the portfolio canary asking for attention?

Start the app host on the Mac that runs TWS or IB Gateway:

```sh
ibkr app
```

Pair a phone from another terminal:

```sh
ibkr app pair
```

Scan the QR code. The QR opens a short-lived pairing URL; it is not a permanent
secret. After pairing, the browser keeps its own device key and connects back to
the app for bootstrap data and live SSE updates.

Useful while developing or testing:

```sh
ibkr restart --app --timeout 15s
make app-smoke APP_SMOKE_BROWSER=webkit
make app-lifecycle-smoke APP_SMOKE_BROWSER=webkit
```

The `/tools` surface is deliberately boring: status, snapshot, events, auth,
push, and relay diagnostics only. Trading workflows, HTTP MCP, and production
relay hosting are future work.

Design and architecture notes live in
[`docs/design/mobile-app-mvp.md`](../../docs/design/mobile-app-mvp.md).
