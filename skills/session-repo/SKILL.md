---
name: session-repo
description: Restart this Claude session inside the git repository it is working on, keeping the conversation. Use when a remote (crctl / screen) session was started outside its repo - e.g. in the home directory - so the Claude apps show no branch, diff or pull request for it; or when the user asks to restart this session, move it, or point it at a repo ("restart yourself in the api repo", "PRs aren't showing up for this session").
---

# session-repo

A session registers its git repository once, at launch, from the directory it
starts in. Started anywhere else (typically `$HOME`), it has no repository, so
the Claude apps cannot show its branch, diff or pull requests - even though the
work itself is fine. The fix is to restart the session inside the repo. `crctl`
does that and resumes the same conversation; `restart-self.sh` (next to this
file) lets the session do it to itself.

## Steps

1. **Check where you are.** Run `restart-self.sh --check`. It prints this
   session's id and current directory, or says this claude is not a
   crctl-managed screen session - in which case stop and tell the user; there
   is nothing to restart.

2. **Pick the repository.** Use the one the user named. Otherwise use the repo
   this conversation has been working in (where the commits and PRs went). If
   the work spans several repos, or you are not sure, ask - a session has one
   primary repo and a wrong guess costs a second restart. The restart marks the
   repo as a trusted folder for Claude (nobody can answer the trust prompt of
   a detached session), so only move into a repo the user works in or named.
   If the session already runs inside the right repo, say so and stop.

3. **Confirm the target.** Run `restart-self.sh --check <dir>`. It resolves the
   repo root and its origin remote and verifies `crctl` supports the move.
   Relay any error as is; do not work around it.

4. **Wrap up, then schedule.** The restart ends this turn's process, so first
   finish or park whatever is in flight: no half-written files, no running
   builds you still need. Background shells and their state do not survive;
   uncommitted changes in the working tree do. Tell the user in one or two
   lines that the session is about to restart into `<repo>` and will come back
   with the same history - and that the Claude apps will list it as a new
   session entry (same name), with the old entry archived.

5. **Run `restart-self.sh <dir>` as the last action of the turn**, then end the
   turn. The script returns immediately; a detached helper waits until the
   session is idle and then runs `crctl restart <id> --dir <repo>`. Do not run
   `crctl restart` on your own session directly - it would kill the command
   mid-flight - and do not keep working after scheduling, since the helper
   waits for idle and anything started now is cut off.

## Afterwards

The session comes back in the repo, as a fork of the conversation (same
history, registered afresh so the repository is attached), and stays there
across later restarts and upgrades. If the user says it did not come back, the helper's output is in
`~/.local/state/crctl/self-restart-<id>.log`, and `crctl ls` shows whether the
session is running or stopped (`crctl resume <id>` brings a stopped one back).
