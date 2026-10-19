package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"claude-remote-api/internal/profile"
)

const (
	idC = "0b6e2f3a-1c4d-4e5f-8a9b-0c1d2e3f4a5b"
	idD = "d4c3b2a1-9f8e-4d7c-8b6a-5f4e3d2c1b0a"
)

// screensLS is `screen -ls` output listing names, in order.
func screensLS(names ...string) string {
	var b strings.Builder
	b.WriteString("There are screens on:\n")
	for i, n := range names {
		b.WriteString("\t" + string(rune('1'+i)) + "0000." + n + "\t(06/15/2026 08:41:26 AM)\t(Detached)\n")
	}
	return b.String()
}

// byID indexes sessions by id.
func byID(ss []Session) map[string]Session {
	out := map[string]Session{}
	for _, s := range ss {
		out[s.ID] = s
	}
	return out
}

// launchedName is the screen name a -dmS launch (as fakeScreen records it) used.
func launchedName(launch string) string {
	f := strings.Fields(launch)
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

func TestListMixedScreenNames(t *testing.T) {
	set, _ := twoProfiles(t, "private")
	fs := &fakeScreen{created: []string{
		"test-rc-" + idA,         // legacy: the ~/.claude profile, not the default
		"test-rc-work-" + idB,    // work
		"test-rc-private-" + idC, // private
		"test-rc-other-" + idD,   // a profile that is not configured: not ours
		"test-rc-private-not-a-uuid",
		"test-rc-private_" + idD, // no "-" before the id
		"test-rc-private-" + strings.ToUpper(idD),
	}}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ScreenBin: "screen", Profiles: set}

	got, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	ix := byID(got)
	if len(got) != 3 {
		t.Fatalf("List = %+v, want the legacy, work and private sessions only", got)
	}
	for id, want := range map[string]struct{ profile, screen string }{
		idA: {"work", "test-rc-" + idA},
		idB: {"work", "test-rc-work-" + idB},
		idC: {"private", "test-rc-private-" + idC},
	} {
		if s := ix[id]; s.Profile != want.profile || s.Screen != want.screen {
			t.Errorf("%s = %+v, want profile %q, screen %q", id, s, want.profile, want.screen)
		}
	}
	for _, id := range []string{idB, idC} {
		if s, ok, _ := m.Get(id); !ok || s.Screen != ix[id].Screen {
			t.Errorf("Get(%s) = %+v, %v; want it found under its profile name", id, s, ok)
		}
	}
}

func TestLegacyNameWithoutUserClaudeIsTheDefault(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CLAUDE_REMOTE_PROFILES", "a="+filepath.Join(user, "a")+",b="+filepath.Join(user, "b"))
	t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "b")
	set, err := profile.FromEnv("", user)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Prefix: "test-rc", Profiles: set}
	got := m.parseSessions(screensLS("test-rc-" + idA))
	if len(got) != 1 || got[0].Profile != "b" {
		t.Errorf("parseSessions = %+v, want the legacy session in the default b", got)
	}
}

// The screen name is a hint: below a stored profile, above the transcript
// probe. A legacy name hints nothing.
func TestScreenNameHint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		screen  string // the name's profile part: "" for the legacy form
		stored  string
		logIn   string // profile whose dir holds the transcript ("" none)
		want    string
		guessed bool
	}{
		{name: "stored profile wins over the screen name", screen: "work", stored: "private", want: "private"},
		{name: "screen name wins over the transcript", screen: "private", logIn: "work", want: "private"},
		{name: "screen name alone", screen: "private", want: "private"},
		{name: "stored profile not configured: the screen name", screen: "private", stored: "gone", logIn: "work", want: "private"},
		{name: "legacy name: the transcript", logIn: "private", want: "private"},
		{name: "legacy name, nothing else: ~/.claude, a guess", want: "work", guessed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, h := twoProfiles(t, "private")
			switch tc.logIn {
			case "work":
				writeTitledLog(t, h.work, idA, "t")
			case "private":
				writeTitledLog(t, h.private, idA, "t")
			}
			name := "test-rc-" + idA
			if tc.screen != "" {
				name = "test-rc-" + tc.screen + "-" + idA
			}
			m := &Manager{Prefix: "test-rc", Profiles: set, Store: &fakeStore{stored: map[string]string{idA: tc.stored}}}
			got := m.parseSessions(screensLS(name))
			if len(got) != 1 || got[0].Profile != tc.want || got[0].guessed != tc.guessed {
				t.Fatalf("parseSessions(%s) = %+v, want profile %q, guessed %v", name, got, tc.want, tc.guessed)
			}
		})
	}
}

func TestCreateNamesTheScreenAfterItsProfile(t *testing.T) {
	set, _ := twoProfiles(t, "")
	for _, prof := range []string{"", "work", "private"} {
		fs := &fakeScreen{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}
		s, err := m.Create("", prof)
		if err != nil {
			t.Fatal(err)
		}
		want := "test-rc-" + s.Profile + "-" + s.ID
		if len(fs.launches) != 1 || launchedName(fs.launches[0]) != want || s.Screen != want {
			t.Errorf("Create(%q) launched %v as %+v, want screen %s", prof, fs.launches, s, want)
		}
	}
}

