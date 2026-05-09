# ibkr — handoff

Status as of 2026-05-09 20:45 CEST. Plan source: `PLAN.html`. Project layout & overview: `README.md`. Released-since-baseline: `CHANGELOG.md`.

## Outstanding (next session, start here)

- **Plugin wrapping is the headline work.** Full plan in "Next session — plugin wrapping" below; deployment shape decided in "Distribution & deployment". End-user happy path: install the binary (`go install github.com/<owner>/ibkr/cmd/ibkr@latest` + `cmd/ibkrd@latest`), then `/plugin marketplace add <owner>/ibkr` + `/plugin install ibkr`. Phase A CLI extensions just landed; SKILL.md + settings allowlist already mention every command including the new `history` subcommand, so the migration is purely structural.
- **Live integration suite needs gateway-up confirmation.** With IB Gateway running:
  ```
  make test                     # full suite incl. -race, all green w/o gateway
  bin/ibkr status               # confirms connected:true
  bin/ibkr history AAPL --days 30   # NEW: HMDS daily bars
  bin/ibkr positions --by underlying  # NEW: grouped view
  bin/ibkr quote AAPL           # NEW: should now show bid_size / ask_size / volume columns
  ```
  Then walk the README pre-release smoke matrix.
- **CHANGELOG: expand v1.0.0** (decision: 2026-05-09). Repo is not a git repo yet, no tag exists, v1.0.0 is unreleased. Fold late-landing CLI/TLS/test/lint/Phase-A work into the existing v1.0.0 bullets; open v1.0.1 only after a real tag is published.
- **Keep Go patched.** `make check` runs `govulncheck` and fails on stdlib vulns, so an outdated Go toolchain is a build failure. Fix: `brew upgrade go` until clean (we're on 1.26.3 at handoff, no findings).

## v1.1 backlog (CLI + library deferrals)

Explicitly declined for v1.0 because they require either protocol extension
in `pkg/ibkr/` or behaviour that depends on live-gateway verification past
v1.0's scope. Deployment-side deferrals live in the "Distribution &
deployment" section above (v1.3+ bucket).

- **`ibkr chain SYM` expiry listing.** Cheap surface (`ibkr chain AAPL`
  with no `--expiry` lists expiries; `--expiry X` keeps current behaviour).
  Blocked on adding `Connector.FetchOptionExpiries` — needs outbound msg
  ID 78 (`reqSecDefOptParams`) and inbound handlers for 75/76 (constants
  defined in `pkg/ibkr/connection.go:1159-1160` but unwired). Optional
  `--with-iv` flag fetches per-expiry ATM IV (slow: N expiries × subscribe
  cycle); `--weeklies=false` filters to 3rd-Friday monthlies. ~1 day with
  integration test.
- **Per-strike IV in the existing `chain` table.** Library has
  `Connector.SubscribeOptionIV` (tick 106). The current chain renderer
  collects bid/ask/last via `fillOptionLeg` but ignores IV. Wiring this is
  smaller than the listing work — straightforward extension of
  `fillOptionLeg`.
- **Intraday history bars.** v1.0 ships daily-only via
  `FetchHistoricalDailyBars`. v1.1 should add `--bar 1h` / `--bar 5m`
  etc. with a wider `Connector.FetchHistoricalBars` wrapper.
- **Net-delta line on `positions --by underlying`.** Requires per-leg
  delta; v1.0 omits it. Lands once the chain-listing work also exposes
  Greeks.
- **`trades --today`, watchlists, history JSON schema versioning.**

## Next session — plugin wrapping (decided 2026-05-09)

**Goal:** ship `ibkr` as a Claude Code plugin (no MCP yet). End-user
happy path is documented in "Distribution & deployment" below: install
the binary first (`go install` for v1.0; `brew install` once a tap
exists in v1.3+), then `/plugin marketplace add <owner>/ibkr` followed
by `/plugin install ibkr`. `./install.sh --merge-settings` stays as
the canonical permissions-merge step (plugins can't ship permissions).

**How Claude discovers what the plugin does:**
- Frontmatter `description` in `SKILL.md` = the trigger. Claude indexes it
  on session start and decides whether to load the skill.
- `SKILL.md` body = parameter docs. Read by Claude when it decides to invoke.
  Currently flags (`--watch`, `--rate`, `--width`, `--expiry`, `--timeout`)
  are shown by example only — tighten to explicit per-command flag lists
  during the migration.
- `schemas.md` = output-shape contract, pulled in when Claude parses `--json`.
- `plugin.json` description is for the user (shown in `/plugin list`), not
  for the model.

SKILL.md tightening already shipped in the Phase A pass — per-command
explicit flag lists, the `history` row, the `by_underlying` example, and
the size/volume worked example are all in place. The plugin migration is
now purely structural.

**Open verification items resolved (research pass 2026-05-09 20:30):**
- `/plugin marketplace add` accepts `owner/repo`, full git URLs, HTTPS to
  a `marketplace.json`, AND local absolute paths. So both local-dev and
  GitHub-published flows are supported.
- Plugins **cannot** ship `permissions.allow`/`deny`. Permissions still
  have to live in user-side `settings.json` — `settings/ibkr.settings.json`
  + `--merge-settings` stay as the canonical permissions path.
- Plugins **can** ship binaries via a top-level `bin/` directory (added to
  Bash PATH while enabled). We are intentionally **not** using this for
  v1.0 — see "Distribution & deployment" below for the rationale.
- Anthropic hosts an official plugin index at `claude.com/plugins` with a
  submission form. v1.0 self-hosts the marketplace (this repo); we'll
  submit after a clean live-gateway smoke and a real GitHub tag.

**Steps:**
1. **Pre-req: turn the directory into a git repo and publish to GitHub.**
   `git init` → first commit covering everything as-is → push to
   `https://github.com/<owner>/ibkr`. Without this `/plugin marketplace
   add <owner>/ibkr` is a no-op (local-path is a fallback for testing,
   not the distribution path we want users on).
2. Move `skill/SKILL.md` → `skills/ibkr/SKILL.md`; same for `schemas.md`.
   Update references in `install.sh`, `Makefile` (`SKILL_DIR`,
   `install-skill`), and `README.md`.
3. Add `.claude-plugin/plugin.json` (`name=ibkr`, `version=1.0.0`,
   `description`, `author`). `version` is load-bearing for `/plugin
   update` resolution — set it explicitly, not implicit-via-commit-SHA.
4. Add `.claude-plugin/marketplace.json` listing the single plugin so
   the repo is its own marketplace. Schema:
   `{"name": "ibkr", "owner": {"name": "..."}, "plugins": [{"name":
   "ibkr", "source": "./", "description": "..."}]}`.
5. Add `hooks/hooks.json` carrying:
   - The existing `PreToolUse` trading-verb jq block from
     `settings/ibkr.settings.json:22-28` (so it works even for users
     who skip the settings merge — defence in depth).
   - A `SessionStart` hook that runs `command -v ibkr >/dev/null ||
     echo "..."` to print a clear install hint when the binary is
     missing. Keep it silent on the happy path.
6. Update `README.md` "Use with Claude Code": three-step happy path
   (install binary via `make install` or `go install ...@latest`,
   `/plugin marketplace add <owner>/ibkr`, `/plugin install ibkr`,
   then optional `./install.sh --merge-settings` to pre-allow the
   read-only commands without per-call confirmations).
7. Update `install.sh` so the post-install "Sanity checks" section
   prints the two `/plugin` commands first, then the `--merge-settings`
   reminder. Keep `--merge-settings` working unchanged.

**Explicitly out of scope for v1.0:** MCP server / connector (would
deliver structured tool calls with JSON-schema parameter validation
instead of prose-based parameter inference; deferred to v1.1+ if
demand surfaces). Bundled binaries inside the plugin's `bin/` dir
(see "Distribution & deployment" for why).

## Distribution & deployment (decided 2026-05-09)

Three audiences, three install paths. The repo is the single source of
truth; what bleeds into a user's environment depends on which audience
they belong to.

| Audience | What they install | Touches `~/.claude`? |
|---|---|---|
| **Repo developer** (cloning + building) | `make install` → `bin/ibkr`+`bin/ibkrd` to `$GOBIN` | No (unless they explicitly run `make install-skill` or `./install.sh --merge-settings` to dogfood) |
| **Library consumer** (e.g. regime importing `pkg/ibkr`) | `go.mod` bump only — no binaries, no plugin | No |
| **End user** (CLI and/or Claude integration) | `go install github.com/<owner>/ibkr/cmd/ibkr@latest` + `cmd/ibkrd@latest`, then optional `/plugin marketplace add <owner>/ibkr` + `/plugin install ibkr` + optional `./install.sh --merge-settings` | Only if they install the plugin |

**Why we are not bundling binaries in the plugin's `bin/` for v1.0:** the
plugin and the CLI have legitimately separate audiences (the regime
library consumer never needs the plugin). Coupling binary distribution
to plugin distribution would force the plugin repo to carry per-platform
binaries (~13 MB × 4 platforms × every release) and would conflate the
two release cadences. For end users who want one-shot install, that's a
v1.3 problem (Homebrew tap or a SessionStart auto-install hook).

**v1.0 expectations are explicit:** the plugin's SessionStart hook
detects a missing `ibkr` binary and prints a clear install hint; it does
**not** try to install anything itself. The README's "Use with Claude
Code" section orders the steps the same way: binary first, plugin
second.

**Deferred to v1.3+ (deployment hardening):**
- Cross-platform GitHub Releases via goreleaser (darwin-arm64/amd64,
  linux-amd64/arm64).
- Homebrew tap (formula pulls from GitHub Releases). Tap repo name TBD —
  the obvious `osauer/homebrew-tap` is the convention but `osauer`
  isn't a memorable namespace; revisit when the tap matters.
- Submission to `claude.com/plugins` (gated on: live-gateway smoke
  green, real GitHub tag, license cleared for redistribution).
- Optional plugin-side auto-install via SessionStart (cross-platform
  shell install script that pulls from GitHub Releases when binary
  missing).

**Pre-submission to `claude.com/plugins`:** README currently says
"Personal use; not yet open-sourced. Contact the author before
redistributing." That posture is incompatible with the registry.
Decision needed at submission time, not now: pick an OSS license
(MIT/Apache-2 are the obvious candidates), update README, then submit.

## What landed in this Phase A pass (2026-05-09 19:30–20:15)

Four CLI surface extensions, all read-only, all gated through the daemon
with the same `gatewayReady()` short-circuit and `errBadRequest` mapping
the rest of the daemon already uses. SKILL.md and settings allowlist are
updated; the plugin migration in the next session inherits this surface
unchanged.

- **Quote sizes + volume.** `pkg/ibkr/connector.go` `Subscription` gained
  `BidSize`/`AskSize` (int64); `handleTickSize` now dispatches IBKR ticks
  0/3/8 (was: 8 only). Daemon's `handleQuoteSnapshot` populates
  `q.BidSize`/`q.AskSize`/`q.Volume`; streaming `handleQuoteSubscribe`
  emits sizes in frames (volume omitted — slow tape, would clutter ticks)
  and the dedupe key now includes sizes so a size-only delta doesn't get
  swallowed. CLI quote tables grew BID_SZ / ASK_SZ / VOLUME columns; a
  generic `formatSize` helper renders compact K/M abbreviations.
  `TestHandleTickSize_DispatchesByTickType` covers the new switch.
- **Conditional `REAL P&L` column in `positions`.** Already on the wire as
  `RealizedPnL`; never rendered. CLI now detects any non-zero realized
  value across stocks+options and shows the column when present, otherwise
  hides it to keep the default table narrow. Test:
  `TestPositionsRealizedColumnOnlyShownWhenNonZero` covers both branches.
- **`ibkr history SYM [--days 90]`.** New RPC method
  `history.daily` (`internal/rpc/rpc.go`) wraps `Connector.FetchHistoricalDailyBars`.
  Calendar-driven lookback; daily granularity only (intraday is v1.1
  backlog). Empty symbol → `bad_request`; gateway down →
  `gateway_unavailable`. `barDate` normalises IBKR's `YYYYMMDD` to
  `YYYY-MM-DD`. Tests: disconnect path + bad-request path.
- **`positions --by underlying`.** New `PositionGroup` type +
  `ByUnderlying` field on `PositionsResult` — always populated, so JSON
  consumers can rely on it regardless of the CLI flag. Pure-options groups
  have `stock: null`; stock-only groups have empty `options`. CLI's
  `--by underlying` switches to a grouped renderer with per-group
  market-value + unrealized-P&L totals. Tests:
  `TestGroupByUnderlying` (stock-only / options-only / mixed) +
  `TestRenderPositionsByUnderlying` (rendering, including the
  pure-options no-Stock-line invariant).

SKILL.md was tightened: per-command explicit flag lists replace the old
example-only inference; `history` row added to the command table; quote
worked-example now shows sizes/volume; positions worked-example shows
`by_underlying`; new `## history` section in `schemas.md`.
`settings/ibkr.settings.json` allowlist now includes `Bash(ibkr history*)`.

## What landed in this evening's lint pass (2026-05-09 18:30–19:20)

Hardened the build gate. `make check` is now binding and green on Go 1.26.3;
`make test` runs `check` before any test target, so format / vet /
staticcheck / vuln failures short-circuit the suite. Daemon-side tests now
run under `-race`.

- **`make check` (NEW, binding):** `gofmt -l` (fails on any unformatted
  file) + `go vet ./...` + `staticcheck ./...` + `govulncheck ./...`.
  Replaces the old non-gating `make lint`. govulncheck is part of `check`
  by design — an outdated Go toolchain with known stdlib CVEs is a build
  failure. Keep Go patched.
- **`make test-daemon` adds `-race`** for `./internal/...` and
  `./test/integration/...`. The pkg/ibkr layer keeps its non-race default
  (the 30s pkg test would balloon under -race; race detector lives where
  the goroutines do — daemon, subscriptions, idle timer).
- **`make test` chain:** `check → test-pkg → test-daemon` (latter two
  unchanged in scope, but test-daemon now races).
- **Tools rebuilt against Go 1.26.0:** staticcheck (was 1.24.4),
  gopls (was 1.23.10), deadcode (was 1.24.4). govulncheck installed fresh.
  `air` left alone — not in this project's workflow. Background: when
  Go's toolchain advances, tool binaries built against the old Go can't
  parse stdlib usage in newer modules; reinstalling rebuilds them
  against the local toolchain.
- **24 staticcheck findings cleaned up:**
  - SA1019 deprecated APIs: `rand.Seed` removed (Go 1.20+ self-seeds);
    `net.Error.Temporary()` replaced with `errors.Is(err, net.ErrClosed)`
    in fake-server accept loop.
  - SA1012 nil contexts: 4 test sites switched to `context.TODO()`.
  - S1000/S1005/S1008 minor style: `for { select {<-ticker.C: ...} }`
    flattened to `for range ticker.C`; redundant blank assignment and
    if/return simplification.
  - U1000 dead code: 16 unexported helpers deleted across `pkg/ibkr/`,
    `internal/cli/`, `internal/daemon/`, `test/integration/`. Removed
    funcs include `floatVal`, `debugDump`, `marketDataTypeName`,
    `lastValue`, `envBool`, `splitMsgBytes`, `readString`,
    `freshThresholdForPhase`, `isInactiveCandidate`, `looksLikeTimeZone`,
    `looksLikeHours`, `isKnownExchange`, `containsRune`,
    `snapshotGenericTicks`, `(*WireInterceptor).collectContext`,
    `gatewayHandshakeOK`. Per the project's "no orphans" rule.
- **8 files reformatted by gofmt** (struct field column alignment in
  `internal/cli/cli_test.go`, `internal/cli/quote.go`,
  `internal/config/config.go`, `internal/daemon/server.go`,
  `internal/rpc/rpc.go`, `pkg/ibkr/connector.go`,
  `pkg/ibkr/connector_tick_validation_test.go`,
  `pkg/ibkr/gateway_test.go`).

**State at end of session:**
- `make check` → exit 0 on Go 1.26.3 (gofmt clean, go vet clean,
  staticcheck clean, govulncheck "No vulnerabilities found.")
- `make test` → exit 0 (pkg 27s, internal/* 1–2s each w/ -race,
  integration 29s w/ -race)
- `bin/ibkr` and `bin/ibkrd` build clean

## Recurring gateway gotcha (read this first)

IB Gateway 10.37+ ships with the API socket disabled by default and exposes no UI toggle for it. The persistence file (`~/Jts/<userdir>/ibg.xml`, encrypted) is shared between TWS and Gateway. Fix sequence:

1. Launch TWS, accept its "Enable ActiveX and Socket Clients" prompt
2. Quit TWS
3. Restart Gateway

Symptoms when the toggle is off: gateway accepts TCP, reads our handshake bytes, then silent — no response within 60s. `~/Jts/<userdir>/api.<clientID>.*.ibgzenc` log files do NOT appear on connection attempts.

`ibkr status` now surfaces this hint inline when `Connected: false`, including the `LastError` string set by the daemon when the handshake doesn't complete.

## What landed in the prior session (CLI UX + TLS, 2026-05-09 morning)

CLI UX overhaul, three commits' worth of work plus a TLS pass:

- **Commit 1 — Connectivity is a first-class state.** Every read handler now short-circuits on `!s.gatewayReady()` (`internal/daemon/handlers.go`). Misclassified errors fixed: `chain` "no spot price" promotes to `gateway_unavailable` when disconnected; `scan` unknown-preset is now `bad_request` not `internal`. CLI's `fail()` appends a `hint: run 'ibkr status' …` line when the daemon reports `gateway_unavailable`. `quote AAPL,MSFT,…` aborts after the first gateway-unavailable error instead of timing out per symbol. Daemon log no longer claims "Connected" when the connector is up but disconnected — it now logs the truth and stores a `LastError` string surfaced via `status.health`. `HealthResult.DataType` is `omitempty` so disconnected status no longer lies with `"live"`.

- **TLS contract pass.** `EnableTLSFallback = !cfg.Profile.TLS` in `internal/daemon/server.go`: `tls=true` is treated as a contract (no silent downgrade); `tls=false` keeps the library's bidirectional fallback. New `Connector.UsingTLS()` exposes the negotiated mode; `HealthResult.NegotiatedTLS` carries it through to the CLI; `ibkr status` shows `(tls=true, configured=false ⚠ fallback)` when configured ≠ negotiated. README has a TLS paragraph under Configure.

- **Commit 2 — `status` promoted as the entrypoint.** First in the help table with a "(run this first if anything fails)" hint. Help footer adds `First run? Try 'ibkr status' …`. The disconnected-state status output now embeds a 3-step troubleshooting block.

- **Commit 3 — Polish.** `MethodOrderCancel` routed to its own (still-disabled) stub; misleading `account --profile` flag deleted; `dataTypeBadge` is silent on the live/empty path (only shows `data=delayed ⚠` when surprising); per-subcommand `--help` now matches top-level style and exits 0 (handler `fs.Parse` errors detect `flag.ErrHelp` via new `parseExit` helper); `commands` slice + `lookupCommand()` replaces the old `registry` map + `commandList` slice; dead `buildOCCSymbol` deleted; `make lint` target added (non-gating, runs `go vet` + `staticcheck`).

- **Test coverage added.** `internal/daemon/handlers_test.go` (5 tests covering disconnect-state for every read handler, scan-unknown-preset bad_request mapping, status reports disconnected, full classifyError matrix). `internal/cli/cli_test.go` (hint logic, hoistFlags table tests, dataTypeBadge quiet-on-live, command-registry consistency, --help interception, formatTLSField).

## Files touched in the prior CLI UX + TLS session

```
internal/cli/cli.go              registry consolidation, --help wiring, parseExit, fail() hint
internal/cli/status.go           degraded header, troubleshooting block, formatTLSField
internal/cli/account.go          --profile removed, suffixBadge
internal/cli/positions.go        suffixBadge, parseExit
internal/cli/quote.go            early abort on gateway_unavailable, parseExit
internal/cli/chain.go            suffixBadge, parseExit
internal/cli/scan.go             parseExit
internal/cli/cli_test.go         NEW: 5 test groups
internal/daemon/server.go        gatewayReady seam, lastConnectError, EnableTLSFallback, classifyError
internal/daemon/handlers.go      gatewayReady guards, errBadRequest, NegotiatedTLS, dead helper removed
internal/daemon/handlers_test.go NEW: 5 tests
internal/rpc/rpc.go              HealthResult: NegotiatedTLS, LastError, DataType omitempty
pkg/ibkr/connection.go           Connection.UsingTLS()
pkg/ibkr/connector.go            Connector.UsingTLS()
cmd/ibkr/main.go                 --help / -h interception so help doesn't spawn the daemon
README.md                        TLS paragraph; make lint mention
Makefile                         lint target (non-gating)
```

## Other deferred work

CLI/library deferrals → "v1.1 backlog" (above). Deployment deferrals →
"Distribution & deployment" v1.3+ bucket (above). The remaining items
that don't fit either:

- **Full IBKR scanner DSL** (filter parameters beyond `type` + `exchange`
  + `limit`) — v1.2.
- **Trading verbs** — v2 only. Three independent layers refuse them
  today: binary stub returns `ErrTradingDisabled` (now via dedicated
  handlers per method), settings.json deny rule, PreToolUse hook.
- **Golden / snapshot tests for CLI text output** — would catch a class
  of formatting regressions cheaply if added later. Currently we test
  rendering by substring match.

## State of the tree at handoff (2026-05-09 20:45 CEST)

- `bin/ibkr` and `bin/ibkrd` built fresh after Phase A landed.
- `make check` clean on Go 1.26.3: `gofmt -l` empty, `go vet ./...`
  clean, `staticcheck ./...` clean, `govulncheck ./...` finds nothing.
- `make test` green end-to-end: pkg/ibkr (27s), pkg/ibkr/protocoltest,
  internal/cache, internal/cli, internal/config, internal/daemon
  (all `-race`), test/integration (`-race`, 30s).
- Phase A added 6 new tests (1 in pkg/ibkr, 3 in internal/cli, 2 in
  internal/daemon); pre-existing tests untouched and still green.
- Not yet a git repo; no GitHub remote; no v1.0.0 tag. Plugin
  packaging next session begins with `git init` + first commit + push
  to `https://github.com/<owner>/ibkr`.
- No daemon processes running, no stale Unix sockets in `~/.cache/ibkr/`,
  no `/tmp` debris.
- `~/.config/ibkr/config.toml` `profiles.live.port = 4001` (Gateway).
  `profiles.paper.port = 4002`. Comment notes 7496 for TWS.
- Live-gateway run from prior session: `ibkr status` returns
  `Connected: true`; `ibkr account` shows EUR balances; `ibkr positions`
  lists 16 positions cleanly. Phase A additions (history, sizes/volume,
  --by underlying) have NOT yet been smoke-tested against the live
  gateway — that's on the next-session todo.

## Reference points

- Working directory: `/Users/osauer/dev/ibkr`
- Source library: `pkg/ibkr/` (extracted from regime; staying in-tree, not split into a separate repo as the original plan envisioned)
- Default socket: `$XDG_RUNTIME_DIR/ibkr/ibkrd.sock` (fallback `~/.cache/ibkr/ibkrd.sock`)
- Default daemon log: `~/.local/state/ibkr/ibkrd.log`
- Skill install target: `~/.claude/skills/ibkr/`
- Settings merge target: `~/.claude/settings.json` (via `./install.sh --merge-settings`; backup at `settings.json.bak.<unix>`)
- Live IBKR account exercised this session: U5091510, base EUR
- Client IDs: daemon defaults to 15; integration tests start at 19 and increment per launch (avoid the 100-104 range used by regime)

## Conventions for the next session

- **No mocks of daemon-internal data.** Wire-decoder tests use captured-from-gateway fixtures; integration tests talk to the real daemon over a real socket. The daemon talks to the real gateway. This is the project's stance and it caught two real protocol bugs in the prior session.
- **Acceptance criteria before code.** Define functional + test-coverage + integration verification up front. Both fixes this session followed this pattern.
- **Gateway health surfaces in logs and `ibkr status`, not in code probes.** If the daemon can't handshake, the relevant logs are `~/.local/state/ibkr/ibkrd.log` and `~/Jts/<userdir>/api.<clientID>.*.ibgzenc`. `ibkr status` now also shows the daemon's `LastError`. Don't add side-channel synthetic probes — they were the cause of the silent-gateway flakiness in the prior session.
- **Disconnected-state behavior is now a first-class test concern.** When adding a new read handler, add a corresponding case to `internal/daemon/handlers_test.go` asserting it returns `ErrIBKRUnavailable` when `gatewayReady()` is false. The CLI's `gateway_unavailable` hint kicks in automatically once the daemon classifies the error correctly.
