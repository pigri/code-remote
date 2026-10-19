// Package session manages detached claude sessions running inside GNU screen.
// Shared by the API server and the crctl CLI (which can drive it directly,
// without the HTTP API).
package session

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"claude-remote-api/internal/profile"
)

// Session is one detached claude run inside GNU screen. The Claude session id
// (a UUID we assign with --session-id) is the stable handle: it's also the
// screen name suffix and the Remote Control name, so the listing can join the
// three. Title is the live display name the user sets inside Claude.
type Session struct {
	ID         string `json:"id"`              // claude session id == --session-id (uuid)
	Screen     string `json:"screen"`          // screen session name (<prefix>-<id>)
	Title      string `json:"title,omitempty"` // claude custom-title, read live
	PID        string `json:"pid,omitempty"`
	Status     string `json:"status,omitempty"` // Detached | Attached | Stopped
	CreatedAt  string `json:"created_at,omitempty"`
	LastActive string `json:"last_active,omitempty"` // RFC3339; mtime of the session log
	Version    string `json:"version,omitempty"`     // claude version the session is running
	Outdated   bool   `json:"outdated,omitempty"`    // running an older claude than the installed one
	NeedsTrust bool   `json:"needs_trust,omitempty"` // launched into a folder claude doesn't trust yet: it is waiting at the prompt
	Profile    string `json:"profile,omitempty"`     // the profile (claude config dir) it runs under

	unrecorded bool     // parseSessions found no profile for it in the Store
	guessed    bool     // its profile is only a guess (see resolveProfile)
	named      string   // the profile its screen name carries; "" for <prefix>-<id>
	screens    []string // every screen parseScreens found for it, as <pid>.<name>
}

// Recorder durably notes sessions the manager starts, so a session stays
// discoverable (resumable) after its screen is gone. Optional; a nil Store
// simply means no resumable tracking. Satisfied by *store.DB.
type Recorder interface {
	Record(uuid, screen, title, cwd, status, createdAt, profile string) error
	// SetConversation / Conversation persist the claude conversation id a
	// session is currently in, which drifts from the session id when claude
	// moves it onto a new conversation (see Manager.liveIndex).
	SetConversation(uuid, conv string) error
	Conversation(uuid string) (string, error)
}

// cwdStore is optionally implemented by the Store to read back the working
// directory a session was last launched in. It is what makes a session moved
// with ResumeIn/RestartIn stay in its new directory on later resumes: claude
// keeps appending to the original log, whose first cwd is the old one.
type cwdStore interface {
	Cwd(uuid string) (string, error)
}

// StoppedLister reads back the resumable set: sessions the recorder knows about
// that are not in the live `running` listing and pass `keep`. Satisfied by
// *store.DB. Kept as an interface here to avoid a session→store import cycle.
type StoppedLister interface {
	StoppedSessions(running []Session, keep func(id string) bool) ([]Session, error)
}

// Manager wraps screen + claude. It only ever touches screen sessions named
// "<Prefix>-<profile>-<uuid>" for a configured profile, or "<Prefix>-<uuid>",
// so it can't see or kill unrelated screens on the host.
type Manager struct {
	Prefix        string      // e.g. "pigri-dev-remote"
	ClaudeBin     string      // path to the claude binary
	ScreenBin     string      // path to the screen binary
	ClaudeHome    string      // ~/.claude: the single profile's dir when Profiles is not Multi
	WorkspaceRoot string      // optional; when set, Create's dir must resolve under it
	Store         Recorder    // optional; records sessions for resumable tracking
	ClaudeConfig  string      // optional; claude's config file outside CLAUDE_REMOTE_PROFILES (see configPath)
	TrustDirs     bool        // mark a session's directory as trusted before launching into it
	Profiles      profile.Set // the configured claude config dirs

	// ProcEnviron reads a process's environment; optional, /proc/<pid>/environ
	// when nil. A seam for tests.
	ProcEnviron func(pid string) ([]string, error)

	upgradeMu sync.Mutex // serializes Upgrade (one updater + restart pass at a time)

	convMu   sync.Mutex
	convSeen map[string]string // session id -> conversation id last written to Store
}

// ErrInvalidDir is returned (wrapped) when a requested session working
// directory is missing, not a directory, or escapes WorkspaceRoot. Callers can
// errors.Is(err, ErrInvalidDir) to surface it as a 400 rather than a 500.
var ErrInvalidDir = errors.New("invalid working directory")

// ErrAlreadyRunning is returned by Resume when the session's screen is already
// live — resuming would double-launch claude against the same session. Callers
// can errors.Is to surface it as a 409.
var ErrAlreadyRunning = errors.New("session already running")

// ErrNotResumable is returned by Resume when no claude session log exists on
// disk for the id, so there is nothing to resume. Callers can errors.Is to
// surface it as a 404.
var ErrNotResumable = errors.New("session not resumable")

