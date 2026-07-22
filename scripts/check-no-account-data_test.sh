#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ibkr-account-data-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

repo="$test_root/repo"
mkdir -p "$repo/scripts"
cp "$repo_root/scripts/check-no-account-data.sh" "$repo/scripts/"
git init --quiet "$repo"
git -C "$repo" config user.name "Account Data Test"
git -C "$repo" config user.email "account-data-test@example.invalid"
printf '%s\n' 'safe fixture DU1234567' > "$repo/fixture.txt"
git -C "$repo" add .
(cd "$repo" && ./scripts/check-no-account-data.sh >/dev/null)

probe_lower="du987""6543"
printf '%s\n' "$probe_lower" > "$repo/fixture.txt"
git -C "$repo" add fixture.txt
if output="$(cd "$repo" && ./scripts/check-no-account-data.sh 2>&1)"; then
	echo "check-no-account-data test: lowercase non-placeholder ID was accepted" >&2
	exit 1
fi
if printf '%s\n' "$output" | grep -Fqi "$probe_lower"; then
	echo "check-no-account-data test: failure output disclosed the matched ID" >&2
	exit 1
fi

probe_upper="$(printf '%s' "$probe_lower" | tr '[:lower:]' '[:upper:]')"
printf 'fixture\000%s\000data\n' "$probe_upper" > "$repo/fixture.bin"
git -C "$repo" add fixture.bin
git -C "$repo" checkout -- fixture.txt
if (cd "$repo" && ./scripts/check-no-account-data.sh >/dev/null 2>&1); then
	echo "check-no-account-data test: binary non-placeholder ID was accepted" >&2
	exit 1
fi

echo "check-no-account-data test: OK"
