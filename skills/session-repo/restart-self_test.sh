#!/usr/bin/env bash
# Tests for restart-self.sh, against a stub crctl that only logs its
# arguments. Nothing here starts a session or touches ~/.claude.
#   bash skills/session-repo/restart-self_test.sh
set -uo pipefail

script=${RESTART_SELF:-$(cd "$(dirname "$0")" && pwd)/restart-self.sh}
tmp=$(cd "$(mktemp -d)" && pwd -P)
trap 'rm -r "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/home/.claude/sessions" "$tmp/claude/sessions" "$tmp/busy/sessions" "$tmp/repo"
HOME="$tmp/home" git -C "$tmp/repo" init -q
cat >"$tmp/bin/crctl" <<'EOF'
#!/bin/sh
if [ "$1" = help ]; then
	echo "  --trust"
	[ "${STUB_OLD:-}" ] || echo "  one moves the session there: its conversation is copied"
	exit 0
fi
echo "prefix=${CLAUDE_REMOTE_SESSION_PREFIX:-} $*" >>"$STUB_LOG"
EOF
chmod +x "$tmp/bin/crctl"
echo '{"pid":42,"status":"idle"}' | tee "$tmp/claude/sessions/42.json" >"$tmp/home/.claude/sessions/42.json"
echo '{"pid":42,"status":"busy"}' >"$tmp/busy/sessions/42.json"

id=0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b
profiles='work=~/.claude, private=~/.claude-private,my-lab=/srv/lab'
fails=0
# rs [VAR=value...] -- args: runs the script in a clean environment and sets
# $out (stdout and stderr) and $rc. It runs in $cwd, else in its $HOME. crctl
# is the stub by its full path, so a helper that outlives the test cannot run
# a real one.
rs() {
	local env=()
	while [ "$1" != -- ]; do env+=("$1"); shift; done
	shift
	out=$(cd "${cwd:-$tmp/home}" && env -i HOME="$tmp/home" PATH="$tmp/bin:/usr/bin:/bin" \
		CRCTL="$tmp/bin/crctl" STUB_LOG="$tmp/crctl.log" "${env[@]}" bash "$script" "$@" 2>&1)
	rc=$?
}
fail() { echo "FAIL: $1"; printf '%s\n' "$out" | sed 's/^/    /'; fails=$((fails + 1)); }
want() { # want <rc> <text> <case>: rc matches and the output has the text
	if [ "$rc" != "$1" ] || ! grep -qF -- "$2" <<<"$out"; then fail "$3 (rc=$rc, want $1 and '$2')"; fi
}
lacks() { ! grep -qF -- "$1" <<<"$out" || fail "$2"; }
# ran <line> <case>: the detached helper, within 10s, ran the stub crctl so.
ran() {
	for _ in $(seq 50); do [ -s "$tmp/crctl.log" ] && break; sleep 0.2; done
	grep -qxF -- "$1" "$tmp/crctl.log" || fail "$2: $(cat "$tmp/crctl.log")"
}

