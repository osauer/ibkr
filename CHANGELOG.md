# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Entries before v0.13 predate the format adoption and use descriptive subsections instead of the standard categories — they are kept verbatim as a historical record.

## [Unreleased]

### Fixed

- **`ibkr positions` no longer prints literal `BASE per +1% FX`.** The portfolio FX-sensitivity line now names the actual base currency (e.g. `EUR per +1% FX`). The daemon resolved the base from a bare `Currency` tag in the streaming account-summary map, but IBKR populates that tag with the literal string `"BASE"` (the gateway's pseudo-currency name, not the account's real base), so `FXBaseCurrency` came back as `"BASE"` and the renderer dutifully printed it. The resolver now scans the `$LEDGER:ALL` rows for an `ExchangeRate_<ccy>=1.0` entry — the currency whose rate is exactly 1.0 is the base by definition — and only uses the `Currency` tag when its value isn't the literal `"BASE"`. Five regression tests in `internal/daemon/fx_decorator_test.go` cover the pseudo-currency case, a real-currency `Currency` tag, the `ExchangeRate`-only fallback, the no-signal case (returns empty, never invents a default), and the empty/pre-handshake map.

### Changed

- **Smaller binaries.** `make build` and `make release-binaries` now pass `-s -w` to the linker, which strips the external symbol table and DWARF debug info. The `bin/ibkr` artefact drops from 9.6 MB to 6.5 MB on darwin/arm64 (~32%); release tarballs shrink proportionally. What's traded away: external debuggers (`delve`), `go tool nm`/`objdump`, and third-party profilers can no longer symbolicate the binary. Go panic stack traces remain fully readable — the runtime carries its own function metadata, separate from the symbol table. Startup time is unchanged; this is purely a size optimisation. Same convention used in Docker, Kubernetes, and most production Go binaries.

## v0.12.2 — 2026-05-12 22:05 CEST

Fix release. Five defects from a code-review pass on v0.12.1, all small, all with regression tests in the same change. No flags removed, no wire shapes broken, no behaviour change for existing successful calls — the changes either close a silent leak, harden the daemon against hostile/buggy clients, or move CI to match the README's gating promise.

### Fixed

- **`cancel` RPC actually cancels now.** `rpc.MethodCancel = "cancel"` was declared and `handleQuoteSubscribe` carefully registered each stream into `s.streams[req.ID]` so a peer could cancel it — but the daemon's dispatcher had no case for the method, so every `cancel` request came back `unknown_method` and the `subEntry` refcount stayed held until the client's TCP socket EOFed. For long-lived MCP clients that subscribe many resources, that's the difference between releasing IBKR market-data slots on-demand and burning them until the session ends. Added the dispatcher case plus `handleCancel` (rejects empty/unknown ids with classified `bad_request` — silent success would mask client-side bugs). Two regression tests cover the happy path and the unknown-id path. Live smoke against the running daemon now returns `{"ok":false,"error":{"code":"bad_request","message":"no active stream with id …"}}` for unknown ids instead of `unknown_method`.

- **`UnsubscribeMarketData` is case-insensitive.** `SubscribeMarketData` upper-cases the symbol before storing it in `c.subscriptions`; `UnsubscribeMarketData` did not, so `Unsubscribe("aapl")` after `Subscribe("aapl")` was a silent no-op — the IBKR-side `reqMktData` line stayed open and ate one of the ~100 subscription slots until the connection bounced. Hits anyone forwarding user-typed symbols straight through without pre-normalising. One-line fix in the library plus a regression test (`TestUnsubscribeMarketData_CaseInsensitive`) that pins the contract.

- **Bundled settings now allow `ibkr size`.** `settings/ibkr.settings.json` listed every read-only verb in `permissions.allow` except `Bash(ibkr size*)` — the position-sizing helper that shipped in v0.11. Users who copied the file into `~/.claude/settings.json` (a path the README explicitly recommends) got a permission prompt every time they ran `ibkr size`. The SKILL.md frontmatter had it; settings did not. One-line addition.

### Security

