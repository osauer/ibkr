#!/usr/bin/env bash
# Run a release-assembly command from an isolated checkout of the exact tag.

set -euo pipefail

version="${1:?usage: with-release-tag-checkout.sh <version> <command> [args...]}"
shift
if [ "$#" -eq 0 ]; then
	echo "with-release-tag-checkout: command is required" >&2
	exit 2
fi
case "$version" in
	v[0-9]*.[0-9]*.[0-9]*) ;;
	*)
		echo "with-release-tag-checkout: version must look like vX.Y.Z (got $version)" >&2
		exit 2
		;;
esac

repo_root="$(git rev-parse --show-toplevel)"
tag_commit="$(git -C "$repo_root" rev-parse --verify "refs/tags/$version^{commit}")"
checkout_root="$(mktemp -d "${TMPDIR:-/tmp}/ibkr-release-checkout.XXXXXX")"
cleanup() {
	rm -rf "$checkout_root"
}
trap cleanup EXIT HUP INT TERM

git clone --quiet --no-checkout "$repo_root" "$checkout_root/source"
git -C "$checkout_root/source" checkout --quiet --detach "$tag_commit"

actual_commit="$(git -C "$checkout_root/source" rev-parse HEAD)"
if [ "$actual_commit" != "$tag_commit" ] || [ -n "$(git -C "$checkout_root/source" status --porcelain)" ]; then
	echo "with-release-tag-checkout: isolated checkout is not the clean requested tag" >&2
	exit 1
fi

echo "==> assembling $version from isolated tag checkout $tag_commit"
cd "$checkout_root/source"
"$@"
