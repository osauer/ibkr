#!/usr/bin/env bash

set -euo pipefail

helper="$(cd "$(dirname "$0")" && pwd)/with-release-tag-checkout.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ibkr-tag-checkout-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

repo="$test_root/repo"
git init --quiet "$repo"
git -C "$repo" config user.name "Release Test"
git -C "$repo" config user.email "release-test@example.invalid"
printf '%s\n' tagged > "$repo/marker"
git -C "$repo" add marker
git -C "$repo" commit --quiet -m tagged
git -C "$repo" tag v1.2.3
tag_commit="$(git -C "$repo" rev-parse HEAD)"
printf '%s\n' current-worktree > "$repo/marker"
git -C "$repo" commit --quiet -am current
printf '%s\n' uncommitted-worktree > "$repo/marker"

result="$(cd "$repo" && "$helper" v1.2.3 sh -c 'printf "%s|%s|%s" "$(cat marker)" "$(git rev-parse HEAD)" "$(git status --porcelain)"')"
case "$result" in
	*"tagged|$tag_commit|") ;;
	*) echo "with-release-tag-checkout test: command did not run from the clean tag: $result" >&2; exit 1 ;;
esac

echo "with-release-tag-checkout test: OK"
