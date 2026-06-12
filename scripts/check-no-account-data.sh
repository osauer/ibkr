#!/bin/sh
# check-no-account-data.sh — fail the pre-commit gate when tracked/staged
# files carry real IBKR account data. Born of the 2026-06-11 incident where
# a root-level scratch page (buying_power_lab.html) with live margin /
# buying-power / net-liq / position figures shipped inside the v1.9.0 tag
# and had to be purged with a history rewrite and force-push.
#
# Three checks, all over the git index (tracked + staged-for-add files):
#   1. No HTML files at the repo root — real pages live under docs/ or
#      web/; root HTML is a scratch page by definition here.
#   2. No scratch-page names anywhere (*lab*.html, *scratch*).
#   3. No IBKR account IDs (U / DU followed by 6-9 digits) anywhere,
#      Go files included — the 2026-06-11 follow-up incident put the live
#      U-account ID in two Go test files while *.go was still excluded
#      here. Only the documentation/test placeholders (U1234567-style
#      dummies) and the DU3136804 paper account (committed by deliberate
#      policy) are allowlisted.
set -eu

# Byte-wise grep: the locale-aware path is ~5x slower over the docs tree.
LC_ALL=C
export LC_ALL

cd "$(dirname "$0")/.."

self=scripts/check-no-account-data.sh
status=0

# Index contents, minus files staged for deletion / missing on disk
# (same scope rationale as gofmt-check in the Makefile).
files=$(git ls-files --cached | while IFS= read -r f; do
  [ -e "$f" ] && printf '%s\n' "$f"
done)

# 1) HTML at repo root.
root_html=$(printf '%s\n' "$files" | grep -E '^[^/]+\.html$' || true)
if [ -n "$root_html" ]; then
  echo "check-no-account-data: HTML file(s) at repo root — scratch pages stay untracked, real pages live under docs/ or web/:" >&2
  printf '  %s\n' $root_html >&2
  status=1
fi

# 2) Scratch-page names anywhere in the tree.
scratch=$(printf '%s\n' "$files" | grep -iE '(^|/)[^/]*lab[^/]*\.html$|scratch' || true)
if [ -n "$scratch" ]; then
  echo "check-no-account-data: scratch-page filename(s) tracked (*lab*.html / *scratch*):" >&2
  printf '  %s\n' $scratch >&2
  status=1
fi

# 3) Account IDs anywhere in the index. git grep scans staged blob
#    contents (multithreaded, ~3x faster than xargs grep over the worktree
#    here). Boundary classes instead of \b for BSD/GNU regex portability;
#    the trailing class rejects longer digit runs.
id_re='(^|[^[:alnum:]_])D?U[0-9]{6,9}([^[:alnum:]]|$)'
allow_re='D?U1234567|D?U7654321|DU123456|DU0000000|DU3136804'
candidates=$(git grep --cached -lIE "$id_re" -- ":!$self" || true)
for f in $candidates; do
  ids=$(git grep --cached -hoIE "$id_re" -- "$f" | grep -oE 'D?U[0-9]{6,9}' |
    grep -vxE "$allow_re" | sort -u || true)
  if [ -n "$ids" ]; then
    echo "check-no-account-data: $f contains IBKR account ID(s): $(printf '%s ' $ids)" >&2
    echo "                       real IDs must never be committed; use the U1234567 / DU1234567 placeholders" >&2
    status=1
  fi
done

[ "$status" -eq 0 ] && echo "check-no-account-data: OK"
exit "$status"
