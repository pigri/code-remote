// Package profile describes the claude config dirs (accounts) the server can
// launch sessions under.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Profile is one claude config dir, under a name callers pick it by.
type Profile struct {
	Name, Dir string

	// inherit marks the single default used when CLAUDE_REMOTE_PROFILES is
	// unset: claude then runs with the server's environment as it is.
	inherit bool
}

// Set is the configured profiles, in the order they were listed, and which of
// them is the default.
type Set struct {
	list  []Profile
	def   int
	multi bool
}

var validName = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Parse reads a "name=dir,name=dir" list. A dir of "~" or starting "~/" is
// relative to userHome; every dir MUST then be absolute, and is cleaned.
func Parse(spec, userHome string) ([]Profile, error) {
	var out []Profile
	names := map[string]bool{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, errors.New("empty entry")
		}
		name, dir, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("%q: missing '='", entry)
		}
		name, dir = strings.TrimSpace(name), strings.TrimSpace(dir)
		if !validName.MatchString(name) {
			return nil, fmt.Errorf("invalid profile name %q: want [a-z0-9_-]+", name)
		}
		if dir == "" {
			return nil, fmt.Errorf("profile %q: empty dir", name)
		}
		if dir == "~" || strings.HasPrefix(dir, "~/") {
			if userHome == "" {
				return nil, fmt.Errorf("profile %q: %q needs a home dir, and none is known", name, dir)
			}
			dir = filepath.Join(userHome, dir[1:])
		}
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("profile %q: dir %q is not absolute", name, dir)
		}
		dir = filepath.Clean(dir)
		if names[name] {
			return nil, fmt.Errorf("duplicate profile name %q", name)
		}
		for _, other := range out {
			if sameDir(other.Dir, dir) {
				return nil, fmt.Errorf("duplicate profile dir %s (profiles %q and %q)", dir, other.Name, name)
			}
		}
		names[name] = true
		out = append(out, Profile{Name: name, Dir: dir})
	}
	return out, nil
}

// FromEnv builds the profile set from CLAUDE_REMOTE_PROFILES, with
// CLAUDE_REMOTE_DEFAULT_PROFILE naming the default (else the first listed).
// When CLAUDE_REMOTE_PROFILES is unset there is one profile, "default", whose
// dir is fallbackDir.
func FromEnv(fallbackDir, userHome string) (Set, error) {
	spec := os.Getenv("CLAUDE_REMOTE_PROFILES")
	if spec == "" {
		return Set{list: []Profile{{Name: "default", Dir: fallbackDir, inherit: true}}}, nil
	}
	list, err := Parse(spec, userHome)
	if err != nil {
		return Set{}, fmt.Errorf("CLAUDE_REMOTE_PROFILES: %w", err)
	}
	s := Set{list: list, multi: true}
	if name := os.Getenv("CLAUDE_REMOTE_DEFAULT_PROFILE"); name != "" {
		i := s.index(name)
		if i < 0 {
			return Set{}, fmt.Errorf("CLAUDE_REMOTE_DEFAULT_PROFILE: no profile named %q", name)
		}
		s.def = i
	}
	return s, nil
}

func (s Set) index(name string) int {
	for i, p := range s.list {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// Default is the profile used when a request names none. Zero for an empty Set.
func (s Set) Default() Profile {
	if len(s.list) == 0 {
		return Profile{}
	}
	return s.list[s.def]
}

// Get returns the profile called name.
func (s Set) Get(name string) (Profile, bool) {
	if i := s.index(name); i >= 0 {
		return s.list[i], true
	}
	return Profile{}, false
}

// Names lists the profile names in their configured order.
func (s Set) Names() []string {
	out := make([]string, len(s.list))
	for i, p := range s.list {
		out[i] = p.Name
	}
	return out
}

// Multi reports whether the set came from CLAUDE_REMOTE_PROFILES rather than
// the single-profile fallback.
func (s Set) Multi() bool { return s.multi }

// sameDir reports whether a and b name the same dir: equal once cleaned, or
// both resolving to the same path through symlinks.
func sameDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// isUserClaude reports whether p is userHome/.claude, the dir claude uses when
// CLAUDE_CONFIG_DIR is unset.
func (p Profile) isUserClaude(userHome string) bool {
	return userHome != "" && sameDir(p.Dir, filepath.Join(userHome, ".claude"))
}

// ConfigFile is claude's global config file for p: ~/.claude.json for
// ~/.claude, since claude only looks there with CLAUDE_CONFIG_DIR unset, and
// <dir>/.claude.json for any other dir. It is "" for the single default
// profile, meaning the caller keeps its own rule (Manager.ClaudeConfig, else
// the file beside Manager.ClaudeHome).
func (p Profile) ConfigFile(userHome string) string {
	if p.inherit {
		return ""
	}
	if p.isUserClaude(userHome) {
		return filepath.Join(userHome, ".claude.json")
	}
	return filepath.Join(p.Dir, ".claude.json")
}

// Env returns a copy of env that makes claude use p: CLAUDE_CONFIG_DIR is
// removed, then set to p.Dir unless p is ~/.claude. Setting it to ~/.claude
// would move claude's config file off ~/.claude.json (see ConfigFile). For the
// single default profile it is an unchanged copy of env, so claude sees the
// server's CLAUDE_CONFIG_DIR, or its absence, as before profiles existed.
func (p Profile) Env(env []string, userHome string) []string {
	if p.inherit {
		return append([]string(nil), env...)
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			out = append(out, kv)
		}
	}
	if !p.isUserClaude(userHome) {
		out = append(out, "CLAUDE_CONFIG_DIR="+p.Dir)
	}
	return out
}
