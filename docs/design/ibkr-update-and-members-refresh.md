# `ibkr update` and SPX members runtime refresh

**Status:** Design — senior-reviewed (2026-05-23), ready for implementation. All blockers and important findings folded into the spec below.
**Created:** 2026-05-23 07:37 CEST
**Last update:** 2026-05-23 08:20 CEST
**Owner:** osauer
**Related:** [scripts/refresh-spx-members/main.go](../../scripts/refresh-spx-members/main.go), [internal/breadth/spx/members.go](../../internal/breadth/spx/members.go), [internal/breadth/spx/members_data.go](../../internal/breadth/spx/members_data.go), [docs/design/gamma-zero-cache-persistence.md](./gamma-zero-cache-persistence.md) (precedent for XDG cache pattern).

## Why now

Two distinct gaps surfaced after the v0.32.0 release pipeline ran:

1. **Stale-binary problem.** v0.32.0 ships a constituent list pulled from Wikipedia at release time. A user installed from a tagged release has no path to a newer list short of re-installing the binary. With Claude Code plugin distribution + future SPA users on the roadmap, this becomes a real concern: a user could trade off six-month-old constituent data without any visible signal.

2. **Self-update gap.** The same future-user group needs a path to update the binary itself without a `git pull && make install` workflow they don't have access to.

Both are pre-existing acceptable trade-offs for the single-dev case (the maintainer rebuilds on every release); both become real problems with non-maintainer users.

## Two independent mechanisms

This doc covers two changes deliberately kept independent:

- **A. SPX members runtime refresh** (automatic, daemon-internal, no CLI surface).
- **B. `ibkr update` command** (manual, CLI-triggered, binary only).

The members refresh is the higher-leverage change — it operates continuously and is invisible to the user. The binary update is the discoverable surface. They are functionally independent (different external services, different trigger paths) but share three small filesystem primitives that already exist as inlined patterns in the gamma cache: `WriteAtomic` (temp+rename), `OpenLock` (flock), `CacheDir` (XDG resolution). Phase 1 extracts these into `internal/xdgcache/` (~80 LOC total, no abstractions, just the three functions) and both paths import them. The gamma-cache code stays as-is for this design; future cleanup can migrate it to the same package without changing behaviour.

---

## A. SPX members runtime refresh

### Source-of-truth and liability

**Decision: fetch directly from Wikipedia, same URL the release script uses.**

The alternative considered was publishing the JSON to GitHub (either as a release asset or a `main`-branch file at `data/sp500-members.json`). Rejected on liability grounds: hosting financial reference data on the project's GitHub puts the maintainer in the content-provider chain. A user who trades on stale data from a self-hosted JSON has a clearer line to the maintainer than one trading on stale data from Wikipedia. The release pipeline already trusts Wikipedia; adding a GitHub middleman is a strict liability increase with no information benefit (same source either way).

Side benefits:
- Single source of truth across release-time and runtime — no possible drift between two parallel publication paths.
- Zero new infrastructure (no GitHub Action, no `data/` directory, no CI).
- Wikipedia editor updates land within hours of S&P's after-close announcements, ahead of the next-morning effective date.

### Storage

`~/.cache/ibkr/spx-members/sp500-members.json` (XDG-aware, fall back to `$HOME/.cache/...`).

Matches the gamma-cache pattern at `~/.cache/ibkr/gamma-zero/gamma-zero-{scope}.json`. Defensible as cache because the embedded `sp500Members` slice always exists as fallback — losing the file is recoverable.

Shape:

```json
{
  "version": 1,
  "as_of":   "2026-05-22T02:32:14Z",
  "source":  "wikipedia",
  "url":     "https://en.wikipedia.org/wiki/List_of_S%26P_500_companies",
  "count":   503,
  "members": ["A", "AAPL", "ABBV", "ABNB", "..."]
}
```

