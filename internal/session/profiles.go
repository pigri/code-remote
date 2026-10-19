package session

import (
	"fmt"

	"claude-remote-api/internal/profile"
)

// profileStore is optionally implemented by the Store to read back the profile
// a session was recorded under ("" when none was). Satisfied by *store.DB.
type profileStore interface {
	Profile(uuid string) (string, error)
}

// profileSetter is optionally implemented by the Store to note a session's
// profile without touching the rest of its row. Satisfied by *store.DB.
type profileSetter interface {
	SetProfile(uuid, profile string) error
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
	p, _, _ := m.resolveProfile(id, "")
	return p
}

// resolveProfile is profileOf for a session whose screen name carries profile
// named ("" for none), which comes after the Store's and before the
// transcript's: a name says where the session was launched, but a profile the
// Store recorded since wins. It also returns whether the Store holds no
// profile for id ("" recorded), and whether the profile is only a guess, found
// by neither the Store, the name nor a transcript.
func (m *Manager) resolveProfile(id, named string) (p profile.Profile, unrecorded, guessed bool) {
	list := m.profiles()
	if len(list) == 1 {
		return list[0], false, false
	}
	if ps, ok := m.Store.(profileStore); ok {
		if name, err := ps.Profile(id); err == nil {
			if p, err := m.lookupProfile(name); err == nil && name != "" {
				return p, false, false
			}
			unrecorded = name == ""
		}
	}
	if named != "" {
		if p, err := m.lookupProfile(named); err == nil {
			return p, unrecorded, false
		}
	}
	for _, p := range list {
		if logIn(p.Dir, id) != "" {
			return p, unrecorded, false
		}
	}
	for _, p := range list {
		if p.IsUserClaude() {
			return p, unrecorded, true
		}
	}
	p, _ = m.lookupProfile("")
	return p, unrecorded, true
}

// noteProfile best-effort records in the Store the profile running session s
// runs under, as its own claude's registration shows it (info), when the
// Store holds none for s. Once s stops, its profile resolves from the Store,
// so it resumes there. A profile the Store holds, configured or not, is never
// overwritten. After a write s resolves from the Store, so it is not repeated.
func (m *Manager) noteProfile(s Session, info liveInfo) {
	ps, ok := m.Store.(profileSetter)
	if !ok || !m.Profiles.Multi() || !s.unrecorded || !info.Known || !info.Reg.owned || info.Reg.Profile != s.Profile {
		return
	}
	_ = ps.SetProfile(s.ID, s.Profile)
}

// sessionProfile is the profile of s, a session List resolved, without
// resolving it again.
func (m *Manager) sessionProfile(s Session) profile.Profile {
	if s.Profile != "" {
		if p, err := m.lookupProfile(s.Profile); err == nil {
			return p
		}
	}
	return m.profileOf(s.ID)
}

// hasConv reports whether session id's conversation has a log in profile p.
// It is true when p has no dir to look in.
func (m *Manager) hasConv(p profile.Profile, id string) bool {
	return p.Dir == "" || logIn(p.Dir, m.storedConv(p.Dir, id)) != ""
}

// launchProfile is the profile to relaunch session id, whose profile is own,
// under when the caller asked for prof: own for "". Another profile would be a
// move between profiles, which is refused with ErrOtherProfile.
func (m *Manager) launchProfile(id string, own profile.Profile, prof string) (profile.Profile, error) {
	if prof == "" || prof == own.Name {
		return own, nil
	}
	if _, err := m.lookupProfile(prof); err != nil {
		return profile.Profile{}, err
	}
	return profile.Profile{}, fmt.Errorf("%w: %s is in profile %q, not %q, and moving it is not supported",
		ErrOtherProfile, id, own.Name, prof)
}

// ProfileView is the part of a Manager that one profile owns: its running
// sessions (those whose Profile it is and not by a guess, see List), its
// registrations, and kills of its own sessions only. See Manager.ForProfile.
type ProfileView struct {
	m       *Manager
	profile string
}

// ForProfile returns the view of profile name ("" for the default). An
// unknown name is ErrInvalidProfile.
func (m *Manager) ForProfile(name string) (*ProfileView, error) {
	p, err := m.lookupProfile(name)
	if err != nil {
		return nil, err
	}
	return &ProfileView{m: m, profile: p.Name}, nil
}

// Profile is the name of the profile v holds.
func (v *ProfileView) Profile() string { return v.profile }

// List is Manager.List for the sessions v's profile owns.
func (v *ProfileView) List() ([]Session, error) {
	all, err := v.m.List()
	if err != nil {
		return nil, err
	}
	return v.own(all), nil
}

// own is the sessions of all that v's profile owns. A session whose profile
// is only a guess is in no view: nothing ties it to the profile's account.
func (v *ProfileView) own(all []Session) []Session {
	var out []Session
	for _, s := range all {
		if s.Profile == v.profile && !s.guessed {
			out = append(out, s)
		}
	}
	return out
}

// Registrations is Manager.Registrations for v's profile's registry.
func (v *ProfileView) Registrations() ([]Registration, error) {
	all, err := v.m.Registrations()
	var out []Registration
	for _, r := range all {
		if r.Profile == v.profile {
			out = append(out, r)
		}
	}
	return out, err
}

// LiveRegistrations is Manager.LiveRegistrations for the sessions v's profile
// owns.
func (v *ProfileView) LiveRegistrations() map[string]Registration {
	return v.ownLive(v.m.LiveRegistrations())
}

// ListLive is List and LiveRegistrations from one read of the registries and
// the process table.
func (v *ProfileView) ListLive() ([]Session, map[string]Registration) {
	all, live := v.m.ListLive()
	return v.own(all), v.ownLive(live)
}

// ownLive is the entries of live in v's profile's registry.
func (v *ProfileView) ownLive(live map[string]Registration) map[string]Registration {
	out := map[string]Registration{}
	for id, r := range live {
		if r.Profile == v.profile {
			out[id] = r
		}
	}
	return out
}

// Kill is Manager.Kill for a session v's profile owns. A session another
// profile owns is refused with ErrOtherProfile and left running.
func (v *ProfileView) Kill(id string) (bool, error) {
	s, ok, c := v.m.get(id)
	if !ok {
		return false, nil
	}
	if s.Profile != v.profile {
		return false, fmt.Errorf("%w: %s runs in profile %q, not %q", ErrOtherProfile, id, s.Profile, v.profile)
	}
	return v.m.stop(s, c)
}
