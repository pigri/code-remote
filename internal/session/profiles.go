package session

import (
	"fmt"
	"os"

	"claude-remote-api/internal/profile"
)

// profileStore is optionally implemented by the Store to read back the profile
// a session was recorded under ("" when none was). Satisfied by *store.DB.
type profileStore interface {
	Profile(uuid string) (string, error)
}

// profiles lists the profiles sessions can run under. Without
// CLAUDE_REMOTE_PROFILES that is one profile, whose dir is ClaudeHome.
func (m *Manager) profiles() []profile.Profile {
	if !m.Profiles.Multi() {
		name := m.Profiles.Default().Name
		if name == "" {
			name = "default"
		}
		return []profile.Profile{{Name: name, Dir: m.ClaudeHome}}
	}
	names := m.Profiles.Names()
	out := make([]profile.Profile, 0, len(names))
	for _, n := range names {
		p, _ := m.Profiles.Get(n)
		out = append(out, p)
	}
	return out
}

// lookupProfile returns the profile called name, or the default for "".
// An unknown name is ErrInvalidProfile.
func (m *Manager) lookupProfile(name string) (profile.Profile, error) {
	list := m.profiles()
	if name == "" {
		if m.Profiles.Multi() {
			return m.Profiles.Default(), nil
		}
		return list[0], nil
	}
	for _, p := range list {
		if p.Name == name {
			return p, nil
		}
	}
	return profile.Profile{}, fmt.Errorf("%w: %q", ErrInvalidProfile, name)
}

// profileOf resolves the profile session id runs under: the one the Store
// recorded; else the first whose dir holds the session's transcript; else the
// one at ~/.claude, where every session from before profiles lives; else the
// default.
func (m *Manager) profileOf(id string) profile.Profile {
	list := m.profiles()
	if len(list) == 1 {
		return list[0]
	}
	if ps, ok := m.Store.(profileStore); ok {
		if name, err := ps.Profile(id); err == nil && name != "" {
			if p, err := m.lookupProfile(name); err == nil {
				return p
			}
		}
	}
	for _, p := range list {
		if logIn(p.Dir, id) != "" {
			return p
		}
	}
	if h := userHome(); h != "" {
		for _, p := range list {
			if p.IsUserClaude(h) {
				return p
			}
		}
	}
	p, _ := m.lookupProfile("")
	return p
}

// home is the claude config dir of session id's profile.
func (m *Manager) home(id string) string { return m.profileOf(id).Dir }

// launchProfile is the profile to relaunch session id under when the caller
// asked for prof: its own for "". Another profile would be a move between
// profiles, which is refused with ErrOtherProfile.
func (m *Manager) launchProfile(id, prof string) (profile.Profile, error) {
	own := m.profileOf(id)
	if prof == "" || prof == own.Name {
		return own, nil
	}
	if _, err := m.lookupProfile(prof); err != nil {
		return profile.Profile{}, err
	}
	return profile.Profile{}, fmt.Errorf("%w: %s is in profile %q, not %q, and moving it is not supported",
		ErrOtherProfile, id, own.Name, prof)
}

// userHome is the user's home dir, "" when unknown.
func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}
