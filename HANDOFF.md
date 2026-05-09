# ibkr — handoff

Status as of 2026-05-09 22:30 CEST. Plan source: `PLAN.html`. Project layout & overview: `README.md`. Released-since-baseline: `CHANGELOG.md`.

## Outstanding (next session, start here)

- **Live integration smoke is the gating task.** v0.1 (initial beta) is
  feature-complete on paper but Phase A additions, the new chain expiry
  listing, and the newly-wired per-strike IV in the chain table have not
  yet been smoke-tested against a live IB Gateway. With Gateway up:
  ```
  make test                          # full suite incl. -race, all green w/o gateway
  bin/ibkr status                    # confirms connected:true
  bin/ibkr account                   # NLV in account base currency
  bin/ibkr positions                 # mark / MV / unrealized P&L populated
  bin/ibkr positions --by underlying # grouped view with per-group totals
  bin/ibkr quote AAPL                # bid_size / ask_size / volume cols
  bin/ibkr history AAPL --days 30    # HMDS daily bars
  bin/ibkr chain AAPL                # NEW: list expiries (no --expiry)
  bin/ibkr chain AAPL --with-iv      # NEW: list expiries + per-expiry ATM IV
  bin/ibkr chain AAPL --expiry <next-monthly>  # full chain, now with call_iv/put_iv
  ```
  Then walk the README pre-release smoke matrix.

- **Push to GitHub.** Repo is initialized locally (`git init` happened this
  session) but not pushed. Plugin install path (`/plugin marketplace add
  osauer/ibkr`) requires the public GitHub remote. User will trigger when
  ready; do not push without explicit confirmation.

- **v0.1.0 tagged + pushed to GitHub** (done 2026-05-09). Plugin marketplace
  flow (`/plugin marketplace add osauer/ibkr` + `/plugin install ibkr`) is
  live. **Next tag is v0.2.0** (or v0.1.1 for hot-fix only). 1.0.0 is
  reserved for the first stable read-only release after live-gateway smoke
  and broader downstream use.

- **Keep Go patched.** `make check` runs `govulncheck` and fails on stdlib
  vulns. We're on 1.26.3 at handoff, no findings.

## What landed in this session (2026-05-09 19:30–22:30)

Five commits on `main`. `make check` + `make test` green at every commit.

- `347f3cb` — **Initial commit.** `git init` on the working tree, first commit
  covering the entire v0.1 source. `.gitignore` extended to exclude
  `.claude/settings.local.json`, `.claude/worktrees/`, and `next-priorities.html`.
- `a423099` — **Claude Code plugin wrap.** `skill/` → `skills/ibkr/`;
  `.claude-plugin/plugin.json` (v0.1.0, owner=osauer) +
  `.claude-plugin/marketplace.json` (single-plugin marketplace, `source: "./"`);
  `hooks/hooks.json` carrying the `PreToolUse` trading-verb guard (defence in
  depth, copied from `settings/ibkr.settings.json:22-28`) and a `SessionStart`
  hook printing an install hint when `ibkr` is missing from PATH; `install.sh`
  skill install moved behind `--install-skill` flag (dogfood path) so the
  plugin owns skill distribution; `Makefile` `SKILL_SRC` variable +
  `install-skill` re-labelled as dogfood-only; `README.md` "Use with Claude
  Code" rewritten as the three-step happy path; `CHANGELOG.md` v0.1.0 expanded
  with three new subsections (Operability and CLI UX, Build and quality gate,
  Claude Code plugin).
- `8feabb9` — **Chain expiry listing** (delegated to a worktree-isolated
  Opus agent on `feat/chain-expiries`, reviewed and rebased). Adds
  `Connector.FetchOptionExpiries` and `Connector.FetchOptionExpiryStrikes`
  sharing one `reqSecDefOptParams` (msg 78) round trip; outbound constant
  declared at `pkg/ibkr/connection.go`; new `connector_expiries.go` with
  defensive parsing + reqID-keyed dedupe; `chain.expiries` RPC method (empty
  symbol → bad_request, gateway down → gateway_unavailable); CLI branches on
  empty `--expiry` to render the listing; `--with-iv` flag fetches per-expiry
  ATM IV via existing `SubscribeOptionIV` (sequential, 2s per strike, never
  fails the whole call). Five new tests in `pkg/ibkr/connector_expiries_test.go`
  using the protocoltest fake-server pattern; disconnect + bad-request +
  closestStrike tests in `internal/daemon/handlers_test.go`; CLI rendering
  test in `internal/cli/cli_test.go`.
- `4ef0c1a` — **Skill docs for chain expiry.** Two-row chain table in
  SKILL.md (listing mode + full mode), per-command `--with-iv` flag with
  guidance on when to reach for it, new `## chain-expiries` schema section,
  README chain bullet split.
