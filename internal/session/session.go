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
}

// Recorder durably notes sessions the manager starts, so a session stays
// discoverable (resumable) after its screen is gone. Optional; a nil Store
// simply means no resumable tracking. Satisfied by *store.DB.
type Recorder interface {
	Record(uuid, screen, title, cwd, status, createdAt string) error
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
// "<Prefix>-<uuid>", so it can't see or kill unrelated screens on the host.
type Manager struct {
	Prefix        string   // e.g. "pigri-dev-remote"
	ClaudeBin     string   // path to the claude binary
	ScreenBin     string   // path to the screen binary
	ClaudeHome    string   // ~/.claude (for reading session titles)
	WorkspaceRoot string   // optional; when set, Create's dir must resolve under it
	Store         Recorder // optional; records sessions for resumable tracking
	ClaudeConfig  string   // optional; claude's config file (default: see configPath)
	TrustDirs     bool     // mark a session's directory as trusted before launching into it

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

// execCommand is the seam for shelling out to screen; overridden in tests.
var execCommand = exec.Command

var (
	uuidRe       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	screenLineRe = regexp.MustCompile(`^(\d+)\.(\S+)`)                // "12345.name" in `screen -ls`
	screenDateRe = regexp.MustCompile(`\((\d{2}/\d{2}/\d{4}[^)]*)\)`) // "(MM/DD/YYYY ...)"
)

// ValidID reports whether id is a well-formed claude session UUID.
func (m *Manager) ValidID(id string) bool { return uuidRe.MatchString(id) }

func (m *Manager) screenName(id string) string { return m.Prefix + "-" + id }

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
func (m *Manager) Create(dir string) (Session, error) {
	id, err := genUUID()
	if err != nil {
		return Session{}, err
	}
	name := m.screenName(id)

	// screen -dmS <prefix>-<id> claude --session-id <id> --remote-control <id>
	// Pinning --session-id makes the screen name, the Remote Control name, and
	// the on-disk session id (~/.claude/.../<id>.jsonl) all the same value.
	cmd := execCommand(m.ScreenBin, "-dmS", name,
		m.ClaudeBin, "--session-id", id, "--remote-control", id)
	scrubEnv(cmd)
	cwd := ""
	if dir != "" {
		resolved, err := m.resolveDir(dir)
		if err != nil {
			return Session{}, err
		}
		cmd.Dir, cwd = resolved, resolved
	}
	needsTrust := m.prepareDir(cwd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Session{}, fmt.Errorf("start screen session: %v: %s", err, strings.TrimSpace(string(out)))
	}

	for i := 0; i < 10; i++ {
		if s, ok, _ := m.Get(id); ok {
			m.record(s, cwd)
			s.NeedsTrust = needsTrust
			return s, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	s := Session{ID: id, Screen: name, Status: "Detached"}
	m.record(s, cwd)
	s.NeedsTrust = needsTrust
	return s, nil
}

// record best-effort notes a live session in the Store so it stays resumable
// after its screen is gone. No-op when no Store is wired; when cwd is unknown
// it's recovered from the session's on-disk log. Errors are swallowed — the
// mirror is a convenience, not a correctness dependency.
func (m *Manager) record(s Session, cwd string) {
	if m.Store == nil {
		return
	}
	if cwd == "" {
		if _, c := m.sessionLog(s.ID); c != "" {
			cwd = c
		}
	}
	status := s.Status
	if status == "" {
		status = "Detached"
	}
	_ = m.Store.Record(s.ID, s.Screen, s.Title, cwd, status, s.CreatedAt)
}

// HasSessionLog reports whether an on-disk claude log exists for id (i.e. the
// session can actually be resumed). Always false when ClaudeHome is unset.
func (m *Manager) HasSessionLog(id string) bool {
	return m.sessionLogPath(id) != ""
}

// lastActive returns the session log's modification time — a proxy for when the
// conversation was last active (claude appends to the log as it runs). Formatted
// RFC3339; empty when there's no log or ClaudeHome is unset.
func (m *Manager) lastActive(id string) string {
	p := m.sessionLogPath(id)
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
	running, err := m.List()
	if err != nil {
		return nil, err
	}
	m.annotateLive(running)
	if store == nil {
		return running, nil
	}
	// A stopped session is resumable when the conversation it was last in
	// (storedConv; the session id itself unless it drifted) still has a log.
	resumable := func(id string) bool { return m.HasSessionLog(m.storedConv(id)) }
	stopped, err := store.StoppedSessions(running, resumable)
	if err != nil {
		return running, nil // best-effort: still surface the running set
	}
	for i := range stopped {
		id := stopped[i].ID
		conv := m.storedConv(id)
		stopped[i].Status = "Stopped"
		stopped[i].LastActive = m.lastActive(conv)
		if t := m.convTitle(id, conv); t != "" {
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
func (m *Manager) annotateLive(running []Session) {
	if len(running) == 0 {
		return
	}
	live := m.liveIndex()
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
			if t := m.lastActive(info.Conv); t != "" {
				s.LastActive = t
			}
			if t := m.convTitle(s.ID, info.Conv); t != "" {
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
	running, _ := m.List()
	if len(running) == 0 {
		return nil
	}
	live := m.liveIndex()
	out := make(map[string]Registration, len(running))
	for _, s := range running {
		if info := live(s); info.Known {
			out[s.ID] = info.Reg
		}
	}
	return out
}

// SyncConversations persists the conversation each running session is
// currently in, so one that later stops without warning (crash, host reboot)
// still resumes where it left off. Cheap; meant to be called periodically.
func (m *Manager) SyncConversations() {
	if m.Store == nil {
		return
	}
	running, _ := m.List()
	if len(running) == 0 {
		return
	}
	live := m.liveIndex()
	for _, s := range running {
		m.noteConv(s.ID, live(s).Conv)
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
// itself when none was recorded or that conversation's log is gone.
func (m *Manager) storedConv(id string) string {
	if m.Store == nil {
		return id
	}
	if c, err := m.Store.Conversation(id); err == nil && c != id && m.ValidID(c) && m.HasSessionLog(c) {
		return c
	}
	return id
}

// convTitle is the display title for session id while it is in conversation
// conv: the conversation's own title, falling back to the one set under the
// original session id (a fresh conversation has none until it is renamed).
func (m *Manager) convTitle(id, conv string) string {
	if conv != id {
		if t := m.title(conv); t != "" {
			return t
		}
	}
	return m.title(id)
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
// ErrNotResumable if no session log exists on disk (when ClaudeHome is set —
// without it we can't check, so we attempt the resume and let claude decide).
func (m *Manager) Resume(id string) (Session, error) { return m.ResumeIn(id, "") }

// ResumeIn is Resume with the session moved to working directory dir
// (validated against WorkspaceRoot, like Create's). The conversation carries
// over; only where claude runs changes — which is what ties the session to a
// git repository (branch, PR and diff tracking). Empty dir = where it last ran.
// A bad dir is reported as ErrInvalidDir.
func (m *Manager) ResumeIn(id, dir string) (Session, error) {
	if !m.ValidID(id) {
		return Session{}, fmt.Errorf("%w: %q is not a valid session id", ErrNotResumable, id)
	}
	return m.resume(id, m.storedConv(id), dir)
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
// A non-empty dir moves the session to that working directory.
func (m *Manager) resume(id, conv, dir string) (Session, error) {
	if !m.ValidID(id) || !m.ValidID(conv) {
		return Session{}, fmt.Errorf("%w: %q is not a valid session id", ErrNotResumable, id)
	}

	// Already running? Resuming would launch a second claude against the same
	// session id — refuse and hand back the live session so the caller can 409.
	if s, ok, err := m.Get(id); err != nil {
		return Session{}, err
	} else if ok {
		return s, ErrAlreadyRunning
	}

	// The session must exist on disk to resume. When ClaudeHome is unset we
	// can't look, so cwd stays empty and we let claude report a missing session.
	logPath, cwd := m.sessionLog(conv)
	if m.ClaudeHome != "" && logPath == "" {
		return Session{}, fmt.Errorf("%w: no claude session log for %s", ErrNotResumable, conv)
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

	name := m.screenName(id)
	// screen -dmS <prefix>-<id> claude --resume <conv> --remote-control <id>
	cmd := execCommand(m.ScreenBin, "-dmS", name,
		m.ClaudeBin, "--resume", conv, "--remote-control", id)
	scrubEnv(cmd)
	// Restore the project dir if it still exists; otherwise fall back
	// to claude's default rather than failing the spawn on a stale path.
	if cwd != "" {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			cmd.Dir = cwd
		}
	}
	needsTrust := m.prepareDir(cmd.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Session{}, fmt.Errorf("resume screen session: %v: %s", err, strings.TrimSpace(string(out)))
	}

	for i := 0; i < 10; i++ {
		if s, ok, _ := m.Get(id); ok {
			m.record(s, cwd)
			m.noteConv(id, conv)
			s.NeedsTrust = needsTrust
			return s, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	s := Session{ID: id, Screen: name, Status: "Detached"}
	m.record(s, cwd)
	m.noteConv(id, conv)
	s.NeedsTrust = needsTrust
	return s, nil
}

// sessionLogPath returns the path to claude's on-disk log for id, or "" when no
// log exists or ClaudeHome is unset. Cheap: a glob with no file read.
func (m *Manager) sessionLogPath(id string) string {
	if m.ClaudeHome == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(m.ClaudeHome, "projects", "*", id+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// sessionLog returns the path to claude's on-disk log for id and the working
// directory recorded inside it. Both are "" when no log exists or ClaudeHome is
// unset. The cwd is read from the first record that carries one.
func (m *Manager) sessionLog(id string) (path, cwd string) {
	path = m.sessionLogPath(id)
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
// environment unless the command already carries its own).
func scrubEnv(cmd *exec.Cmd) {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = sessionEnv(env)
}

// List returns all running sessions owned by this manager.
func (m *Manager) List() ([]Session, error) {
	// `screen -ls` exits non-zero when sessions exist; ignore the code, parse stdout.
	out, _ := execCommand(m.ScreenBin, "-ls").CombinedOutput()
	return m.parseSessions(string(out)), nil
}

// parseSessions turns `screen -ls` output into our sessions (prefix-scoped,
// UUID-validated). Pure except for the per-session title read, which is skipped
// when ClaudeHome is empty.
func (m *Manager) parseSessions(out string) []Session {
	var sessions []Session
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		match := screenLineRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		pid, name := match[1], match[2]
		id, ok := strings.CutPrefix(name, m.Prefix+"-")
		if !ok || !m.ValidID(id) {
			continue
		}
		s := Session{ID: id, Screen: name, PID: pid, Title: m.title(id), LastActive: m.lastActive(id)}
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
	sessions, err := m.List()
	if err != nil {
		return Session{}, false, err
	}
	for _, s := range sessions {
		if s.ID == id {
			return s, true, nil
		}
	}
	return Session{}, false, nil
}

// Kill terminates the session. The bool reports whether it existed.
func (m *Manager) Kill(id string) (bool, error) {
	s, ok, err := m.Get(id)
	if err != nil || !ok {
		return false, err
	}
	// Last chance to learn which conversation the session is in: once the
	// process is gone only the Store remembers, and Resume depends on it.
	if m.Store != nil {
		m.noteConv(id, m.liveIndex()(s).Conv)
	}
	if out, err := execCommand(m.ScreenBin, "-S", m.screenName(id), "-X", "quit").CombinedOutput(); err != nil {
		return true, fmt.Errorf("quit screen session: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// title reads the current display name claude persists for a session. The file
// is ~/.claude/projects/<cwd>/<id>.jsonl (named by id); the latest
// {"type":"custom-title",...} record wins. Best-effort: returns "" on any miss.
//
// NOTE: this reads claude's internal on-disk format, which is not a stable
// public API and could change across claude versions.
func (m *Manager) title(id string) string {
	if m.ClaudeHome == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(m.ClaudeHome, "projects", "*", id+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	data, err := os.ReadFile(matches[0])
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
}

// Registrations reads claude's process registry under
// $CLAUDE_HOME/sessions/*.json. Best-effort: unreadable/!malformed files are
// skipped. Used to join local sessions to server-side state (cwd + bridge id).
func (m *Manager) Registrations() ([]Registration, error) {
	if m.ClaudeHome == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(m.ClaudeHome, "sessions", "*.json"))
	if err != nil {
		return nil, err
	}
	var regs []Registration
	for _, p := range matches {
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		var rec struct {
			SessionID       string  `json:"sessionId"`
			PID             int     `json:"pid"`
			Version         string  `json:"version"`
			Cwd             string  `json:"cwd"`
			BridgeSessionID *string `json:"bridgeSessionId"`
			Status          string  `json:"status"`
		}
		if json.Unmarshal(data, &rec) != nil || rec.SessionID == "" {
			continue
		}
		reg := Registration{SessionID: rec.SessionID, PID: rec.PID, Version: rec.Version, Cwd: rec.Cwd, Status: rec.Status}
		if rec.BridgeSessionID != nil {
			reg.BridgeSessionID = *rec.BridgeSessionID
		}
		regs = append(regs, reg)
	}
	return regs, nil
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