Write semantics: atomic temp+rename, pretty-printed JSON. Same pattern as [internal/daemon/gamma_zero_store.go:158](../../internal/daemon/gamma_zero_store.go#L158) (`writeAtomic`). One file, one writer (the daemon).

### Shared parser

Extract the working parsing logic from [scripts/refresh-spx-members/main.go](../../scripts/refresh-spx-members/main.go) into a new package:

```
internal/breadth/spx/wikipedia.go    (~80 LOC)
  FetchAndParse(ctx context.Context, url string) ([]string, time.Time, error)
  ParseHTML(html []byte) ([]string, error)
```

The release script becomes a thin caller of `FetchAndParse` and writes the generated Go file. The daemon's runtime refresher becomes a different thin caller writing the JSON file. Same parser, same 450–520 sanity bound, same User-Agent string.

Bounds-fail behavior: refuse to install the new list; log a warning; current state is unchanged. Same fail-closed posture the release script has today.

### Refresh triggers

Three triggers, all converging on one fetcher goroutine:

1. **Daily ticker at 02:30 ET.** Between midnight NY-session-key roll and 04:00 ET pre-market open. Catches reconstitution effective dates — Wikipedia editors typically update within hours of S&P's after-close announcement (~5 trading days before effective date), so by 02:30 ET on the morning of the effective date, the new list is live.

2. **Daemon startup catch-up.** If the loaded file's `as_of` is from a NY trading date earlier than today, kick a background refresh immediately. Covers the laptop-closed-at-02:30 case.

3. **First breadth call after NY-date rollover.** Same condition as #2 but evaluated at request time. Belt-and-suspenders: if the daemon was already running but the ticker fired during a network outage, the next breadth call picks up the refresh.

All three converge on a singleflight refresher — at most one fetch in flight per daemon. Concurrent triggers join the existing job (mirrors the gamma cache's `kickOrJoin` pattern).

**Holidays are not modeled.** The 02:30 ET ticker fires every weekday including Thanksgiving / July 4 / Christmas / half-day mornings. Constituent membership doesn't change on holidays so the fetch hits unchanged HTML — wasted but harmless.

Network-failure posture: log a warning, keep the prior file, breadth keeps computing. Same fall-back chain as a corrupt file: external file → embedded list. Network outages never break breadth.

### Pinning / disabling auto-refresh

Some users need a frozen constituent list — regulated traders running reproducibility audits, air-gapped boxes, anyone debugging breadth drift. Two override layers:

1. **TOML config** at `internal/config/config.go`:
   ```toml
   [spx]
   members_auto_refresh = false
   ```
   Persistent. When `false`: daemon never starts the ticker, never opportunistically refreshes, never fetches on startup. Loads the existing cache file if present, otherwise embedded list. The cache file is not deleted — flipping back to `true` resumes refresh from whatever was last persisted. Lives under `[spx]` rather than `[members]` so the section name is unambiguous about what membership it refers to (the SPX constituents, not anything about IBKR account members).

2. **Environment variable** `IBKR_SPX_MEMBERS_AUTO_REFRESH`. Symmetric bidirectional override of the TOML field: `=1` force-enables (overrides TOML `false`), `=0` force-disables (overrides TOML `true`), unset / garbage / empty defers to TOML. The principle is least surprise — env vars are explicit overrides, config is defaults; if you set `=1` you mean it, and you get it. Symmetric semantics also match the project's existing IBKR_* convention (e.g. `IBKR_SMOKE_STRICT=0/1`).

`ibkr status` surfaces the pin so a user looking at unexpected breadth values can immediately see whether the list is being refreshed (see next section).

### Status surface

`ibkr status` gains one line under the breadth section that surfaces the current members-list state. Without this, silent parser rot (Wikipedia HTML changes break the regex, daemon falls back to embedded list, user never knows their numbers are based on a 6-month-old list) is invisible.

Format:
```
Members  <source>:<as_of>  count:<N>  [refresh:<state>]
```

The `refresh:` segment is omitted in the healthy case. It appears only when something needs the user's attention:

```
Members  cache:2026-05-22  count:503                                          # healthy
Members  embedded:2026-05-22  count:503  refresh:parse_failed                 # silent rot
Members  embedded:2026-05-22  count:503  refresh:network_failed               # offline / DNS down
Members  embedded:2026-05-22  count:503  refresh:disabled (config)            # pinned via TOML
Members  cache:2026-05-22  count:503  refresh:disabled (env)                  # pinned via env
```

Source token (`cache:DATE` vs `embedded:DATE`) makes it obvious whether the in-process list is from the auto-refresh path or the binary's compiled-in baseline. The cache `as_of` already covers "how fresh," so a separate fetch timestamp would be noise on the healthy line.

### Wikipedia etiquette

- `User-Agent: ibkr/{version} (https://github.com/osauer/ibkr; +breadth indicator)` — identifies the tool and gives Wikipedia ops a contact path.
- One fetch per daemon per day at most (the three triggers all singleflight to one).
- 15-second HTTP timeout, no retries (a failed fetch falls back; we'll try again at the next trigger).

This is well under Wikipedia's tolerance for scraping. Comparable to a personal RSS reader.

### Impact on SPX breadth recalc

**This is the load-bearing piece.** A members-list change mid-session is not a no-op for downstream breadth state.

Removed tickers: silently drop on next compute. No invalidation needed.

**Added tickers (new constituents from reconstitution):** a stock added today has no meaningful 50-DMA-vs-its-S&P-membership signal — the 50-day average covers a period when the stock wasn't in the index. Two policy choices:

- **(b) Pending until 50 trading days accrue** (recommended). The new constituent is excluded from breadth until it has 50d of post-inclusion history. Breadth temporarily computes over 502 instead of 503 names; <1% noise. Reconstitution adds 1–3 names per quarter, well within the existing 450–520 sanity bounds. Honest — we don't pretend to information we don't have.
- **(a) Backfill 50d via IBKR on inclusion.** Pull the new symbol's 50-day bar history via IBKR on the first breadth call after inclusion. More mathematically correct (breadth set always matches index size). Adds a historical-bar fetch path the breadth code doesn't have today; ~50 IBKR API calls per added symbol, amortized once.

Default: **(b)**. Cleaner. Migration to (a) is additive if we ever care.

**Cache invalidation hook:** when the loaded list differs from the in-process snapshot, invalidate any breadth-cache state keyed by the prior list. Symmetric to the gamma cache's NY-session-key boundary.

Need to read the breadth code (`internal/breadth/spx/breadth.go` and adjacent) before implementing to confirm what state needs invalidating. Out of scope for this design — flagged for the implementation session.

### What we explicitly don't add

- `ibkr update --members-only` CLI command. Refresh is fully automatic; no user surface.
- GitHub-hosted JSON or release asset (see liability above).
- GitHub Action for JSON refresh (no JSON to refresh).
- Manual ceremony around a missing file (embedded fallback covers it).

### Sizing

- `internal/breadth/spx/wikipedia.go` — extracted parser, ~80 LOC shared between the script and the daemon.
- `internal/breadth/spx/refresh.go` — daemon fetcher with ticker + opportunistic triggers + singleflight, ~150 LOC.
- `internal/breadth/spx/members.go` — modified to prefer external file over embedded list, with fall-back, ~30 LOC delta.
- `internal/config/config.go` — `[spx] members_auto_refresh` TOML field + `IBKR_SPX_MEMBERS_AUTO_REFRESH` symmetric env-var override resolution (`=1` force-on, `=0` force-off, unset defers), ~15 LOC delta.
- `internal/daemon/status.go` (or wherever `ibkr status` renders) — one-line members row with source / fetch-outcome / pin-reason, ~15 LOC delta.
- Breadth cache-invalidation hook — ~10 LOC delta in the existing breadth code.
- Tests — parse round-trip (reuse `scripts/refresh-spx-members/testdata`), sanity-bound rejection, file → embedded fallback, singleflight, list-change cache invalidation, env-var-overrides-config, status-row rendering. ~250 LOC.

Total: ~310 LOC new + ~70 LOC modified. Shares `internal/xdgcache/` (~80 LOC) with path B. The release script keeps working as-is (it's now a parser caller too).

---

## B. `ibkr update` command — binary self-update

### CLI surface

```
ibkr update                  # default: fetch latest, install, prompt to restart daemon (TTY)
ibkr update --check          # dry-run, report what would change (no install)
ibkr update --force          # install latest even if same version (corrupt-binary recovery)
ibkr update --restart        # explicit "restart daemon after install" (skips prompt)
ibkr update --no-restart     # explicit "don't restart" (skips prompt)
```

No `--members-only` / `--binary-only` split — `ibkr update` is binary-only. Members refresh is the daemon's job (path A).

No `--prerelease` flag in v0.33.0. The GitHub `/releases/latest` endpoint filters out pre-release tags by default, so "stable only" is free. Add `--prerelease` the same release a prerelease tag actually ships, and gate the addition on a CI test against a real prerelease asset. Today there are zero prerelease tags on the repo; the flag would be untestable on real data.

### Source

GitHub releases at `https://api.github.com/repos/osauer/ibkr/releases/latest` (no auth required for public releases). Match host OS/arch against asset names:

```
ibkr-vX.Y.Z-darwin-arm64.tar.gz
ibkr-vX.Y.Z-darwin-amd64.tar.gz
ibkr-vX.Y.Z-linux-amd64.tar.gz
ibkr-vX.Y.Z-linux-arm64.tar.gz
SHA256SUMS
```

`runtime.GOOS` + `runtime.GOARCH` pick the right tarball. Refuse to install if no match (windows, etc.) with a clear "no binary for your platform" error.

### Verification and install

All downloads land in `~/.cache/ibkr/update/` (XDG-resolved, fallback `$HOME/.cache/ibkr/update/`). Single tempfile per artefact, no leftover state on any non-success exit (deferred `os.Remove` covers errors, panics, AND signal handling — install a SIGTERM/SIGINT handler that triggers cleanup).

1. Acquire a `flock` on `~/.cache/ibkr/update/update.lock` for the duration of the entire flow — download + verify + install + rollback rotation. Reuses the `internal/daemon/lock.go` pattern. Concurrent `ibkr update` invocations queue rather than racing on `.bak` rotation.
2. Download `SHA256SUMS` to the temp dir.
3. Download the matching tarball to the temp dir.
4. Compute SHA256 of the tarball, compare against the `SHA256SUMS` entry. Mismatch → abort, leave existing binary intact.
5. Extract tarball to a sibling temp dir.
6. Verify the extracted binary's first few bytes are an ELF/Mach-O magic (smoke check for corruption).
7. **macOS quarantine strip** runs here, before rename, on the extracted binary in the temp dir. If `xattr -d com.apple.quarantine` fails (permissions, missing `xattr` binary on a stripped macOS, etc.) the install aborts with the prior binary intact. Doing this post-rename would leave a quarantined binary in place with no rollback signal.
8. Stash prior binary as `~/.local/bin/ibkr.bak` (overwrites any existing `.bak`; see rollback semantics below).
9. Atomic install: temp → `~/.local/bin/ibkr` via `os.Rename`. Unix inode-swap means a running daemon keeps its old inode; new invocations get the new binary.
10. Release the flock.

**`.bak` rollback semantics**: `.bak` is the *immediately prior* binary, not "the version before everything went wrong." Users wanting longer rollback history should keep their own copies.

No code signing today. If/when we add notarization, the xattr-strip step is the natural hook point.

### Daemon restart — TTY-aware

The headless case (cron, systemd timers, CI, stdin-redirected shells) cannot use a `[Y/n]` prompt — reading stdin blocks indefinitely. Two flags cover every case unambiguously: `--restart` and `--no-restart`. Either flag suppresses the prompt and encodes the decision directly.

**TTY detection.** The CLI calls `term.IsTerminal(int(os.Stdin.Fd()))` at startup. If false (non-TTY) and neither `--restart` nor `--no-restart` was supplied, the command exits non-zero with `ibkr update: ambiguous in non-interactive mode — pass --restart or --no-restart` and does NOT install. Silent default-to-N would be a footgun for systemd timers expecting auto-restart.

**Matrix:**

| Mode  | TTY | Flags          | Behaviour                                                              |
|-------|-----|----------------|------------------------------------------------------------------------|
| Default (TTY)     | yes | (none)         | Install, then prompt `restart now? [Y/n]`. Default Y on enter.   |
| Auto-restart      | any | `--restart`    | Install, restart without prompting.                                    |
| Skip-restart      | any | `--no-restart` | Install, print "restart manually" hint, exit 0.                        |
| Headless ambiguous| no  | (none)         | Exit non-zero with ambiguity error; do NOT install.                    |

**On restart**: `pkill -f "ibkr daemon"` + autospawn (the daemon autospawns on the next `ibkr <command>`, so no explicit restart command needed). If no daemon is running (PID file check), the restart step is a no-op regardless of flags.

`--force` stays purely about version-bypass ("install latest even if same version"); it does NOT toggle restart behaviour. The two concerns are kept orthogonal so future flag additions don't have to disambiguate overloaded semantics.

### Version comparison

Use `semver` parsing on the installed version (`bin/ibkr version` output) and the latest tag. Skip the install if installed >= latest; print "already on latest" and exit 0.

`--force` bypasses the check (handles "binary is corrupt, please reinstall same version").

### Network-failure posture

- GitHub API unreachable → exit 1, "could not reach GitHub releases API", clear error. No automatic retry — user can re-run.
- Tarball download fails partway → exit 1, no partial state on disk.
- SHA mismatch → exit 1, "downloaded asset failed checksum", leave existing binary intact.

In all failure modes, the prior binary is untouched and the prior daemon keeps running. No degraded state.

### What we explicitly don't add

- Auto-update on a timer / startup. Binary update is explicitly user-triggered.
- `--prerelease` flag in v0.33.0. Project has zero prerelease tags today; defaults to stable via `/releases/latest` for free. Add the flag the release a prerelease tag actually ships.
- Update channels beyond `stable`. No `dev` / `nightly` / `lts`.
- Cross-version migration of `~/.cache/ibkr/`. Cache is recreatable; nothing to migrate.
- Plugin manifest update. The `.claude-plugin/plugin.json` version travels with the binary; nothing extra to do.
- Self-rebuild from source. `ibkr update` fetches release artefacts only.

### Implementation hooks the implementer must remember

- **MCP-parity exclude.** `internal/mcp/mcp.go` asserts every CLI command has an MCP counterpart or is on an exclude list (enforced by the parity test that runs in `make check`). `update` is a binary-management command, not a daemon RPC — add it to the exclude list, or the release pipeline blocks.
- **CLI registration.** Help is custom: append to the `commands` slice in [internal/cli/cli.go](../../internal/cli/cli.go); wire `flag.FlagSet` in `flagSet()`. No cobra, no man pages, no separate README table to sync.
- **README.** The "Releasing" section (added per the doc-task spawned earlier) should grow a one-line note that end-users on a tagged release can `ibkr update` to the next one — distinct from the maintainer's `make release` path.

### Sizing

- `internal/cli/update.go` — CLI command wiring, flag matrix, TTY detection, prompt rendering, ~100 LOC (was 50 — flag-matrix coverage and the ambiguous-mode error path added the delta).
- `internal/update/github.go` — GitHub release fetcher (API + tarball), ~100 LOC.
- `internal/update/install.go` — verify + extract + xattr-strip + atomic install + flock + signal-handler cleanup, ~200 LOC (was 150 — flock and signal cleanup added).
- `internal/update/daemon.go` — PID check + restart logic + `pkill`, ~50 LOC (was 60 — prompt logic moved to update.go).
- Tests — fixture HTTP server for GitHub API, tarball round-trip, SHA mismatch rejection, version-comparison branches, flock contention between parallel invocations, TTY-vs-non-TTY flag-matrix branches, signal-handler cleanup of temp files. ~200 LOC (was 150).

Total: ~450 LOC new. Shares `internal/xdgcache/` (~80 LOC) with path A — see the "Two independent mechanisms" section above.

---

## Roll-out

**Both paths ship in v0.33.0.** One release, focused changelog (auto-refresh + CLI surface for self-update + status row + config knob). The chicken-and-egg is real but bounded: v0.32.0 users can't `ibkr update` to v0.33.0 because their installed binary doesn't know the command. They install v0.33.0 manually one time (release tarball, brew, or `make install`), and `ibkr update` carries them forward from there.

Document the manual-upgrade path prominently in the v0.33.0 changelog `### What's new` and in the README's `Releasing` section:

> Upgrading TO v0.33.0: manual install (this version is the first with `ibkr update`).
> Upgrading FROM v0.33.0: `ibkr update`.

Rollback path stays via `~/.local/bin/ibkr.bak` and / or re-installing a prior release tarball.

## Out of scope

- **Notarization / code-signing of the released binary.** Worth doing eventually; out of scope for this design.
- **SHA256SUMS signing.** The current scheme verifies tarball SHA against `SHA256SUMS` but doesn't verify `SHA256SUMS` itself — a MITM that swaps both files passes. The standard fix is cosign / minisign / GPG on `SHA256SUMS`. Named here as a known gap, not an oversight; revisit in the same release notarization lands.
- **Update of the embedded constituent fallback list.** Stays managed by the release script (`make refresh-spx-members`); the runtime refresh layers on top.
- **Members refresh for indices other than S&P 500** (NDX, RUT, etc.). Same pattern would apply but each has its own Wikipedia page and its own constituent stability rules.
- **Auto-update for the binary.** Explicitly rejected — the binary is the trust boundary; automatic replacement removes user agency.

## Open decisions (still open)

The interview and senior review closed most; these remain for the implementation session:

- **New-constituent handling**: default to (b) "pending until 50d accrue." If the breadth code already has a mechanism for "include with caveat," prefer that; otherwise add the exclusion list. Reconsider if a user complains that breadth temporarily reads over 502 instead of 503.
- **Breadth cache invalidation hook surface**: needs reading `internal/breadth/spx/breadth.go` and adjacent before implementing to confirm what state needs invalidating when the members list changes mid-session.
- **`ibkr update --check` exit codes**: 0 if already-latest, 0 if update available (informational), non-zero only on actual fetch failures. So `ibkr update --check && ibkr update` is the idiomatic confirm-then-install pattern.

## Phase 2 (post-v0.33.0, sequencing intentional)

Two pipeline tightenings that dog-food the new mechanisms at release time. Both are deferred until v0.33.0 has shipped and the runtime fetcher + `ibkr update` have been live for at least one release without surfacing issues. The chicken-and-egg is the reason: if the release pipeline depends on either mechanism and that mechanism breaks, the *fix* for it can't go out via the normal pipeline.

### P2-A. Release pipeline refreshes members data through the runtime fetcher

Today `make refresh-spx-members` runs the standalone script (`scripts/refresh-spx-members/main.go`) that scrapes Wikipedia and rewrites `internal/breadth/spx/members_data.go` directly. After v0.33.0, the daemon also scrapes Wikipedia via the shared parser. Same source, two callers, one Wikipedia hit per release plus one per daemon-day.

Phase 2 collapses this. The refresh-spx-members script becomes a thin caller of the same shared `FetchAndParse` the daemon uses (both are Go programs in the same module — direct package call, no RPC needed) and transcribes the result into `members_data.go`. One Wikipedia hit per release instead of two.

**Embedded fallback stays alive.** "No recompile necessary" means *for production updates* (the daemon picks up new members from the cache file at runtime), not *for release-time refreshes* (the embedded `sp500Members` baseline still tracks reality at release cadence). Removing the embedded fallback entirely would break first-run-without-network and lose the deterministic-binary property — neither worth giving up.

What this buys:
- End-to-end validation of the shared parser on every release. Wikipedia HTML breakage surfaces at release time rather than in production.
- Schema-drift catch: the same JSON the daemon writes is what the embedded-fallback regenerator reads. Format changes break the release, not silently in production.

### P2-B. Release pipeline dog-foods `ibkr update`

After `release-publish` lands the tarballs on GitHub, run `bin/ibkr update --force --no-prompt --restart --install-to=<sandbox>` to verify the just-released binary is actually installable via the update path. Catches the failure modes unit tests can't:

- Asset filename mismatches between `make release-binaries` output and what `internal/update/install.go` expects
- `SHA256SUMS` format drift
- Tarball extraction working on the host's `tar` version
- Daemon-restart sequence working end-to-end against a real installed binary
- Flock contention with whatever daemon happens to be running on the host

**Sandboxing is load-bearing.** The release pipeline cannot overwrite the maintainer's `~/.local/bin/ibkr` mid-release — that mutates the system the release is being cut from. `internal/update/install.go` honours an `IBKR_INSTALL_DIR` env-var override when set; ships in Phase 1 alongside `IBKR_SPX_MEMBERS_AUTO_REFRESH` so Phase 2 only needs the Makefile wiring. Release-pipeline knob, not user-facing.

The dog-food run installs to `/tmp/ibkr-release-dogfood-XXXX/bin/ibkr`, verifies the copy is launchable (`./bin/ibkr version` matches the release tag), and cleans up the temp dir on success. Failure aborts the release-publish step.

What this buys:
- The "first user of the release" is the release pipeline itself. Any breakage in the install path surfaces before users see it.
- Confidence that the maintainer's manual upgrade path (download tarball, extract, `make install`) and the user's `ibkr update` path produce equivalent results.

### Sequencing

Both P2-A and P2-B land in the same release — call it v0.34.0 or v0.35.0 depending on what else is in flight. The order within that release: A before B, because A increases confidence in the runtime fetcher path that B exercises end-to-end. If A surfaces a bug, B can wait one more release.

Neither is added to the v0.33.0 changelog. v0.33.0 ships the user-facing surface; Phase 2 is invisible to users and lives in the release pipeline.
