#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

master="${IBKR_CANARY_ICON_MASTER:-icon-512.png}"
source_sheet="${IBKR_CANARY_ICON_SOURCE_SHEET:-}"
crop_geometry="${IBKR_CANARY_ICON_CROP:-}"

require_sips() {
  if ! command -v sips >/dev/null 2>&1; then
    echo "generate-icons: sips is required on this machine" >&2
    exit 1
  fi
}

resize_png() {
  local size="$1"
  local src="$2"
  local dst="$3"
  sips -z "$size" "$size" "$src" --out "$dst" >/dev/null
}

if [[ -n "$source_sheet" ]]; then
  if [[ ! -f "$source_sheet" ]]; then
    echo "generate-icons: source sheet not found: $source_sheet" >&2
    exit 1
  fi
  if [[ -z "$crop_geometry" ]]; then
    echo "generate-icons: set IBKR_CANARY_ICON_CROP as y,x,height,width for source-sheet recrops" >&2
    exit 1
  fi

  IFS=, read -r crop_y crop_x crop_h crop_w extra <<<"$crop_geometry"
  if [[ -n "${extra:-}" || -z "${crop_y:-}" || -z "${crop_x:-}" || -z "${crop_h:-}" || -z "${crop_w:-}" ]]; then
    echo "generate-icons: invalid IBKR_CANARY_ICON_CROP; expected y,x,height,width" >&2
    exit 1
  fi

  require_sips
  tmp="$(mktemp -t ibkr-canary-icon.XXXXXX.png)"
  trap 'rm -f "$tmp"' EXIT
  sips -c "$crop_h" "$crop_w" --cropOffset "$crop_y" "$crop_x" "$source_sheet" --out "$tmp" >/dev/null
  resize_png 512 "$tmp" icon-512.png
  master="icon-512.png"
fi

if [[ ! -f "$master" ]]; then
  echo "generate-icons: master icon not found: $master" >&2
  exit 1
fi

require_sips
resize_png 192 "$master" icon-192.png
resize_png 64 "$master" favicon-64.png
resize_png 32 "$master" favicon-32.png
resize_png 16 "$master" favicon-16.png

echo "generated ibkr canary PNG icons from $master"