func TestSingleProfileNamesAreUnchanged(t *testing.T) {
	fastStop(t)
	t.Setenv("CLAUDE_REMOTE_PROFILES", "")
	set, err := profile.FromEnv("", "")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	upgradeHome(t, home, idA, "", "")
	fs := &fakeScreen{created: []string{"test-rc-" + idA, "test-rc-default-" + idB}}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home, Profiles: set}

	if got, _ := m.List(); len(got) != 1 || got[0].ID != idA {
		t.Fatalf("List = %+v, want only %s: one profile has no profile-named screens", got, idA)
	}
	s, err := m.Create("", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "test-rc-" + s.ID; launchedName(fs.launches[0]) != want || s.Screen != want {
		t.Errorf("Create launched %v as %+v, want screen %s", fs.launches, s, want)
	}
	if _, err := m.Restart(idA); err != nil {
		t.Fatal(err)
	}
	if want := "test-rc-" + idA; !slices.Equal(fs.killed, []string{want}) || launchedName(fs.launches[1]) != want {
		t.Errorf("Restart killed %v, launched %v; want %s both times", fs.killed, fs.launches, want)
	}
}

func TestKillFindsTheRunningName(t *testing.T) {
	set, _ := twoProfiles(t, "")
	for _, name := range []string{"test-rc-" + idA, "test-rc-work-" + idA, "test-rc-private-" + idA} {
		fs := &fakeScreen{created: []string{name}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen", Profiles: set}
		if ok, err := m.Kill(idA); !ok || err != nil {
			t.Fatalf("Kill(%s) = %v, %v", name, ok, err)
		}
		if !slices.Equal(fs.killed, []string{name}) {
			t.Errorf("killed %v, want %s", fs.killed, name)
		}
	}
}

func TestRelaunchUsesTheTargetProfileName(t *testing.T) {
	fastStop(t)
	set, h := twoProfiles(t, "")

	t.Run("upgrade renames a legacy session", func(t *testing.T) {
		upgradeHome(t, h.private, idA, "1.0.0", "idle")
		fs := &fakeScreen{created: []string{"test-rc-" + idA}, version: "1.0.0", updateTo: "2.0.0"}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set,
			Store: &fakeStore{stored: map[string]string{idA: "private"}}}
		res, err := m.Upgrade(true, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Sessions) != 1 || res.Sessions[0].Action != ActionRestarted {
			t.Fatalf("Upgrade = %+v, want the session restarted", res.Sessions)
		}
		if !slices.Equal(fs.killed, []string{"test-rc-" + idA}) || len(fs.launches) != 1 ||
			launchedName(fs.launches[0]) != "test-rc-private-"+idA {
			t.Errorf("killed %v, launched %v; want the legacy name quit and test-rc-private-%s launched", fs.killed, fs.launches, idA)
		}
	})

	t.Run("restart renames a legacy session", func(t *testing.T) {
		writeTitledLog(t, h.work, idB, "t")
		fs := &fakeScreen{created: []string{"test-rc-" + idB}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}
		s, err := m.Restart(idB)
		if err != nil {
			t.Fatal(err)
		}
		if want := "test-rc-work-" + idB; !slices.Equal(fs.killed, []string{"test-rc-" + idB}) ||
			launchedName(fs.launches[0]) != want || s.Screen != want {
			t.Errorf("killed %v, launched %v as %+v; want %s", fs.killed, fs.launches, s, want)
		}
	})

	t.Run("resume of a stopped session", func(t *testing.T) {
		writeTitledLog(t, h.private, idC, "t")
		fs := &fakeScreen{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set,
			Store: &fakeStore{stored: map[string]string{idC: "private"}}}
		s, err := m.Resume(idC)
		if err != nil {
			t.Fatal(err)
		}
		if want := "test-rc-private-" + idC; launchedName(fs.launches[0]) != want || s.Screen != want {
			t.Errorf("launched %v as %+v, want %s", fs.launches, s, want)
		}
	})
}

// Names match [a-z0-9_-]+ and the prefix may hold "-" too, so a name is split
// by the configured prefix and the fixed-length id at its end, never at a "-".
func TestHyphenatedProfileNames(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,my-work=~/.claude-my,rc-work=~/.claude-rc")
	t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "")
	set, err := profile.FromEnv("", user)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".claude", ".claude-my", ".claude-rc"} {
		if err := os.Mkdir(filepath.Join(user, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{Prefix: "test-rc", Profiles: set}
	got := byID(m.parseSessions(screensLS(
		"test-rc-my-work-"+idA,
		"test-rc-work-"+idB,
		"test-rc-rc-work-"+idC,
		"test-rc-my-"+idD, // "my" is not a profile, though "my-work" is
	)))
	if len(got) != 3 {
		t.Fatalf("parseSessions = %+v, want three sessions", got)
	}
	for id, want := range map[string]string{idA: "my-work", idB: "work", idC: "rc-work"} {
		if got[id].Profile != want {
			t.Errorf("%s in %q, want %q", id, got[id].Profile, want)
		}
	}

	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	m.ClaudeBin, m.ScreenBin = "claude", "screen"
	s, err := m.Create("", "my-work")
	if err != nil {
		t.Fatal(err)
	}
	if s.Screen != "test-rc-my-work-"+s.ID || s.Profile != "my-work" {
		t.Errorf("Create(my-work) = %+v, want screen test-rc-my-work-<id> listed in my-work", s)
	}
}
