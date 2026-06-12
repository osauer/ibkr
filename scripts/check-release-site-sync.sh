#!/bin/sh
# check-release-site-sync.sh — require the public product site to be updated
# and pushed before non-patch releases.
set -eu

version=${1:-}

usage() {
  echo "usage: $0 vX.Y.Z" >&2
}

[ -n "$version" ] || {
  usage
  exit 1
}

case "$version" in
  v[0-9]*.[0-9]*.[0-9]* | v[0-9]*.[0-9]*.[0-9]*-*) ;;
  *)
    echo "release-site-check: RELEASE_VERSION must look like vX.Y.Z (got $version)" >&2
    exit 1
    ;;
esac

core=${version#v}
core=${core%%-*}
major=${core%%.*}
rest=${core#*.}
minor=${rest%%.*}
patch=${rest#*.}

if [ -z "$major" ] || [ -z "$minor" ] || [ -z "$patch" ]; then
  echo "release-site-check: cannot parse semantic version $version" >&2
  exit 1
fi
case "$major:$minor:$patch" in
  *[!0-9:]*)
    echo "release-site-check: cannot parse semantic version $version" >&2
    exit 1
    ;;
esac

if [ "$patch" -ne 0 ]; then
  echo "release-site-check: $version is a patch release; static site push not required"
  exit 0
fi

if [ ! -d docs ]; then
  echo "release-site-check: docs/ missing; run from the ibkr repo root" >&2
  exit 1
fi

if [ -n "$(git status --porcelain)" ]; then
  echo "release-site-check: working tree is dirty; commit the docs/ website update before releasing $version" >&2
  git status --short >&2
  exit 1
fi

head=$(git rev-parse HEAD)
main=$(git rev-parse origin/main 2>/dev/null) || {
  echo "release-site-check: origin/main is missing locally; run git fetch origin main" >&2
  exit 1
}
if [ "$head" != "$main" ]; then
  echo "release-site-check: HEAD ($head) does not match origin/main ($main)" >&2
  echo "                    push the docs/ website update before non-patch releases" >&2
  exit 1
fi

plain=${version#v}
if ! grep -q "\"softwareVersion\": \"$plain\"" docs/index.html; then
  echo "release-site-check: docs/index.html softwareVersion is not $plain" >&2
  echo "                    update the osauer.dev/ibkr landing page for this non-patch release" >&2
  exit 1
fi

# Every public version stamp must move together: spoke-page JSON-LD plus the
# MCP discovery JSONs (the v1.10.0 prep caught all of these lagging at the
# previous release version while only index.html was gated).
if ! grep -q "\"softwareVersion\": \"$plain\"" docs/interactive-brokers-mcp-server/index.html; then
  echo "release-site-check: docs/interactive-brokers-mcp-server/index.html softwareVersion is not $plain" >&2
  exit 1
fi
for f in docs/mcp-server.json docs/.well-known/mcp/server.json docs/.well-known/mcp/server-card.json; do
  if ! grep -q "\"version\": \"$plain\"" "$f"; then
    echo "release-site-check: $f version is not $plain" >&2
    exit 1
  fi
done

echo "release-site-check: $version requires and has a pushed osauer.dev/ibkr docs update"
