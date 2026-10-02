package session

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Tunables for the upgrade/restart flow; vars so tests can shorten them.
var (
	upgradeTimeout = 5 * time.Minute        // cap on `claude update`
	stopTimeout    = 5 * time.Second        // wait for a killed screen/claude to go away
	stopPoll       = 100 * time.Millisecond // poll interval while waiting
)

// Restart outcomes reported per session by Upgrade.
const (
	ActionRestarted = "restarted"
	ActionSkipped   = "skipped"
	ActionFailed    = "failed"
)

// ErrInvalidID is returned by Upgrade when asked to restart something that
// isn't a session id. Callers can errors.Is to surface it as a 400.
var ErrInvalidID = errors.New("invalid session id")

// RestartOutcome is what Upgrade did with one running session.
type RestartOutcome struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Action string `json:"action"` // restarted | skipped | failed
	Reason string `json:"reason,omitempty"`
}

// UpgradeResult reports a claude upgrade: the installed version before and
// after, the updater's output, and (when restarts were requested) the outcome
// for each running session.
type UpgradeResult struct {
	Before   string           `json:"before,omitempty"`
	After    string           `json:"after,omitempty"`
	Output   string           `json:"output,omitempty"`
	Sessions []RestartOutcome `json:"sessions"`
}

// Version returns the installed claude version (e.g. "2.1.287"), or "" when it
// can't be determined.
func (m *Manager) Version() string {
	out, err := execCommand(m.ClaudeBin, "--version").Output()
	if err != nil {
		return ""
	}
	// "2.1.287 (Claude Code)" -> "2.1.287"
	if f := strings.Fields(string(out)); len(f) > 0 {
		return f[0]
	}
	return ""
}

// Upgrade runs `claude update` and then, when restart is set, restarts the
// running sessions so they pick up the new binary. Each restart resumes the
// ORIGINAL session id (see Restart), so the conversation, screen name, and
// Remote Control name all carry over.
//
// Only sessions that need it are restarted: one already running the installed
// version is skipped, and so is one that isn't idle (restarting would cut off
// an in-flight turn) unless force is set. A session with no entry in claude's
// process registry can't be judged either way and is restarted.
//
// With ids, only those sessions are considered (under the same rules); one that
// isn't running is reported as skipped — it picks up the new version whenever
// it is next resumed.
//
// If the update itself fails nothing is restarted and the error (with the
// updater's output in the result) is returned.
func (m *Manager) Upgrade(restart, force bool, ids ...string) (UpgradeResult, error) {
	for _, id := range ids {
		if !m.ValidID(id) {
			return UpgradeResult{Sessions: []RestartOutcome{}}, fmt.Errorf("%w: %q", ErrInvalidID, id)
		}
	}

	m.upgradeMu.Lock()
	defer m.upgradeMu.Unlock()

	res := UpgradeResult{Before: m.Version(), Sessions: []RestartOutcome{}}

	var buf bytes.Buffer
	cmd := execCommand(m.ClaudeBin, "update")
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("claude update: %w", err)
	}
	timer := time.AfterFunc(upgradeTimeout, func() { _ = cmd.Process.Kill() })
	err := cmd.Wait()
	timer.Stop()
	res.Output = strings.TrimSpace(buf.String())
	if err != nil {
		return res, fmt.Errorf("claude update: %w", err)
	}
	res.After = m.Version()

	if !restart {
		return res, nil
	}

	running, err := m.List()
	if err != nil {
		return res, err
	}
	if len(ids) > 0 {
		byID := make(map[string]Session, len(running))
		for _, s := range running {
			byID[s.ID] = s
		}
		running = running[:0:0]
		for _, id := range ids {
			if s, ok := byID[id]; ok {
				running = append(running, s)
			} else {
				res.Sessions = append(res.Sessions, RestartOutcome{ID: id, Action: ActionSkipped, Reason: "not running"})
			}
		}
	}
	live := m.liveIndex()
	for _, s := range running {
		info := live(s)
		reg, known := info.Reg, info.Known
		o := RestartOutcome{ID: s.ID, Title: m.convTitle(s.ID, info.Conv)}
		switch {
		case known && res.After != "" && reg.Version == res.After:
			o.Action, o.Reason = ActionSkipped, "already on "+res.After
		case known && !force && reg.Status != "" && reg.Status != "idle":
			o.Action, o.Reason = ActionSkipped, reg.Status+" (use force to restart anyway)"
		default:
			if _, rerr := m.restart(s, info); rerr != nil {
				o.Action, o.Reason = ActionFailed, rerr.Error()
			} else {
				o.Action = ActionRestarted
			}
		}
		res.Sessions = append(res.Sessions, o)
	}
	return res, nil
}