- **Daemon survives handler panics and oversize frames** — closes two latent denial-of-service surfaces on the Unix-socket RPC server. `serveConn` used `bufio.ReadBytes('\n')` with no upper bound, so a peer sending a newline-free megabyte would grow the read buffer until OOM. The dispatcher had no panic recovery, so a `json.Marshal(NaN)` or any other handler panic would unwind through the per-connection goroutine and disconnect every other client sharing the listener. Added a 1 MiB per-frame cap (`readBoundedLine` + `errFrameTooLarge`) — well above any real CLI/MCP payload — and a `defer recoverHandler(...)` in `dispatch` that converts a panic into a classified `internal` error on the request's own id, with the full stack trace landing in the daemon log for postmortem. Five regression tests: two unit tests on the bounded reader (rejects oversize, accepts at exactly the cap), two on the recover helper (writes error response, tolerates nil request), one end-to-end on `serveConn` that pushes a 2 MiB blob through a `net.Pipe` and asserts a classified `bad_request` response without OOM or hang.

### Changed

- **CI now invokes `make check && make test`, not an inlined re-implementation.** The README labels `make check` and `make test` as the binding gates — but CI re-implemented gofmt/vet/staticcheck/govulncheck inline and skipped `plugin-check` and `parity-check` entirely. The MCP↔CLI drift test (`parity-check`) ran only by side effect of `go test ./internal/...`; the plugin-manifest validation never ran in CI at all. The new CI workflow shells out to `make check CHECK_DEPS=parity-check`, `make test-pkg`, and `make test-daemon`, with `CHECK_DEPS` introduced in the Makefile as the documented escape hatch for environments without the `claude` CLI on PATH (the parity gate stays strict). `make check` and `make test` are now single-source-of-truth gates: a contributor's local run is the same gate CI applies. Test timeouts in `test-pkg` and `test-daemon` bumped to match the previous CI values (180 s / 240 s / 420 s) so the consolidation doesn't tighten anything CI was depending on.

## v0.12.1 — 2026-05-12 21:36 CEST

Bug-fix release. The headline feature from v0.10.0 — per-leg option Greeks on `ibkr positions` — has been quietly broken since the IBKR gateway rolled forward to server version 165 or later. The handler that parses the model-computation tick was reading the wrong field as `reqID`, so every Greek the gateway sent landed on a key nobody was looking up, and `greeks_coverage` came back 0/N on every call. That's fixed here, alongside two related issues that compounded the symptom and one zombie-position bug. Three new optional fields show up on each option row in JSON: `option_bid`, `option_ask`, `option_prev_close`, plus `iv`. No flags or wire shapes were removed; existing consumers see the same output plus those four optional fields.

### Greeks now arrive and get routed to the right contract

`pkg/ibkr.handleOptionComputation` decoded tick 21 (model-computation Greeks) using offsets that only matched the pre-server-version-165 wire layout — `fields[2]` was treated as `reqID`, `fields[3]` as `tickType`. Modern gateways send `[msgID, reqID, tickType, tickAttrib, impliedVol, delta, optPrice, pvDividend, gamma, vega, theta, underlyingPrice]`, so the handler was reading `tickType` (10/11/12/13) as the reqID. That ID isn't in the connector's option-request map, so the handler exited early and the Greek values fell on the floor. Result: `greeks_coverage: 0` on every option-bearing positions call, including the agent-feedback example. Fixed by reading `reqID` at `fields[1]` and `tickType` at `fields[2]`. New regression test in `connector_greeks_test.go` (`TestHandleOptionComputationWireOffsetIsAtFieldOne`) pins the wire offset so a future revert can't silently re-introduce the bug. Existing handler tests were updated to use the modern layout — under the old layout the test rows happened to align by coincidence, which is part of why the bug shipped.

### Held options skip the slow contract round-trip

