# Security policy

## Reporting a vulnerability

Please report security issues privately — not via public GitHub issues.

1. **Preferred:** open a draft advisory via GitHub Private Vulnerability Reporting at <https://github.com/osauer/ibkr/security/advisories/new>.
2. **Email:** `oliver.sauer@gmail.com`, subject `[ibkr] security`. Plain English is fine; a proof-of-concept is appreciated but not required.

## Response

- Acknowledged within 7 days.
- Investigated and triaged within 30 days for most reports.
- Verified issues get a patched release before the advisory is made public. You will be credited in the advisory and in `CHANGELOG.md` under `### Security`, unless you prefer to stay anonymous.

This is a personal open-source project, not a funded program — responses are best-effort, but reports are taken seriously.

## Supported versions

While `ibkr` is on the `0.x` line, only the latest minor version receives security fixes. A long-term-support backport policy will be defined at the `1.0` release.

## Scope

**In scope** — the daemon, CLI, stdio MCP server, Claude Code plugin, the `pkg/ibkr` wire-protocol implementation, the install script, and the published release artifacts in this repository.

**Out of scope** — vulnerabilities in Interactive Brokers' TWS / IB Gateway software (please report those directly to IBKR), vulnerabilities in upstream Go modules (please notify the upstream maintainer; this project will re-release after the fix lands), and denial-of-service against the local daemon by a user who already has shell access on the same machine (the daemon is designed for single-user local use).

## Threat model

`ibkr` is structurally read-only — four independent layers refuse `order`, `trade`, and `cancel` verbs (see [README §Safety](README.md#safety)). The daemon listens only on a Unix-domain socket in the user's runtime directory, never on a TCP port. It speaks to a locally-running IB Gateway or TWS over loopback. No market data, credentials, or account state leave the local machine via this code.

Reports that demonstrate a deviation from any of those properties — a successful `order` / `trade` / `cancel` reaching the gateway, a daemon listener on a non-loopback or non-Unix socket, or data egress beyond the local IB Gateway — are priority.
