#!/usr/bin/env bash
#
# build-release-target.sh - build and package one release target.
#
# Called by `make release-binaries` via xargs -P so the OS/arch matrix
# can compile in parallel. Each target produces TWO artefacts:
#   dist/ibkr-vX.Y.Z-<os>-<arch>.tar.gz          read-only (no broker writes)
#   dist/ibkr-trading-vX.Y.Z-<os>-<arch>.tar.gz  broker-write capable
#
# The read-only artefact keeps the historical name so existing links and
# install.sh keep resolving to the safe variant; trading is an explicit
# download (the release notes carry the warning).

set -euo pipefail

target="${1:?usage: build-release-target.sh <os-arch> <version> <ldflags> <dist-dir>}"
version="${2:?release version required}"
ldflags="${3:?release ldflags required}"
dist_dir="${4:?dist dir required}"

os="${target%-*}"
arch="${target#*-}"

build_variant() {
	local prefix="$1" tags="$2"
	local base="${prefix}-${version}-${target}"
	local stage="${dist_dir}/${base}"

	rm -rf "$stage"
	mkdir -p "$stage"
	GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false ${tags:+-tags "$tags"} -ldflags "$ldflags" -o "$stage/ibkr" ./cmd/ibkr
	cp LICENSE README.md "$stage/"
	if [ -n "$tags" ]; then
		cat > "$stage/TRADING-WARNING.md" << 'WARN'
# Broker-write capable build

This binary can place, modify, and cancel orders with your broker once the
trading gates in `~/.config/ibkr/config.toml` are configured. If you only
want market data, dashboards, and previews, download the standard `ibkr`
artefact instead — it is the same tool without order transmission compiled
in. See SECURITY.md and docs/guides/trading-preview.md before enabling
trading.
WARN
	fi
	( cd "$dist_dir" && tar -czf "$base.tar.gz" "$base" )
	rm -rf "$stage"
}

echo "==> ${os}/${arch} (read-only + trading)"
build_variant "ibkr" ""
build_variant "ibkr-trading" "trading"
