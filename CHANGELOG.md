# Changelog

## v0.11.0 — 2026-05-12 05:48 CEST

Two trader-math additions that fit the existing snapshot surface — both pure derivations from data the daemon already pulls. No new RPCs, no new gateway round trips, no journaling. The wire response shapes for `chain.expiries` and `size` carry new optional fields; consumers that ignore them work unchanged. Plugin tag and binary tag both move in lockstep.

### `ibkr size --target` adds R-multiple and breakeven win rate

`ibkr size` already returned the fixed-fractional share count from entry + stop + risk %. Pass an additional `--target` (long: `target > entry`; short: `target < entry`) and the response now carries:

- `r` — reward-to-risk multiple, `|target − entry| / |entry − stop|`. The standard discretionary filter; ≥ 2R is the common "this trade is worth the risk" threshold from Van Tharp / Minervini / O'Neil.
- `reward_quote` / `reward_base` — max gain at target, in trade-quote and account-base currency respectively (same FX treatment as `risk_*`).
- `breakeven_win_rate` — `1 / (1 + R)`, the strategy's break-even hit rate at this R. Reads "at R = 2 you need to be right 33.3% of the time to break even."

The text renderer adds a three-line "reward" block right after "Max loss at stop" when `--target` is supplied, and suppresses it otherwise so the no-target output stays identical to v0.10.x. Long/short asymmetry is enforced in `ComputeSize` (covered in `size_test.go::TestComputeSizeRMultiple`) so a fat-fingered target on the wrong side of entry gets a structured validation error, not a negative R. `ibkr_size` (MCP tool) carries the new optional `target` arg with matching schema.

### `ibkr chain SYM` adds DTE and 1-σ implied move per expiry

The expiry-listing path now decorates each row with two new fields:

- `dte` — calendar days from today (local) to the expiry. Same-day expiries get `dte = 0`.
- `implied_move` / `implied_move_pct` — the canonical 1-σ expected dollar move by expiration, computed `spot × IV × √(DTE/365)`. Same formula CBOE's option calculator uses; the desk-standard "what move is the market pricing in" number for earnings sizing and strike selection. Populated only when both spot and IV are known; nil otherwise — never a substituted proxy.

The result body also carries top-level `spot` (the mid the daemon used to pick the ATM strike — previously implicit). The text table grows two columns (`DTE`, `EXPECTED MOVE`) when IV is requested, or one (`DTE`) when `--no-iv` is passed. No new round trips: spot was already fetched once per call to pick the ATM strike; the math is pure post-processing on the existing IV data.

`schemas.md` and `SKILL.md` updated so Claude knows when to surface the new fields ("what move is priced into Friday?", "is this 2R trade worth taking?"). Tests in `internal/daemon/implied_move_test.go` cover the day-count helper and the formula against hand-computed references including the √(4×DTE) = 2× scaling property.

## v0.10.3 — 2026-05-11 22:17 CEST

Hardening pass after an end-to-end review: panic recovery on the wire reader, a non-atomic close() in the connection pool, a context leak in the rate limiter retry path, MCP subscription contexts now scoped to the server's lifecycle, and a new GitHub Actions CI workflow. Two minor cleanups round it out. No CLI flag changes; safe drop-in upgrade from v0.10.2.

### Panic recovery on the TWS reader goroutine

`Connection.readMessages` is the sole consumer of the gateway socket. Pre-fix, a panic inside any message handler (bad protobuf shape, unexpected wire field, downstream nil deref) silently killed the reader while the connection's status field still read `Connected` — every subsequent write queued forever waiting for a reply that no one was reading. The reader is now wrapped in a `defer recover()` that logs the panic with a full stack trace and converts it into a structured disconnect, so the existing reconnect-with-backoff loop takes over instead of leaving the process wedged.

### `ConnectionPool.Stop()` race fix

The pool's `Stop()` used a `select { case <-stopChan: default: close(stopChan) }` pattern that is not atomic with respect to a concurrent caller. Two goroutines hitting `Stop()` simultaneously could both observe the default branch and race into `close()`, panicking on the double close. Now guarded by `sync.Once` — `Stop()` is idempotent and concurrent-safe.

### Rate-limiter retry no longer leaks on shutdown

