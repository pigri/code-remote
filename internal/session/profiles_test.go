package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"claude-remote-api/internal/profile"
)

const (
	idA = "6fd0b321-a454-4b40-9aed-131afe120d36"
	idB = "a9c1cf1e-ce20-4833-9eeb-7acf5c327506"
)

// homes is a temp $HOME holding the two profile dirs of twoProfiles.
type homes struct{ user, work, private string }

// twoProfiles points $HOME at a temp dir and configures
// work=~/.claude,private=~/.claude-private, both created, with def as the
// default ("" for the first listed).
func twoProfiles(t *testing.T, def string) (profile.Set, homes) {
	t.Helper()
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,private=~/.claude-private")
	t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", def)
	set, err := profile.FromEnv("", user)
	if err != nil {
		t.Fatal(err)
	}
	h := homes{user, filepath.Join(user, ".claude"), filepath.Join(user, ".claude-private")}
	for _, d := range []string{h.work, h.private} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return set, h
}

// writeTitledLog drops a session log for id under home, titled title.
func writeTitledLog(t *testing.T, home, id, title string) {
	t.Helper()
	proj := filepath.Join(home, "projects", "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","cwd":"` + t.TempDir() + `","sessionId":"` + id + `"}` + "\n" +
		`{"type":"custom-title","customTitle":"` + title + `","sessionId":"` + id + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// envValue is key's value in env, and whether it is set.
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v, true
		}
	}
	return "", false
}

func TestListReadsEachSessionFromItsProfile(t *testing.T) {
	set, h := twoProfiles(t, "")
	writeTitledLog(t, h.work, idA, "work title")
	writeTitledLog(t, h.private, idB, "private title")
	fs := &fakeScreen{created: []string{"test-rc-" + idA, "test-rc-" + idB}}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ScreenBin: "screen", Profiles: set}

	got, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Session{}
	for _, s := range got {
		byID[s.ID] = s
	}
	for id, want := range map[string]struct{ profile, title string }{
		idA: {"work", "work title"},
		idB: {"private", "private title"},
	} {
		s := byID[id]
		if s.Profile != want.profile || s.Title != want.title || s.LastActive == "" {
			t.Errorf("%s = %+v, want profile %q, title %q and a last-active time", id, s, want.profile, want.title)
		}
		if logIn(m.profileOf(id).Dir, id) == "" {
			t.Errorf("no log for %s in its profile", id)
		}
	}
}

func TestProfileResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		def     string // default profile
		stored  string // profile in the store
		logIn   string // profile whose dir holds the transcript ("" none)
		want    string
		noStore bool
	}{
		{name: "stored profile wins over the transcript", stored: "private", logIn: "work", want: "private"},
		{name: "transcript when none is stored", logIn: "private", want: "private"},
		{name: "transcript when the stored one is not configured", stored: "gone", logIn: "private", want: "private"},
		{name: "no store: transcript", noStore: true, logIn: "private", want: "private"},
		{name: "nothing known: the ~/.claude profile, not the default", def: "private", want: "work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, h := twoProfiles(t, tc.def)
			switch tc.logIn {
			case "work":
				writeTitledLog(t, h.work, idA, "t")
			case "private":
				writeTitledLog(t, h.private, idA, "t")
			}
			m := &Manager{Profiles: set}
			if !tc.noStore {
				m.Store = &fakeStore{stored: map[string]string{idA: tc.stored}}
			}
			if got := m.profileOf(idA).Name; got != tc.want {
				t.Errorf("profile = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("no ~/.claude profile: the default", func(t *testing.T) {
		user := t.TempDir()
		t.Setenv("HOME", user)
		t.Setenv("CLAUDE_REMOTE_PROFILES", "a="+filepath.Join(user, "a")+",b="+filepath.Join(user, "b"))
		t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "b")
		set, err := profile.FromEnv("", user)
		if err != nil {
			t.Fatal(err)
		}
		if got := (&Manager{Profiles: set}).profileOf(idA).Name; got != "b" {
			t.Errorf("profile = %q, want the default b", got)
		}
	})
}

func TestRegistrationsReadEveryProfile(t *testing.T) {
	set, h := twoProfiles(t, "")
	upgradeHome(t, h.work, idA, "1.0.0", "idle")
	upgradeHome(t, h.private, idB, "1.0.0", "busy")
	m := &Manager{Profiles: set}

	regs, err := m.Registrations()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range regs {
		got[r.SessionID] = r.Profile
	}
	if want := map[string]string{idA: "work", idB: "private"}; len(got) != 2 || got[idA] != want[idA] || got[idB] != want[idB] {
		t.Errorf("registrations by profile = %v, want %v", got, want)
	}
}

