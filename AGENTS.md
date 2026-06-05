# Project rules

## Done means
`make check && make smoke` pass, and the relevant `ibkr` output is pasted in the completion message — that output is the artifact.

`make check` is static (gofmt, vet, staticcheck, govulncheck, modernize, parity). `make smoke` runs the binary against a live TWS gateway and skips cleanly if no gateway is reachable; if you touched daemon or CLI code paths, ensure TWS is up and watch it bind.

After editing daemon/CLI code, restart the installed daemon through the CLI: `make install && ibkr restart --timeout 15s`, then run `ibkr status` plus a command exercising your change. Do not use `pkill` for normal restarts; reserve it only for a broken/stuck daemon when `ibkr restart` cannot stop it. `make smoke` uses an isolated daemon — it doesn't refresh the one you run.

## Releases
`make release RELEASE_VERSION=vX.Y.Z`. It orchestrates refresh-spx-members → check/test → build → release-smoke (strict: TWS required) → tag → release-binaries → push → plugin-tag → release-publish. `release-smoke` runs the actual version-stamped binary against a live TWS gateway and checks both JSON contracts and wire-level invariants — a release without TWS is a failed release. Never tag, push, or `gh release create` directly. If `make release` fails, fix the root cause.

## Canary app browser preview
When the user asks to open or show the canary/mobile app in the Codex browser side panel, use the Browser plugin/in-app Browser and make it visible. Do not use macOS `open`.

The app is the paired PWA served by `ibkr app`. Keep the shared app host on its default LAN-capable bind (`0.0.0.0:8765`) so a phone can pair through the Mac's LAN URL while the Codex Browser uses `http://127.0.0.1:8765`. The root URL is authenticated, so open a pairing URL rather than navigating straight to `/` in a fresh browser:

```sh
ibkr app pair --public-url http://127.0.0.1:8765 --json
```

Navigate the Codex Browser to the returned `.url`; successful pairing redirects to `http://127.0.0.1:8765/` with title `Canary · IBKR`. If port `8765` is already bound, assume the app host is already running and pair against it. If no app host is running, start/restart it with `ibkr restart --app --timeout 15s` or plain `ibkr app`; if an old local-only host is running, repair it with `ibkr restart --app --addr 0.0.0.0:8765 --timeout 15s`. Do not start the shared host loopback-only unless deliberately testing a local-only preview.

## MCP tool descriptions are documentation
Adding or changing an entry in `internal/mcp/tools.go`: every `Description` string and every parameter `description` in the JSON schema is what an LLM reads to decide whether to invoke the tool. Hold them to documentation standard, not implementation comment standard:
- **Tells the model when to invoke** — the use case in the user's language ("what you own", "is the regime favorable"), not just "calls handleX RPC".
- **Tells the model when NOT to invoke** — name the overlapping tool a confused LLM might pick instead (e.g. `ibkr_quote` calls out "NOT for options — use `ibkr_chain`").
- **Parameter descriptions explain semantics, not just type** — case-sensitivity, defaults, what good values look like.

After changes run `make docs-regen` to update `docs/reference/mcp-tools.md`; `make check` enforces no drift via `docs-check`.

## Adding or removing IBKR_* env vars
Every read of an `IBKR_*` environment variable must be flagged with a `// docgen:env NAME | description` comment next to the `os.Getenv` call. `scripts/docgen/config-ref` walks the tree for these and emits `docs/reference/config.md`; `make check` fails when the generated file and source disagree. New env var → add the read, add the comment, run `make docs-regen`, commit all three together.
