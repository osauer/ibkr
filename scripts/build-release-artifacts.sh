#!/usr/bin/env bash
# Assemble release assets while cwd is an isolated checkout of the exact tag.

set -euo pipefail

mode="${1:?usage: build-release-artifacts.sh <all|mcpb|checksums> <version> <dist-dir> <targets> [jobs] [strip-ldflags]}"
version="${2:?release version required}"
dist_dir="${3:?dist dir required}"
targets="${4:?release targets required}"
jobs="${5:-1}"
strip_ldflags="${6:--s -w}"

case "$mode" in all|mcpb|checksums) ;; *) echo "build-release-artifacts: invalid mode: $mode" >&2; exit 2 ;; esac
case "$version" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "build-release-artifacts: invalid version: $version" >&2; exit 2 ;; esac
case "$dist_dir" in /*) ;; *) echo "build-release-artifacts: dist directory must be absolute" >&2; exit 2 ;; esac
if [ "$dist_dir" = "/" ] || [ "$dist_dir" = "$PWD" ]; then
	echo "build-release-artifacts: refusing unsafe dist directory: $dist_dir" >&2
	exit 2
fi
case "$jobs" in ''|*[!0-9]*) echo "build-release-artifacts: jobs must be a positive integer" >&2; exit 2 ;; esac
if [ "$jobs" -lt 1 ]; then
	echo "build-release-artifacts: jobs must be a positive integer" >&2
	exit 2
fi

tag_commit="$(git rev-parse --verify "refs/tags/$version^{commit}")"
head_commit="$(git rev-parse HEAD)"
if [ "$head_commit" != "$tag_commit" ] || [ -n "$(git status --porcelain)" ]; then
	echo "build-release-artifacts: source must be a clean checkout of $version" >&2
	exit 1
fi
release_date="$(git show -s --format=%cI HEAD)"
release_ldflags="$strip_ldflags -X main.version=$version -X main.commit=$head_commit -X main.date=$release_date"

build_mcpb() {
	./scripts/build-mcpb.sh "$version" "$dist_dir" "$targets"
}

build_checksums() {
	if ! ls "$dist_dir"/ibkr-"$version"-*.tar.gz >/dev/null 2>&1; then
		echo "build-release-artifacts: missing read-only release tarballs" >&2
		exit 1
	fi
	if ! ls "$dist_dir"/ibkr-trading-"$version"-*.tar.gz >/dev/null 2>&1; then
		echo "build-release-artifacts: missing trading release tarballs" >&2
		exit 1
	fi
	for asset in "ibkr-$version.mcpb" ibkr.mcpb; do
		if [ ! -f "$dist_dir/$asset" ]; then
			echo "build-release-artifacts: missing $dist_dir/$asset" >&2
			exit 1
		fi
	done
	(
		cd "$dist_dir"
		shasum -a 256 ibkr-"$version"-*.tar.gz ibkr-trading-"$version"-*.tar.gz "ibkr-$version.mcpb" ibkr.mcpb > SHA256SUMS
	)
	command -v gpg >/dev/null 2>&1 || {
		echo "build-release-artifacts: gpg not on PATH" >&2
		exit 1
	}
	expected_fp="$(awk -F\" '/ReleaseSigningKeyFingerprint =/{print $2; exit}' internal/update/keyring.go)"
	if [ -z "$expected_fp" ] || ! gpg --list-secret-keys --with-colons "$expected_fp" >/dev/null 2>&1; then
		echo "build-release-artifacts: the tag's release signing key is unavailable" >&2
		exit 1
	fi
	echo "==> signing SHA256SUMS with the key pinned by $version"
	(
		cd "$dist_dir"
		gpg --batch --yes --local-user "$expected_fp" --armor --detach-sign --output SHA256SUMS.asc SHA256SUMS
		gpg --verify SHA256SUMS.asc SHA256SUMS >/dev/null 2>&1
	)
}

case "$mode" in
	all)
		rm -rf "$dist_dir"
		mkdir -p "$dist_dir"
		printf '%s\n' $targets | xargs -P "$jobs" -I {} ./scripts/build-release-target.sh {} "$version" "$release_ldflags" "$dist_dir"
		build_mcpb
		build_checksums
		;;
	mcpb)
		build_mcpb
		;;
	checksums)
		build_checksums
		;;
esac
