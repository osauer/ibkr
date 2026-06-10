# Agent-origin gating for broker writes

Updated: 2026-06-10 09:12 CEST
Status: draft — pending implementation review

Contract per `docs/templates/daemon-cli-trading-contract.md`.

## Scope

- **Goal:** order placement originating from AI-agent contexts is hard-blocked
  when the trading gate routes live, with no override flag; humans at an
  interactive terminal keep a live path behind explicit confirmation. Paper
  stays fully open to agents so protection flows can be tested end-to-end.
- **User-facing command/tool/API:** `ibkr proposals submit`, `ibkr order
  place|modify`, `ibkr purge … / purge restore --execute`, app HTTP write
  endpoints, MCP `ibkr_order_preview` (token redaction only).
- **Owner layer:** origin *detection* is adapter-owned (CLI process state, app
  pairing); origin *enforcement* is daemon policy in the broker-write
  authorization choke point. Config/build stay the owners of trading
  capability; origin is request metadata, never persisted config.
- **Existing behavior:** RPC envelope carries no caller identity; CLI, MCP
  server, and any same-uid process are indistinguishable on the daemon socket.
  Claude/Codex hook layers gate some Bash verbs client-side only.

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback / unavailable |
|---|---|---|---|---|
| Request origin | invoking adapter at call time | `origin` field on broker-write params (`agent`, `human-tty`, `human-paired-device`) | journaled per order event; surfaced in blockers | missing/unknown → treated as `agent` (fail closed) |
| Origin policy | daemon `brokerWriteAuthorization` | blocker `live_agent_origin_blocked` | `ibkr trading status`, submit/place errors | n/a — no config knob, no override |
| Live human confirmation | CLI prompt on a real TTY | typed `live/<account>` ack, compared verbatim | CLI only | non-TTY cannot confirm → write refused |
| Agent detection (CLI) | process env + stdin | env markers `CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `CODEX_SANDBOX`, `OPENAI_CODEX`, or `IBKR_AGENT_CONTEXT=1`, or `!isatty(stdin)` | n/a | any marker or non-TTY → `agent` |
| App origin | paired-device auth in `internal/app` | app sets `human-paired-device` on daemon calls | SPA | unauthenticated callers never reach writes |

## Mechanism

1. **CLI** resolves origin once at startup into `cli.Env`. `IBKR_AGENT_CONTEXT`
   can only force the *more restrictive* classification (`agent`); there is no
   env/flag that yields `human-tty`. Gets `// docgen:env` comment.
2. **RPC**: `Origin string` added to `OrderPlaceParams`, `OrderModifyParams`,
   `OrderCancelParams` (journaled, not enforced — see exemption),
   `TradeProposalSubmitParams`, `PurgeExecuteParams`,
   `PurgeRestoreExecuteParams`, and platform-settings update params for
   `[trading]` limit keys.
3. **Daemon**: `brokerWriteAuthorization` (single choke point used by place,
   modify, proposal submit, purge execute, restore execute) refuses when the
   gate routes **live** and origin ≠ human: blocker `live_agent_origin_blocked`,
   action text explains agents may operate paper only. Paper: no origin check.
   **Cancel is exempt** (strictly risk-reducing); origin still journaled.
   Trading-limit settings writes (`max_notional`, `max_option_contracts`,
   `allow_stock_short`, `allow_option_sell_to_open`) refuse agent origin when
   mode is live.
4. **MCP**: `ibkr_order_preview` redacts the raw `preview_token` (returns
   `preview_token_id` only), aligning with the proposal surface's
   `sanitizeProposalPreview`. Closes the mint-over-MCP → redeem-over-Bash
   laundering path in both modes; paper agents preview/place via CLI.
5. **Journal**: every write attempt records `origin`, giving the audit trail
   for "who placed this".
6. **Hook layer (client-side, defense in depth, this repo's plugin):**
   `hooks/ibkr-pre-tool-use.sh` already gates write verbs on live
   `trading status --json` paper-readiness; it gains: composition ban narrowed
   to write-verb invocations (read-only `ibkr … --json | jq` stays usable),
   and ships to the installed plugin (cache today, release next).

## Safety invariants (supersedes the template's blanket line)

- Agent-origin requests cannot place, modify, close, submit, or transmit
  broker orders **when the trading gate routes live** — enforced daemon-side,
  no override. (Replaces "Codex cannot place… orders", which paper-mode
  testing intentionally relaxes; `docs/templates/daemon-cli-trading-contract.md`
  is updated in the same change.)
- Paper-mode agent writes remain gated by the full existing stack: build tag,
  `mode=paper`, pinned port/account/client-id, DU-account paper gate at
  `pkg/ibkr`, single-use WhatIf-accepted preview tokens.
- Preview tokens are not submit eligibility; both fields stay separate.
- Origin is honest-by-construction for compliant agents and is an interlock,
  not a security boundary: a same-uid adversary can forge params or edit
  config. Documented in SECURITY.md.
- Missing origin == `agent`. New adapters must opt *in* to a human origin.

## Residual risks (accepted, documented)

- Agent driving the paired PWA (e.g. browser automation) inherits
  `human-paired-device`. Mitigation: pairing approval is a human act on the
  phone; purge/submit confirmations remain in the UI.
- Direct socket callers can claim `human-tty`. Same-uid trust boundary as
  today; the live ack stack (allow_live + acks + paper-smoke evidence) still
  applies on top.

## Before/After artifact

Before: `ibkr trading status --json` (can_write=true, paper), agent-context
`ibkr proposals submit … --json` succeeds on paper.
After: same paper submit still succeeds (agent, paper); a simulated live gate
(unit-level: gate fixture with `route=live`) refuses agent origin with
`live_agent_origin_blocked`; `ibkr trading status --json` unchanged.
Live-gateway artifact impossible on this machine (no live session configured);
covered by unit tests on the authorization function.

## Verification

- Unit: authorization matrix (origin × mode × verb incl. cancel exemption),
  CLI origin detection (env/TTY), MCP redaction, settings gating.
- `make check` (incl. docs-regen for the env var + MCP description), and a
  `-tags trading` test leg so the enforced path actually compiles under CI.
- `make smoke` vs paper TWS; paper E2E proposals submit as agent (must pass).

## Rollback

- Revert: rpc params fields, daemon authorization branch, CLI env detection,
  MCP redaction, hook script. No runtime state: origin is per-request metadata;
  journal entries with origin are additive. User-facing change reverts to
  "agents indistinguishable from humans" (pre-change behavior).