- `aba704b` — **Per-strike IV in chain table.** Library
  `SubscribeOption` now also registers `optReqIDs[reqID] = chainKey` so the
  existing tick-21 (option computation) handler routes IV through to the
  per-strike key (e.g. `AAPL_260619C200`). `fillOptionLeg` keeps polling
  past the first bid/ask print, looking for IV until the existing 2.5s
  deadline. Schema field `call_iv` / `put_iv` was already there; just unwired.

## Wire-format notes (record from agent's research pass)

The chain-expiry agent verified the IBKR wire format against
`/Users/osauer/Library/Python/3.9/lib/python/site-packages/ibapi/{client,decoder}.py`.
Two refinements over the prior HANDOFF:

- **Outbound msg 78 has no version field.** Six fields total:
  `[msgID=78, reqID, underlyingSymbol, futFopExchange, underlyingSecType, underlyingConId]`.
- **Inbound msg 75 has no version field either.** After msgID:
  `[reqID, exchange, underlyingConId, tradingClass, multiplier,
  expCount, expirations[], strikeCount, strikes[]]`. Multiple 75 frames per
  request (one per exchange — SMART, AMEX, CBOE, …), one 76 end marker.

Both are post-versionless-protocol additions to IBKR. Older messages (orders,
historical data) still carry version fields; new feature messages don't.

## v0.2 backlog (current)

Two items burned down this session (chain expiry listing, per-strike IV in
chain table). Remaining:

- **Net-delta line on `positions --by underlying`** (deferred 2026-05-09).
  Bigger than HANDOFF originally estimated. `handleOptionComputation` parses
  IV/gamma/vega/theta from tick 13 but **silently drops `delta` (field index 5)**;
  `Connector.GetOptionDelta` accessor doesn't exist. Plus position rows today
  get mark/MV/PnL from `reqAccountUpdates` (account-level), not market-data —
  so delta needs fresh per-leg subscriptions, sequentially, ~2.5s each. A
  10-option portfolio would add ~25s to `positions`. Recommendation when
  picked up: opt-in `--with-greeks` flag (off by default), small parallel
  worker pool (3-4 concurrent subs is safe per gateway throttle). ~1-1.5h.
- **Intraday history bars.** v0.1 ships daily-only via
  `FetchHistoricalDailyBars`. v0.2 should add `--bar 1h` / `--bar 5m` etc.
  via a wider `Connector.FetchHistoricalBars` wrapper.
- **`trades --today`, watchlists, history JSON schema versioning.**
- **NEW: `SubscribeOptionIV` cancellation cleanup** (flagged by agent on
  chain-expiry merge). The library's `SubscribeOptionIV` doesn't expose a
  per-strike market-data key for `UnsubscribeMarketData`; cancellation in
  `--with-iv` keys on the underlying symbol (best-effort). Repeated calls
  can leak gateway-side subscriptions. Daemon-lifetime acceptable today;
  worth fixing for long-running daemon hardening. Same shape applies to
  the new per-strike IV path: `optReqIDs` / `optIV` / `optQuoteMid` grow
  per-key per session and `UnsubscribeMarketData` doesn't clean them up.

## Distribution & deployment (decided 2026-05-09, unchanged)

Three audiences, three install paths. The repo is the single source of truth.

| Audience | What they install | Touches `~/.claude`? |
|---|---|---|
| **Repo developer** | `make install` → `bin/ibkr`+`bin/ibkrd` to `$GOBIN` | No (unless `make install-skill` or `./install.sh --install-skill --merge-settings`) |
| **Library consumer** (e.g. regime importing `pkg/ibkr`) | `go.mod` bump only | No |
| **End user** (CLI + Claude integration) | `go install github.com/osauer/ibkr/cmd/ibkr@latest` + `cmd/ibkrd@latest`, then `/plugin marketplace add osauer/ibkr` + `/plugin install ibkr` + optional `./install.sh --merge-settings` | Yes (plugin) |

We are intentionally not bundling binaries inside the plugin's `bin/` for
v0.1: the plugin and the CLI have legitimately separate audiences (regime
library consumer never needs the plugin). End-users who want one-shot
install: that's a v0.4+ problem (Homebrew tap or SessionStart auto-install).

**Deferred to v0.4+ (deployment hardening, pre-1.0):**
- Cross-platform GitHub Releases via goreleaser (darwin-arm64/amd64,
  linux-amd64/arm64).
- Homebrew tap. Tap repo name TBD; revisit when the tap actually matters.
- Submission to `claude.com/plugins` (gated on: live-gateway smoke green,
  real GitHub tag, license cleared for redistribution).
- Optional plugin-side auto-install via SessionStart (cross-platform shell
  installer that pulls from GitHub Releases when binary missing).

