#!/bin/sh
# check-docs-html-sync.sh — keep docs/ md↔html twins from drifting apart.
#
# Several docs/ markdown files have a hand-maintained .html twin that
# GitHub Pages actually serves. The 2026-06-11 audit found the published
# concepts.html still describing the pre-v1.9.0 gamma methodology a week
# after concepts.md moved on — nothing forced the twin to follow.
#
# Mechanism: every twin .html carries a marker comment near the top,
#     <!-- md-source: <name>.md sha256:<64 hex> -->
# recording the hash of the .md it was last synced against. This script:
#   check (default): for each docs/**/*.md with a sibling .html of the
#     same stem, recompute the md hash and compare with the marker.
#     A mismatch means the md changed after the html was last synced —
#     update the html content, then re-stamp. A tripwire, not a proof
#     of semantic sync: stamping without syncing is on you.
#   --stamp: rewrite (or insert) the marker in every twin html from the
#     current md hashes. Run only after the html content matches the md.
set -eu

cd "$(dirname "$0")/.."

sha() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    sha256sum "$1" | cut -d' ' -f1
  fi
}

mode=${1:-check}
status=0
found=0

for md in $(git ls-files --cached 'docs/*.md' 'docs/**/*.md'); do
  html="${md%.md}.html"
  [ -e "$html" ] || continue
  found=$((found + 1))
  want=$(sha "$md")
  base=$(basename "$md")

  case "$mode" in
  --stamp)
    marker="<!-- md-source: $base sha256:$want -->"
    if grep -q '^<!-- md-source: ' "$html"; then
      awk -v m="$marker" '/^<!-- md-source: / { print m; next } { print }' \
        "$html" > "$html.tmp"
    else
      # Insert after the doctype line so the marker stays out of <head>.
      awk -v m="$marker" 'NR==1 { print; print m; next } { print }' \
        "$html" > "$html.tmp"
    fi
    mv "$html.tmp" "$html"
    ;;
  check)
    got=$(sed -n 's/^<!-- md-source: .* sha256:\([0-9a-f]\{64\}\) -->$/\1/p' "$html")
    if [ -z "$got" ]; then
      echo "check-docs-html-sync: $html has no md-source marker" >&2
      echo "                      sync its content with $md, then run: make docs-html-stamp" >&2
      status=1
    elif [ "$got" != "$want" ]; then
      echo "check-docs-html-sync: $md changed after $html was last synced" >&2
      echo "                      update the html to match, then run: make docs-html-stamp" >&2
      status=1
    fi
    ;;
  *)
    echo "check-docs-html-sync: unknown mode $mode (use no args or --stamp)" >&2
    exit 2
    ;;
  esac
done

if [ "$found" -eq 0 ]; then
  echo "check-docs-html-sync: no md/html twins found under docs/ — script scope is stale" >&2
  exit 1
fi

case "$mode" in
--stamp) echo "check-docs-html-sync: stamped $found twin(s)" ;;
check) [ "$status" -eq 0 ] && echo "check-docs-html-sync: $found twin(s) OK" ;;
esac
exit "$status"
