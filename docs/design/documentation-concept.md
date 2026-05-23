# Documentation concept

**Status:** Design — revised after second pass surfaced the agentic audience and the intelligently-generated reference preference. Awaiting green-light to write Phase 1.
**Created:** 2026-05-23 15:55 CEST
**Last update:** 2026-05-23 16:20 CEST
**Owner:** osauer
**Related:** [README.md](../../README.md), [docs/design/ibkr-update-and-members-refresh.md](./ibkr-update-and-members-refresh.md) (the update changes whose docs are folded in here), [Diátaxis framework](https://diataxis.fr) (the OSS pattern this draws from selectively).

## Why now

Two pressures landed at once:

1. **The v0.33.0 / v1.0.0 changes need docs that don't exist yet.** `ibkr update` is a new user-facing surface; the runtime members refresh has env-var and TOML knobs (`IBKR_SPX_MEMBERS_AUTO_REFRESH`, `[spx] members_auto_refresh`) with subtle semantics. Burying them in `--help` text alone isn't enough; readers need to understand "why does this knob exist" not just "what does it do."

2. **The overall tool has outgrown a README-only docs surface.** 328 lines of README is workable but bloated for a front door, and there's no home for concept prose (what *is* dealer zero-gamma? why is breadth computed this way?). New users — and the future Claude Code plugin / SPA / MCP consumer audiences — need more than the README can carry without becoming a manual.

## Audiences

Four distinct consumers, each reading different surfaces:

| Audience | Primary surface | Secondary | What they need |
|---|---|---|---|
| Maintainer | `CLAUDE.md`, `docs/design/`, `docs/specs/` | source, commit history | Project rules, design rationale, why-this-way |
| Human CLI user | `README.md`, `<command> --help` | concept docs, guides | Install, what to run, what the output means |
| Claude-Code-plugin user (human) | `README.md`, `docs/guides/agentic-use.md` | concept docs | Setup, example prompts, what to ask Claude |
| **Agent itself** | `internal/mcp/tools.go` descriptions, JSON schemas | (none — agents don't browse docs) | Tool semantics rich enough to pick correctly |

The agent audience is load-bearing and was under-weighted in the first pass. See **The agentic audience** section below.

## What "lean and elegant" means here

Four operating principles, in priority order:

1. **Don't write docs nobody reads.** Every page must answer a question a real reader is asking — not a question we think they should be asking.
2. **`<command> --help` is the authoritative reference for the CLI surface.** Hand-written command tables drift; tool-generated reference doesn't. Prose docs explain *why*; `--help` covers *what*.
3. **Tool descriptions in `internal/mcp/tools.go` ARE documentation** — they're what Claude reads to decide whether to invoke a tool. A poorly-described tool means Claude doesn't pick it. The prose-vs-`tools.go`-string boundary is artificial; both are user-facing surfaces and both get maintained with the same care.
4. **Concept docs are short and prose-driven.** The domain (gamma, breadth, regime) has real depth, but a reader doesn't need a textbook — they need enough mental model to use the tool correctly. 200–400 words per concept page.

## The agentic audience

ibkr is already shipped as a Claude Code plugin with an MCP server (`internal/mcp/`). The default deployment story for non-maintainer users is "install the plugin, ask Claude things." This audience has been under-served by docs that assume CLI use.

### How agentic consumption differs

When a user asks Claude *"is the market regime favorable right now?"*, the chain is:

1. Claude reads its MCP tool registry (descriptions + JSON schemas, NOT markdown docs).
2. Claude picks a tool based on description semantics. `ibkr_regime` wins this round only if its description tells Claude what regime means and when it's the right tool.
3. Claude invokes the tool, gets JSON back.
4. Claude composes a natural-language answer for the human.
5. The human, reading Claude's answer, needs to understand "DISAGREEMENT regime" or "dealer long-γ" enough to act on it — and **that** is where prose docs serve them.

So:

- **Tool descriptions in `tools.go` directly determine whether Claude finds the right tool**. Drift here is silent and high-cost: a vague description means Claude punts ("I can't find a tool for that") or picks the wrong one. An audit pass on `internal/mcp/tools.go` is a prerequisite for taking the agentic audience seriously.
- **JSON schemas need rich `description` fields on every parameter**, not just types. `{"type": "string"}` is useless to an LLM; `{"type": "string", "description": "ticker symbol, e.g. SPY. Equity symbols only — indices like SPX are passed via the scope flag instead."}` tells Claude when to use the field.
- **Markdown concept docs serve the human watching Claude**, not Claude itself. They become indirect Claude inputs via the human's mental model.
- **`README.md` needs to address the plugin user explicitly** — not just "here's how the CLI works." A "What can your agent do with this" framing in the first screen.

### Implications for the docs structure

Adds two surfaces that didn't exist in the first pass:

- **`docs/guides/agentic-use.md`** — example prompts and conversation patterns. Replaces the previously-proposed `mcp-integration.md`; sharper scope. Shows actual exchanges: *"What's the current regime?"*, *"Show me my SPY positions and how they're hedged against tomorrow's open."* Includes a few "Claude won't be able to" notes for explicit non-features.
- **`docs/reference/mcp-tools.md`** — every MCP tool with its description and arguments. **Generated** from the registry in `internal/mcp/`, not hand-maintained — the tool list will outgrow human curation immediately. Mirrors `kubectl explain` or Terraform's provider docs pattern.

Adds one quality-gate task that isn't a doc page:

- **`tools.go` description audit** — pass over every tool's description string and every parameter's JSON-schema description. Check that an LLM reading just these can decide whether to invoke. This is documentation work even though it lives in `.go` files.

## What we pull from the OSS market

Surveyed: Diátaxis (the framework), Litestream (small CLI, lean docs), Caddy (well-regarded scaffolded depth), `gh` GitHub CLI (man-page-style command reference). Patterns worth pulling:

- **Diátaxis's four-quadrant model** ([diataxis.fr/compass](https://diataxis.fr/compass/)): tutorial / how-to guide / reference / explanation, distinguished by *action vs cognition* × *acquisition vs application*. We use it **selectively**: skip the tutorial quadrant entirely (the README's "Install in two commands" IS the tutorial), and keep the other three. Mixing categories — the most common Diátaxis pitfall — is what causes the README to feel "too much."
- **Litestream's lean landing page**: problem-solution → benefits → call-to-action, with deep detail deferred to linked pages. Our README should shrink toward this shape — the first screen says what the tool is and gets the user installing; everything else lives one click away.
- **Caddy's scaffolded depth**: Getting Started → Tutorials → Reference progression, with explicit guidance ("we suggest *everyone* go through Getting Started"). Maps cleanly to our reader profile: traders who know what they want, devs who want to integrate, plugin users who want the headline features.
- **`gh`'s tool-generated reference**: `gh help <command>` is the source of truth for the command surface; the docs site links out for prose. Our `--help` already does this; the docs concept formalizes it as the authoritative reference layer.

## What stays, what moves, what's new

Current state:

```
README.md          ← 328 lines, doing the work of: install + tour + reference + config + troubleshoot
CHANGELOG.md       ← tiered, well-defined, keep as-is
SECURITY.md        ← 52 lines, keep as-is
CLAUDE.md          ← agent rules, internal — keep as-is
docs/design/       ← engineering decisions, internal — keep as-is
docs/specs/        ← wire contracts and roadmaps, internal — keep as-is
docs/research/     ← notes — keep as-is
```

Proposed target state:

```
README.md          ← shrinks to ~200 lines: install + 90-second tour + plugin/agent framing + links out
CHANGELOG.md       ← unchanged
SECURITY.md        ← unchanged
CLAUDE.md          ← unchanged
docs/
├── concepts.md    ← NEW. Single page, three sections (regime / gamma / breadth). Split later if any one section outgrows scrolling.
├── guides/        ← NEW. Task-oriented (Diátaxis: how-to)
│   ├── updating.md          # `ibkr update`, members auto-refresh, env vars, pinning
│   ├── agentic-use.md       # what to ask Claude, example conversations, when Claude can't help
│   ├── tws-setup.md         # the painful onboarding step, deserves its own page
│   └── troubleshooting.md   # symptoms → diagnosis → fix
├── reference/     ← NEW. Just the facts (Diátaxis: reference). GENERATED.
│   ├── config.md            # all [*] TOML sections + IBKR_* env vars + defaults
│   ├── mcp-tools.md         # every MCP tool with description + arguments
│   └── exit-codes.md        # subcommand exit codes (Phase 3, post-v1.0.0)
├── design/        ← unchanged: engineering decisions
├── specs/         ← unchanged: wire contracts
└── research/      ← unchanged: notes
scripts/
└── docgen/        ← NEW. Tiny Go programs that emit docs/reference/*.md from source. Run via `make docs-regen`; gated by `make docs-check` in CI.
```

8 new pages + 1 generator dir. Each page scoped to one concern; no page does double duty. Concept docs consolidated into a single scroll for v1.0.0; split per-concept later if earned.

## Per-page scope and what each page is NOT

The "is not" line is load-bearing — without it, pages drift.

### concepts.md (~800 words total, three sections)
**Is**: single-scroll page with three sub-sections — Regime (the 5-indicator dashboard, what each indicator signals, how the classifier combines them), Gamma (dealer zero-gamma methodology, signs, the regime-agreement classifier for SPY+SPX), Breadth (% above 50/200DMA, constituent-fanout approach, why the daemon embeds + runtime-refreshes the membership list).
**Is not**: a trading guide, a textbook, or a defense of any indicator's predictive value. Links to `docs/design/` for engineering rationale; links to external references for options theory. Splits into per-concept pages only if a section grows past ~400 words AND people are scroll-fatiguing on it.

### guides/updating.md (~300 words)
**Is**: how to update the binary (`ibkr update` plus the manual tarball path), how the members auto-refresh works at runtime, when and why to pin (`IBKR_SPX_MEMBERS_AUTO_REFRESH=0` or `[spx] members_auto_refresh = false`), the headless flag matrix, rollback via `.bak`.
**Is not**: the design rationale (that's in `docs/design/`); per-flag exit codes (those live in `--help`).

### guides/agentic-use.md (~400 words)
**Is**: example prompts and conversation patterns for using ibkr as a Claude-Code MCP server. Real exchanges: *"What's the current regime?"*, *"Show me my SPY positions and how they're hedged."*, *"Is dealer gamma supporting or amplifying today?"* Includes 2–3 "Claude won't be able to" notes for explicit non-features (e.g. trade execution, options chains for non-equity underlyings).
**Is not**: MCP-protocol explanation; a Claude-Code-extension tutorial; an LLM-prompt-engineering guide.

### guides/tws-setup.md (~400 words)
**Is**: IB Gateway / TWS configuration steps, port number conventions, the live-vs-paper account distinction, OPRA entitlement check for SPX. The painful onboarding step.
**Is not**: an IBKR account-opening guide; a TWS feature tour.

### guides/troubleshooting.md (~400 words)
**Is**: symptom → diagnosis → fix table. Common failure modes: gateway handshake hangs, missing entitlements (354 errors), stale cache files, off-hours quote behavior, members refresh silent rot (parse_failed surfacing).
**Is not**: a runbook for the maintainer; a postmortem template. User-facing only.

### reference/config.md — GENERATED
**Is**: every `[*]` TOML section field + every `IBKR_*` env var, with description + default + override precedence. Emitted by `scripts/docgen/config-ref/`. Source of truth: Go struct tags in `internal/config/config.go` (for TOML) + a `// docgen:env IBKR_FOO_BAR | description` comment convention (for env vars). `make docs-check` fails CI when source and generated docs disagree.
**Is not**: prose on when to use each; that lives in the relevant guide or concept page.

### reference/mcp-tools.md — GENERATED
**Is**: every MCP tool with name, description, argument schema. Emitted by `scripts/docgen/mcp-tools/`. Source of truth: the tool registry in `internal/mcp/tools.go`.
**Is not**: prompt examples (those live in `guides/agentic-use.md`); CLI command reference (`--help` covers that).

### reference/exit-codes.md — Phase 3
**Is**: a small table of subcommand exit codes (especially `ibkr update --check` which is load-bearing for scripting). Generated via a `// docgen:exit N | description` comment convention at each `os.Exit` callsite.
**Is not**: a manpage rewrite; per-error-class enumeration. Deferred to post-v1.0.0 because the convention needs to be established for env vars first.

## Where the update changes land

The v0.33.0 / v1.0.0 update work needs four doc touchpoints:

1. **`docs/guides/updating.md`** (new) — primary surface. Covers both the `ibkr update` CLI and the members auto-refresh, with the env-var / TOML knobs and pinning rationale.
2. **`docs/reference/config.md`** (generated) — picks up `[spx] members_auto_refresh`, `IBKR_SPX_MEMBERS_AUTO_REFRESH`, `IBKR_INSTALL_DIR` automatically from struct tags + `// docgen:env` comments. Adding the docgen comments is part of the update implementation, not a separate doc task.
3. **`docs/reference/mcp-tools.md`** (generated) — confirms `ibkr_update` isn't on the MCP exclude list by mistake (it shouldn't be exposed as an MCP tool — it's a binary-management command, not a daemon RPC). The generator surfaces this clearly.
4. **`README.md`** (modify) — under "Other install paths," add one line: "Already on a tagged release? `ibkr update` checks GitHub for the next stable and installs it." Links to `docs/guides/updating.md`.

No standalone update page in the README — keeping the README's job as "get the user installed" intact.

## README shrink plan (Phase 2, light)

Currently 328 lines across 12 sections. Target ~200 lines across 7 sections (less aggressive than the first pass's 150):

| Section | Today | Phase 2 action |
|---|---|---|
| Install in two commands | keep | keep |
| What you get | keep | keep (trim by 20%); add a sentence framing the Claude/MCP path explicitly |
| Pick your path | keep | keep (trim by 30%); add an "Ask Claude" path alongside the CLI paths |
| Architecture | keep | trim to one paragraph; link to `docs/concepts.md` for the indicator details |
| Protocol coverage | keep | trim to one paragraph; link to `docs/reference/mcp-tools.md` |
| Configure | keep | trim to one paragraph; link to `docs/reference/config.md` |
| Safety | keep | keep |
| Other install paths | keep | keep (gains the `ibkr update` line) |
| Testing | keep | leave for now; revisit when a CONTRIBUTING.md is earned |
| Troubleshooting | keep | trim to one paragraph; link to `docs/guides/troubleshooting.md` |
| Disclaimer & trademarks | keep | keep |
| License | keep | keep |

The shrunk README answers three questions in its first screen: *what is this*, *how do I install it (CLI **or** Claude plugin)*, *what can I do with it*. Detail is one click away.

## Implementation order

Three phases, each shippable on its own.

**Phase 1 — Required for v1.0.0** (half a day of writing + ~150 LOC of generator):
- `docs/guides/updating.md` — required for the v1.0.0 changes to have a home
- `scripts/docgen/config-ref/` generator + `docs/reference/config.md` it emits — required for the new env var / TOML knob to be discoverable
- `scripts/docgen/mcp-tools/` generator + `docs/reference/mcp-tools.md` it emits — the agentic-audience reference layer
- `make docs-regen` and `make docs-check` Makefile targets; `docs-check` wired into `make check` so generated docs can't drift
- `internal/mcp/tools.go` audit pass: every tool description + every parameter description gets an LLM-readability review
- `README.md` one-line update under "Other install paths" + agentic-framing tweak in the first screen

**Phase 2 — Minimal, also alongside v1.0.0** (a day):
- `docs/guides/agentic-use.md` — load-bearing for the audience Phase 1's docgen exposes
- `docs/concepts.md` (single page, three sections — not yet split per-concept)
- `docs/guides/tws-setup.md`
- `docs/guides/troubleshooting.md`
- README light shrink to ~200 lines per the table above

**Phase 3 — Post-v1.0.0**:
- `docs/reference/exit-codes.md` + generator + `// docgen:exit` comment convention
- Splitting `concepts.md` into per-concept pages if/when any one section earns it
- `CONTRIBUTING.md` if/when there are non-maintainer contributors

## Out of scope

- **Auto-generated CLI command reference.** Worth doing eventually (point a `gh`-style tool at `flag.FlagSet` definitions to emit `docs/reference/commands.md`), but `<command> --help` is the canonical reference for v1.0.0 and the README's "Pick your path" section covers discoverability. Revisit when the CLI surface outgrows what `--help` browsing can cover.
- **Hosted documentation site.** GitHub renders Markdown well enough; no need for Hugo / Docusaurus / MkDocs for ~8 pages.
- **Versioned docs.** The tool is at v0.x; one version of the docs (latest) is fine. Revisit if we ever need to maintain back-compat docs across multiple supported releases.
- **Tutorials.** The README's "Install in two commands" IS the tutorial. A separate handholding walkthrough would duplicate. Diátaxis allows skipping a quadrant when it doesn't earn its keep.
- **CONTRIBUTING.md / DEVELOPERS.md.** Single-dev project today; CLAUDE.md covers maintainer needs. Add when there are non-maintainer contributors.
- **Per-concept page split**. `concepts.md` is single-scroll for v1.0.0. Split only if a section grows past ~400 words AND people complain about scroll-fatigue. Premature splitting fragments the mental model.
- **A glossary or index.** Concept docs cross-link inline (`[regime classifier](concepts.md#regime)`); a separate glossary duplicates without adding signal.

## Open decisions

- **Phase 1 vs Phase 2 sequencing** — RESOLVED: ship Phase 1 AND Phase 2 (minimal) alongside v1.0.0. Phase 3 deferred.
- **Troubleshooting page** — RESOLVED: standalone `docs/guides/troubleshooting.md` in Phase 2.
- **`docs/reference/` codegen** — RESOLVED: intelligently generated via `scripts/docgen/*` from struct tags + a `// docgen:env` / `// docgen:exit` comment convention. The convention is the discipline; the generator is the gate (`make docs-check` fails CI when source and generated docs disagree).
- **`tools.go` audit cadence** — open: do tool descriptions get reviewed every release, every doc-touching PR, or only when behaviour changes? Recommendation: gate `tools.go` changes via an MCP-description-quality checklist in CLAUDE.md ("description tells Claude when AND when NOT to use this tool"), revisited if Claude regression-picks the wrong tool in practice.
- **`agentic-use.md` example sourcing** — open: hand-write the example conversations, or pull from real Claude transcripts (with PII scrubbing)? Recommendation: hand-write for v1.0.0 (cleaner, no PII risk); harvest real transcripts in a future pass when there's a user base.