rs STY=1.pigri-dev-remote-$id -- --check
if [ "$rc" != 0 ] || [ "$out" != "session: $id
current: $tmp/home" ]; then fail "legacy --check without profiles"; fi
rs STY=1.pigri-dev-remote-$id -- --check --profile work
want 1 "needs CLAUDE_REMOTE_PROFILES" "--profile without profiles"

P=CLAUDE_REMOTE_PROFILES=$profiles
rs "$P" STY=1.pigri-dev-remote-$id -- --check
want 0 "profile: not in the screen name" "legacy name with profiles"
for p in work private my-lab; do
	rs "$P" STY=1.pigri-dev-remote-$p-$id -- --check
	want 0 "profile: $p" "$p name"
	want 0 "session: $id" "$p name id"
done
rs "$P" CLAUDE_REMOTE_SESSION_PREFIX=a-b STY=1.a-b-my-lab-$id -- --check
want 0 "profile: my-lab" "hyphenated prefix and profile"
rs "$P" STY=1.pigri-dev-remote-other-$id -- --check
want 1 "names profile 'other'" "unconfigured profile in the name"
rs "$P" STY=1.someone-$id -- --check
want 1 "does not start with 'pigri-dev-remote-'" "foreign prefix"
cwd=$tmp/repo rs "$P" STY=1.pigri-dev-remote-work-$id -- --check --profile private
want 0 "move:    to profile private" "move check"
want 0 "stays in the work account's Claude apps" "move warning"
want 0 "trust:   $tmp/repo (in profile private)" "move trusts the current dir"
rs "$P" STY=1.pigri-dev-remote-work-$id -- --check --profile private
want 1 "runs in $tmp/home, which the move would mark as a trusted folder" "move from the home dir"
cwd=/ rs "$P" STY=1.pigri-dev-remote-work-$id -- --check --profile private
want 1 "runs in /, which the move would mark" "move from /"
rs "$P" STY=1.pigri-dev-remote-work-$id -- --check --profile private "$tmp/repo"
want 0 "trust:   $tmp/repo (in profile private)" "move into a repo trusts it"
rs "$P" STY=1.pigri-dev-remote-work-$id -- --check --profile work
want 1 "already runs in profile 'work'" "same profile"
rs "$P" STY=1.pigri-dev-remote-work-$id -- --check --profile work "$tmp/repo"
want 0 "target:  $tmp/repo" "same profile with a dir"
rs "$P" STY=1.pigri-dev-remote-work-$id -- --check --profile nope
want 1 "no profile 'nope'; the profiles are: work private my-lab" "unknown profile"
cwd=$tmp/repo rs "$P" STUB_OLD=1 STY=1.pigri-dev-remote-work-$id -- --check --profile private
want 1 "cannot restart a session under profiles" "crctl without the move"
rs STUB_OLD=1 STY=1.pigri-dev-remote-$id -- --check "$tmp/repo"
want 0 "target:  $tmp/repo" "crctl without the move, no profiles"

# The helper, run in the foreground: the session is idle, so it runs crctl
# after its 2s grace period.
: >"$tmp/crctl.log"
S=SESSIONS_DIR=$tmp/claude/sessions
rs "$S" CLAUDE_PID=42 -- --run "$id" pigri-dev-remote "$tmp/repo"
grep -qxF "prefix=pigri-dev-remote restart $id --dir $tmp/repo --trust" "$tmp/crctl.log" ||
	fail "helper without profiles: $(cat "$tmp/crctl.log")"
: >"$tmp/crctl.log"
rs "$P" "$S" CLAUDE_PID=42 -- --run "$id" "" "" private
grep -qxF "prefix= restart $id --profile private --trust" "$tmp/crctl.log" ||
	fail "helper for a move: $(cat "$tmp/crctl.log")"
# Its idle status comes from ~/.claude (no CLAUDE_CONFIG_DIR), not from an
# inherited CLAUDE_HOME, where the session would look busy.
: >"$tmp/crctl.log"
rs "$P" CLAUDE_PID=42 CLAUDE_HOME="$tmp/busy" IDLE_WAIT=2 -- --run "$id" "" "" private
grep -qxF "prefix= restart $id --profile private --trust" "$tmp/crctl.log" ||
	fail "helper status under profiles ignores CLAUDE_HOME: $(cat "$tmp/crctl.log")"

# Scheduling, end to end: the detached helper runs crctl once idle.
: >"$tmp/crctl.log"
rs "$P" "$S" CLAUDE_PID=42 XDG_STATE_HOME="$tmp/state" STY=1.pigri-dev-remote-work-$id -- --profile private "$tmp/repo"
want 0 "scheduled: moves to profile private in $tmp/repo" "schedule a move"
ran "prefix= restart $id --dir $tmp/repo --profile private --trust" "scheduled move"
: >"$tmp/crctl.log"
rs "$S" CLAUDE_PID=42 XDG_STATE_HOME="$tmp/state" STY=1.a-b-c-$id -- "$tmp/repo"
want 0 "scheduled: restarts into $tmp/repo" "schedule without profiles"
ran "prefix=a-b-c restart $id --dir $tmp/repo --trust" "scheduled restart without profiles"

if [ "$fails" != 0 ]; then echo "restart-self: $fails failed"; exit 1; fi
echo "restart-self: all tests passed"
