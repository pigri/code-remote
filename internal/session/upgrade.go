package session

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claude-remote-api/internal/profile"
)

// Tunables for the upgrade/restart flow; vars so tests can shorten them.
var (
	upgradeTimeout = 5 * time.Minute        // cap on `claude update`
	versionTimeout = 5 * time.Second        // cap on `claude --version`
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
	// Bounded: this runs on every listing, and a claude that doesn't answer
	// promptly must not hang it.
	out, err := runTimeout(execCommand(m.ClaudeBin, "--version"), versionTimeout)
	if err != nil {
		return ""
	}
	// "2.1.287 (Claude Code)" -> "2.1.287"
	if f := strings.Fields(out); len(f) > 0 {
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

	out, err := runTimeout(execCommand(m.ClaudeBin, "update"), upgradeTimeout)
	res.Output = strings.TrimSpace(out)
	if err != nil {
		return res, fmt.Errorf("claude update: %w", err)
	}
	res.After = m.Version()

	if !restart {
		return res, nil
	}

	running, c := m.list()
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
	live := m.liveIndex(c)
	for _, s := range running {
		info := live(s)
		reg, known := info.Reg, info.Known
		p := m.sessionProfile(s)
		o := RestartOutcome{ID: s.ID, Title: convTitle(p.Dir, s.ID, info.Conv)}
		switch {
		case known && res.After != "" && reg.Version == res.After:
			o.Action, o.Reason = ActionSkipped, "already on "+res.After
		case known && !force && reg.Status != "" && reg.Status != "idle":
			o.Action, o.Reason = ActionSkipped, reg.Status+" (use force to restart anyway)"
		default:
			if _, rerr := m.restart(s, info, "", p); rerr != nil {
				o.Action, o.Reason = ActionFailed, rerr.Error()
			} else {
				o.Action = ActionRestarted
			}
		}
		res.Sessions = append(res.Sessions, o)
	}
	return res, nil
}

// runTimeout runs cmd to completion, killing it after timeout, and returns its
// combined output.
func runTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	timer := time.AfterFunc(timeout, func() { _ = cmd.Process.Kill() })
	err := cmd.Wait()
	timer.Stop()
	return buf.String(), err
}

// Restart stops a running session and relaunches it under the same screen and
// Remote Control name, resuming its conversation (`claude --resume`) in the
// working directory it was running in. A session that is already stopped is
// simply resumed.
//
// The conversation resumed is the one the session is in NOW: claude can move a
// running session onto a new conversation id (e.g. after /clear), in which case
// the id it was launched with points at a stale log. See liveIndex for how the
// current conversation is determined.
//
// It refuses (ErrNotResumable) before killing anything when there is no on-disk
// log to resume from, so a restart can't turn into a plain kill.
func (m *Manager) Restart(id string) (Session, error) { return m.RestartIn(id, "", "") }

// RestartIn is Restart with the session moved to working directory dir, under
// profile prof (see ResumeIn). A bad dir or profile is refused before anything
// is stopped.
func (m *Manager) RestartIn(id, dir, prof string) (Session, error) {
	if !m.ValidID(id) {
		return Session{}, fmt.Errorf("%w: %q is not a valid session id", ErrNotResumable, id)
	}
	s, running, c := m.get(id)
	var own profile.Profile
	if running {
		own = m.sessionProfile(s)
	} else {
		own = m.profileOf(id)
		// Not found comes before any check of prof, as it would without one.
		if prof != "" && !m.hasConv(own, id) {
			return Session{}, fmt.Errorf("%w: no session %s", ErrNotResumable, id)
		}
	}
	p, err := m.launchProfile(id, own, prof)
	if err != nil {
		return Session{}, err
	}
	if !running {
		return m.resume(id, m.storedConv(p.Dir, id), dir, p)
	}
	return m.restart(s, m.liveIndex(c)(s), dir, p)
}

