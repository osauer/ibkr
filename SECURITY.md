# Security policy

## Reporting a vulnerability

Please report security issues privately — not via public GitHub issues.

Open a draft advisory via GitHub Private Vulnerability Reporting at
<https://github.com/osauer/ibkr/security/advisories/new>. Plain English is
fine; a proof-of-concept is appreciated but not required. GitHub will email
the maintainer privately and let you correspond in the advisory thread
without exposing details until the fix ships.

No GitHub account? Open a regular issue titled `security: request private
channel` (no details), and the maintainer will reply with a one-time
reporting address.

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

Reports that demonstrate a deviation from any of those properties — a successful `order` / `trade` / `cancel` reaching the gateway, a daemon listener on a non-loopback or non-Unix socket, or data egress beyond the local IB Gateway — take priority.

## Release integrity (v1.0.0+)

Every GitHub release from v1.0.0 onward ships **three artefacts per platform** that together form the trust chain:

1. `ibkr-vX.Y.Z-<os>-<arch>.tar.gz` — the binary tarball.
2. `SHA256SUMS` — one line per tarball with its SHA-256.
3. `SHA256SUMS.asc` — a PGP detached signature over `SHA256SUMS`, produced by the maintainer's release-signing key.

`ibkr update` (from v1.0.0 onward) **refuses** any release that does not publish `SHA256SUMS.asc`, and refuses any release whose `SHA256SUMS.asc` does not verify against the public key embedded in the running binary. There is no fallback path. A release whose signature cannot be checked is treated as a compromised release.

### The maintainer's release-signing key

| | |
|---|---|
| Owner | Oliver Sauer (`oliver.sauer@gmail.com`) |
| Algorithm | Ed25519 |
| Fingerprint | `D984 26D4 8FED 85EF A339  0469 4D92 2A4F 922B 7D7D` |
| Embedded in | every `ibkr` binary from v1.0.0 onward, at `internal/update/release-signing-key.asc` |
| Also published at | `https://github.com/osauer.gpg` (served by GitHub) |

### Verifying a release by hand

If you want to verify a downloaded tarball without trusting `ibkr update`:

```sh
# 1. Get the maintainer's key (one of two equivalent paths).
curl -fsSL https://github.com/osauer.gpg | gpg --import
# or, from a cloned repo at a trusted commit:
gpg --import internal/update/release-signing-key.asc

# 2. Confirm the fingerprint matches the line in this file.
gpg --fingerprint oliver.sauer@gmail.com

# 3. Download the three release artefacts for your platform.
VERSION=v1.0.0
PLAT=darwin-arm64
BASE=https://github.com/osauer/ibkr/releases/download/$VERSION
curl -fLO $BASE/ibkr-$VERSION-$PLAT.tar.gz
curl -fLO $BASE/SHA256SUMS
curl -fLO $BASE/SHA256SUMS.asc

# 4. Verify the signature over SHA256SUMS, then the tarball SHA.
gpg --verify SHA256SUMS.asc SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing
```

Both lines must end in `Good signature` and `OK` respectively. Either failing means the release is corrupted or tampered.

### What this does and doesn't defend against

**Defends against**: a GitHub account compromise that swaps both the tarball and `SHA256SUMS` — the attacker doesn't have the maintainer's private key, so the produced `SHA256SUMS.asc` won't verify. Also defends against MITM scenarios past github.com's TLS.

**Does not defend against**: theft of the maintainer's private key (handled via revocation — see below) and supply-chain attacks on the Go module graph at build time (separate from release integrity; tracked by `govulncheck` in `make check`).

### Key rotation and revocation

The signing key is long-lived (no expiration date) because it is embedded in every shipped binary. Rotation requires shipping a new ibkr binary with the new public key embedded. A revocation certificate is held offline by the maintainer; if used, it will be published to keyservers and announced in `SECURITY.md`.

## Diagnostic data sensitivity

Two opt-in environment variables write the raw IBKR wire protocol to disk. Both are off by default and only active when explicitly set; neither is on the autospawn path. Captured frames include account IDs, contract identifiers (symbol / conid / strike / expiry), order references, P&L numbers, and execution details — anything the gateway sends. Treat the resulting files as account-sensitive.

| Variable | Effect |
|---|---|
| `IBKR_WIRE_INTERCEPTOR=1` | Activates the in-process wire recorder. Frames are mirrored into a per-process ring buffer (in-memory). |
| `IBKR_WIRE_LOG_PATH=/path/to/wire.jsonl` | When set with `IBKR_WIRE_INTERCEPTOR=1`, every frame is also appended to this file as one JSON object per line. The file is created with mode `0644` — restrict the directory if other UIDs share the host. |
| `IBKR_WIRE_RING_SIZE=N` | Bound on the in-memory ring (default 256 frames). |
| `IBKR_PACKET_LOG_TEMPLATE=/path/to/packets.bin` | Independent low-level packet logger writing raw bytes (length-prefixed). Same sensitivity as the wire log; lower-level shape. |

If you share a wire log for debugging, redact the `Symbol`, `ReqID`, and `Fields` columns where they carry account-level data. The README's Troubleshooting section has the user-facing pointer; this entry is the security-side warning.
