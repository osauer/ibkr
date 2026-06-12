#!/usr/bin/env bash
#
# with-gateway-lock.sh — serialize gateway-touching gates across concurrent
# dev sessions.
#
# TWS accepts one connection per client ID, and the integration suite,
# wire smoke, and release smokes each spawn daemons against the same
# gateway. Two sessions overlapping used to mean TWS error 326 ("client
# id already in use") and a full re-run of a multi-minute gate. This
# wrapper turns that collision into a short wait: the second session
# blocks on an exclusive flock until the first finishes.
#
# Usage:
#   scripts/with-gateway-lock.sh <command> [args...]
#
# Environment hooks:
#   IBKR_GATEWAY_LOCK_FILE — lock path (default: $TMPDIR/ibkr-gateway.lock,
#                            per-user on macOS so multi-user hosts isolate)
#   IBKR_GATEWAY_LOCK_WAIT — max seconds to wait for the lock (default: 900,
#                            sized for a worst-case off-hours release smoke)
#
# Implementation notes:
#   - macOS ships no flock(1), so the lock is taken via perl's flock on an
#     fd the shell already holds. perl exiting does NOT release the lock:
#     fd 9 stays open in this shell, and flock locks belong to the open
#     file description, not the process.
#   - The wrapped command runs with fd 9 closed (9>&-) so long-lived
#     children it spawns (autospawned daemons!) cannot inherit the fd and
#     hold the lock after the gate finishes. The lock lifetime is exactly
#     this wrapper's lifetime.
set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "usage: with-gateway-lock.sh <command> [args...]" >&2
    exit 2
fi

LOCKFILE="${IBKR_GATEWAY_LOCK_FILE:-${TMPDIR:-/tmp}/ibkr-gateway.lock}"
WAIT="${IBKR_GATEWAY_LOCK_WAIT:-900}"
if [[ ! "$WAIT" =~ ^[0-9]+$ ]]; then
    echo "with-gateway-lock: invalid IBKR_GATEWAY_LOCK_WAIT: $WAIT" >&2
    exit 2
fi

exec 9>>"$LOCKFILE"

perl -e '
    use Fcntl qw(:flock);
    my ($timeout, $lockfile) = @ARGV;
    open(my $fh, "<&=", 9) or die "with-gateway-lock: cannot adopt fd 9: $!\n";
    exit 0 if flock($fh, LOCK_EX | LOCK_NB);
    print STDERR "with-gateway-lock: gateway busy (another session holds $lockfile); waiting up to ${timeout}s...\n";
    my $deadline = time + $timeout;
    my $next_note = time + 30;
    until (flock($fh, LOCK_EX | LOCK_NB)) {
        die "with-gateway-lock: timed out after ${timeout}s waiting for $lockfile\n"
            if time >= $deadline;
        if (time >= $next_note) {
            printf STDERR "with-gateway-lock: still waiting for %s (%ds left)\n",
                $lockfile, $deadline - time;
            $next_note = time + 30;
        }
        sleep 1;
    }
    print STDERR "with-gateway-lock: lock acquired, continuing\n";
' "$WAIT" "$LOCKFILE"

"$@" 9>&-