func TestTrustIsPerProfile(t *testing.T) {
	set, h := twoProfiles(t, "")
	root := evalDir(t)
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	other := filepath.Join(t.TempDir(), "other.json")
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", WorkspaceRoot: root,
		TrustDirs: true, Profiles: set, ClaudeConfig: other}

	if _, err := m.Create(root, "private"); err != nil {
		t.Fatal(err)
	}
	trusted := func(path string) bool {
		data, _ := os.ReadFile(path)
		return strings.Contains(string(data), `"`+root+`"`)
	}
	privCfg, workCfg := filepath.Join(h.private, ".claude.json"), filepath.Join(h.user, ".claude.json")
	if !trusted(privCfg) || trusted(workCfg) || trusted(other) {
		t.Errorf("after a private create: private %v, work %v, ClaudeConfig %v; want only private's trusted",
			trusted(privCfg), trusted(workCfg), trusted(other))
	}
	if _, err := m.Create(root, "work"); err != nil {
		t.Fatal(err)
	}
	if !trusted(workCfg) || trusted(filepath.Join(h.work, ".claude.json")) {
		t.Errorf("after a work create: want ~/.claude.json trusted, not ~/.claude/.claude.json")
	}
}

func TestLaunchEnvIsPerProfile(t *testing.T) {
	set, h := twoProfiles(t, "")
	t.Setenv("CLAUDE_CONFIG_DIR", "/parent/config")
	fs := &fakeScreen{parentEnv: true}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}

	if _, err := m.Create("", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("", "private"); err != nil {
		t.Fatal(err)
	}
	if v, ok := envValue(fs.cmds[0].Env, "CLAUDE_CONFIG_DIR"); ok {
		t.Errorf("work child has CLAUDE_CONFIG_DIR=%q, want it removed", v)
	}
	if v, _ := envValue(fs.cmds[1].Env, "CLAUDE_CONFIG_DIR"); v != h.private {
		t.Errorf("private child CLAUDE_CONFIG_DIR = %q, want %q", v, h.private)
	}

	t.Run("resume launches in the session's profile", func(t *testing.T) {
		writeTitledLog(t, h.private, idA, "t")
		fs := &fakeScreen{parentEnv: true}
		withFakeScreen(t, fs)
		if _, err := m.Resume(idA); err != nil {
			t.Fatal(err)
		}
		if v, _ := envValue(fs.cmds[0].Env, "CLAUDE_CONFIG_DIR"); v != h.private {
			t.Errorf("resumed child CLAUDE_CONFIG_DIR = %q, want %q", v, h.private)
		}
	})

	t.Run("single profile passes the variable through", func(t *testing.T) {
		fs := &fakeScreen{parentEnv: true}
		withFakeScreen(t, fs)
		single := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen"}
		if _, err := single.Create("", ""); err != nil {
			t.Fatal(err)
		}
		if v, _ := envValue(fs.cmds[0].Env, "CLAUDE_CONFIG_DIR"); v != "/parent/config" {
			t.Errorf("CLAUDE_CONFIG_DIR = %q, want the parent's /parent/config", v)
		}
	})
}

