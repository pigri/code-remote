package session

import (
	"errors"
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

	if got, _ := m.List(); len(got) != 2 {
		t.Fatalf("List = %+v, want both %s and %s: a profile-named screen from before profiles were disabled is still visible", got, idA, idB)
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

// A screen launched under a profile that is no longer configured (profiles
// disabled) is still listed as running: its SCREEN is alive, and hiding it
// makes ListAll show it as Stopped while its process is up.
func TestOrphanedProfileScreenIsListed(t *testing.T) {
	fastStop(t)
	t.Setenv("CLAUDE_REMOTE_PROFILES", "")
	set, err := profile.FromEnv("", "")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	upgradeHome(t, home, idA, "", "")
	orphan := "test-rc-private-" + idA

	t.Run("listed as running", func(t *testing.T) {
		fs := &fakeScreen{created: []string{orphan}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen", ClaudeHome: home, Profiles: set}
		got, _ := m.List()
		if len(got) != 1 || got[0].ID != idA {
			t.Fatalf("List = %+v, want %s listed as running despite its profile being gone", got, idA)
		}
		if got[0].Status != "Detached" {
			t.Errorf("status = %q, want Detached", got[0].Status)
		}
	})

	t.Run("restart kills the orphan and relaunches", func(t *testing.T) {
		fs := &fakeScreen{created: []string{orphan}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home, Profiles: set}
		s, err := m.Restart(idA)
		if err != nil {
			t.Fatalf("Restart: %v", err)
		}
		want := "test-rc-" + idA
		if !slices.Equal(fs.killed, []string{orphan}) || len(fs.launches) != 1 || launchedName(fs.launches[0]) != want || s.Screen != want {
			t.Errorf("killed %v, launched %v as %+v; want %s killed and %s launched", fs.killed, fs.launches, s, orphan, want)
		}
	})

	t.Run("resume blocks", func(t *testing.T) {
		fs := &fakeScreen{created: []string{orphan}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home, Profiles: set}
		_, err := m.Resume(idA)
		if !errors.Is(err, ErrAlreadyRunning) {
			t.Errorf("Resume = %v, want ErrAlreadyRunning", err)
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

// A screen under the prefix whose name ends in the session's id is that
// session running, whether or not this Manager's profiles can parse it: a
// relaunch MUST NOT start a second claude next to it.
func TestNoRelaunchNextToAnUnparsedScreen(t *testing.T) {
	fastStop(t)
	renamed := func(t *testing.T) *Manager {
		set, h := twoProfiles(t, "")
		upgradeHome(t, h.work, idA, "", "")
		return &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}
	}
	for _, tc := range []struct {
		name    string
		manager func(*testing.T) *Manager
		screen  string
	}{
		{"a profile no longer configured", renamed, "test-rc-old-" + idA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, op := range []string{"resume", "restart"} {
				fs := &fakeScreen{created: []string{tc.screen}}
				withFakeScreen(t, fs)
				m := tc.manager(t)
				var err error
				if op == "resume" {
					_, err = m.Resume(idA)
				} else {
					_, err = m.Restart(idA)
				}
				if !errors.Is(err, ErrAlreadyRunning) || len(fs.launches) != 0 || len(fs.killed) != 0 {
					t.Errorf("%s next to %s: err %v, launched %v, killed %v; want ErrAlreadyRunning and nothing touched",
						op, tc.screen, err, fs.launches, fs.killed)
				}
			}
		})
	}

	t.Run("other ids and foreign screens do not count", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_PROFILES", "")
		set, err := profile.FromEnv("", "")
		if err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		upgradeHome(t, home, idA, "", "")
		fs := &fakeScreen{created: []string{"test-rc-work-" + idB, "other-rc-" + idA, "test-rcx-" + idA}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home, Profiles: set}
		if _, err := m.Resume(idA); err != nil || len(fs.launches) != 1 {
			t.Errorf("Resume = %v, launched %v; want one launch", err, fs.launches)
		}
	})
}

// Two screens for one id (a legacy one and a profile-named one) are one
// session: listed once, under the profile-named screen, and stopped whole.
func TestTwoScreensForOneID(t *testing.T) {
	set, _ := twoProfiles(t, "")
	for _, order := range [][]string{
		{"test-rc-" + idA, "test-rc-work-" + idA},
		{"test-rc-work-" + idA, "test-rc-" + idA},
	} {
		fs := &fakeScreen{created: slices.Clone(order)}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen", Profiles: set}
		got, _ := m.List()
		if len(got) != 1 || got[0].Screen != "test-rc-work-"+idA {
			t.Errorf("List of %v = %+v, want one session under test-rc-work-%s", order, got, idA)
		}
		if ok, err := m.Kill(idA); !ok || err != nil {
			t.Fatalf("Kill = %v, %v", ok, err)
		}
		if slices.Sort(fs.killed); !slices.Equal(fs.killed, []string{"test-rc-" + idA, "test-rc-work-" + idA}) {
			t.Errorf("killed %v, want both screens", fs.killed)
		}
		if _, up := m.running(idA); up {
			t.Errorf("still running after Kill: %v", fs.created)
		}
	}
}

// Kill quits each of a session's screens by its pid, tries them all, and
// succeeds once none runs.
func TestKillTriesEveryScreen(t *testing.T) {
	set, _ := twoProfiles(t, "")
	legacy, named := "test-rc-"+idA, "test-rc-work-"+idA
	for _, tc := range []struct {
		name    string
		created []string
		vanish  []string
		killed  []string
	}{
		{"the first one exits before its quit", []string{legacy, named}, []string{legacy}, []string{named}},
		{"three screens of one name", []string{named, named, named}, nil, []string{named, named, named}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeScreen{created: slices.Clone(tc.created)}
			withFakeScreen(t, fs)
			m := &Manager{Prefix: "test-rc", ScreenBin: "screen", Profiles: set}
			if _, ok, _ := m.Get(idA); !ok {
				t.Fatal("not listed")
			}
			fs.vanish = tc.vanish
			if ok, err := m.Kill(idA); !ok || err != nil {
				t.Errorf("Kill = %v, %v; want true, nil", ok, err)
			}
			if !slices.Equal(fs.killed, tc.killed) || len(fs.created) != 0 {
				t.Errorf("killed %v, still running %v; want %v quit and nothing running", fs.killed, fs.created, tc.killed)
			}
		})
	}

	t.Run("a screen that does not quit is an error", func(t *testing.T) {
		fs := &fakeScreen{created: []string{legacy, named}, failKill: true}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen", Profiles: set}
		if ok, err := m.Kill(idA); !ok || err == nil {
			t.Errorf("Kill = %v, %v; want true and an error", ok, err)
		}
	})
}

// A session that also runs under a screen this Manager does not list is not
// restarted: stopping the listed screen would leave the hidden claude running
// and the restart could not relaunch next to it.
func TestNoRestartBesideAHiddenScreen(t *testing.T) {
	fastStop(t)
	set, h := twoProfiles(t, "")
	upgradeHome(t, h.work, idA, "1.0.0", "idle")
	hidden := "test-rc-old-" + idA
	for _, op := range []string{"restart", "upgrade"} {
		t.Run(op, func(t *testing.T) {
			fs := &fakeScreen{created: []string{"test-rc-" + idA, hidden}, version: "1.0.0", updateTo: "2.0.0"}
			withFakeScreen(t, fs)
			m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}
			var err error
			if op == "restart" {
				_, err = m.Restart(idA)
			} else {
				var res UpgradeResult
				res, err = m.Upgrade(true, false)
				if err == nil && (len(res.Sessions) != 1 || res.Sessions[0].Action != ActionFailed ||
					!strings.Contains(res.Sessions[0].Reason, hidden)) {
					t.Errorf("Upgrade = %+v, want the session failed, naming %s", res.Sessions, hidden)
				}
				err = nil
			}
			if op == "restart" && (!errors.Is(err, ErrAlreadyRunning) || !strings.Contains(err.Error(), hidden)) {
				t.Errorf("Restart err = %v, want ErrAlreadyRunning naming %s", err, hidden)
			}
			if len(fs.killed) != 0 || len(fs.launches) != 0 || len(fs.created) != 2 {
				t.Errorf("killed %v, launched %v, running %v; want both screens left alone", fs.killed, fs.launches, fs.created)
			}
		})
	}
}

// A session listed with two screens restarts as one: both quit, one relaunch
// under its profile's name.
func TestRestartOfTwoScreens(t *testing.T) {
	fastStop(t)
	set, h := twoProfiles(t, "")
	legacy, named := "test-rc-"+idA, "test-rc-work-"+idA
	for _, op := range []string{"restart", "upgrade"} {
		for _, order := range [][]string{{legacy, named}, {named, legacy}} {
			t.Run(op+" "+strings.Join(order, ","), func(t *testing.T) {
				upgradeHome(t, h.work, idA, "1.0.0", "idle")
				fs := &fakeScreen{created: slices.Clone(order), version: "1.0.0", updateTo: "2.0.0"}
				withFakeScreen(t, fs)
				m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}
				if op == "restart" {
					if _, err := m.Restart(idA); err != nil {
						t.Fatalf("Restart: %v", err)
					}
				} else {
					res, err := m.Upgrade(true, false)
					if err != nil || len(res.Sessions) != 1 || res.Sessions[0].Action != ActionRestarted {
						t.Fatalf("Upgrade = %+v, %v; want the session restarted", res.Sessions, err)
					}
				}
				killed := slices.Sorted(slices.Values(fs.killed))
				if !slices.Equal(killed, []string{legacy, named}) || len(fs.launches) != 1 ||
					launchedName(fs.launches[0]) != named || !slices.Equal(fs.created, []string{named}) {
					t.Errorf("killed %v, launched %v, running %v; want both quit and %s relaunched", fs.killed, fs.launches, fs.created, named)
				}
			})
		}
	}
}
