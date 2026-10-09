#!/usr/bin/env bash
# Restart the crctl-managed claude session this script is run from, moving it
# into a git repository. The restart is handed to a detached helper that waits
# for the session to go idle, so the turn that asked for it can finish first.
#
#   restart-self.sh <dir>          schedule the move (returns immediately)
#   restart-self.sh --check [dir]  only report what would happen
set -euo pipefail

CRCTL=${CRCTL:-crctl}
SESSIONS_DIR=${SESSIONS_DIR:-${CLAUDE_HOME:-$HOME/.claude}/sessions}
IDLE_WAIT=${IDLE_WAIT:-600} # seconds to wait for the session to go idle
uuid_re='[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'

die() { echo "restart-self: $*" >&2; exit 1; }

# The screen name is <prefix>-<session id>; $STY is "<pid>.<screen name>".
session() {
	[ -n "${STY:-}" ] || die "not inside a screen session, so this claude is not managed by crctl"
	name=${STY#*.}
	[[ $name =~ ^(.+)-($uuid_re)$ ]] || die "screen '$name' is not a crctl session"
	prefix=${BASH_REMATCH[1]} id=${BASH_REMATCH[2]}
}

# State of this session's claude ("idle", "busy", ...); "" when unknown.
status() {
	[ -n "${CLAUDE_PID:-}" ] || return 0
	grep -o '"status":"[a-z_]*"' "$SESSIONS_DIR/$CLAUDE_PID.json" 2>/dev/null | head -1 | cut -d'"' -f4
}

if [ "${1:-}" = "--run" ]; then # detached helper: <id> <prefix> <repo>
	id=$2 prefix=$3 repo=$4
	if [ -n "$(status)" ]; then
		waited=0
		until [ "$(status)" = idle ]; do
			[ "$waited" -lt "$IDLE_WAIT" ] || { echo "session never went idle; not restarting"; exit 1; }
			sleep 2; waited=$((waited + 2))
		done
		sleep 2 # let the finished turn reach the transcript
	else
		sleep 20 # no status to watch; give the turn time to end
	fi
	echo "$(date -Is) restarting $id in $repo"
	# --trust: nobody is there to answer claude's folder-trust prompt, and the
	# session has been working in this repo already.
	CLAUDE_REMOTE_SESSION_PREFIX=$prefix exec "$CRCTL" restart "$id" --dir "$repo" --trust
fi

check=false
[ "${1:-}" = "--check" ] && { check=true; shift; }
session
echo "session: $id"
echo "current: $(pwd)"
dir=${1:-}
if [ -z "$dir" ]; then
	$check || die "usage: restart-self.sh [--check] <dir>"
	exit 0
fi

repo=$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null) || die "$dir is not inside a git repository"
echo "target:  $repo ($(git -C "$repo" remote get-url origin 2>/dev/null || echo 'no origin remote'))"
"$CRCTL" help 2>&1 | grep -q -- '--trust' ||
	die "$CRCTL has no 'restart --dir --trust'; upgrade code-remote (crctl) to a version that does"
$check && exit 0

log=${XDG_STATE_HOME:-$HOME/.local/state}/crctl/self-restart-$id.log
mkdir -p "$(dirname "$log")"
# Own session + no terminal: the helper must outlive the screen it restarts.
setsid nohup "$0" --run "$id" "$prefix" "$repo" >"$log" 2>&1 </dev/null &
echo "scheduled: restarts into $repo once this session is idle (log: $log)"