`RateLimiter`'s exponential-backoff retry goroutine slept on a bare `time.Sleep(backoff)` with no awareness of the limiter's context. A shutdown during the sleep left the goroutine running out the full delay before noticing — wasting work and delaying clean exit. The sleep now selects on `ctx.Done()` and the retry-enqueue also bails on cancellation. Tracked via the limiter's existing `sync.WaitGroup` so `Stop()` waits for in-flight retries.

### MCP resource subscriptions scoped to server lifecycle

`handleResourcesSubscribe` was creating its streaming context from `context.Background()`, which severed each subscription from the MCP client's lifecycle. If the client crashed without an explicit `resources/unsubscribe`, the subscription persisted until `shutdownSubscriptions()` happened to run — which it did on a clean EOF, but not on the process being SIGKILLed mid-stream. Subscriptions are now children of the `Serve()`-scoped context, so an outer context cancel (or the existing client-EOF path) reaches every active subscription deterministically.

### Tautological assertion removed

`Connection.sendMessage` re-decoded the four-byte big-endian length prefix it had just encoded and panicked if the round-trip disagreed — a check that cannot fire short of `encoding/binary` malfunctioning. Removed; the value was zero and it sat on the hot send path.

### CI: GitHub Actions workflow

Added `.github/workflows/ci.yml`. Three jobs run on every push to `main` and every PR: `check` (gofmt + go vet + staticcheck + govulncheck), `test` (matrix on `ubuntu-latest` and `macos-latest` — `pkg/ibkr` unit tests + `internal/...` and `test/integration/...` under `-race`; live-gateway integration tests skip cleanly with no gateway present), and `cross-compile` (full release matrix on `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`). The README now carries a CI badge.

### Modernized to Go 1.21+ sort idiom

`internal/cli/positions.go` switched from `sort.SliceStable` to `slices.SortStableFunc` with `cmp.Compare`. Same behaviour, type-safe comparator, and lines up with the rest of the codebase's `slices`-based usage.

### Reproducible-builds note in the README

The release pipeline already built with `-trimpath -buildvcs=false` and stamped version/commit/date via `-ldflags`. Surfaced this in the "Other install paths" section so a security-conscious user knows they can rebuild any tag and compare against the published `SHA256SUMS`.

## v0.10.2 — 2026-05-11 21:46 CEST

Pre-publish hygiene + a README that earns its first scroll. No behavioural changes; safe drop-in upgrade from v0.10.1.

### README rewrite

The landing page now leads with what the binary does rather than how it's structured: a faithful sample of the account snapshot (including the v0.10.0 currency-exposure block and v0.10.1 currency-aware money symbols), a `--json | jq` recipe, and a streaming `--watch` example. The features list surfaces the v0.9.x and v0.10.x additions that were buried before — option Greeks per leg, portfolio aggregates with FX sensitivity, daemon-side chain ATM-IV cache with phase-aware TTL, MCP resource subscriptions for streaming, sign-coloring with `NO_COLOR` / `IBKR_COLOR` opt-out. All flag names, JSON field paths, and rendered widths in the examples are checked against the actual code; no fabricated output.

### Disclaimer & trademarks block

Added an explicit "not affiliated with Interactive Brokers" notice naming the trademarks used nominatively, calling out that `pkg/ibkr` is a clean-room re-implementation that bundles no IBKR-distributed code, and reaffirming the no-market-data-redistribution / IBKR-Pro-required / AS-IS posture. Mitigates the standard third-party-client legal exposure before the project gets broader attention.

### Personal-path hygiene

