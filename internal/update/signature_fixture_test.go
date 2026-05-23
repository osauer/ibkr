package update

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// TestEmbeddedKeyAcceptsRealGPGSignature is the always-on
// cross-implementation gate. It verifies that a SHA256SUMS.asc produced
// by the maintainer's REAL gpg invocation (the same command
// `make release-binaries` runs) is accepted by the production
// VerifySignature path against the production embedded key — no test-key
// swap. Catches RFC-interpretation drift between gpg and ProtonMail
// go-crypto that the in-memory keypair tests cannot.
//
// Skipped (not failed) only when testdata/sample-sha256sums.asc is
// missing — the documented setup state before the maintainer signs the
// fixture for the first time, or transiently after a key rotation that
// hasn't re-signed yet. Once the .asc lands in the repo, this test
// always runs and never skips in CI / clean clones.
//
// Regeneration after key rotation:
//
//	./scripts/sign-testdata.sh
//
// See internal/update/testdata/README.md for the lockstep policy.
func TestEmbeddedKeyAcceptsRealGPGSignature(t *testing.T) {
	const (
		sumsPath = "testdata/sample-sha256sums"
		sigPath  = "testdata/sample-sha256sums.asc"
	)

	if _, err := os.Stat(sumsPath); err != nil {
		t.Fatalf("testdata/sample-sha256sums missing — fixture should be checked in: %v", err)
	}
	if _, err := os.Stat(sigPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Skip("testdata/sample-sha256sums.asc not present — run ./scripts/sign-testdata.sh once with the maintainer's key, then commit the result. See internal/update/testdata/README.md.")
		}
		t.Fatalf("stat testdata/sample-sha256sums.asc: %v", err)
	}

	// No useTestKey: this exercises the production embedded key and
	// the production fingerprint constant. That's the whole point.
	if err := VerifySignature(sumsPath, sigPath); err != nil {
		t.Fatalf("production VerifySignature rejected real-gpg-signed fixture: %v\n"+
			"This is the cross-implementation gate failing — either:\n"+
			"  (1) the embedded release-signing-key.asc disagrees with the key the .asc was signed with (rotation drift), or\n"+
			"  (2) gpg and ProtonMail go-crypto disagree on signature format (re-investigate dep versions).\n"+
			"Regenerate the fixture with scripts/sign-testdata.sh after confirming the embedded key is the intended one.", err)
	}
}
