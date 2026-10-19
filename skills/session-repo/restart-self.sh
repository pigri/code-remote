#!/usr/bin/env bash
# Restart the crctl-managed claude session this script is run from, moving it
# into a git repository, or with CLAUDE_REMOTE_PROFILES into another profile
# (Claude account). The restart is handed to a detached helper that waits for
# the session to go idle, so the turn that asked for it can finish first.
#
#   restart-self.sh <dir>                    schedule the move (returns immediately)
#   restart-self.sh --profile <name> [dir]   schedule a move to profile <name>
#   restart-self.sh --check [--profile <name>] [dir]
#                                            only report what would happen
set -euo pipefail

CRCTL=${CRCTL:-crctl}
profiles=${CLAUDE_REMOTE_PROFILES:-} # a session inherits it from whatever launched it
if [ -n "$profiles" ]; then
	# Where this session's claude writes: CLAUDE_HOME is the server's setting,
	# and a session can inherit one that is not its own profile.
	SESSIONS_DIR=${SESSIONS_DIR:-${CLAUDE_CONFIG_DIR:-$HOME/.claude}/sessions}
else
	SESSIONS_DIR=${SESSIONS_DIR:-${CLAUDE_CONFIG_DIR:-${CLAUDE_HOME:-$HOME/.claude}}/sessions}
fi
IDLE_WAIT=${IDLE_WAIT:-600} # seconds to wait for the session to go idle
uuid_re='[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'

die() { echo "restart-self: $*" >&2; exit 1; }

# Profile names in CLAUDE_REMOTE_PROFILES ("name=dir,name=dir"), one per line.
names() { printf '%s\n' "$profiles" | tr ',' '\n' | sed 's/=.*//; s/^[[:space:]]*//; s/[[:space:]]*$//'; }

# $STY is "<pid>.<screen name>". The name is <prefix>-<session id>, and with
# profiles also <prefix>-<profile>-<session id>. Without profiles the split
# below gives the prefix, which the helper passes on to crctl. With them it
# cannot tell a profile from the end of the prefix, so the profile is what
# follows the prefix crctl uses, and crctl finds the session by its id.
session() {
	[ -n "${STY:-}" ] || die "not inside a screen session, so this claude is not managed by crctl"
	name=${STY#*.}
	[[ $name =~ ^(.+)-($uuid_re)$ ]] || die "screen '$name' is not a crctl session"
	prefix=${BASH_REMATCH[1]} id=${BASH_REMATCH[2]} profile=
	[ -n "$profiles" ] || return 0
	local p=${CLAUDE_REMOTE_SESSION_PREFIX:-pigri-dev-remote}
	case $prefix in
	"$p") ;; # a name from before profiles: no hint, crctl resolves the profile
	"$p"-*) profile=${prefix#"$p"-} ;;
	*) die "screen '$name' does not start with '$p-', the CLAUDE_REMOTE_SESSION_PREFIX crctl looks for" ;;
	esac
	[ -z "$profile" ] || names | grep -qxF -- "$profile" ||
		die "screen '$name' names profile '$profile', which is not in CLAUDE_REMOTE_PROFILES"
	prefix=
}

# State of this session's claude ("idle", "busy", ...); "" when unknown.
status() {
	[ -n "${CLAUDE_PID:-}" ] || return 0
	grep -o '"status":"[a-z_]*"' "$SESSIONS_DIR/$CLAUDE_PID.json" 2>/dev/null | head -1 | cut -d'"' -f4
}

if [ "${1:-}" = "--run" ]; then # detached helper: <id> <prefix> <repo> [profile]
	id=$2 prefix=$3 repo=$4 to=${5:-}
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
	echo "$(date -Is) restarting $id${repo:+ in $repo}${to:+ under profile $to}"
	args=(restart "$id")
	[ -z "$repo" ] || args+=(--dir "$repo")
	[ -z "$to" ] || args+=(--profile "$to")
	# --trust: nobody is there to answer claude's folder-trust prompt, and the
	# session has been working in this repo already (for a move, in another
	# profile, whose trusted folders the new one does not share).
	args+=(--trust)
	[ -z "$prefix" ] || export CLAUDE_REMOTE_SESSION_PREFIX=$prefix
	exec "$CRCTL" "${args[@]}"
fi

usage="restart-self.sh [--check] <dir>"
[ -z "$profiles" ] || usage="restart-self.sh [--check] (<dir> | --profile <name> [dir])"
check=false to='' dir=''
while [ $# -gt 0 ]; do
	case $1 in
	--check) check=true ;;
	--profile)
		[ $# -ge 2 ] || die "usage: $usage"
		to=$2; shift ;;
	*)
		[ -z "$dir" ] || die "usage: $usage"
		dir=$1 ;;
	esac
	shift
done
session
echo "session: $id"
[ -z "$profiles" ] || echo "profile: ${profile:-not in the screen name (a session from before profiles)}"
echo "current: $(pwd)"
if [ -n "$to" ]; then
	[ -n "$profiles" ] || die "--profile needs CLAUDE_REMOTE_PROFILES, which this session does not have"
	names | grep -qxF -- "$to" || die "no profile '$to'; the profiles are: $(names | paste -sd' ')"
	[ "$to" != "$profile" ] || [ -n "$dir" ] || die "this session already runs in profile '$to'"
	echo "move:    to profile $to; it comes back under that account, and its old entry stays in the" \
		"${profile:-current} account's Claude apps until it is archived there by hand"
fi
if [ -z "$dir$to" ]; then
	$check || die "usage: $usage"
	exit 0
fi

repo=
if [ -n "$dir" ]; then
	repo=$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null) || die "$dir is not inside a git repository"
	echo "target:  $repo ($(git -C "$repo" remote get-url origin 2>/dev/null || echo 'no origin remote'))"
fi
help=$("$CRCTL" help 2>&1) || true
grep -q -- '--trust' <<<"$help" ||
	die "$CRCTL has no 'restart --dir --trust'; upgrade code-remote (crctl) to a version that does"
# Only a crctl whose help says a session "moves" between profiles finds a
# session under a profile's screen name and moves it. Its help is read rather
# than a dry run of restart, which would open the session store first.
[ -z "$profiles" ] || grep -q 'moves the session there' <<<"$help" ||
	die "$CRCTL cannot restart a session under profiles or move it between them; upgrade code-remote (crctl) to a version that can"
$check && exit 0

log=${XDG_STATE_HOME:-$HOME/.local/state}/crctl/self-restart-$id.log
mkdir -p "$(dirname "$log")"
# Own session + no terminal: the helper must outlive the screen it restarts.
setsid nohup "$0" --run "$id" "$prefix" "$repo" "$to" >"$log" 2>&1 </dev/null &
if [ -n "$to" ]; then
	echo "scheduled: moves to profile $to${repo:+ in $repo} once this session is idle (log: $log)"
else
	echo "scheduled: restarts into $repo once this session is idle (log: $log)"
fi