func TestRecordWritesTheResolvedProfile(t *testing.T) {
	set, h := twoProfiles(t, "")

	t.Run("create", func(t *testing.T) {
		fs, fst := &fakeScreen{}, &fakeStore{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}
		for _, p := range []string{"", "private", "work"} {
			s, err := m.Create("", p)
			if err != nil {
				t.Fatal(err)
			}
			want := p
			if p == "" {
				want = "work" // the default
			}
			if s.Profile != want {
				t.Errorf("Create(%q).Profile = %q, want %q", p, s.Profile, want)
			}
		}
		if want := []string{"work", "private", "work"}; !slices.Equal(fst.profiles, want) {
			t.Errorf("recorded profiles = %q, want %q", fst.profiles, want)
		}
	})

	t.Run("resume of a legacy row", func(t *testing.T) {
		writeTitledLog(t, h.private, idB, "t")
		fs, fst := &fakeScreen{}, &fakeStore{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}
		s, err := m.ResumeIn(idB, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if s.Profile != "private" || !slices.Equal(fst.profiles, []string{"private"}) {
			t.Errorf("Profile = %q, recorded %q; want private", s.Profile, fst.profiles)
		}
	})

	// Single-profile mode records no profile: "default" is not a name the
	// row could be resolved by once profiles are configured.
	t.Run("single profile", func(t *testing.T) {
		fs, fst := &fakeScreen{}, &fakeStore{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Store: fst}
		if _, err := m.Create("", ""); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(fst.profiles, []string{""}) {
			t.Errorf("recorded profiles = %q, want [\"\"]", fst.profiles)
		}
	})

	t.Run("single profile keeps a stored profile across a restart", func(t *testing.T) {
		fastStop(t)
		home := t.TempDir()
		upgradeHome(t, home, idA, "", "")
		fs, fst := &fakeScreen{created: []string{"test-rc-" + idA}}, &fakeStore{stored: map[string]string{idA: "private"}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home, Store: fst}
		if _, err := m.RestartIn(idA, "", ""); err != nil {
			t.Fatal(err)
		}
		if len(fs.launches) != 1 || !slices.Equal(fst.profiles, []string{""}) || fst.stored[idA] != "private" {
			t.Errorf("launches %d, recorded %q, stored %q; want one launch recording no profile, private kept",
				len(fs.launches), fst.profiles, fst.stored[idA])
		}
	})
}

func TestUnknownProfileLaunchesNothing(t *testing.T) {
	set, h := twoProfiles(t, "")
	writeTitledLog(t, h.work, idA, "t")
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}

	if _, err := m.Create("", "nope"); !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("Create(nope) err = %v, want ErrInvalidProfile", err)
	}
	if _, err := m.ResumeIn(idA, "", "nope"); !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("ResumeIn(nope) err = %v, want ErrInvalidProfile", err)
	}
	if _, err := (&Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen"}).Create("", "work"); !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("single-profile Create(work) err = %v, want ErrInvalidProfile", err)
	}
	if len(fs.launches) != 0 {
		t.Errorf("launches = %v, want none", fs.launches)
	}
}

func TestOtherProfileIsRefused(t *testing.T) {
	fastStop(t)
	set, h := twoProfiles(t, "")
	writeTitledLog(t, h.private, idA, "t")
	fst := &fakeStore{stored: map[string]string{idA: "private"}}

	t.Run("resume", func(t *testing.T) {
		fs := &fakeScreen{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}
		if _, err := m.ResumeIn(idA, "", "work"); !errors.Is(err, ErrOtherProfile) {
			t.Fatalf("ResumeIn(work) err = %v, want ErrOtherProfile", err)
		}
		if len(fs.launches) != 0 {
			t.Errorf("launches = %v, want none", fs.launches)
		}
		if _, err := m.ResumeIn(idA, "", "private"); err != nil {
			t.Errorf("ResumeIn(its own profile) err = %v", err)
		}
	})

	t.Run("restart", func(t *testing.T) {
		fs := &fakeScreen{created: []string{"test-rc-" + idA}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}
		if _, err := m.RestartIn(idA, "", "work"); !errors.Is(err, ErrOtherProfile) {
			t.Fatalf("RestartIn(work) err = %v, want ErrOtherProfile", err)
		}
		if len(fs.killed) != 0 || len(fs.launches) != 0 {
			t.Errorf("killed %v, launched %v; want the session left alone", fs.killed, fs.launches)
		}
	})
}

func TestListAllResolvesStoppedProfiles(t *testing.T) {
	set, h := twoProfiles(t, "")
	writeTitledLog(t, h.private, idA, "private title")
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ScreenBin: "screen", Profiles: set}

	got, err := m.ListAll(&fakeStore{stopped: []Session{{ID: idA}}}) // a legacy row: no profile
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Profile != "private" || got[0].Title != "private title" || got[0].LastActive == "" {
		t.Errorf("ListAll = %+v, want the stopped session in private with its title", got)
	}
}

func TestUpgradeRestartsInTheSessionsProfile(t *testing.T) {
	fastStop(t)
	set, h := twoProfiles(t, "")
	upgradeHome(t, h.private, idA, "1.0.0", "idle")
	fs := &fakeScreen{created: []string{"test-rc-" + idA}, version: "1.0.0", updateTo: "2.0.0", parentEnv: true}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}

	res, err := m.Upgrade(true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].Action != ActionRestarted {
		t.Fatalf("Upgrade = %+v, want the private session restarted", res.Sessions)
	}
	if v, _ := envValue(fs.cmds[0].Env, "CLAUDE_CONFIG_DIR"); v != h.private {
		t.Errorf("relaunch CLAUDE_CONFIG_DIR = %q, want %q", v, h.private)
	}
}

