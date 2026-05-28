#!/bin/sh
# changelog-stub.sh — prepend a CHANGELOG.md entry skeleton for
# RELEASE_VERSION above the current topmost entry. Run from
# `make changelog-stub RELEASE_VERSION=vX.Y.Z`.
set -eu

ver=${RELEASE_VERSION:-}
[ -n "$ver" ] || {
  echo "changelog-stub: RELEASE_VERSION env required (e.g. v0.27.12)" >&2
  exit 1
}

case "$ver" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *)
    echo "changelog-stub: RELEASE_VERSION must look like vX.Y.Z (got '$ver')" >&2
    exit 1
    ;;
esac

cd "$(dirname "$0")/.."

if grep -q "^## $ver " CHANGELOG.md; then
  echo "changelog-stub: $ver entry already exists in CHANGELOG.md" >&2
  exit 1
fi

ts=$(TZ="Europe/Berlin" date +"%Y-%m-%d %H:%M %Z")

stub_file=$(mktemp -t changelog-stub.XXXXXX)
tmp=$(mktemp -t changelog-out.XXXXXX)
trap 'rm -f "$stub_file" "$tmp"' EXIT

{
printf '## %s — %s\n' "$ver" "$ts"
cat <<'STUB_EOF'

### What's new

### Changed

### Fixed
STUB_EOF
} >"$stub_file"

awk -v stub_file="$stub_file" '
  !inserted && /^## v/ {
    while ((getline line < stub_file) > 0) print line
    close(stub_file)
    inserted = 1
  }
  { print }
' CHANGELOG.md > "$tmp"
mv "$tmp" CHANGELOG.md

echo "changelog-stub: prepended skeleton for $ver"
echo "                edit CHANGELOG.md and fill in ### What's new + ### Changed/### Fixed"
cat <<'GUIDANCE_EOF'

Guidance:
  - Keep CHANGELOG.md source public; do not leave template comments or maintainer notes in it.
  - ### What's new: three bullets max, plain English, user-visible impact.
  - Keep-a-Changelog bullets: one user-visible change per bullet.
  - No internal finding IDs, session notes, bare workflow-step numbers, or internal symbol drops without reader value.
  - Use **Action required:** for user action and **Breaking (Go library):** for Go-library-only breakage.
  - Omit ### Engineering notes unless short cross-release context is genuinely needed.
GUIDANCE_EOF
