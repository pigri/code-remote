package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Claude asks "do you trust this folder?" the first time it starts in a
// directory, and waits for the answer. Nobody is watching a detached session,
// so one launched into a new directory just sits at that prompt. The answer
// lives in claude's config file as projects[<dir>].hasTrustDialogAccepted.
//
// NOTE: like the session logs, this is claude's internal on-disk format, not a
// stable public API.

const trustKey = "hasTrustDialogAccepted"

// Tunables for taking claude's config lock; vars so tests can shorten them.
var (
	trustLockWait  = 3 * time.Second
	trustLockStale = 15 * time.Second
)

// configPath is claude's config file: ClaudeConfig when set, otherwise
// ".claude.json" in $CLAUDE_CONFIG_DIR or beside ClaudeHome (~/.claude.json).
func (m *Manager) configPath() string {
	if m.ClaudeConfig != "" {
		return m.ClaudeConfig
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	if m.ClaudeHome == "" {
		return ""
	}
	return m.ClaudeHome + ".json"
}

// DirTrusted reports whether claude has dir recorded as a trusted folder. It
// is false when that can't be determined.
func (m *Manager) DirTrusted(dir string) bool {
	path := m.configPath()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	return json.Unmarshal(data, &cfg) == nil && cfg.Projects[dir].Trusted
}

// TrustDir records dir as a trusted folder in claude's config, leaving every
// other setting as it is. It is the same answer as accepting the prompt, so
// callers only do it for a directory the operator asked for.
func (m *Manager) TrustDir(dir string) error {
	path := m.configPath()
	if path == "" {
		return fmt.Errorf("trust %s: claude config location unknown", dir)
	}
	unlock, err := lockConfig(path)
	if err != nil {
		return fmt.Errorf("trust %s: %w", dir, err)
	}
	defer unlock()

	// RawMessage throughout: only the one flag is touched, the rest is carried
	// over byte for byte.
	cfg := map[string]json.RawMessage{}
	mode := os.FileMode(0o600)
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("trust %s: parse %s: %w", dir, path, err)
		}
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("trust %s: %w", dir, err)
	}
	projects := map[string]json.RawMessage{}
	if raw, ok := cfg["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("trust %s: parse projects: %w", dir, err)
		}
	}
	project := map[string]json.RawMessage{}
	if raw, ok := projects[dir]; ok {
		if err := json.Unmarshal(raw, &project); err != nil {
			return fmt.Errorf("trust %s: parse project entry: %w", dir, err)
		}
	}
	project[trustKey] = json.RawMessage("true")

	var err2 error
	if projects[dir], err2 = marshalRaw(project, ""); err2 != nil {
		return err2
	}
	if cfg["projects"], err2 = marshalRaw(projects, ""); err2 != nil {
		return err2
	}
	out, err2 := marshalRaw(cfg, "  ")
	if err2 != nil {
		return err2
	}

	// Replace atomically so a claude reading the file never sees half of it.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.*")
	if err != nil {
		return fmt.Errorf("trust %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("trust %s: %w", dir, err)
	}
	return nil
}

// marshalRaw encodes v without HTML-escaping, so untouched values keep their
// spelling.
func marshalRaw(v any, indent string) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// lockConfig takes the lock running claude processes use when they rewrite
// their config (a "<file>.lock" directory), so the two don't overwrite each
// other. A lock left behind by a dead process is taken over once stale.
func lockConfig(path string) (unlock func(), err error) {
	lock := path + ".lock"
	deadline := time.Now().Add(trustLockWait)
	for {
		if err = os.Mkdir(lock, 0o700); err == nil {
			return func() { _ = os.Remove(lock) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > trustLockStale {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("claude config is locked (%s)", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// prepareDir is called with the directory a session is about to be launched
// in. With TrustDirs it records the directory as trusted; otherwise it reports
// whether the session will be left waiting at the trust prompt.
func (m *Manager) prepareDir(dir string) (needsTrust bool) {
	if dir == "" || m.configPath() == "" || m.DirTrusted(dir) {
		return false
	}
	if m.TrustDirs && m.TrustDir(dir) == nil {
		return false
	}
	return true
}