func TestSessionJSONCarriesProfile(t *testing.T) {
	data, err := json.Marshal(Session{ID: idA, Profile: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"profile":"private"`) {
		t.Errorf("JSON = %s, want a profile field", data)
	}
	if data, _ := json.Marshal(Session{ID: idA}); strings.Contains(string(data), "profile") {
		t.Errorf("JSON = %s, want no profile field when there is none", data)
	}
}

// A listing resolves each session's profile once, and a restart resolves it
// once before it launches.
func TestOneResolutionPerSession(t *testing.T) {
	set, h := twoProfiles(t, "")
	writeTitledLog(t, h.private, idA, "a")
	writeTitledLog(t, h.private, idB, "b")
	fs := &fakeScreen{created: []string{"test-rc-" + idA}}
	withFakeScreen(t, fs)
	fst := &fakeStore{stored: map[string]string{idA: "private", idB: "private"}, stopped: []Session{{ID: idB}}}
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}

	if _, err := m.ListAll(fst); err != nil {
		t.Fatal(err)
	}
	if fst.lookups[idA] != 1 || fst.lookups[idB] != 1 {
		t.Errorf("ListAll lookups = %v, want one per session", fst.lookups)
	}

	fst.lookups = nil
	fs.failCreate = true // stop at the launch: count what comes before it
	if _, err := m.RestartIn(idB, "", ""); err == nil {
		t.Fatal("RestartIn with a failing screen = nil error")
	}
	if fst.lookups[idB] != 1 {
		t.Errorf("RestartIn(stopped) lookups = %d, want 1", fst.lookups[idB])
	}
}

// A session that does not exist is not found, whatever profile is asked for.
func TestUnknownSessionWithProfileIsNotFound(t *testing.T) {
	fastStop(t)
	set, _ := twoProfiles(t, "")
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}
	for _, prof := range []string{"work", "private", "nope"} {
		if _, err := m.ResumeIn(idA, "", prof); !errors.Is(err, ErrNotResumable) {
			t.Errorf("ResumeIn(unknown, %q) err = %v, want ErrNotResumable", prof, err)
		}
		if _, err := m.RestartIn(idA, "", prof); !errors.Is(err, ErrNotResumable) {
			t.Errorf("RestartIn(unknown, %q) err = %v, want ErrNotResumable", prof, err)
		}
	}
	if len(fs.launches) != 0 || len(fs.killed) != 0 {
		t.Errorf("launches %v, kills %v; want none", fs.launches, fs.killed)
	}
}

// Profiles keep the home they were configured with: a later change of $HOME
// moves neither the launch env nor the ~/.claude match.
func TestProfilesKeepTheirParseTimeHome(t *testing.T) {
	set, _ := twoProfiles(t, "private")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/parent/config")
	fs := &fakeScreen{parentEnv: true}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set}

	if _, err := m.Create("", "work"); err != nil {
		t.Fatal(err)
	}
	if v, ok := envValue(fs.cmds[0].Env, "CLAUDE_CONFIG_DIR"); ok {
		t.Errorf("work child has CLAUDE_CONFIG_DIR=%q, want it removed", v)
	}
	if got := m.profileOf(idB).Name; got != "work" {
		t.Errorf("profile of an unknown session = %q, want work, the ~/.claude profile", got)
	}
}

// A profile's view holds the sessions whose claude runs in that profile: the
// profile of its live registration when it has one, else its resolved one.
func TestForProfileHoldsOneProfile(t *testing.T) {
	const idC = "3c4b2a1e-0f9d-4e8c-b7a6-5d4c3b2a1f0e"
	set, h := twoProfiles(t, "")
	upgradeHome(t, h.private, idA, "1.0.0", "idle") // resolves to work, registered in private
	upgradeHome(t, h.work, idB, "1.0.0", "idle")
	writeTitledLog(t, h.private, idC, "c") // private, not registered
	fs := &fakeScreen{created: []string{"test-rc-" + idA, "test-rc-" + idB, "test-rc-" + idC}}
	withFakeScreen(t, fs)
	fst := &fakeStore{stored: map[string]string{idA: "work", idB: "work", idC: "private"}}
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}

	ids := func(l []Session) []string {
		var out []string
		for _, s := range l {
			out = append(out, s.ID)
		}
		return out
	}
	for _, tc := range []struct {
		name       string
		list, regs []string
		live       []string
	}{
		{"work", []string{idB}, []string{idB}, []string{idB}},
		{"private", []string{idA, idC}, []string{idA}, []string{idA}},
	} {
		v, err := m.ForProfile(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if l, _ := v.List(); !slices.Equal(ids(l), tc.list) {
			t.Errorf("%s List = %v, want %v", tc.name, ids(l), tc.list)
		}
		regs, _ := v.Registrations()
		var got []string
		for _, r := range regs {
			got = append(got, r.SessionID)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.regs) {
			t.Errorf("%s Registrations = %v, want %v", tc.name, got, tc.regs)
		}
		got = got[:0]
		for id := range v.LiveRegistrations() {
			got = append(got, id)
		}
		if !slices.Equal(got, tc.live) {
			t.Errorf("%s LiveRegistrations = %v, want %v", tc.name, got, tc.live)
		}
	}
	if _, err := m.ForProfile("nope"); !errors.Is(err, ErrInvalidProfile) {
		t.Errorf("ForProfile(nope) err = %v, want ErrInvalidProfile", err)
	}
}

// A profile's view kills only its own sessions.
func TestForProfileKillRefusesAnotherProfile(t *testing.T) {
	set, h := twoProfiles(t, "")
	upgradeHome(t, h.private, idA, "1.0.0", "idle")
	upgradeHome(t, h.work, idB, "1.0.0", "idle")
	fs := &fakeScreen{created: []string{"test-rc-" + idA, "test-rc-" + idB}}
	withFakeScreen(t, fs)
	fst := &fakeStore{stored: map[string]string{idA: "work", idB: "work"}}
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}
	v, err := m.ForProfile("work")
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := v.Kill(idA); ok || !errors.Is(err, ErrOtherProfile) {
		t.Errorf("work Kill(private session) = %v, %v; want false, ErrOtherProfile", ok, err)
	}
	if len(fs.killed) != 0 {
		t.Fatalf("killed %v after a refused kill", fs.killed)
	}
	if ok, err := v.Kill("9e8d7c6b-5a49-4382-a1b0-c9d8e7f6a5b4"); ok || err != nil {
		t.Errorf("work Kill(unknown) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := v.Kill(idB); !ok || err != nil {
		t.Errorf("work Kill(work session) = %v, %v; want true, nil", ok, err)
	}
	if !slices.Equal(fs.killed, []string{"test-rc-" + idB}) {
		t.Errorf("killed = %v, want only the work session", fs.killed)
	}
}

// When two profiles' registries hold the same pid, the entry in the session's
// own profile is its registration. When neither is in its own profile, it
// has none: no entry a stale file left behind decides where it runs.
func TestRegistryCollisionAcrossProfiles(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,private=~/.claude-private,third=~/.claude-third")
	set, err := profile.FromEnv("", user)
	if err != nil {
		t.Fatal(err)
	}
	reg := func(dir, file, id string) {
		sdir := filepath.Join(user, dir, "sessions")
		if err := os.MkdirAll(sdir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"sessionId":"` + id + `","pid":7777,"bridgeSessionId":"bridge_` + dir + `"}`
		if err := os.WriteFile(filepath.Join(sdir, file+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Neither entry carries the session's id: the live claude has moved to a
	// new conversation, and the stale one was another session.
	const conv, stale = "9e8d7c6b-5a49-4382-a1b0-c9d8e7f6a5b4", "3c4b2a1e-0f9d-4e8c-b7a6-5d4c3b2a1f0e"
	reg(".claude", "a", conv) // the live claude, listed first
	reg(".claude-third", "s", stale)

	for _, tc := range []struct {
		stored, want, bridge string
	}{
		{"work", "work", "bridge_.claude"}, // the session's own entry wins over the later stale one
		{"private", "private", ""},         // neither entry is its own: no registration
	} {
		t.Run(tc.stored, func(t *testing.T) {
			fs := &fakeScreen{created: []string{"test-rc-" + idA}, ps: "7777 10000\n"}
			withFakeScreen(t, fs)
			fst := &fakeStore{stored: map[string]string{idA: tc.stored}}
			m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}
			l, _ := m.List()
			if len(l) != 1 || l[0].Profile != tc.want {
				t.Fatalf("List = %+v, want the session in %s", l, tc.want)
			}
			live := m.LiveRegistrations()
			if got := live[idA].BridgeSessionID; got != tc.bridge {
				t.Errorf("live bridge = %q, want %q", got, tc.bridge)
			}
			for _, name := range []string{"work", "private", "third"} {
				v, _ := m.ForProfile(name)
				vl, _ := v.List()
				if in := len(vl) == 1; in != (name == tc.want) {
					t.Errorf("%s view holds the session: %v, want %v", name, in, name == tc.want)
				}
			}
		})
	}
}
