# Daemon/CLI Trading Contract Template

Updated: 2026-06-06 06:35 CEST

Use this before changing daemon, RPC, CLI, MCP, trading capability, order
preview/status, purge, account, position, canary, regime, gamma, breadth, or
market-data semantics.

## Scope

- Goal:
- User-facing command/tool/API:
- Owner layer: daemon state / RPC contract / CLI renderer / MCP description /
  app snapshot / docs:
- Existing behavior and artifact:

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback or unavailable state |
|---|---|---|---|---|
|  |  |  |  |  |

## Safety Invariants

- Codex cannot place, modify, cancel, close, submit, or transmit broker orders.
- Preview tokens are not submit eligibility. Report both fields separately.
- Trading capability and live acknowledgements remain operator-owned config/build
  state, not inferred daemon policy.
- Nil means unavailable, not zero, for IV, Greeks, FX, open interest, and money
  fields.
- `data_type`, `as_of`, stale flags, warning details, and source health are
  preserved all the way to user-facing output.
- MCP descriptions explain when to invoke and when not to invoke overlapping
  tools.

## Before/After Artifact

Before changing behavior, capture the narrow command that proves the current
contract:

```sh
ibkr status --json
ibkr <command that exercises the behavior> --json
```

After implementation, capture the same or stricter artifact:

```sh
make install
ibkr restart --timeout 15s
ibkr status --json
ibkr <command that exercises the behavior> --json
```

Paste the relevant `ibkr` output in the completion message. If live gateway
state is unavailable, say exactly which artifact could not be produced.

## Verification

- Narrow unit/package test:
- Generated docs needed: `make docs-regen` yes/no:
- Static gate: `make check`
- Live gate: `make smoke`
- Smoke result: pass / fail / skip:
- If smoke skipped, exact reason and residual risk:

## Rollback Notes

- Files to revert:
- Runtime state touched:
- User-facing behavior that changes:
