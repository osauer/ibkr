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
implementation.

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

The repo has a project `.codex` layer with hooks, rules, and custom agents.
When those files change, inspect/trust hooks with `/hooks` in the next Codex
session so broker-adjacent guardrails actually run.

## Done means
`make check && make smoke` pass, and the relevant `ibkr` output is pasted in the completion message — that output is the artifact.

`make check` is static (gofmt, vet, staticcheck, govulncheck, modernize, parity). `make smoke` runs the binary against a live TWS gateway and skips cleanly if no gateway is reachable; if you touched daemon or CLI code paths, ensure TWS is up and watch it bind.

After editing daemon/CLI code, restart the installed daemon through the CLI: `make install && ibkr restart --timeout 15s`, then run `ibkr status` plus a command exercising your change. Do not use `pkill` for normal restarts; reserve it only for a broken/stuck daemon when `ibkr restart` cannot stop it. `make smoke` uses an isolated daemon — it doesn't refresh the one you run.

## Releases
`make release RELEASE_VERSION=vX.Y.Z`. It orchestrates refresh-spx-members → check/test → build → release-smoke (strict: TWS required) → tag → release-binaries → push → plugin-tag → release-publish. `release-smoke` runs the actual version-stamped binary against a live TWS gateway and checks both JSON contracts and wire-level invariants — a release without TWS is a failed release. Never tag, push, or `gh release create` directly. If `make release` fails, fix the root cause.

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