**Pre-submission to `claude.com/plugins`:** README currently says "Personal
use; not yet open-sourced." That posture is incompatible with the registry.
Decision needed at submission time, not now: pick an OSS license (MIT or
Apache-2.0 are the obvious candidates), update README, then submit.

## Recurring gateway gotcha (read this first)

IB Gateway 10.37+ ships with the API socket disabled by default and exposes
no UI toggle. The persistence file (`~/Jts/<userdir>/ibg.xml`, encrypted) is
shared between TWS and Gateway. Fix sequence:

1. Launch TWS, accept its "Enable ActiveX and Socket Clients" prompt
2. Quit TWS
3. Restart Gateway

Symptoms when the toggle is off: gateway accepts TCP, reads our handshake
bytes, then silent — no response within 60s.
`~/Jts/<userdir>/api.<clientID>.*.ibgzenc` log files do NOT appear on
connection attempts. `ibkr status` surfaces this hint inline when
`Connected: false`, including the `LastError` string set by the daemon.

## State of the tree at handoff (2026-05-09 22:30 CEST)

- `bin/ibkr` and `bin/ibkrd` may be stale; run `make build` to refresh.
- `make check` clean on Go 1.26.3: `gofmt -l` empty, `go vet ./...` clean,
  `staticcheck ./...` clean, `govulncheck ./...` finds nothing.
- `make test` green end-to-end: pkg/ibkr (~25s), pkg/ibkr/protocoltest,
  internal/cache, internal/cli, internal/config, internal/daemon
  (all `-race`), test/integration (`-race`, fast skip when no gateway).
- 5 commits on main: `347f3cb`, `a423099`, `8feabb9`, `4ef0c1a`, `aba704b`.
  No remote, no tag.
- Plugin layout in place: `skills/ibkr/{SKILL.md,schemas.md}`,
  `.claude-plugin/{plugin.json,marketplace.json}`, `hooks/hooks.json`.
- No daemon processes running, no stale Unix sockets in `~/.cache/ibkr/`,
  no `/tmp` debris.
- `~/.config/ibkr/config.toml` `profiles.live.port = 4001` (Gateway).
  `profiles.paper.port = 4002`. Comment notes 7496 for TWS.
- Live-gateway runs from prior session: `ibkr status` returns
  `Connected: true`; `ibkr account` shows EUR balances; `ibkr positions`
  lists 16 positions cleanly. **Smoke for everything since
  2026-05-09 19:00 has not yet been run against the live gateway** —
  that's the headline next-session task.

## Reference points

- Working directory: `/Users/osauer/dev/ibkr`
- Source library: `pkg/ibkr/` (extracted from regime; staying in-tree)
- Default socket: `$XDG_RUNTIME_DIR/ibkr/ibkrd.sock` (fallback `~/.cache/ibkr/ibkrd.sock`)
- Default daemon log: `~/.local/state/ibkr/ibkrd.log`
- Skill source (in repo): `skills/ibkr/`
- Skill install target (dogfood path only): `~/.claude/skills/ibkr/`
- Settings merge target: `~/.claude/settings.json` (via `./install.sh --merge-settings`; backup at `settings.json.bak.<unix>`)
- Live IBKR account exercised in prior sessions: U5091510, base EUR
- Client IDs: daemon defaults to 15; integration tests start at 19 and
  increment per launch (avoid the 100-104 range used by regime)

## Conventions for the next session

- **No mocks of daemon-internal data.** Wire-decoder tests use
  captured-from-gateway fixtures; integration tests talk to the real daemon
  over a real socket. The daemon talks to the real gateway. This is the
  project's stance.
- **Acceptance criteria before code.** Define functional + test-coverage +
  integration verification up front. Big changes get explicit sign-off
  before code lands.
- **Gateway health surfaces in logs and `ibkr status`, not in code probes.**
  If the daemon can't handshake, the relevant logs are
  `~/.local/state/ibkr/ibkrd.log` and `~/Jts/<userdir>/api.<clientID>.*.ibgzenc`.
  `ibkr status` also shows the daemon's `LastError`.
- **Disconnected-state behaviour is a first-class test concern.** When
  adding a new read handler, add a corresponding case to
  `internal/daemon/handlers_test.go` asserting it returns
  `ErrIBKRUnavailable` when `gatewayReady()` is false. CLI's
  `gateway_unavailable` hint kicks in automatically once the daemon
  classifies the error correctly.
- **Worktree isolation requires `git worktree add` manually.** The
  Agent-tool's `isolation: "worktree"` flag wants WorktreeCreate hooks we
  don't have. For parallel agent work: create the worktree by hand
  (`git worktree add -b <branch> ../<dir> main`), spawn the agent with an
  explicit "your working directory is X — never touch the parent" brief.
- **Trading verbs** — refused at three layers (binary stub, settings.json
  deny rule, PreToolUse hook in both `settings/ibkr.settings.json` and the
  plugin's `hooks/hooks.json`). v2 only.