// restart is RestartIn for a session known to be running in profile p; info is
// what liveIndex found for it.
func (m *Manager) restart(s Session, info liveInfo, dir string, p profile.Profile) (Session, error) {
	info = m.reread(s, info)
	id, conv := s.ID, info.Conv
	if dir != "" {
		if _, err := m.resolveDir(dir); err != nil {
			return Session{}, err
		}
	}
	if p.Dir != "" && logIn(p.Dir, conv) == "" {
		return Session{}, fmt.Errorf("%w: no claude session log for %s (not restarting)", ErrNotResumable, conv)
	}

	// Wait for the claude process itself, not just its screen, to exit before a
	// second claude opens the same conversation.
	pid := info.Reg.PID

	if _, up := m.running(id); up {
		if err := m.kill(s, info); err != nil {
			return Session{}, err
		}
	}
	deadline := time.Now().Add(stopTimeout)
	for {
		_, alive := m.running(id)
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
	return m.resume(id, conv, dir, p)
}

// liveInfo is what is known about a running session's claude process.
type liveInfo struct {
	Reg   Registration // its entry in claude's process registry
	Known bool         // false when it has no registry entry (Reg is zero)
	Conv  string       // the conversation it is in; never empty
	args  string       // its command line, when its process was found
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
func (m *Manager) liveIndex(c *regCache) func(Session) liveInfo {
	ix := c.index()
	return func(s Session) liveInfo {
		prof := m.sessionProfile(s)
		var info liveInfo
		var p proc
		var hasProc bool
		info.Reg, info.Known, p, hasProc = ix.lookup(s, prof.Name)
		if hasProc {
			info.args = p.args
		}
		info.Conv = m.liveConv(s, prof.Dir, info)
		return info
	}
}

// liveConv is the conversation running session s, logged under home, is in,
// given what liveIndex found (info): see liveIndex.
func (m *Manager) liveConv(s Session, home string, info liveInfo) string {
	usable := func(conv string) bool {
		return m.ValidID(conv) && (home == "" || logIn(home, conv) != "")
	}
	candidates := []string{info.Reg.SessionID}
	if c := m.storedConv(home, s.ID); c != s.ID { // s.ID means "nothing recorded"
		candidates = append(candidates, c)
	}
	if a := resumeArgRe.FindStringSubmatch(info.args); a != nil {
		candidates = append(candidates, a[1])
	}
	for _, c := range candidates {
		if c != "" && usable(c) {
			return c
		}
	}
	return s.ID
}

// reread is info with s's registration read again from its file, so a stop
// that comes a while after the index was built notes where s is by then.
func (m *Manager) reread(s Session, info liveInfo) liveInfo {
	if !info.Known || info.Reg.path == "" {
		return info
	}
	regs := readRegistrations([]string{info.Reg.path}, info.Reg.Profile)
	if len(regs) != 1 || regs[0].PID != info.Reg.PID {
		return info
	}
	info.Reg = regs[0]
	info.Conv = m.liveConv(s, m.sessionProfile(s).Dir, info)
	return info
}

// proc is a process: its pid and its command line.
type proc struct{ pid, args string }

// regIndex joins running sessions to their claude process and its entry in
// some profile's registry.
type regIndex struct {
	byID, byPID map[string][]Registration
	child       map[string]proc // screen pid -> its child
}

// regCache builds a regIndex on first use and keeps it, so one operation
// reads the registries and the process table at most once.
type regCache struct {
	m  *Manager
	ix *regIndex
}

func (c *regCache) index() regIndex {
	if c.ix == nil {
		ix := c.m.regIndex()
		c.ix = &ix
	}
	return *c.ix
}

// regSkew is how much older than the start of the process holding its pid a
// registry file may look and still be that process's own: ps gives the start
// in whole seconds.
const regSkew = 2 * time.Second

// regIndex reads every profile's registry and the process table once. An
// entry whose file was last written before the process now holding its pid
// started was left by an earlier process, one that crashed, and is dropped.
func (m *Manager) regIndex() regIndex {
	out, _ := execCommand("ps", "-e", "-o", "pid=,ppid=,etimes=,args=").Output()
	now := time.Now()
	started := map[string]time.Time{}
	var procs [][2]string // ppid, pid
	args := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		et, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		started[f[0]] = now.Add(-time.Duration(et) * time.Second)
		procs = append(procs, [2]string{f[1], f[0]})
		args[f[0]] = strings.Join(f[3:], " ")
	}
	regs, _ := m.Registrations()
	ix := regIndex{
		byID:  make(map[string][]Registration, len(regs)),
		byPID: make(map[string][]Registration, len(regs)),
		child: map[string]proc{},
	}
	for _, r := range regs {
		pid := strconv.Itoa(r.PID)
		if st, ok := started[pid]; ok && r.PID > 0 && !r.mtime.IsZero() && r.mtime.Before(st.Add(-regSkew)) {
			continue
		}
		ix.byID[r.SessionID] = append(ix.byID[r.SessionID], r)
		if r.PID > 0 {
			ix.byPID[pid] = append(ix.byPID[pid], r)
		}
	}
	for _, pp := range procs {
		ppid, pid := pp[0], pp[1]
		// A screen has one child (claude); if a pid somehow has several, prefer
		// the one claude registered.
		if _, dup := ix.child[ppid]; dup && len(ix.byPID[pid]) == 0 {
			continue
		}
		ix.child[ppid] = proc{pid: pid, args: args[pid]}
	}
	return ix
}

// lookup finds running session s's claude process and its registration, for
// a session in profile prof: by the process's pid, else by s's id. When s's
// profile is a guess (see resolvedBy), so is prof.
func (ix regIndex) lookup(s Session, prof string) (reg Registration, known bool, p proc, hasProc bool) {
	guess := s.by == byGuess
	p, hasProc = ix.child[s.PID]
	hasProc = hasProc && s.PID != ""
	ids := []string{s.ID}
	if hasProc {
		if a := resumeArgRe.FindStringSubmatch(p.args); a != nil {
			ids = append(ids, a[1])
		}
		if reg, known = pickReg(ix.byPID[p.pid], prof, guess, ids); known {
			return reg, known, p, hasProc
		}
	}
	reg, known = pickReg(ix.byID[s.ID], prof, guess, ids)
	return reg, known, p, hasProc
}

// pickReg is the registration among regs, which share a pid or a session id,
// for a session in profile prof. Within one profile the last read wins.
// Across profiles a stale file left by a crashed claude can share a live
// claude's pid, so an entry in another profile proves nothing, and neither
// does one in a profile that was guessed. Then an entry in prof wins, unless
// prof is a guess; else one naming the session (its id or the conversation on
// its claude's command line, in ids); else none is picked.
func pickReg(regs []Registration, prof string, guess bool, ids []string) (Registration, bool) {
	if len(regs) == 0 {
		return Registration{}, false
	}
	last := regs[len(regs)-1]
	if !slices.ContainsFunc(regs, func(r Registration) bool { return r.Profile != last.Profile }) {
		return last, true
	}
	for i := len(regs) - 1; i >= 0 && !guess; i-- {
		if regs[i].Profile == prof {
			return regs[i], true
		}
	}
	for i := len(regs) - 1; i >= 0; i-- {
		if slices.Contains(ids, regs[i].SessionID) {
			return regs[i], true
		}
	}
	return Registration{}, false
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
