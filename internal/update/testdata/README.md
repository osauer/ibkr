# `internal/update/testdata/`

Fixtures for the always-on signature-verification test
(`TestEmbeddedKeyAcceptsRealGPGSignature` in `signature_fixture_test.go`).

## Files

| File | Source | Regenerate when |
|------|--------|-----------------|
| `sample-sha256sums` | Hand-written; obviously-fake hashes (all zeroes + 1/2/3/4) so it can't be confused with real release data. | Never. The bytes are fixed; rotating the signing key does NOT change this file. |
| `sample-sha256sums.asc` | `gpg --detach-sign --armor` over `sample-sha256sums`, using the maintainer's current release-signing key. | Only when `ReleaseSigningKeyFingerprint` (and `release-signing-key.asc`) change. The signature is over the bytes above, so it stays valid forever otherwise. |

## Why this exists

The in-package tests (`keyring_test.go` etc.) cover the verification path with a throwaway PGP keypair generated inline in Go. Those tests prove the verifier *logic* is correct, but they prove it against signatures produced by the *same library* doing the verification — ProtonMail go-crypto's `ArmoredDetachSign` consumed by ProtonMail go-crypto's `CheckArmoredDetachedSignature`.

What they don't catch: an RFC-interpretation drift between **gpg** (which the release pipeline uses) and **ProtonMail go-crypto** (which the verifier uses). If a future Go-crypto release changes how it parses some armored signature edge case, or if gpg starts emitting a variant we don't decode, the in-process tests stay green and a real `make release` ships an unverifiable signature.

`signature_fixture_test.go` closes that loop by loading the **real-gpg-produced** `.asc` file checked in here and verifying it against the **production embedded key** (no test-key swap). Runs on every `go test` and `make check`.

## Regeneration

When the maintainer's signing key rotates:

1. Update `internal/update/release-signing-key.asc` with the new public key.
2. Update `ReleaseSigningKeyFingerprint` in `internal/update/keyring.go`.
3. Re-sign the fixture:
   ```sh
   ./scripts/sign-testdata.sh
   ```
4. Commit `release-signing-key.asc`, `keyring.go`, `testdata/sample-sha256sums.asc` together so the embedded key + the constant + the fixture stay in lockstep.

`TestEmbeddedKeyMatchesFingerprint` catches (1) vs (2) drift; `TestEmbeddedKeyAcceptsRealGPGSignature` catches (1)/(2) vs (3) drift.
