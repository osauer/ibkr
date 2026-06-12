#!/usr/bin/env bash
#
# install-govuln-prewarm.sh — install (or remove) a user LaunchAgent that
# runs `make govulncheck-check` daily at 06:00.
#
# govulncheck's daily stamp (see Makefile govulncheck-check) means the
# first gate run of each day pays the full cold scan — measured at
# 8-10 minutes inside the first interactive `make check`/`make test` of
# the morning. Pre-warming at 06:00 moves that cost out of the dev loop:
# the scan runs unattended, stamps, and every interactive run that day
# takes the skip path. launchd runs missed StartCalendarInterval jobs on
# wake, so a Mac asleep at 06:00 still pre-warms when it opens.
#
# `/bin/zsh -lc` runs the user's login profile so `go` is on PATH under
# launchd's minimal environment.
#
# Usage:
#   scripts/install-govuln-prewarm.sh             # install + load
#   scripts/install-govuln-prewarm.sh --uninstall
set -euo pipefail

LABEL="dev.osauer.ibkr.govuln-prewarm"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
LOG="$HOME/.cache/ibkr/govuln-prewarm.log"

if [[ "${1:-}" == "--uninstall" ]]; then
    launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
    rm -f "$PLIST"
    echo "govuln-prewarm: LaunchAgent removed"
    exit 0
fi

mkdir -p "$(dirname "$PLIST")" "$(dirname "$LOG")"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/zsh</string>
        <string>-lc</string>
        <string>make -C $REPO govulncheck-check</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict><key>Hour</key><integer>6</integer><key>Minute</key><integer>0</integer></dict>
    <key>StandardOutPath</key><string>$LOG</string>
    <key>StandardErrorPath</key><string>$LOG</string>
</dict>
</plist>
EOF

launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
echo "govuln-prewarm: installed $PLIST (daily 06:00, log: $LOG)"
