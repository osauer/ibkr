#!/usr/bin/env bash
#
# build-release-target.sh - build and package one release target.
#
# Called by `make release-binaries` via xargs -P so the OS/arch matrix
# can compile in parallel while preserving the existing artefact shape:
#   dist/ibkr-vX.Y.Z-<os>-<arch>.tar.gz

set -euo pipefail

target="${1:?usage: build-release-target.sh <os-arch> <version> <ldflags> <dist-dir>}"
version="${2:?release version required}"
ldflags="${3:?release ldflags required}"
dist_dir="${4:?dist dir required}"

os="${target%-*}"
arch="${target#*-}"
base="ibkr-${version}-${target}"
stage="${dist_dir}/${base}"

echo "==> ${os}/${arch}"
rm -rf "$stage"
mkdir -p "$stage"
trap 'rm -rf "$stage"' EXIT

GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$stage/ibkr" ./cmd/ibkr
cp LICENSE README.md "$stage/"
( cd "$dist_dir" && tar -czf "$base.tar.gz" "$base" )
