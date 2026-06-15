// Package session manages detached claude sessions running inside GNU screen.
// Shared by the API server and the crctl CLI (which can drive it directly,
// without the HTTP API).
package session

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Session is one detached claude run inside GNU screen. The Claude session id
// (a UUID we assign with --session-id) is the stable handle: it's also the
// screen name suffix and the Remote Control name, so the listing can join the
// three. Title is the live display name the user sets inside Claude.
type Session struct {
	ID        string `json:"id"`              // claude session id == --session-id (uuid)
	Screen    string `json:"screen"`          // screen session name (<prefix>-<id>)
	Title     string `json:"title,omitempty"` // claude custom-title, read live
	PID       string `json:"pid,omitempty"`
	Status    string `json:"status,omitempty"` // Detached | Attached
	CreatedAt string `json:"created_at,omitempty"`
}

// Manager wraps screen + claude. It only ever touches screen sessions named
// "<Prefix>-<uuid>", so it can't see or kill unrelated screens on the host.
type Manager struct {
	Prefix     string // e.g. "pigri-dev-remote"
	ClaudeBin  string // path to the claude binary
	ScreenBin  string // path to the screen binary
	ClaudeHome string // ~/.claude (for reading session titles)
}

var (
	uuidRe       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	screenLineRe = regexp.MustCompile(`^(\d+)\.(\S+)`)                // "12345.name" in `screen -ls`
	screenDateRe = regexp.MustCompile(`\((\d{2}/\d{2}/\d{4}[^)]*)\)`) // "(MM/DD/YYYY ...)"
)

// ValidID reports whether id is a well-formed claude session UUID.
func (m *Manager) ValidID(id string) bool { return uuidRe.MatchString(id) }

func (m *Manager) screenName(id string) string { return m.Prefix + "-" + id }

// Create assigns a UUID, launches a detached claude bound to it, and returns
// the session (best-effort enriched with PID/status/title once it registers).
func (m *Manager) Create() (Session, error) {
	id, err := genUUID()
	if err != nil {
		return Session{}, err
	}
	name := m.screenName(id)

	// screen -dmS <prefix>-<id> claude --session-id <id> --remote-control <id>
	// Pinning --session-id makes the screen name, the Remote Control name, and
	// the on-disk session id (~/.claude/.../<id>.jsonl) all the same value.
	cmd := exec.Command(m.ScreenBin, "-dmS", name,
		m.ClaudeBin, "--session-id", id, "--remote-control", id)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Session{}, fmt.Errorf("start screen session: %v: %s", err, strings.TrimSpace(string(out)))
	}

	for i := 0; i < 10; i++ {
		if s, ok, _ := m.Get(id); ok {
			return s, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return Session{ID: id, Screen: name, Status: "Detached"}, nil
}

// List returns all running sessions owned by this manager.
func (m *Manager) List() ([]Session, error) {
	// `screen -ls` exits non-zero when sessions exist; ignore the code, parse stdout.
	out, _ := exec.Command(m.ScreenBin, "-ls").CombinedOutput()
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
		s := Session{ID: id, Screen: name, PID: pid, Title: m.title(id)}
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
	if _, ok, err := m.Get(id); err != nil || !ok {
		return false, err
	}
	if out, err := exec.Command(m.ScreenBin, "-S", m.screenName(id), "-X", "quit").CombinedOutput(); err != nil {
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
			Cwd             string  `json:"cwd"`
			BridgeSessionID *string `json:"bridgeSessionId"`
			Status          string  `json:"status"`
		}
		if json.Unmarshal(data, &rec) != nil || rec.SessionID == "" {
			continue
		}
		reg := Registration{SessionID: rec.SessionID, Cwd: rec.Cwd, Status: rec.Status}
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