// ErrInvalidProfile is returned (wrapped) when a request names a profile that
// is not configured. Callers can errors.Is to surface it as a 400.
var ErrInvalidProfile = errors.New("unknown profile")

// ErrOtherProfile is returned (wrapped) by ProfileView.Kill for a session
// another profile owns. API servers from before moves between profiles also
// answer a resume or restart into another profile with it. Callers can
// errors.Is to surface it as a 409.
var ErrOtherProfile = errors.New("session is in another profile")

// execCommand is the seam for shelling out to screen; overridden in tests.
var execCommand = exec.Command

// procEnviron is what reads a process's environment when the Manager sets no
// ProcEnviron; overridden in tests.
var procEnviron = readProcEnviron

// readProcEnviron reads process pid's environment from /proc (Linux only; the
// process must be the caller's user's).
func readProcEnviron(pid string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "environ"))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), nil
}

// procStart reads the start time of process pid as claude records it for its
// own process in a registry entry's procStart; overridden in tests.
var procStart = readProcStart

// readProcStart is field 22 of /proc/<pid>/stat (starttime), as a decimal
// string, the way claude writes procStart on Linux.
func readProcStart(pid string) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return "", err
	}
	return parseStart(string(data))
}

// parseStart is the starttime field of a /proc/<pid>/stat line. Fields are
// counted after the command name's last ")".
func parseStart(stat string) (string, error) {
	_, rest, _ := strings.Cut(stat[strings.LastIndex(stat, ")")+1:], " ")
	if f := strings.Fields(rest); len(f) > 19 {
		return f[19], nil
	}
	return "", errors.New("no starttime in stat")
}

// environ is process pid's environment.
func (m *Manager) environ(pid string) ([]string, error) {
	if m.ProcEnviron != nil {
		return m.ProcEnviron(pid)
	}
	return procEnviron(pid)
}

var (
	uuidRe       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	screenLineRe = regexp.MustCompile(`^(\d+)\.(\S+)`)                // "12345.name" in `screen -ls`
	screenDateRe = regexp.MustCompile(`\((\d{2}/\d{2}/\d{4}[^)]*)\)`) // "(MM/DD/YYYY ...)"
)

// ValidID reports whether id is a well-formed claude session UUID.
func (m *Manager) ValidID(id string) bool { return uuidRe.MatchString(id) }

// launchName is the screen name session id launches under in profile p:
// <Prefix>-<profile>-<id> with CLAUDE_REMOTE_PROFILES, else <Prefix>-<id>.
func (m *Manager) launchName(p profile.Profile, id string) string {
	if m.Profiles.Multi() {
		return m.Prefix + "-" + p.Name + "-" + id
	}
	return m.Prefix + "-" + id
}

// parseName is the session id a screen called name runs, and the profile the
// name carries: "" for <Prefix>-<id>. With CLAUDE_REMOTE_PROFILES it is also
// <Prefix>-<profile>-<id> for a configured profile. The id is the name's
// fixed-length tail, so a "-" in the prefix or a profile name splits nothing.
// ok is false for a name that is not one of ours.
func (m *Manager) parseName(name string) (id, named string, ok bool) {
	rest, ok := strings.CutPrefix(name, m.Prefix+"-")
	switch {
	case !ok:
		return "", "", false
	case m.ValidID(rest):
		return rest, "", true
	case !m.Profiles.Multi():
		return "", "", false
	}
	cut := len(rest) - len("00000000-0000-0000-0000-000000000000") - 1
	if cut < 1 || rest[cut] != '-' {
		return "", "", false
	}
	named, id = rest[:cut], rest[cut+1:]
	if _, known := m.Profiles.Get(named); !known || !m.ValidID(id) {
		return "", "", false
	}
	return id, named, true
}

// resolveDir validates that dir exists, is a directory, and lives inside the
// configured WorkspaceRoot — with no traversal escapes. Returns the
// symlink-resolved absolute path. Errors wrap ErrInvalidDir.
//
// Fail-closed: the dir feature requires CLAUDE_WORKSPACE_ROOT to be set. Without
// a root there is no containment boundary, so we refuse rather than allow an
// arbitrary working directory. Symlinks are resolved on BOTH sides so a symlink
// *inside* the workspace can't point outside it (a lexical check would miss that).
//
// Note: there is a small TOCTOU window between this check and cmd.Dir taking
// effect at process spawn — a path component could be swapped for a symlink in
// between. The returned path is fully canonical (EvalSymlinks'd) so the window
// is narrow, and exploiting it requires local write access to a component
// already inside WorkspaceRoot (i.e. inside the trust boundary). For a
// single-tenant workspace that's acceptable; a multi-tenant deployment would
// want openat2(RESOLVE_BENEATH)-style enforcement instead.
func (m *Manager) resolveDir(dir string) (string, error) {
	if m.WorkspaceRoot == "" {
		return "", fmt.Errorf("%w: CLAUDE_WORKSPACE_ROOT is not configured", ErrInvalidDir)
	}

	// Anchor relative inputs to the workspace root (not the daemon's CWD) so the
	// API contract — "dir is under the workspace root" — matches behaviour.
	abs := dir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(m.WorkspaceRoot, abs)
	}
	abs = filepath.Clean(abs)

	// EvalSymlinks resolves every symlink in the path and also fails if the
	// path does not exist — so this both canonicalizes and confirms existence.
	realDir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidDir, err)
	}
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(m.WorkspaceRoot))
	if err != nil {
		return "", fmt.Errorf("%w: workspace root %q: %v", ErrInvalidDir, m.WorkspaceRoot, err)
	}

	rel, err := filepath.Rel(realRoot, realDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q is outside the workspace root", ErrInvalidDir, dir)
	}

	info, err := os.Stat(realDir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %q is not a directory", ErrInvalidDir, dir)
	}
	return realDir, nil
}

