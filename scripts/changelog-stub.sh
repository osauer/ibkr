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

cat >"$stub_file" <<STUB_EOF
## $ver — $ts

### What's new

<!--
  Three bullets max. Plain English. No symbol names or file paths.
  Answer for the reader:
    - Did anything I run differently? (CLI / MCP / config)
    - Do I need to reinstall or reconfigure?
    - Did MCP tool names, fields, or wire formats change?
  Mark user-action items with **Action required:**.
  Mark Go-library-only breaking changes with **Breaking (Go library):**.
-->

### Changed

<!--
  One user-visible change per bullet. Frame each as the consumer-visible
  effect — what the API caller / CLI user / MCP consumer notices — not
  the internal mechanism that produced it.
  Rules (apply to all KaC sections — Added/Changed/Fixed/...):
    - No internal finding IDs (F-NN, F#NN, finding-N). They belong in
      commit messages, not the CHANGELOG. (Lint-enforced.)
    - No bare "step N" references. If you name a workflow step, name
      the workflow on the same line AND say what the step tests
      (e.g. "release-verify step 7 (regime call-sequence drop check)").
    - No internal symbol-drops without a reader-value gloss. Mention
      `internalHelper` only if it's part of the surface the reader
      touches (an exported API, a wire field, a CLI flag). Otherwise
      describe the user-visible effect; put the symbol in commits.
    - No relative dates or session-internal references ("yesterday",
      "the YYYY-MM-DD failure"). Use "this release" or version refs.
    - Mark Go-library-only breakage **Breaking (Go library):**.
-->

### Fixed

<!--
  One user-visible bug per bullet. "X no longer happens" / "Y now
  works as documented" / "Z is honoured" framing. Same rules as Changed.
-->

<!--
  Optional ### Engineering notes section. Defaults to OMITTED.
  Add ONLY if you have one of:
    - Multi-release bug-class lineage (briefly link prior versions)
    - Why-this-way rationale that isn't visible from the code
    - Cross-cutting context that doesn't fit a one-line bullet
  Rules:
    - Short: <= 15 lines of content. If you need more, you're duplicating
      Changed/Fixed bullets above — move it there instead.
    - Self-contained. NO internal finding IDs (F-NN) unless defined
      inline; NO bare "step N" without naming the workflow it belongs
      to; NO relative dates ("yesterday", "the YYYY-MM-DD failure") or
      session-internal references. A reader six months from now must be
      able to follow it without external context.
    - Not a duplicate. If a fact fits a one-line bullet, it belongs in
      Changed/Fixed, not here.
-->
STUB_EOF

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
