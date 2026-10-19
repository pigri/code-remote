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
// Remote Control name all carry over (a screen still named <prefix>-<id> is
// renamed after its profile, see launchName).
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
		info := m.reread(s, live(s)) // as it is now, after the restarts before it
		reg, known := info.Reg, info.Known
		p := m.sessionProfile(s)
		o := RestartOutcome{ID: s.ID, Title: convTitle(p.Dir, s.ID, info.Conv)}
		switch {
		case known && res.After != "" && reg.Version == res.After:
			o.Action, o.Reason = ActionSkipped, "already on "+res.After
		case known && !force && reg.Status != "" && reg.Status != "idle":
			o.Action, o.Reason = ActionSkipped, reg.Status+" (use force to restart anyway)"
		default:
			if _, rerr := m.restartAs(s, info, "", p, p); rerr != nil {
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
// Remote Control name (a screen still named <prefix>-<id> is renamed after its
// profile, see launchName), resuming its conversation (`claude --resume`) in the
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
// profile prof (see ResumeIn). A bad dir or profile, or a move that would
// overwrite a different copy of the conversation, is refused before anything
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
	p, err := m.launchProfile(own, prof)
	if err != nil {
		return Session{}, err
	}
	if !running {
		return m.resume(id, m.storedConv(own.Dir, id), dir, own, p)
	}
	return m.restart(s, m.liveIndex(c)(s), dir, p)
}

// restart is RestartIn for a running session, relaunched in profile p; info is
// what liveIndex found for it.
func (m *Manager) restart(s Session, info liveInfo, dir string, p profile.Profile) (Session, error) {
	return m.restartAs(s, m.reread(s, info), dir, m.sessionProfile(s), p)
}

// restartAs is restart with info already read again, for s running in
// profile from.
func (m *Manager) restartAs(s Session, info liveInfo, dir string, from, p profile.Profile) (Session, error) {
	id, conv := s.ID, info.Conv
	if dir != "" {
		if _, err := m.resolveDir(dir); err != nil {
			return Session{}, err
		}
	}
	if from.Dir != "" && logIn(from.Dir, conv) == "" {
		return Session{}, fmt.Errorf("%w: no claude session log for %s (not restarting)", ErrNotResumable, conv)
	}
	if from.Name != p.Name {
		if _, err := m.planMove(from, p, conv); err != nil {
			return Session{}, err
		}
	}

	// Wait for the claude process itself, not just its screen, to exit before a
	// second claude opens the same conversation.
	pid := info.Reg.PID

	// A screen of id that s was not listed with (a profile not configured
	// here) would keep its claude running past the stop, and the relaunch
	// could not start beside it.
	live := m.screensOf(id)
	for _, l := range live {
		if !slices.Contains(s.screenTargets(), l.PID+"."+l.Screen) {
			return Session{}, fmt.Errorf("%w: %s also runs as screen %s, which is not listed here; not restarting",
				ErrAlreadyRunning, id, l.Screen)
		}
	}
	if len(live) > 0 {
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
	return m.resume(id, conv, dir, from, p)
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
		info.Reg, info.Known, p, hasProc = ix.lookup(s)
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
// that comes a while after the index was built notes where s is by then. A
// file another process has written at the pid since is not s's.
func (m *Manager) reread(s Session, info liveInfo) liveInfo {
	if !info.Known || info.Reg.path == "" {
		return info
	}
	regs := rereadFile(info.Reg.path, info.Reg.Profile)
	if len(regs) != 1 || regs[0].PID != info.Reg.PID || regs[0].procStart != info.Reg.procStart {
		return info
	}
	regs[0].owned = info.Reg.owned
	info.Reg = regs[0]
	info.Conv = m.liveConv(s, m.sessionProfile(s).Dir, info)
	return info
}

// rereadFile reads one registry file again. A seam for tests.
var rereadFile = func(path, profile string) []Registration {
	return readRegistrations([]string{path}, profile)
}

// proc is a process: its pid and its command line.
type proc struct{ pid, args string }

// regIndex joins running sessions to their claude process and its entry in
// some profile's registry.
type regIndex struct {
	byID, byPID map[string][]Registration
	child       map[string]proc    // screen pid -> its child
	owner       func(string) owner // the profile a pid runs under; nil with one profile
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

// regIndex reads every profile's registry and the process table once. An
// entry counts only while its pid is alive, and only from the process that
// wrote it: a crashed claude leaves its file behind, and another claude can
// get its pid.
func (m *Manager) regIndex() regIndex {
	out, _ := execCommand("ps", "-e", "-o", "pid=,ppid=,args=").Output()
	var procs [][2]string // ppid, pid
	args := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		procs = append(procs, [2]string{f[1], f[0]})
		args[f[0]] = strings.Join(f[2:], " ")
	}
	regs, _ := m.Registrations()
	ix := regIndex{
		byID:  make(map[string][]Registration, len(regs)),
		byPID: make(map[string][]Registration, len(regs)),
		child: map[string]proc{},
	}
	if m.Profiles.Multi() {
		owners := map[string]owner{}
		ix.owner = func(pid string) owner {
			o, seen := owners[pid]
			if !seen {
				o = m.procProfile(pid)
				owners[pid] = o
			}
			return o
		}
	}
	for _, r := range regs {
		pid := strconv.Itoa(r.PID)
		if _, alive := args[pid]; r.PID > 0 && (len(procs) > 0 && !alive || !wroteBy(r, pid)) {
			continue // its process is gone, or another one has its pid
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

// wroteBy reports whether r may have been written by process pid: unless r
// records its process's start (procStart) and pid's differs. The start is in
// clock ticks since boot, which a step of the wall clock does not move.
func wroteBy(r Registration, pid string) bool {
	if r.procStart == "" {
		return true // an older claude records none
	}
	start, err := procStart(pid)
	return err != nil || start == r.procStart
}

// owner is the profile a process runs under, when known.
type owner struct {
	name  string
	known bool // false when its environment is unreadable, empty or names no profile
}

// procProfile is the profile process pid runs under, as the CLAUDE_CONFIG_DIR
// in its environment names it (see profile.Set.ForConfigDir).
func (m *Manager) procProfile(pid string) owner {
	env, err := m.environ(pid)
	if err != nil || !slices.ContainsFunc(env, func(kv string) bool { return kv != "" }) {
		return owner{} // a zombie's environment is empty
	}
	dir := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CONFIG_DIR="); ok {
			dir = v
		}
	}
	p, ok := m.Profiles.ForConfigDir(dir)
	return owner{name: p.Name, known: ok}
}

// lookup finds running session s's claude process and its registration: by
// the process's pid, else by s's id. With several profiles, only an entry in
// the profile the process's environment names is owned, and may tell which
// profile s is in. Without that, an entry in s's own profile still tells its
// state, unowned, unless that profile is a guess; one in another profile
// counts for nothing.
func (ix regIndex) lookup(s Session) (reg Registration, known bool, p proc, hasProc bool) {
	p, hasProc = ix.child[s.PID]
	hasProc = hasProc && s.PID != ""
	if hasProc {
		var o owner
		if ix.owner != nil {
			o = ix.owner(p.pid)
		}
		if reg, known = ix.pick(ix.byPID[p.pid], s, o); known {
			return reg, known, p, hasProc
		}
	}
	var byID []Registration
	for _, r := range ix.byID[s.ID] {
		if !hasProc || strconv.Itoa(r.PID) != p.pid { // those were weighed above
			byID = append(byID, r)
		}
	}
	reg, known = ix.pick(byID, s, owner{})
	return reg, known, p, hasProc
}

// pick is the registration among regs, which share a pid or a session id,
// for session s whose process runs under o: the last read in o's profile,
// owned, when o is known, else the last read in s's profile (see lookup).
func (ix regIndex) pick(regs []Registration, s Session, o owner) (Registration, bool) {
	for i := len(regs) - 1; i >= 0; i-- {
		r := regs[i]
		switch {
		case ix.owner == nil:
		case o.known && r.Profile == o.name:
			r.owned = true
		case o.known || s.guessed || r.Profile != s.Profile:
			continue
		}
		return r, true
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