// Restart stops a running session and relaunches it under the same screen and
// Remote Control name, resuming its conversation (`claude --resume`) in the
// original working directory. A session that is already stopped is simply
// resumed.
//
// The conversation resumed is the one the session is in NOW: claude can move a
// running session onto a new conversation id (e.g. after /clear), in which case
// the id it was launched with points at a stale log. See liveIndex for how the
// current conversation is determined.
//
// It refuses (ErrNotResumable) before killing anything when there is no on-disk
// log to resume from, so a restart can't turn into a plain kill.
func (m *Manager) Restart(id string) (Session, error) {
	if !m.ValidID(id) {
		return Session{}, fmt.Errorf("%w: %q is not a valid session id", ErrNotResumable, id)
	}
	s, running, err := m.Get(id)
	if err != nil {
		return Session{}, err
	}
	if !running {
		return m.Resume(id)
	}
	return m.restart(s, m.liveIndex()(s))
}

// restart is Restart for a session known to be running; info is what liveIndex
// found for it.
func (m *Manager) restart(s Session, info liveInfo) (Session, error) {
	id, conv := s.ID, info.Conv
	if m.ClaudeHome != "" && !m.HasSessionLog(conv) {
		return Session{}, fmt.Errorf("%w: no claude session log for %s (not restarting)", ErrNotResumable, conv)
	}

	// Wait for the claude process itself, not just its screen, to exit before a
	// second claude opens the same conversation.
	pid := info.Reg.PID

	if _, err := m.Kill(id); err != nil {
		return Session{}, err
	}
	deadline := time.Now().Add(stopTimeout)
	for {
		_, alive, _ := m.Get(id)
		if !alive && !pidAlive(pid) {
			break
		}
		if time.Now().After(deadline) {
			if alive {
				return Session{}, fmt.Errorf("restart %s: screen session did not stop", id)
			}
			break // screen is gone; don't block forever on a lingering process
		}
		time.Sleep(stopPoll)
	}
	return m.resume(id, conv)
}

// liveInfo is what is known about a running session's claude process.
type liveInfo struct {
	Reg   Registration // its entry in claude's process registry
	Known bool         // false when it has no registry entry (Reg is zero)
	Conv  string       // the conversation it is in; never empty
}

// resumeArgRe pulls the conversation id out of a claude command line as we
// launch it (`--session-id <uuid>` on create, `--resume <uuid>` on resume).
var resumeArgRe = regexp.MustCompile(`--(?:resume|session-id)[ =]([0-9a-f-]{36})`)

// liveIndex returns a lookup from a running session to its claude process.
//
// The process is found by parentage — the claude whose parent is the session's
// screen — because the registry's session id drifts from ours once claude moves
// the session onto a new conversation (e.g. /clear); matching the registry by
// session id is only the fallback for when the process table can't be read.
//
// The session's current conversation is, in order of trust: the registry
// entry's session id; the conversation last recorded in the Store; the one on
// the process's command line (what it was launched with); the session id
// itself. Candidates without an on-disk log are passed over, so Conv is always
// something that can be resumed if anything is.
func (m *Manager) liveIndex() func(Session) liveInfo {
	regs, _ := m.Registrations()
	byID := make(map[string]Registration, len(regs))
	byPID := make(map[string]Registration, len(regs))
	for _, r := range regs {
		byID[r.SessionID] = r
		if r.PID > 0 {
			byPID[strconv.Itoa(r.PID)] = r
		}
	}

	// One pass over the process table: each screen's child and its argv.
	type proc struct{ pid, args string }
	child := map[string]proc{} // parent pid -> child
	out, _ := execCommand("ps", "-e", "-o", "pid=,ppid=,args=").Output()
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		p := proc{pid: f[0], args: strings.Join(f[2:], " ")}
		// A screen has one child (claude); if a pid somehow has several, prefer
		// the one claude registered.
		if _, dup := child[f[1]]; dup {
			if _, registered := byPID[p.pid]; !registered {
				continue
			}
		}
		child[f[1]] = p
	}

	usable := func(conv string) bool {
		return m.ValidID(conv) && (m.ClaudeHome == "" || m.HasSessionLog(conv))
	}
	return func(s Session) liveInfo {
		info := liveInfo{Conv: s.ID}
		p, hasProc := child[s.PID]
		if s.PID == "" {
			hasProc = false
		}
		if hasProc {
			info.Reg, info.Known = byPID[p.pid]
		}
		if !info.Known {
			info.Reg, info.Known = byID[s.ID]
		}

		candidates := []string{info.Reg.SessionID}
		if c := m.storedConv(s.ID); c != s.ID { // s.ID means "nothing recorded"
			candidates = append(candidates, c)
		}
		if hasProc {
			if a := resumeArgRe.FindStringSubmatch(p.args); a != nil {
				candidates = append(candidates, a[1])
			}
		}
		for _, c := range candidates {
			if c != "" && usable(c) {
				info.Conv = c
				break
			}
		}
		return info
	}
}

// pidAlive reports whether a process with the given pid exists. A zero pid
// (unknown) counts as not alive.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
