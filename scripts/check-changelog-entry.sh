#!/bin/sh
# check-changelog-entry.sh — assert the topmost CHANGELOG.md entry matches
# RELEASE_VERSION and has the required shape (heading + "What's new" stanza
# + at least one Keep-a-Changelog subsection). Runs from `make release`
# before `git tag` so a malformed entry aborts the release.
set -eu

ver=${RELEASE_VERSION:-}
[ -n "$ver" ] || {
  echo "check-changelog-entry: RELEASE_VERSION env required" >&2
  exit 1
}

cd "$(dirname "$0")/.."

./scripts/check-changelog-public.sh

# 1) Topmost ## v entry must match RELEASE_VERSION with a timestamp suffix.
head=$(grep -m1 '^## v' CHANGELOG.md || true)
case "$head" in
  "## $ver — "*) ;;
  "")
    echo "check-changelog-entry: CHANGELOG.md has no '## v' entries" >&2
    exit 1
    ;;
  *)
    echo "check-changelog-entry: topmost CHANGELOG entry is '$head'" >&2
    echo "                       expected '## $ver — <YYYY-MM-DD HH:MM TZ>'" >&2
    exit 1
    ;;
esac

# 2) Matching entry must have a non-empty `### What's new` section.
if ! awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" "); next }
  in_ver && /^### What.s new$/ { in_new = 1; next }
  in_ver && in_new && /^###/ { exit }
  in_new && NF { found = 1 }
  END { exit !found }
' CHANGELOG.md; then
  echo "check-changelog-entry: $ver has no non-empty '### What'\''s new' section" >&2
  echo "                       (must follow the version heading; describes user-visible change)" >&2
  exit 1
fi

# 3) Matching entry must have at least one Keep-a-Changelog subsection.
has_kac=$(awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" "); next }
  in_ver && /^### (Added|Changed|Deprecated|Removed|Fixed|Security)$/ { print "yes"; exit }
' CHANGELOG.md)
[ "$has_kac" = yes ] || {
  echo "check-changelog-entry: $ver has no ### Added/Changed/Deprecated/Removed/Fixed/Security section" >&2
  exit 1
}

# 4) If `### Engineering notes` is present, content must be <= 15 lines.
#    Long Engineering notes are almost always duplicating Changed/Fixed
#    bullets or restating commit-message context — neither earns its keep
#    in a CHANGELOG.
eng_lines=$(awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" "); next }
  in_ver && /^### Engineering notes$/ { in_eng = 1; next }
  in_ver && in_eng && /^###/ { exit }
  in_ver && in_eng && /^## v[0-9]/ { exit }
  in_eng { n++ }
  END { print n+0 }
' CHANGELOG.md)
if [ "$eng_lines" -gt 15 ]; then
  echo "check-changelog-entry: $ver '### Engineering notes' has $eng_lines lines (limit 15)" >&2
  echo "                       trim it. If a fact fits a one-line bullet, move it to Changed/Fixed." >&2
  exit 1
fi

# 5) No internal finding IDs (F-NN, F#NN, finding-N) inside KaC bullets.
#    These are maintainer-internal handles with no value for the section's
#    reader. The finding ID belongs in the commit message or the issue
#    tracker, not in the user-facing changelog.
finding=$(awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" "); next }
  in_ver && /^### (Added|Changed|Deprecated|Removed|Fixed|Security)$/ { in_kac = 1; next }
  in_ver && in_kac && /^###/ { in_kac = 0 }
  in_ver && in_kac && /(\*\*F-[0-9]+|F#[0-9]+|finding-[0-9]+)/ { print "L"NR": "$0; exit }
' CHANGELOG.md)
if [ -n "$finding" ]; then
  echo "check-changelog-entry: $ver KaC bullet references an internal finding ID" >&2
  echo "                       $finding" >&2
  echo "                       Finding IDs belong in commit messages, not the CHANGELOG." >&2
  echo "                       Frame the bullet for the section's reader (what the user notices)." >&2
  exit 1
fi

echo "check-changelog-entry: $ver OK"
