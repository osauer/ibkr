# Project rules

## Architecture primers
Fresh sessions should read `docs/architecture.md` for the repo layer map and
`docs/design/platform-settings.md` before changing settings/config/state
surfaces. The platform settings mechanism is cross-cutting by design: daemon
state owns runtime preferences, TOML/build own trading capability, and adapters
must not duplicate daemon policy.

## Rules for every agent
These apply to any coding agent working in this repo (Claude Code, Codex, or
other), not just one tool.

Use read-only subagents for exploration/review, then keep implementation
writes in the main session unless the user explicitly asks for parallel
implementation. Standing carve-out: once fix batches are reviewed and
independent, dispatch them to fresh-context worktree agents instead of
grinding them through a large main context — base worktrees on local main,
not origin/main.

Trading safety: paper-account broker writes are open to agents; live writes
are hard-blocked daemon-side by origin checks, and the trading freeze switch
(`ibkr settings set trading.freeze=true`) is human-only. Never weaken these
guardrails in code, config, or hooks without an explicit human go.

Public marketing/site pages for `osauer.dev/ibkr` are deployed from this
product repo's GitHub Pages config, currently `main:/docs` with
`html_url=https://osauer.dev/ibkr/`. Before editing or pushing public site copy,
verify the active publisher with `gh api repos/osauer/ibkr/pages` and a live
header check on `https://osauer.dev/ibkr/`; do not infer publisher ownership from
similarly named static folders in `/Users/osauer/dev/osauer.github.io` or
`/Users/osauer/dev/osauer.dev`.

Before daemon/CLI/MCP/trading semantic changes, use
`docs/templates/daemon-cli-trading-contract.md`. Before Canary SPA semantic or
rendered-flow changes, use `docs/templates/spa-authority-matrix.md`.

The Makefile is the canonical target inventory. Run `make help` before using an
unfamiliar project target instead of relying on duplicated target lists.

## Codex workflow
For larger Codex sessions, read `docs/guides/codex-workflow.md` after the
architecture primers.

For IBKR account/order/protection investigations, first load the repo-local
`.agents/skills/ibkr/SKILL.md`, then use read-only `ibkr ... --json` surfaces
before broad code search. Start with status/settings/trading/proposals/orders,
then inspect code only for gaps the artifacts expose.

The repo has a project `.codex` layer with hooks, rules, and custom agents.
When those files change, inspect/trust hooks with `/hooks` in the next Codex
session so broker-adjacent guardrails actually run.

## Session hygiene
Binding defaults for long sessions; rationale and measured numbers in
`docs/guides/agent-session-hygiene.md`.
- Compact or hand off at phase boundaries (explore → implement → verify)
  once context is large; never carry a fat context across a topic pivot or
  a multi-hour pause. Handoff notes restate guardrail state: gateway pins,
  freeze status, committed vs in-flight work.
- Batch independent one-liner probes into one call; read files by range,
  not wholesale; after changing a file via shell, re-read the target hunk
  before using the Edit tool on it.
- Waits are backgrounded until-loops or Monitor conditions, never
  foreground sleeps or repeated polls from a large context.
- `make test` already runs `check` first — run it alone, backgrounded or
  teed to a file, never as a foreground pipe (the 600s tool cap kills the
  run and loses its output).

## Done means
`make check` plus the right smoke tier pass, and the relevant `ibkr` output is pasted in the completion message — that output is the artifact.

Smoke tiers: `make smoke-fast` (~15s: boot + quote + account against a real gateway) is the default per-change gate. The full `make smoke` (chain/regime/gamma/SPX wire matrix) is binding when you touched daemon, CLI, or wire-path code — and always at release. Both tiers serialize against other sessions via `scripts/with-gateway-lock.sh`, so a busy gateway means a short wait, not a 326 flake.

`make check` is the static gate — no gateway needed; it bundles the format/vet/lint/vuln/docs/changelog/account-data checks. The Makefile (`make help`) is the canonical list, not this file. Hermetic test suites use Go's test cache (only edited packages re-run); govulncheck skips when deps are unchanged and already scanned today (`make govuln-prewarm-install` schedules the daily cold scan at 06:00, outside the dev loop). Both run in full on the release path.

After editing daemon/CLI code, refresh the installed daemon with `make restart-daemon` — it skips the bounce when the binary is byte-identical and the daemon is running (FORCE=1 bounces anyway), then run `ibkr status` plus a command exercising your change. Do not use `pkill` for normal restarts; reserve it only for a broken/stuck daemon when `ibkr restart` cannot stop it. `make smoke` uses an isolated daemon — it doesn't refresh the one you run.

## Releases
`make release RELEASE_VERSION=vX.Y.Z`. Fail-fast preconditions: clean tree, HEAD == origin/main, unused tag, `.claude-plugin/plugin.json` version matches, changelog-lint, release-site-check (non-patch). Then it orchestrates refresh-spx-members → test → build → release-smoke (strict: TWS required) → paper smoke (`scripts/release-paper-smoke.sh`, binding: 1-share paper round-trip; no paper login aborts the release) → tag → release-binaries → push → plugin-tag → release-publish → registry-publish. `release-smoke` runs the actual version-stamped binary against a live TWS gateway and checks both JSON contracts and wire-level invariants — a release without TWS is a failed release. Never tag, push, or `gh release create` directly. If `make release` fails, fix the root cause. After it succeeds, verify artifacts landed (`gh release view`, `git ls-remote --tags origin`, registry check) — registry-publish can strand silently.

## Canary app browser preview
When the user asks to open or show the canary/mobile app in the Codex browser side panel, use the Browser plugin/in-app Browser and make it visible. Do not use macOS `open`.

The app is the paired PWA served by `ibkr app`. Keep the shared app host on its default LAN-capable bind (`0.0.0.0:8765`) so a phone can pair through the Mac's LAN URL while the Codex Browser uses `http://127.0.0.1:8765`. For detailed SPA workflow, serving, pairing, and browser-QA rules, see `web/app/AGENTS.md` and `docs/guides/canary-spa-dev.md`.

## MCP tool descriptions are documentation
Adding or changing an entry in `internal/mcp/tools.go`: every `Description` string and every parameter `description` in the JSON schema is what an LLM reads to decide whether to invoke the tool. Hold them to documentation standard, not implementation comment standard:
- **Tells the model when to invoke** — the use case in the user's language ("what you own", "is the regime favorable"), not just "calls handleX RPC".
- **Tells the model when NOT to invoke** — name the overlapping tool a confused LLM might pick instead (e.g. `ibkr_quote` calls out "NOT for options — use `ibkr_chain`").
- **Parameter descriptions explain semantics, not just type** — case-sensitivity, defaults, what good values look like.

After changes run `make docs-regen` to update `docs/reference/mcp-tools.md`; `make check` enforces no drift via `docs-check`.

## Adding or removing IBKR_* env vars
Every read of an `IBKR_*` environment variable must be flagged with a `// docgen:env NAME | description` comment next to the `os.Getenv` call. `scripts/docgen/config-ref` walks the tree for these and emits `docs/reference/config.md`; `make check` fails when the generated file and source disagree. New env var → add the read, add the comment, run `make docs-regen`, commit all three together.