`SubscribeOption` resolves each option's ConID via `reqContractData` before requesting market data. That round-trip can take a few hundred ms when the gateway is warm — but under load it sometimes silently times out, and the code then tries a second exchange, eating the full 10 s before giving up. For a 13-leg book that wipes the 30 s positions deadline before Greeks can be requested at all. The fix is honest: `msgPortfolioValue` already carries the full contract spec (ConID, exchange, trading class) for every held position, so the daemon now seeds the option-contract cache from portfolio data as it arrives. `SubscribeOption` hits cache for held positions and skips the round-trip entirely. New test `TestHandlePortfolioValueSeedsOptionContractCache` covers it.

### Per-leg option market data exposed in JSON

Each option row in the `positions` response now carries four new optional fields populated from the per-leg market-data subscription the daemon already opens for Greeks:

- `option_bid` / `option_ask` — the option contract's own bid and ask, not derived from the underlying. The mark sits between them. When the spread is wide on illiquid contracts (RDDT $185C, GME $30C), the mark may not be tradable — these two fields are how callers can tell.
- `option_prev_close` — the option contract's own previous regular-session close (tick 9 from the option's own market-data feed). The existing `prev_close` field on option rows continues to carry the underlying's prev close for backward compatibility, which the agent feedback correctly flagged as confusing. The new field is the one to use for option-level day-over-day P&L.
- `iv` — the implied volatility from the model-computation tick, as a fraction (0.30 = 30%).

All four fields are nil-omitted when the subscription didn't capture them in the budget — no fabricated zeros.

### Delisted positions no longer inflate `effective_delta`