// Create assigns a UUID, launches a detached claude bound to it, and returns
// the session (best-effort enriched with PID/status/title once it registers).
// If dir is non-empty the claude process is started with that working
// directory (validated against WorkspaceRoot); empty dir = process default.
func (m *Manager) Create(dir, prof string) (Session, error) {
	p, err := m.lookupProfile(prof)
	if err != nil {
		return Session{}, err
	}
	id, err := genUUID()
	if err != nil {
		return Session{}, err
	}
	name := m.launchName(p, id)

	// screen -dmS <screen> claude --session-id <id> --remote-control <id>
	// Pinning --session-id makes the screen name, the Remote Control name, and
	// the on-disk session id (~/.claude/.../<id>.jsonl) all the same value.
	cmd := execCommand(m.ScreenBin, "-dmS", name,
		m.ClaudeBin, "--session-id", id, "--remote-control", id)
	scrubEnv(cmd, p)
	cwd := ""
	if dir != "" {
		resolved, err := m.resolveDir(dir)
		if err != nil {
			return Session{}, err
		}
		cmd.Dir, cwd = resolved, resolved
	}
	needsTrust := m.prepareDir(p, cwd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Session{}, fmt.Errorf("start screen session: %v: %s", err, strings.TrimSpace(string(out)))
	}

	for i := 0; i < 10; i++ {
		if s, ok := m.running(id); ok {
			moveTo(&s, p)
			m.record(&s, cwd, p)
			s.NeedsTrust = needsTrust
			return s, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	s := Session{ID: id, Screen: name, Status: "Detached"}
	m.record(&s, cwd, p)
	s.NeedsTrust = needsTrust
	return s, nil
}

// record sets s's profile to p, the one it was launched under, and
// best-effort notes it in the Store so it stays resumable after its screen is
// gone. No Store, no note; when cwd is unknown it's recovered from the
// session's on-disk log. Errors are swallowed — the mirror is a convenience,
// not a correctness dependency.
func (m *Manager) record(s *Session, cwd string, p profile.Profile) {
	s.Profile = p.Name
	if m.Store == nil {
		return
	}
	if cwd == "" {
		if _, c := sessionLog(p.Dir, s.ID); c != "" {
			cwd = c
		}
	}
	status := s.Status
	if status == "" {
		status = "Detached"
	}
	// One profile records none: its name would not resolve once profiles are
	// configured, and "" keeps a profile an earlier run recorded.
	name := ""
	if m.Profiles.Multi() {
		name = p.Name
	}
	_ = m.Store.Record(s.ID, s.Screen, s.Title, cwd, status, s.CreatedAt, name)
}

// lastActiveIn returns the modification time of id's log under home — a proxy
// for when the conversation was last active (claude appends to the log as it
// runs). Formatted RFC3339; empty when there's no log or home is "".
func lastActiveIn(home, id string) string {
	p := logIn(home, id)
	if p == "" {
		return ""
	}
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fi.ModTime().UTC().Format(time.RFC3339)
}

// ListAll returns the live sessions plus, when a store is provided, the
// resumable (stopped) ones — those the store recorded whose screen is no longer
// running and whose on-disk log still exists. Stopped entries carry live titles
// and Status "Stopped". A nil store yields just the running sessions.
func (m *Manager) ListAll(store StoppedLister) ([]Session, error) {
	running, c := m.list()
	m.annotateLive(running, c)
	if store == nil {
		return running, nil
	}
	// Each stopped session's profile is resolved once, here or below.
	resolved := map[string]profile.Profile{}
	profileOf := func(id string) profile.Profile {
		p, ok := resolved[id]
		if !ok {
			p = m.profileOf(id)
			resolved[id] = p
		}
		return p
	}
	// A stopped session is resumable when the conversation it was last in
	// (storedConv; the session id itself unless it drifted) still has a log.
	resumable := func(id string) bool {
		home := profileOf(id).Dir
		return logIn(home, m.storedConv(home, id)) != ""
	}
	stopped, err := store.StoppedSessions(running, resumable)
	if err != nil {
		return running, nil // best-effort: still surface the running set
	}
	for i := range stopped {
		id := stopped[i].ID
		p := profileOf(id)
		conv := m.storedConv(p.Dir, id)
		stopped[i].Status = "Stopped"
		stopped[i].Profile = p.Name
		stopped[i].LastActive = lastActiveIn(p.Dir, conv)
		if t := convTitle(p.Dir, id, conv); t != "" {
			stopped[i].Title = t
		}
	}
	return append(running, stopped...), nil
}

// annotateLive fills in what only the session's live claude process knows: the
// version it is running (and whether that is behind the installed one, i.e. it
// needs a restart to pick up an upgrade) and, when claude has moved the session
// onto a new conversation id, that conversation's title and last-active time.
// It also persists each session's current conversation (see noteConv).
func (m *Manager) annotateLive(running []Session, c *regCache) {
	if len(running) == 0 {
		return
	}
	live := m.liveIndex(c)
	installed := m.Version()
	for i := range running {
		s := &running[i]
		info := live(*s)
		m.noteConv(s.ID, info.Conv)
		if info.Known {
			s.Version = info.Reg.Version
			s.Outdated = installed != "" && s.Version != "" && s.Version != installed
		}
		if info.Conv != s.ID {
			home := m.sessionProfile(*s).Dir
			if t := lastActiveIn(home, info.Conv); t != "" {
				s.LastActive = t
			}
			if t := convTitle(home, s.ID, info.Conv); t != "" {
				s.Title = t
			}
		}
	}
}

// LiveRegistrations maps each running session's id to its claude process's
// registry entry. Unlike Registrations, it joins by process parentage, so a
// session whose claude registered under a different id (a resumed session
// registers under the conversation it resumed) is still found.
func (m *Manager) LiveRegistrations() map[string]Registration {
	running, c := m.list()
	return m.liveRegistrations(running, c)
}

// ListLive is List and LiveRegistrations from one read of the registries and
// the process table.
func (m *Manager) ListLive() ([]Session, map[string]Registration) {
	running, c := m.list()
	return running, m.liveRegistrations(running, c)
}

// liveRegistrations is LiveRegistrations of running, read through c.
func (m *Manager) liveRegistrations(running []Session, c *regCache) map[string]Registration {
	if len(running) == 0 {
		return nil
	}
	live := m.liveIndex(c)
	out := make(map[string]Registration, len(running))
	for _, s := range running {
		if info := live(s); info.Known {
			out[s.ID] = info.Reg
		}
	}
	return out
}

// SyncConversations persists the conversation each running session is
// currently in, and the profile it registered in, so one that later stops
// without warning (crash, host reboot) still resumes where it left off. Cheap;
// meant to be called periodically.
func (m *Manager) SyncConversations() {
	if m.Store == nil {
		return
	}
	running, c := m.list()
	if len(running) == 0 {
		return
	}
	live := m.liveIndex(c)
	for _, s := range running {
		info := live(s)
		m.noteConv(s.ID, info.Conv)
		m.noteProfile(s, info)
	}
}

// noteConv best-effort records in the Store that session id is in conversation
// conv, skipping the write when this manager already recorded the same value.
func (m *Manager) noteConv(id, conv string) {
	if m.Store == nil || conv == "" {
		return
	}
	m.convMu.Lock()
	defer m.convMu.Unlock()
	if m.convSeen[id] == conv {
		return
	}
	if m.Store.SetConversation(id, conv) == nil {
		if m.convSeen == nil {
			m.convSeen = map[string]string{}
		}
		m.convSeen[id] = conv
	}
}

// storedConv returns the conversation a session was last recorded in, or id
// itself when none was recorded or that conversation's log is gone from home,
// the session's profile dir.
func (m *Manager) storedConv(home, id string) string {
	if m.Store == nil {
		return id
	}
	if c, err := m.Store.Conversation(id); err == nil && c != id && m.ValidID(c) && logIn(home, c) != "" {
		return c
	}
	return id
}

// convTitle is the display title for session id while it is in conversation
// conv, both logged under home: the conversation's own title, falling back to
// the one set under the original session id (a fresh conversation has none
// until it is renamed).
func convTitle(home, id, conv string) string {
	if conv != id {
		if t := titleIn(home, conv); t != "" {
			return t
		}
	}
	return titleIn(home, id)
}

// Resume relaunches a detached claude bound to an EXISTING session id whose
// screen is no longer running — the inverse of a kill. It reads the session's
// recorded working directory from claude's on-disk log so the resumed process
// starts in the same project (claude scopes --resume to the project dir).
//
// The conversation resumed is the one the session was last recorded in, which
// is the session id itself unless claude moved it onto a new conversation while
// it ran (see liveIndex).
//
// Errors: ErrAlreadyRunning if the screen is still live (nothing to do);
// ErrNotResumable if no session log exists on disk (when its profile has a dir —
// without it we can't check, so we attempt the resume and let claude decide).
func (m *Manager) Resume(id string) (Session, error) { return m.ResumeIn(id, "", "") }

// ResumeIn is Resume with the session moved to working directory dir
// (validated against WorkspaceRoot, like Create's). Empty dir = where it last
// ran. A bad dir is reported as ErrInvalidDir.
//
// prof names the profile to launch under; "" is the session's own, and an
// unknown one is ErrInvalidProfile. Another profile moves the session there:
// its conversation is copied into that profile (see planCopy) and resumed
// there as a fork. A different copy already there is ErrMoveConflict.
//
// A move forks the conversation (`--fork-session`): the history carries over,
// under a new conversation id. That is what makes the move count server-side —
// the Remote Control session a conversation is bound to keeps the repository it
// was created with, so resuming the same conversation elsewhere would stay
// attached to one that knows nothing of the new directory (no repository, hence
// no PR or diff tracking). The fork registers a fresh one from dir; the session
// id, screen and Remote Control name stay the same.
func (m *Manager) ResumeIn(id, dir, prof string) (Session, error) {
	if !m.ValidID(id) {
		return Session{}, fmt.Errorf("%w: %q is not a valid session id", ErrNotResumable, id)
	}
	own := m.profileOf(id)
	if prof != "" && !m.hasConv(own, id) {
		// Not found comes before any check of prof, as it would without one.
		if _, running := m.running(id); !running {
			return Session{}, fmt.Errorf("%w: no session %s", ErrNotResumable, id)
		}
	}
	p, err := m.launchProfile(own, prof)
	if err != nil {
		return Session{}, err
	}
	return m.resume(id, m.storedConv(own.Dir, id), dir, own, p)
}

// storedCwd returns the working directory the Store last recorded for a
// session, or "" when there is no Store or it doesn't track one.
func (m *Manager) storedCwd(id string) string {
	cs, ok := m.Store.(cwdStore)
	if !ok {
		return ""
	}
	c, _ := cs.Cwd(id)
	return c
}

// resume relaunches session id's screen, resuming conversation conv. The two
// differ only when claude moved the session onto a new conversation id while it
// ran (see Restart); the screen and Remote Control names always stay on id.
// A non-empty dir moves the session to that working directory. from is the
// session's profile and p the one it launches under; when they differ, the
// conversation is copied from one into the other first, and the Store
// records p only once the launch has started.
func (m *Manager) resume(id, conv, dir string, from, p profile.Profile) (Session, error) {
	if !m.ValidID(id) || !m.ValidID(conv) {
		return Session{}, fmt.Errorf("%w: %q is not a valid session id", ErrNotResumable, id)
	}

	// Already running? Resuming would launch a second claude against the same
	// session id — refuse and hand back the live session so the caller can 409.
	if r, up := m.running(id); up {
		if s, ok, _ := m.get(id); ok {
			return s, ErrAlreadyRunning
		}
		return r, ErrAlreadyRunning
	}

	// The session must exist on disk to resume. When its profile has no dir we
	// can't look, so cwd stays empty and we let claude report a missing session.
	logPath, cwd := sessionLog(from.Dir, conv)
	if from.Dir != "" && logPath == "" {
		return Session{}, fmt.Errorf("%w: no claude session log for %s", ErrNotResumable, conv)
	}
	move := from.Name != p.Name
	var cp *convCopy
	if move {
		var err error
		if cp, err = m.planMove(id, from, p, conv); err != nil {
			return Session{}, err
		}
	}
	// Where to run: an explicit dir wins, then the directory the session was
	// last launched in (it may have been moved since the log was started), then
	// the log's own.
	if dir != "" {
		resolved, err := m.resolveDir(dir)
		if err != nil {
			return Session{}, err
		}
		cwd = resolved
	} else if c := m.storedCwd(id); c != "" {
		cwd = c
	}

	name := m.launchName(p, id)
	// screen -dmS <screen> claude --resume <conv> [--fork-session] --remote-control <id>
	args := []string{"-dmS", name, m.ClaudeBin, "--resume", conv}
	rc := []string{"--remote-control", id}
	fork := dir != "" || move
	if fork {
		args = append(args, "--fork-session") // a move: see ResumeIn
		// The fork's new Remote Control session is titled after its name, and
		// the title set on the old one doesn't follow. Name it after the
		// session's title so it keeps showing under that. ("=" keeps a title
		// that starts with "-" from being read as a flag.)
		if t := convTitle(from.Dir, id, conv); t != "" {
			rc = []string{"--remote-control=" + t}
		}
	}
	undo := func() {}
	if move {
		var err error
		if undo, err = cp.run(); err != nil {
			if errors.Is(err, ErrMoveConflict) {
				return Session{}, fmt.Errorf("%w (moving to profile %q)", err, p.Name)
			}
			return Session{}, fmt.Errorf("copy the conversation to profile %q: %w", p.Name, err)
		}
	}
	cmd := execCommand(m.ScreenBin, append(args, rc...)...)
	scrubEnv(cmd, p)
	// Restore the project dir if it still exists; otherwise fall back
	// to claude's default rather than failing the spawn on a stale path.
	if cwd != "" {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			cmd.Dir = cwd
		}
	}
	needsTrust := m.prepareDir(p, cmd.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		undo()
		return Session{}, fmt.Errorf("resume screen session: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// A fork leaves conv behind for an id claude picks once it is up, so there
	// is nothing to record yet: the periodic sync (and Kill) note it from the
	// process registry. Until then the Store keeps the pre-fork conversation,
	// which is still a valid place to resume from.
	noteConv := func() {
		if !fork {
			m.noteConv(id, conv)
		}
	}

	for i := 0; i < 10; i++ {
		if s, ok := m.running(id); ok {
			moveTo(&s, p)
			m.record(&s, cwd, p)
			noteConv()
			s.NeedsTrust = needsTrust
			return s, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	s := Session{ID: id, Screen: name, Status: "Detached"}
	m.record(&s, cwd, p)
	noteConv()
	s.NeedsTrust = needsTrust
	return s, nil
}

// logIn returns the path to claude's on-disk log for id under the claude config
// dir home, or "" when no log exists or home is "". Cheap: a glob with no file
// read.
func logIn(home, id string) string {
	if home == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, "projects", "*", id+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// sessionLog returns the path to claude's on-disk log for id under home and the
// working directory recorded inside it. Both are "" when no log exists or home
// is "". The cwd is read from the first record that carries one.
func sessionLog(home, id string) (path, cwd string) {
	path = logIn(home, id)
	if path == "" {
		return "", ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return path, ""
	}
	return path, parseCwd(data)
}

// parseCwd returns the first cwd recorded in a claude session .jsonl. Records
// vary in shape, so we scan for any line carrying a non-empty "cwd".
func parseCwd(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"cwd"`) {
			continue // cheap filter before the JSON parse
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}

// inheritedClaudeVars are the per-process markers a running claude exports to
// its children. A session launched from inside another claude session (e.g.
// crctl run from a claude shell) must not inherit them: with
// CLAUDE_CODE_CHILD_SESSION set the new claude treats itself as a child — it
// neither saves its transcript nor registers in the process registry.
var inheritedClaudeVars = map[string]bool{
	"CLAUDECODE":                    true,
	"CLAUDE_PID":                    true,
	"AI_AGENT":                      true,
	"CLAUDE_CODE_CHILD_SESSION":     true,
	"CLAUDE_CODE_SESSION_ID":        true,
	"CLAUDE_CODE_BRIDGE_SESSION_ID": true,
	"CLAUDE_CODE_SESSION_ATTENDED":  true,
	"CLAUDE_CODE_ENTRYPOINT":        true,
	"CLAUDE_CODE_EXECPATH":          true,
	"CLAUDE_CODE_MESSAGING_SOCKET":  true,
	"CLAUDE_CODE_MESSAGING_TOKEN":   true,
}

// toolShellVars are what claude sets for the commands its tools run (git made
// non-interactive, its own effort level, ...). They are dropped only when the
// launch comes from inside a claude session: there they are that session's
// plumbing, anywhere else they are the user's own configuration.
var toolShellVars = map[string]bool{
	"CLAUDE_EFFORT":                      true,
	"GIT_EDITOR":                         true,
	"GIT_TERMINAL_PROMPT":                true,
	"GIT_SSH_COMMAND":                    true,
	"GCM_INTERACTIVE":                    true,
	"COREPACK_ENABLE_AUTO_PIN":           true,
	"NoDefaultCurrentDirectoryInExePath": true,
}

// sessionEnv returns env without the markers of an enclosing claude session, so
// a launched session always starts as a top-level one. Everything else —
// including deliberate CLAUDE_CODE_* configuration — is passed through.
func sessionEnv(env []string) []string {
	nested := false
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == "CLAUDECODE" {
			nested = true
			break
		}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); inheritedClaudeVars[k] || (nested && toolShellVars[k]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// scrubEnv applies sessionEnv to a launch command's environment (the process
// environment unless the command already carries its own), then points claude
// at profile p (see profile.Profile.Env).
func scrubEnv(cmd *exec.Cmd, p profile.Profile) {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = p.Env(sessionEnv(env))
}

// List returns all running sessions owned by this manager, each in its
// profile: the one its claude registered in, else its resolved one.
func (m *Manager) List() ([]Session, error) {
	sessions, _ := m.list()
	return sessions, nil
}

// list is List, with the registry cache it used, so the rest of the caller's
// operation reads the registries and the process table no second time.
func (m *Manager) list() ([]Session, *regCache) {
	c := &regCache{m: m}
	sessions := m.screens()
	m.registeredProfiles(sessions, c)
	return sessions, c
}

// screens returns the running sessions, each in its resolved profile, without
// reading any registry.
func (m *Manager) screens() []Session {
	return m.parseSessions(m.screenLS())
}

// screenLS is the output of `screen -ls`.
func (m *Manager) screenLS() string {
	// `screen -ls` exits non-zero when sessions exist; ignore the code, parse stdout.
	out, _ := execCommand(m.ScreenBin, "-ls").CombinedOutput()
	return string(out)
}

// running returns session id, with no profile, if a screen of it runs: it
// reads screen names only, enough to tell whether a session runs. Any screen
// <Prefix>-...-<id> counts, even one parseName rejects (a profile that is not
// configured here, or a profile name with one profile): a launch next to it
// would run a second claude in the conversation.
func (m *Manager) running(id string) (Session, bool) {
	return find(m.screensOf(id), id)
}

// screensOf is every screen running counts for session id, one per screen.
func (m *Manager) screensOf(id string) []Session {
	return scanScreens(m.screenLS(), func(name string) (string, string, bool) {
		rest, ok := strings.CutPrefix(name, m.Prefix+"-")
		return id, "", ok && (rest == id || strings.HasSuffix(rest, "-"+id))
	})
}

// screenTargets is how screen -S names each screen of s: <pid>.<name>, which
// picks one screen even when two share a name.
func (s Session) screenTargets() []string {
	switch {
	case len(s.screens) > 0:
		return s.screens
	case s.PID != "":
		return []string{s.PID + "." + s.Screen}
	}
	return []string{s.Screen}
}

// find returns the session with id among sessions.
func find(sessions []Session, id string) (Session, bool) {
	for _, s := range sessions {
		if s.ID == id {
			return s, true
		}
	}
	return Session{}, false
}

// registeredProfiles puts each running session whose claude has an owned
// registration (see lookup) in the profile it registered in, so every caller
// sees one profile for it. With one profile there is nothing to move.
func (m *Manager) registeredProfiles(sessions []Session, c *regCache) {
	if !m.Profiles.Multi() || len(sessions) == 0 {
		return
	}
	ix := c.index()
	for i := range sessions {
		s := &sessions[i]
		r, ok, _, _ := ix.lookup(*s)
		if !ok || !r.owned || r.Profile == "" || r.Profile == s.Profile {
			continue
		}
		p, err := m.lookupProfile(r.Profile)
		if err != nil {
			continue
		}
		moveTo(s, p)
	}
}

// moveTo puts s in profile p, reading its title and last-active time there.
func moveTo(s *Session, p profile.Profile) {
	if s.Profile == p.Name {
		return
	}
	s.Profile, s.guessed = p.Name, false
	s.Title, s.LastActive = titleIn(p.Dir, s.ID), lastActiveIn(p.Dir, s.ID)
}

// parseSessions is parseScreens with each session's resolved profile, and
// its title and last-active time read there (nothing when the profile has no
// dir).
func (m *Manager) parseSessions(out string) []Session {
	sessions := m.parseScreens(out)
	for i := range sessions {
		s := &sessions[i]
		p, unrecorded, guessed := m.resolveProfile(s.ID, s.named)
		s.Profile, s.unrecorded, s.guessed = p.Name, unrecorded, guessed
		s.Title, s.LastActive = titleIn(p.Dir, s.ID), lastActiveIn(p.Dir, s.ID)
	}
	return sessions
}

// parseScreens turns `screen -ls` output into our sessions (see parseName),
// with no profile. Pure. Several screens for one id are one session, listed
// under a profile-named screen when there is one: a <Prefix>-<id> screen can
// only be left from before profiles, and the name tells the profile.
func (m *Manager) parseScreens(out string) []Session {
	var sessions []Session
	at := map[string]int{}
	for _, s := range scanScreens(out, m.parseName) {
		i, dup := at[s.ID]
		if !dup {
			at[s.ID] = len(sessions)
			s.screens = s.screenTargets()
			sessions = append(sessions, s)
			continue
		}
		d := &sessions[i]
		d.screens = append(d.screens, s.screenTargets()...)
		if d.named == "" && s.named != "" {
			s.screens = d.screens
			*d = s
		}
	}
	return sessions
}

// scanScreens is the screens in `screen -ls` output whose names match
// accepts, as sessions of the id it returns, one per screen.
func scanScreens(out string, accepts func(name string) (id, named string, ok bool)) []Session {
	var sessions []Session
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		match := screenLineRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		pid, name := match[1], match[2]
		id, named, ok := accepts(name)
		if !ok {
			continue
		}
		s := Session{ID: id, Screen: name, PID: pid, named: named}
		switch {
		case strings.Contains(line, "(Detached)"):
			s.Status = "Detached"
		case strings.Contains(line, "(Attached)"):
			s.Status = "Attached"
		}
		if dm := screenDateRe.FindStringSubmatch(line); dm != nil {
			s.CreatedAt = dm[1]
		}
		sessions = append(sessions, s)
	}
	return sessions
}

// Get returns the session for the given claude session id, if running.
func (m *Manager) Get(id string) (Session, bool, error) {
	s, ok, _ := m.get(id)
	return s, ok, nil
}

// get is Get, with the registry cache it used (see list).
func (m *Manager) get(id string) (Session, bool, *regCache) {
	sessions, c := m.list()
	s, ok := find(sessions, id)
	return s, ok, c
}

// Kill terminates the session. The bool reports whether it existed.
func (m *Manager) Kill(id string) (bool, error) {
	s, ok, c := m.get(id)
	if !ok {
		return false, nil
	}
	return m.stop(s, c)
}

// stop is Kill for s, which get found running with registry cache c.
func (m *Manager) stop(s Session, c *regCache) (bool, error) {
	var info liveInfo
	if m.Store != nil {
		info = m.liveIndex(c)(s)
	}
	return true, m.kill(s, info)
}

// kill quits every screen running session s was listed with, whose claude
// liveIndex found as info. It tries each one, and fails only when a screen of
// s still runs afterwards (one may exit on its own before its quit). Last
// chance to learn which conversation and profile the session is in: once the
// process is gone only the Store remembers, and Resume depends on both.
func (m *Manager) kill(s Session, info liveInfo) error {
	m.noteConv(s.ID, info.Conv)
	m.noteProfile(s, info)
	var errs []error
	for _, target := range s.screenTargets() {
		if out, err := execCommand(m.ScreenBin, "-S", target, "-X", "quit").CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("quit screen session %s: %v: %s", target, err, strings.TrimSpace(string(out))))
		}
	}
	if _, up := m.running(s.ID); !up {
		return nil
	}
	return errors.Join(errs...)
}

// titleIn reads the current display name claude persists for a session. The
// file is <home>/projects/<cwd>/<id>.jsonl (named by id); the latest
// {"type":"custom-title",...} record wins. Best-effort: returns "" on any miss.
//
// NOTE: this reads claude's internal on-disk format, which is not a stable
// public API and could change across claude versions.
func titleIn(home, id string) string {
	path := logIn(home, id)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return parseTitle(data)
}

// parseTitle returns the latest custom-title from a claude session .jsonl.
func parseTitle(data []byte) string {
	title := ""
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"custom-title"`) {
			continue // cheap filter before the JSON parse
		}
		var rec struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Type == "custom-title" && rec.CustomTitle != "" {
			title = rec.CustomTitle // last one wins
		}
	}
	return title
}

// Registration is one entry from claude's live process registry
// (~/.claude/sessions/<pid>.json). It links our session id (SessionID) to the
// working directory and, when present, the server-side bridge session id.
type Registration struct {
	SessionID       string // claude session UUID (== our screen suffix)
	PID             int    // claude process id
	Version         string // claude version the process is running
	Cwd             string
	BridgeSessionID string // server session id; "" when not bridged
	Status          string // idle | busy | shell | waiting | ...
	Profile         string // the profile whose registry holds the entry

	path      string // the registry file
	owned     bool   // its process's environment names its profile
	procStart string // the start of the process that wrote it; "" when not recorded
}

// Registrations reads claude's process registry under <dir>/sessions/*.json of
// every profile. Best-effort: unreadable/!malformed files are skipped. Used to
// join local sessions to server-side state (cwd + bridge id).
func (m *Manager) Registrations() ([]Registration, error) {
	var regs []Registration
	for _, prof := range m.profiles() {
		if prof.Dir == "" {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(prof.Dir, "sessions", "*.json"))
		if err != nil {
			return nil, err
		}
		regs = append(regs, readRegistrations(matches, prof.Name)...)
	}
	return regs, nil
}

// readRegistrations parses the registry files at paths, tagging each entry
// with profile.
func readRegistrations(paths []string, profile string) []Registration {
	var regs []Registration
	for _, p := range paths {
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		var rec struct {
			SessionID       string          `json:"sessionId"`
			PID             int             `json:"pid"`
			Version         string          `json:"version"`
			Cwd             string          `json:"cwd"`
			BridgeSessionID *string         `json:"bridgeSessionId"`
			Status          string          `json:"status"`
			ProcStart       json.RawMessage `json:"procStart"`
		}
		if json.Unmarshal(data, &rec) != nil || rec.SessionID == "" {
			continue
		}
		var start string
		_ = json.Unmarshal(rec.ProcStart, &start) // only a string is a start, as claude reads it
		reg := Registration{SessionID: rec.SessionID, PID: rec.PID, Version: rec.Version, Cwd: rec.Cwd, Status: rec.Status, Profile: profile, path: p, procStart: start}
		if rec.BridgeSessionID != nil {
			reg.BridgeSessionID = *rec.BridgeSessionID
		}
		regs = append(regs, reg)
	}
	return regs
}

func genUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