Removed hard-coded `/Users/osauer/…` paths that had crept into three places: the `pkg/ibkr/connection.go` package doc (now points at IBKR's public GitHub mirror), the two `pkg/ibkr/testdata/generate_*.py` fixture generators (now read `IBPY_ROOT` from the environment with an actionable error when unset), and the macOS-shaped TWS process line in `internal/discover/process_test.go` (anonymised to `/Users/local/...`). No behavioural impact — the tests still verify the same substring match — just clean for outside contributors reading the repo.

## v0.10.1 — 2026-05-11 21:16 CEST

Bug-fix pass after user testing v0.10.0 against a live multi-currency account. Five issues — one critical (the `--watch` regression), three rendering, one Greeks pipeline gap.

### `ibkr quote SYM --watch` no longer exits after ~1s (CRITICAL)

The pre-flight version-skew check (v0.9.1+) sets a 1-second context deadline on the shared socket connection via `Conn.applyDeadline` and never resets it. When the next operation on the same `*Conn` was `Stream` (the watch path), the stale deadline fired ~1 second into reading frames; `net.ReadBytes` returned a timeout, the loop interpreted `ctx.Err()` (the watch's own ctx, still valid) as nil, and the CLI exited cleanly with no error message. Users saw a few rows then a silent shell prompt.

Fix: `Conn.Call` now clears the socket deadline (`SetDeadline(time.Time{})`) on return, success or failure, so the next operation starts with no inherited deadline. Regression test in `internal/dial/dial_test.go::TestCallDeadlineDoesNotLeakIntoStream` reproduces the original failure mode against a stub daemon and proves the fix.

### Maintenance margin restored

v0.10.0 added `$LEDGER:ALL` to the account-summary request, which shifted IBKR's `MaintenanceMarginReq` field from the bare base-currency form to the per-currency form. The parser was still asking for the long-form tag name; the result was a silent em-dash where the maintenance margin should have been. The canonical IBKR tag is `MaintMarginReq` (shorter); the parser now accepts both forms so neither the old nor the new wire shape loses the value.

### Currency exposure no longer duplicates the base currency

For a EUR base account the `currency_exposure` block also included an "EUR" row with FX=1.0 — duplicating the top-level totals and confusing the renderer. The base-currency row is now dropped explicitly when `base_currency` is known, with an `ExchangeRate==1.0` fallback for the pre-handshake state.

### Currency-aware money rendering

Account and currency-exposure rendering used a hardcoded `$` prefix everywhere, which read wrong for a EUR account. Money values now use `€`/`$`/`£`/`¥` symbols for the common cases and fall back to the ISO code prefix (e.g. `CHF 825.06`) otherwise. The portfolio `Dollar delta` and `FX sensitivity` lines drop the symbol entirely — the currency code is named on the same line, so prefix and suffix would be redundant.

### Option Greeks pipeline now actually subscribes

The v0.10.0 Greeks pipeline silently skipped every option position because `optionGreeksKey` rejected `SecType="OPTION"` — the long-form enum value that `convertIBKRPositions` stamps onto position views — accepting only the wire-level `SecType="OPT"`. Result: every account reported `greeks_coverage 0/N` for option-bearing books regardless of what IBKR delivered. The key builder now accepts both forms.

Positions request deadline bumped from 10 s to 30 s so cold-cache option contract resolution (4-way parallel × up to 5 s per leg) fits within budget. After the option contract cache is warm, subsequent positions calls return under 5 s.

### Known follow-up: Greeks pipeline serialization (v0.10.2)

Even with the fixes above, only the first option in a multi-leg book currently completes its model-computation tick capture within the call — the other legs appear to serialize behind an internal rate limiter or contention point in `pkg/ibkr`. Aggregate effective delta and dollar delta still benefit from the cached values that DO land between invocations, but `greeks_coverage` lags reality. Tracked for v0.10.2; needs deeper instrumentation of the option-subscribe path.

## v0.10.0 — 2026-05-11 19:58 CEST

Two feedback items from a user audit of the MCP surface landed: option Greeks per leg + portfolio aggregates, and FX exposure attribution for non-base currency holdings. A third item (per-position margin contribution) was deferred to v0.11.0 — it needs wire-protocol additions (a `WhatIf` slot in the placeOrder encoder plus an openOrder response parser for margin-delta fields) and paper-account verification that's outside the scope of this release. None of these v0.10.0 additions place orders, charge anything, or require any new IBKR entitlement.

### Option Greeks per leg + portfolio aggregates

`ibkr positions` now returns delta, gamma, theta, and vega per option leg, plus a `portfolio` block summed across legs:

- `effective_delta` (share-equivalents — long calls contribute +qty×delta×100, etc.)
- `dollar_delta` (effective delta × underlying spot, in `dollar_delta_currency`)
- `daily_theta` (IBKR reports theta as daily decay, so the sum is the daily P&L bleed assuming everything else holds)
- `gamma`, `vega`
- `greeks_coverage` / `greeks_total` so partial-coverage state is explicit when the gateway didn't model some legs (typical for far-OTM / illiquid OOH)

The Greeks come from IBKR's model-computation tick (msg 21, tickType 13) that already arrives on every option subscription — pre-fix the daemon parsed the row and discarded everything except IV (`_ = vega` was literally in the source). A new daemon-side cache (60 s TTL) means back-to-back `ibkr positions` calls within a decision pause pay zero extra gateway round trips; the cold first call adds ~3-5 s for a typical option book at 4-way parallelism. Same bounded fan-out pattern as the chain ATM-IV fetch.

`ibkr chain SYM --expiry YYYY-MM-DD --json` now also populates `call_delta` / `put_delta` per strike — the wire fields existed but were unused.

Nil discipline preserved: a leg whose model tick didn't arrive within budget shows `null` for each Greek, never a zero substitute. The renderer flags partial coverage on the summary line.

### Currency exposure for multi-currency accounts

`ibkr account` now returns `currency_exposure`: one row per non-base currency holding, with `net_liquidation_ccy`, `cash_ccy`, `stock_market_value_ccy`, `option_market_value_ccy`, `unrealized_pnl_ccy`, `realized_pnl_ccy`, `exchange_rate` (base-per-CCY), and `net_liquidation_base` (reconciled within 0.5%). For a EUR account holding $90k of USD positions, the row makes clear how much of NLV is FX-exposed.

`ibkr positions` decorates each non-base position with `fx_rate` and `market_value_ccy`. The `portfolio` block adds `fx_sensitivity_per_pct` — the base-currency P&L change per 1% FX move (Σ non-base NetLiq × FX × 0.01). That answers "how much of my book is currency-exposed" in the actionable form; it is **not** historical FX-vs-underlying P&L attribution (which would require per-lot execution-time FX tracking — a v2 feature).

Implementation: the daemon now asks for `$LEDGER:ALL` alongside the existing tags, which makes IBKR emit one block per currency. The data was always available — we just weren't requesting it. No extra round trip on `ibkr positions`: the per-currency snapshot is read from the connector's continuously-fresh `reqAccountUpdates` state.

### Discovery now fails over to alternate ports

When both IB Gateway and TWS are running on localhost, the daemon used to pick whichever responded to the TCP probe first (4001 → 4002 → 7496 → 7497) and stop there. If that app's API socket accepted TCP but never completed the IBKR handshake — the textbook signature of "Gateway is up but not logged in" or "checkbox unchecked" — the daemon stayed degraded indefinitely, even though the other app was sitting in `Endpoint.Alternates` ready to talk.

The connect path now walks the primary endpoint *then each alternate* in preference order. Each candidate gets a hard 25s budget — long enough for a healthy slow handshake (~sub-second to ~20s) but short enough that the loop reliably advances even when the SDK's TLS retry would otherwise hang against a black-hole peer. The first candidate to complete the handshake wins; the alternates that responded to TCP but never handshook are torn down cleanly between attempts. On exhaustion, `ibkr status` shows a verdict that names every endpoint that was tried — not just the original probe winner — so the user knows where the real problem is.

User-visible effect: a stale Gateway window left over from earlier in the day no longer blocks `ibkr` from picking up the active TWS session.

## v0.9.3 — 2026-05-11 16:14 CEST

### Discovery now fails over to alternate ports

When both IB Gateway and TWS are running on localhost, the daemon used to pick whichever responded to the TCP probe first (4001 → 4002 → 7496 → 7497) and stop there. If that app's API socket accepted TCP but never completed the IBKR handshake — the textbook signature of "Gateway is up but not logged in" or "checkbox unchecked" — the daemon stayed degraded indefinitely, even though the other app was sitting in `Endpoint.Alternates` ready to talk.

The connect path now walks the primary endpoint *then each alternate* in preference order. Each candidate gets a hard 25s budget — long enough for a healthy slow handshake (~sub-second to ~20s) but short enough that the loop reliably advances even when the SDK's TLS retry would otherwise hang against a black-hole peer. The first candidate to complete the handshake wins; the alternates that responded to TCP but never handshook are torn down cleanly between attempts. On exhaustion, `ibkr status` shows a verdict that names every endpoint that was tried — not just the original probe winner — so the user knows where the real problem is.

```
ibkr daemon v0.9.3  ·  uptime 24s  ·  degraded — gateway not connected

  Reason: none of 2 discovered endpoint(s) completed TWS handshake
          (tried 127.0.0.1:4001, 127.0.0.1:7496); confirm the IBKR app
          you intend to use has 'Enable ActiveX and Socket Clients' on
          and is logged in
```

User-visible effect: a stale Gateway window left over from earlier in the day no longer blocks `ibkr` from picking up the active TWS session.

## v0.9.2 — 2026-05-11 15:39 CEST

### Daemon no longer deadlocks on shutdown

`Connector.Stop` was holding the connector's mutex across `pool.ReleaseLease()`, which fires the registered `onDisconnect` callback synchronously. That callback calls back into `onConnectionLost`, which tries to lock the same mutex — deadlock. Effect: every daemon idle-shutdown (and every SIGTERM) hung the daemon process indefinitely; you had to `kill -9` it. Now `c.running` is flipped to false and the lock is released *before* the lease releases, so the disconnect callback can acquire the mutex cleanly. The user-visible win is that idle daemons actually exit when they say they do.

### Autospawn handles shutdown-race gracefully

The v0.9.1 pre-check refused to spawn a duplicate daemon when the lock file pointed at a live PID. That was correct for the genuinely-stuck case but misfired on the legitimate shutdown window: the daemon's `Stop` sequence removes the socket *before* releasing the lock, so a CLI invocation arriving mid-shutdown saw "PID alive + lock present + socket gone" and emitted a misleading "PID is stuck, run `kill X`" error. Fixed: the pre-check now polls PID liveness alongside socket appearance. When the daemon finishes exiting during the wait, the CLI falls through to spawn a fresh daemon automatically. Only a daemon whose PID stays alive through the full budget surfaces the stuck-daemon error.

Combined with the deadlock fix above, the user experience is: after idle-shutdown (or any SIGTERM), the next CLI invocation transparently spawns a fresh daemon — no manual intervention, no confusing errors. The whole round trip is under 5 seconds.

## v0.9.1 — 2026-05-11 14:03 CEST

### Better chain timeout error

`ibkr chain SYM` now surfaces a useful, action-oriented message when the IBKR security-definition data farm is degraded (typical pre-market / post-maintenance state):

```
ibkr: chain: timeout: option chain unavailable for AMD: gateway did not deliver
  security definitions in time. This is usually transient — try again in a
  moment, or run `ibkr status` to verify the gateway connection.
```

Replaces the previous generic `ibkr: chain: internal: timeout waiting for contract details`, which left users guessing whether it was a daemon bug, an invalid symbol, or something they could retry. Other surfaces classify the underlying timeout (`ibkrlib.ErrContractDetailsTimeout`) as `rpc.CodeTimeout` instead of falling through to `CodeInternal`, so JSON consumers get a meaningful error code too.

### Single-daemon enforcement, harder

Autospawn now refuses to spawn a duplicate daemon when the lock file points at a live PID — it either connects to the existing daemon's socket when it appears, or surfaces a clear "daemon PID X is running but never opened the socket" error with a `kill X` hint. Pre-fix, racing CLI invocations would each spawn a daemon process; most exited cleanly via the existing flock contention, but a deleted lock file (manual `rm`, aggressive cleanup script) could let two daemons co-exist with two gateway connections fighting over the same client ID. The flock layer remains the final defense — this just stops us from making the race in the first place.

### CLI ↔ daemon version drift warning

Every CLI invocation (other than `status`) now runs a fast `status.health` round-trip after connect and prints a stderr warning when the daemon was built from a different revision than the CLI binary:

```
ibkr: warning: CLI version v0.9.1 does not match daemon version v0.9.0 —
  restart the daemon to pick up the new binary (kill the running ibkr daemon;
  the next CLI call will respawn it).
```

The warning is silenced when either side stamps the literal `dev` placeholder so working-tree builds don't nag against themselves. The check uses a 1-second timeout and silently skips on any RPC failure — it must never interfere with the user's actual command.

## v0.9.0 — 2026-05-11 12:58 CEST

### Quote & positions show "change vs prev close"

`ibkr quote` now renders three new columns: **PREV CLOSE**, **CHG**, **CHG%** — the daily anchor every retail platform shows by default. The fields land in JSON as `prev_close`, `change`, `change_pct`. Pre-market, where regular-session ticks may not be flowing yet, prev-close arrives reliably so the user sees "yesterday closed at 455.19, no live print yet" instead of a row of em-dashes.

`ibkr positions` gains a **DAY CHG** column showing `±$X.XX (±Y.YY%)` between the position's mark and the underlying's prev close — separates today's move from cumulative P&L. The daemon pre-warms a per-symbol prev-close cache (12 h TTL) on the first call, so subsequent invocations are instant. JSON gets `prev_close`, `day_change`, `day_change_pct` on each `PositionView`. Options' `PrevClose` reflects the underlying's prev close (anchor only — contract-level option prev close is not tracked).

All new columns paint green/red by sign with em-dash placeholders when source data is missing — no fabrication, never substitute a proxy.

### Chain expiry list now shows ATM IV by default

`ibkr chain SYM` (no `--expiry`) now fetches and renders ATM implied volatility per expiry **by default**, so the answer to "which expiry has the richest premium?" appears without an extra flag. Three behaviours are new:

- **Default cap of 12 nearest expiries.** A typical equity lists 25–40 expiries; the back half (LEAPS) is rarely on the decision path. The renderer's footer flags when the cap was applied and points at `--all-expiries` to expand.
- **Daemon-side cache.** Per-(symbol, expiry) IV memoized with phase-aware TTL: 60 s during RTH (9:30–16:00 ET, weekdays), 4 h otherwise. First call pays the round trip; subsequent ones within the TTL are instant — and survive across CLI invocations because the daemon is persistent.
- **Parallel ATM IV fetch.** 4 concurrent workers (matches the chain strikes loop) reduce the typical fan-out from ~30 s sequential to ~5 s parallel.

Flag changes:
- `--with-iv` is gone — IV is now the default.
- `--no-iv` added for the fast skeleton (date list only).
- `--all-expiries` added to lift the default cap.

MCP `ibkr_chain` tool: `with_iv` is gone, replaced by `no_iv` + `all_expiries` JSON args (both default false, both opt-in).

### Chain strikes table now shows IV pre-market and after hours

The strikes-table view (`ibkr chain SYM --expiry YYYY-MM-DD`) used to leave the **IV** column blank for most legs when bid/ask/last weren't flowing — typical pre-market and after-hours. Two fixes:

- **`SubscribeOption` now explicitly requests generic tick 106** (Option Implied Volatility), mirroring what `SubscribeOptionIV` already does. Without 106 the strikes table relied on opportunistic model-computation ticks, which only arrive when the book is recomputing.
- **The IV poll runs regardless of whether prices arrived.** Pre-market, a dead option book has no quotes but IBKR can still deliver IV via the model-computation path — the previous code's "only poll IV if pricesArrived" guard threw those away.

## v0.8.2 — 2026-05-11 09:17 CEST

### Color-coded output

Tables now paint sign-meaningful values when stdout is a terminal: P&L green for gains, red for losses, dim for zero. `ibkr quote --watch` colors each Last tick green/red/dim by direction vs. the previous tick. `ibkr account` paints negative balances (cash debit) red and zero placeholders dim — positive balances stay uncolored to keep balance views from looking celebratory. Non-live data badges (`data=delayed ⚠`, `data=frozen ⚠`) and the `ibkr size` `⚠ status:` warning render in yellow.

Color is opt-out: pipes, file redirects, and `--json` are always plain. `NO_COLOR=1` disables; `IBKR_COLOR=always|never` overrides. Top-level help advertises both env vars.

### Column alignment fixes

Quote, positions, options, history, and chain tables now line up labels precisely over their data — across both populated and empty cells. The em-dash placeholder used for missing values now matches the configured column width visually (the bug: `—` is one terminal column but three UTF-8 bytes, so `%Ns` byte-count padding shifted downstream columns left whenever a value was nil). Table headers are now generated from the same field widths as the data row, so any future width tweak only edits one verb instead of a hand-spaced label string.

### Better help on a typo

A mistyped subcommand (`ibkr quotee`) now prints the full top-level usage to stderr instead of just the bare error line — matches the git/kubectl/gh pattern. The top-level help itself has a new footer pointing at `ibkr <subcommand> --help` for per-command flags.

## v0.8.1 — 2026-05-11 08:07 CEST

### Faster, friendlier "where's my gateway?" failure

When the daemon can't reach an IBKR endpoint, the error now names the real cause instead of timing out generically. Two cases, two hints:

- **TWS / IB Gateway / IBKR Desktop is running but the API socket isn't open** — the daemon detects the process and tells you so, with the PID. Most likely 'Enable ActiveX and Socket Clients' is unchecked under Global Configuration → API → Settings, login hasn't completed (2FA / day-end dialog), or you set a non-default Socket port and need to pin it in `~/.config/ibkr/config.toml` under `[gateway]`. The API checkbox is known to silently un-tick itself when more than one of TWS / Gateway / Desktop is launched against the same login.
- **No IBKR app is running at all** — the daemon says so directly. Start one and the daemon reconnects automatically; no daemon restart needed.

`ibkr status`'s degraded-state block is now a single line pointing at the daemon log; the verdict itself goes in `Reason:`. The reconnect loop's `WARN` line is now emitted once per distinct verdict instead of every ~500 ms while `ibkr status` polls.

### Strict TOML config

`~/.config/ibkr/config.toml` is now parsed strictly: unknown top-level keys or section names cause the daemon to fail at startup with a message that names the offending keys. Previously the TOML library silently dropped unknown sections, so a stale-schema config (e.g. one using `[profiles.live]` from an older proposal) parsed cleanly but every `[gateway]` field stayed `nil` — the daemon then fell back to AUTO discovery with `client_id = 15`, masking the misconfiguration. Supported schema is unchanged: `[gateway]`, `[daemon]`, `[scans.<name>]`.

## v0.8.0 — 2026-05-11 07:56 CEST

### MCP streaming subscriptions

Live streaming quotes are now an MCP resource. Two URI templates:

- `ibkr://quote/{symbol}` — stocks / ETFs
- `ibkr://option/{symbol}/{expiry}/{right}/{strike}` — option contracts (`expiry` is `YYMMDD`, `right` is `C` or `P`)

`resources/templates/list` advertises both. `resources/read` returns the current snapshot in a single text content block; `resources/subscribe` delivers coalesced ticks via `notifications/resources/updated`, with the JSON frame embedded in `params.contents`. Unsubscribe explicitly via `resources/unsubscribe`, or close the MCP server's stdio — the subscription drops either way.

No transparent reconnect: a gateway disconnect, daemon shutdown, or IBKR rejection emits a structured terminal frame (one of `gateway_lost`, `entitlement_lost`, `subscription_rejected`, `daemon_shutdown`) and closes the subscription. The MCP client decides whether to re-subscribe.

### Daemon-internal subscription fan-out

The daemon now refcounts market-data subscriptions above the `pkg/ibkr` layer. Two `quote --watch AAPL` watchers, an MCP subscriber, and a snapshot poll on the same symbol now share **one** IBKR market-data line — pre-`v0.8.0`, the second concurrent subscriber would error with "already subscribed" or silently truncate the first. The line is released the moment the last subscriber goes away.

Wire-protocol-additive: `rpc.Frame` gains an optional `error` field (`omitempty`). Tick frames look the same as before; a frame with `error` populated is the last frame on the subscription. Older parsers that ignore unknown fields keep working.

### Other

- After upgrading the binary, restart any long-running daemon (`pkill -x ibkr`, then re-invoke any subcommand) — the daemon's subscription-state shape changed and the daemon-restart-on-upgrade rule from the README applies.
- `internal/mcp.ExcludedCLI` no longer carries a `quote` entry: streaming `quote --watch` is now a real MCP surface gated by `TestStreamingParity`.

## v0.7.0 — 2026-05-10 22:21 CEST

### Surface

- CLI subcommands: `account`, `positions`, `quote`, `chain`, `history`, `scan`, `size`, `status`, `setup`, `version`, plus the system subcommands `mcp` (stdio MCP server) and `daemon` (long-running gateway connection). Every user-facing command supports `--json`.
- Stateful daemon (same binary, `ibkr daemon`) auto-spawned on first call, idle-shuts after 5 minutes.
- Auto-discovery across the four standard IB Gateway / TWS ports (4001/4002/7496/7497), with strict pinning when configured.
- Two-command install (`install.sh` + `ibkr setup claude-desktop`) for the common case. `go install`, manual tarball, or local build for everything else.

### Safety

Read-only by design. Four independent layers refuse `order`, `trade`, `cancel`:

1. The daemon's order-handler dispatch is stubbed via `//go:build !trading`. `MethodOrderPlace` and `MethodOrderCancel` always return `ErrTradingDisabled`.
2. The bundled `settings/ibkr.settings.json` denies the verbs in `permissions.deny`.
3. The plugin's `PreToolUse` hook hard-blocks the verb patterns and fails closed if `jq` is missing from PATH.
4. A unit test in `internal/mcp` refuses to ship the MCP server with any tool whose name contains `order`, `trade`, `cancel`, `submit`, or `place`.

`pkg/ibkr` exposes order types for forward compatibility, but no CLI subcommand reaches them. A future major release may add trading behind an explicit build tag.

Per [semver](https://semver.org/#spec-item-4), 0.x releases may break compatibility between minor versions. 1.0 is reserved for the first stable read-only line.