A held delisted ticker (the user's HGENQ-style zombie) arrives via `msgPortfolioValue` with mark=0 — the gateway streams the position but rejects market-data subscriptions for it. On the first `positions` call after daemon start, the connector hasn't yet flagged the symbol inactive, so the zombie contributed its full share count (20 000 in the test book) to `effective_delta`. The second call would correctly exclude it once the inactive flag landed, so the same daemon reported wildly different effective deltas back-to-back. `buildPortfolioAggregates` now skips stocks with mark ≤ 0 from the aggregate; the position row still renders with mark=0, which is the honest answer. New test `TestBuildPortfolioAggregatesExcludesZombieStocks` covers it.

### Other notes

- `optionGreeksBudget` was 1.5 s; bumped to 2.5 s to give the gateway a comfortable margin once Greeks actually flow. Per-leg observed latency on the test book was 750–1100 ms in cache-warm conditions; 2.5 s leaves room without blowing the 30 s positions deadline (4-way parallel × 13 legs × 2.5 s worst case = 8.1 s).
- The wire-decode fix means `handleOptionComputation` now reads `fields[3]` as `tickAttrib` (option-computation flags). The field is parsed but not yet consumed; we'll wire it through to the renderer if it turns out to carry information worth surfacing.

## v0.12.0 — 2026-05-12 07:45 CEST

Four things land together: a real ad-hoc scanner path that lets agents compose a scan without rewriting the user's config, per-row enrichment so scanner output actually carries last/change/volume/IV/52w instead of bare symbols, a fresh seven-preset default set validated against the live gateway catalog, and two longstanding hardening fixes (wire-frame cap, status/scan readiness consistency) that surfaced during scanner work. Plus a test-harness orphan-prevention fix for `make test`. No CLI flag removals; all wire shapes back-compatible (existing `last`/`change`/`volume` fields on `rpc.ScanRow` switched from scalars to pointers, which is JSON-wire-compatible — `omitempty` drops nil same as zero). The default `[scans]` map changed — see migration note below.

### Scanner subscription timeout bumped 20 s → 35 s; clearer error on cold-start

The wire-level scanner-subscription timeout was 20 s — fine during RTH, too tight off-hours when IBKR's scanner farm needs 25-45 s to warm up for the time-of-day-dependent scanCodes (HIGH_OPEN_GAP, TOP_PERC_GAIN, HIGH_OPT_IMP_VOLAT_OVER_HIST, HOT_BY_OPT_VOLUME). Bumped to 35 s. The timeout error text now says *"scanner subsystem did not respond within Ns (often a cold-start off-hours; retry in a few seconds, especially for HIGH_OPEN_GAP / TOP_PERC_GAIN / option-IV scans)"* instead of the previous "scanner timed out after Ns" so users know retry is the right response. Daemon `MethodScanRun` ceiling raised 30 → 75 s and the CLI per-invocation budget for `scan` raised 60 → 90 s so the daemon's classified error reaches the user instead of a socket timeout. The matching `Scan.Timeout` field in `config.toml` still overrides the default — useful for users who want to fail fast or wait longer per preset.

### `ibkr scan` rows now carry market data, not just symbols

IBKR's `reqScannerSubscription` protocol returns only `rank` + `symbol` per row (plus three free-text fields that are empty for the common scan types — verified at the wire level for `MOST_ACTIVE` and `HOT_BY_VOLUME` against server v203). v0.11 surfaced that bare leaderboard verbatim, which made the output essentially useless: a list of tickers with no way to tell whether they were up or down, liquid or illiquid, near 52-week highs or lows. v0.12 enriches each row by issuing parallel `Hold`-based market-data subscriptions in a bounded worker pool (20 concurrent × 6 s per-row window), then merging the resulting ticks back into the row before serialisation. Fields added to `rpc.ScanRow`: `last`, `prev_close`, `change`, `change_pct`, `volume` (compact K/M/B in the text renderer), `iv` (averaged option IV from generic tick 106 — fraction, 0.234 = 23.4%), `week_52_high`, `week_52_low`. The text renderer adds matching columns with green/red colour on `change_pct` and dim 52w range, identical width/colour conventions to `ibkr quote` so the eye doesn't have to re-train. Nil fields stay nil — no fabricated proxies, em-dash in the text renderer — which is the load-bearing read: off-hours, most ticks don't arrive, and the honest column is empty rather than misleading. Enrichment happens daemon-side so MCP / JSON consumers see the same enriched payload as the text renderer.

Plumbing: `pkg/ibkr/connector.go` `Subscription` struct gains `Week13/26/52Low/High` and `IV`; `handleTickPrice` switch extended for tick types 15-20; `handleTickGeneric` for tick 106 now also writes to the subscription (it previously routed only to the chain-IV cache); `MarketData` / `GetMarketData()` surface the new fields; the daemon's `subManager.Hold` now requests generic ticks `100,101,104,106,165` so the gateway actually delivers the new ticks (previously asked for `100,101,104` — IV and 52w were unreachable from the snapshot path). `MethodScanRun` unary deadline bumped from 30 s to 50 s to accommodate enrichment waves.

### `ibkr scan` — three new shapes

Until v0.11 the only way to run a scan was a preset by name, which forced anyone wanting to try a different `scanCode` / `locationCode` to first edit `~/.config/ibkr/config.toml` and restart the daemon. That hard-coded gate has been replaced with two new modes:

- **Ad-hoc:** `ibkr scan --type TOP_PERC_GAIN --exchange STK.NASDAQ --limit 25 [--json]`. No preset required. Rows are capped at 50. MCP tool `ibkr_scan` accepts the same `type` and `exchange` fields. Designed for agent workflows that need to compose a scan on the fly.
- **Catalog dump:** `ibkr scan params [--instrument STK] [--raw] [--json]`. Pulls IBKR's full `reqScannerParameters` XML, parses it, and returns the three lists agents need to compose a valid scan: `instruments` (e.g. STK / OPT / ETF), `locations` (every `locationCode`), and `scan_types` (every `scanCode` with display name + applicable instrument types). The catalog varies by gateway version and market-data permissions — never assume a `scanCode` exists without checking. `--instrument STK` narrows to stock scans; `--raw` attaches the full XML (~200 KB–2 MB) for the rare case where a field outside the parsed result is needed. MCP exposes this as `ibkr_scan_params`.

Preset mode is unchanged. The MCP `ibkr_scan` tool's empty-args branch (no `preset` / `type` / `exchange`) returns the preset list, same as before.

### New default preset set (replaces the four v0.10.x defaults)

Validated against live IB Gateway server-version 203 via the new `scan params` dump before being committed. The selection covers the screens an active US stock + options trader actually runs:

| preset             | scanCode                        | exchange      | rationale                                  |
|--------------------|---------------------------------|---------------|--------------------------------------------|
| `top-movers`       | TOP_PERC_GAIN                   | STK.US.MAJOR  | unchanged                                  |
| `top-losers`       | TOP_PERC_LOSE                   | STK.US.MAJOR  | symmetric counterpart (was missing)        |
| `most-active`      | MOST_ACTIVE                     | STK.US.MAJOR  | unchanged                                  |
| `unusual-vol`      | HOT_BY_VOLUME                   | STK.US.MAJOR  | unchanged                                  |
| `gappers`          | HIGH_OPEN_GAP                   | STK.US.MAJOR  | opening earnings/news reactions            |
| `high-iv-rank`     | HIGH_OPT_IMP_VOLAT_OVER_HIST    | STK.US        | replaces `high-iv`; IV vs own history is the option-seller signal — absolute IV always surfaces the same biotech/SPAC names |
| `unusual-opt-vol`  | HOT_BY_OPT_VOLUME               | STK.US.MAJOR  | the canonical "smart money flow" scan      |

**Migration note.** `high-iv` is gone, replaced by `high-iv-rank` with a different `scanCode` (`HIGH_OPT_IMP_VOLAT_OVER_HIST` vs. `HIGH_OPT_IMP_VOLAT`). Users who pinned `[scans.*]` blocks in their own `config.toml` are unaffected (the table is replace-not-merge — your file always wins). Users who relied on the built-in defaults will see the new set after upgrading; run `ibkr scan list` to view it.

### Wire-frame cap raised to 16 MB, stream-desync recovery hardened

`pkg/ibkr.readMessage` had a 1 MB cap on a single TWS message frame. The IBKR scanner-parameters XML on a US Pro gateway with options data is 1–2 MB. Hitting the cap was silent until v0.12: the previous read loop logged the error and `continue`d, which left the reader positioned mid-frame and turned subsequent body bytes into bogus length prefixes — one local repro saw 500 k+ "message too large" log lines in a few seconds before disconnect. Two surgical changes: (a) cap raised to 16 MB, well above any realistic IBKR frame; (b) any non-timeout / non-EOF read error now signals disconnection and exits the read goroutine, so reconnect logic can rebuild a clean stream rather than blindly continuing. Unit test in `pkg/ibkr/scanner_params_test.go` plus integration test `TestScanParamsReturnsCatalog` pin the cap behavior.

### `status.connected` now reflects `IsReady`, not `IsConnected`

The daemon was using TCP-level `IsConnected()` for `status.health.connected` but every data verb (quote, chain, scan, positions) gated on `IsReady()` — the post-handshake "handlers armed" state. When the connector landed in `{ready=false, conn=up}` (overnight TWS hiccup, market-data farm reconnect), `ibkr status` cheerfully reported `connected: true, data_type: "live"` while every other call returned `gateway_unavailable`. Worse, `triggerReconnect` only fired when TCP dropped — so the stuck state was unrecoverable without a daemon restart. Three lines changed: status.connected, `gatewayConnector`, and the early-exit guard in `triggerReconnect` all now consult `IsReady()`. Stuck-state recovery is now self-healing. Pinned by `TestConnector_IsReadyAndIsConnectedCanDiverge` in `pkg/ibkr/connector_ready_test.go`.

### Integration test harness no longer orphans daemons

`test/integration` spawned `ibkr daemon --foreground` without `Setpgid`. macOS doesn't propagate parent death; if `go test` was SIGKILL'd, timed out, or panicked before `TestMain` reached `stop()`, the spawned daemon stayed alive indefinitely. The harness now: (a) places the daemon in its own process group via `SysProcAttr.Setpgid`, (b) signals the whole group via `kill(-pgid, …)` in `stop()` so any future grandchildren die too, (c) installs a `signal.Notify` handler for SIGINT/SIGTERM that routes through `stop()` before `os.Exit`. SIGKILL is still unrecoverable — nothing we can do there — but every other interrupt path now cleans up. File now has `//go:build !windows` (Setpgid is Unix-only; the package was already Unix-only in practice).

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
