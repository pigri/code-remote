package profile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const home = "/home/u"

// pr is a profile as Parse makes it, from CLAUDE_REMOTE_PROFILES.
// pr is a listed profile configured with home as the user's home dir.
func pr(name, dir string) Profile { return prh(name, dir, home) }

// prh is a listed profile configured with h as the user's home dir.
func prh(name, dir, h string) Profile {
	return Profile{Name: name, Dir: dir, explicit: true, home: h}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		home    string
		want    []Profile
		wantErr string
	}{
		{name: "one", spec: "work=/a", home: home, want: []Profile{pr("work", "/a")}},
		{name: "ordered", spec: "work=/a,private=/b", home: home,
			want: []Profile{pr("work", "/a"), pr("private", "/b")}},
		{name: "tilde", spec: "work=~,dev=~/.claude-dev", home: home,
			want: []Profile{pr("work", home), pr("dev", home+"/.claude-dev")}},
		{name: "cleaned", spec: "work=/a/./b/../c/", home: home, want: []Profile{pr("work", "/a/c")}},
		{name: "spaces trimmed", spec: " work = /a , dev=/b ", home: home,
			want: []Profile{pr("work", "/a"), pr("dev", "/b")}},
		{name: "name charset", spec: "a_b-9=/a", home: home, want: []Profile{pr("a_b-9", "/a")}},
		{name: "empty spec", spec: "", home: home, wantErr: "empty entry"},
		{name: "empty entry", spec: "work=/a,,dev=/b", home: home, wantErr: "empty entry"},
		{name: "trailing comma", spec: "work=/a,", home: home, wantErr: "empty entry"},
		{name: "missing =", spec: "work", home: home, wantErr: "missing '='"},
		{name: "empty name", spec: "=/a", home: home, wantErr: "invalid profile name"},
		{name: "upper case name", spec: "Work=/a", home: home, wantErr: "invalid profile name"},
		{name: "dotted name", spec: "a.b=/a", home: home, wantErr: "invalid profile name"},
		{name: "empty dir", spec: "work=", home: home, wantErr: "empty dir"},
		{name: "duplicate name", spec: "work=/a,work=/b", home: home, wantErr: "duplicate profile name"},
		{name: "duplicate dir after cleaning", spec: "work=/a,dev=/a/", home: home, wantErr: "duplicate profile dir"},
		{name: "duplicate dir via tilde", spec: "work=~/.claude,dev=/home/u/.claude", home: home, wantErr: "duplicate profile dir"},
		{name: "tilde without home", spec: "work=~/.claude", home: "", wantErr: "home"},
		{name: "tilde user form is relative", spec: "work=~bob", home: home,
			wantErr: `profile "work": dir "~bob" is not absolute`},
		{name: "dot dir is relative", spec: "work=.claude-private", home: home,
			wantErr: `profile "work": dir ".claude-private" is not absolute`},
		{name: "nested relative", spec: "work=/a,dev=foo/bar", home: home,
			wantErr: `profile "dev": dir "foo/bar" is not absolute`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.spec, tt.home)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse(%q) error = %v, want one containing %q", tt.spec, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.spec, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestFromEnv(t *testing.T) {
	tests := []struct {
		name      string
		profiles  string
		def       string
		fallback  string
		wantNames []string
		wantDef   Profile
		wantMulti bool
		wantErr   string
	}{
		{name: "unset is a single default", fallback: "/x/.claude",
			wantNames: []string{"default"}, wantDef: pr("default", "/x/.claude")},
		{name: "unset keeps an empty fallback", fallback: "",
			wantNames: []string{"default"}, wantDef: pr("default", "")},
		{name: "default profile ignored when profiles unset", def: "nope", fallback: "/x",
			wantNames: []string{"default"}, wantDef: pr("default", "/x")},
		{name: "first listed is the default", profiles: "work=~/.claude,private=/p", fallback: "/ignored",
			wantNames: []string{"work", "private"}, wantDef: pr("work", home+"/.claude"), wantMulti: true},
		{name: "default picked by name", profiles: "work=/w,private=/p", def: "private",
			wantNames: []string{"work", "private"}, wantDef: pr("private", "/p"), wantMulti: true},
		{name: "one listed profile is still multi", profiles: "work=/w",
			wantNames: []string{"work"}, wantDef: pr("work", "/w"), wantMulti: true},
		{name: "unknown default", profiles: "work=/w", def: "private", wantErr: `"private"`},
		{name: "bad spec", profiles: "work", wantErr: "CLAUDE_REMOTE_PROFILES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_REMOTE_PROFILES", tt.profiles)
			t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", tt.def)
			s, err := FromEnv(tt.fallback, home)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("FromEnv error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if got := s.Names(); !reflect.DeepEqual(got, tt.wantNames) {
				t.Errorf("Names() = %v, want %v", got, tt.wantNames)
			}
			if got := s.Default(); got.Name != tt.wantDef.Name || got.Dir != tt.wantDef.Dir {
				t.Errorf("Default() = %v, want %v", got, tt.wantDef)
			}
			if got := s.Multi(); got != tt.wantMulti {
				t.Errorf("Multi() = %v, want %v", got, tt.wantMulti)
			}
		})
	}
}

func TestSetGet(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=/w,private=/p")
	t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "")
	s, err := FromEnv("", home)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := s.Get("private"); !ok || p != pr("private", "/p") {
		t.Errorf("Get(private) = %v, %v", p, ok)
	}
	if p, ok := s.Get("nope"); ok {
		t.Errorf("Get(nope) = %v, true; want false", p)
	}
	s.Names()[0] = "mutated"
	if got := s.Names()[0]; got != "work" {
		t.Errorf("Names() exposes the set's storage: got %q after mutation", got)
	}
}

func TestZeroSet(t *testing.T) {
	var s Set
	if got := s.Default(); got != (Profile{}) {
		t.Errorf("zero Set Default() = %v, want zero Profile", got)
	}
	if s.Multi() || len(s.Names()) != 0 {
		t.Errorf("zero Set: Multi=%v Names=%v", s.Multi(), s.Names())
	}
	// A Manager built without Profiles MUST keep today's env and trust rule.
	env := []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/c"}
	if got := s.Default().Env(env); !reflect.DeepEqual(got, env) {
		t.Errorf("zero Set Default().Env = %v, want %v unchanged", got, env)
	}
	if got := s.Default().ConfigFile(); got != "" {
		t.Errorf("zero Set Default().ConfigFile = %q, want \"\"", got)
	}
}

func TestConfigFile(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		home string
		want string
	}{
		{name: "user claude dir uses the home file", dir: home + "/.claude", home: home, want: home + "/.claude.json"},
		{name: "unclean user claude dir", dir: home + "/./.claude/", home: home, want: home + "/.claude.json"},
		{name: "other dir uses its own file", dir: "/p", home: home, want: "/p/.claude.json"},
		{name: "dir under home", dir: home + "/.claude-dev", home: home, want: home + "/.claude-dev/.claude.json"},
		{name: "no home known", dir: "/p", home: "", want: "/p/.claude.json"},
		{name: "relative claude dir is not the user's", dir: ".claude", home: "", want: ".claude/.claude.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prh("x", tt.dir, tt.home).ConfigFile(); got != tt.want {
				t.Errorf("ConfigFile(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestEnv(t *testing.T) {
	base := []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/old", "HOME=/home/u", "CLAUDE_CONFIG_DIR=/older"}
	tests := []struct {
		name string
		dir  string
		env  []string
		want []string
	}{
		{name: "user claude dir runs with the var unset", dir: home + "/.claude", env: base,
			want: []string{"PATH=/bin", "HOME=/home/u"}},
		{name: "other dir sets the var once", dir: "/p", env: base,
			want: []string{"PATH=/bin", "HOME=/home/u", "CLAUDE_CONFIG_DIR=/p"}},
		{name: "prefix lookalike kept", dir: "/p", env: []string{"CLAUDE_CONFIG_DIRX=1"},
			want: []string{"CLAUDE_CONFIG_DIRX=1", "CLAUDE_CONFIG_DIR=/p"}},
		{name: "empty env", dir: "/p", env: nil, want: []string{"CLAUDE_CONFIG_DIR=/p"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.env...)
			got := pr("x", tt.dir).Env(tt.env)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Env() = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.env, in) {
				t.Errorf("Env() modified its input: %v, was %v", tt.env, in)
			}
		})
	}
}

// symlinkedHome returns a real home dir holding .claude, and a symlink to it.
func symlinkedHome(t *testing.T) (real, link string) {
	t.Helper()
	real, link = symlinkedEmptyHome(t)
	if err := os.Mkdir(filepath.Join(real, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	return real, link
}

// symlinkedEmptyHome returns an empty real home dir and a symlink to it.
func symlinkedEmptyHome(t *testing.T) (real, link string) {
	t.Helper()
	real = t.TempDir()
	link = filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	return real, link
}

func TestUserClaudeThroughSymlink(t *testing.T) {
	real, link := symlinkedHome(t)
	for _, tc := range []struct{ name, dir, home string }{
		{"home is a symlink", real + "/.claude", link},
		{"dir is under a symlink", link + "/.claude", real},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prh("x", tc.dir, tc.home)
			if got, want := p.ConfigFile(), filepath.Join(tc.home, ".claude.json"); got != want {
				t.Errorf("ConfigFile = %q, want %q", got, want)
			}
			if got := p.Env([]string{"CLAUDE_CONFIG_DIR=/old"}); len(got) != 0 {
				t.Errorf("Env = %v, want CLAUDE_CONFIG_DIR removed and not set", got)
			}
		})
	}
	t.Run("missing dirs are not aliases", func(t *testing.T) {
		if got := prh("x", "/no/such/.claude", "/no/such-home").ConfigFile(); got != "/no/such/.claude/.claude.json" {
			t.Errorf("ConfigFile = %q", got)
		}
	})
}

func TestParseDuplicateDirThroughSymlink(t *testing.T) {
	real, link := symlinkedHome(t)
	_, err := Parse("work="+real+"/.claude,dev="+link+"/.claude", "")
	if err == nil || !strings.Contains(err.Error(), "duplicate profile dir") {
		t.Fatalf("Parse error = %v, want a duplicate profile dir", err)
	}
	if _, err := Parse("work=~/.claude,dev="+real+"/.claude", link); err == nil || !strings.Contains(err.Error(), "duplicate profile dir") {
		t.Fatalf("Parse via ~ error = %v, want a duplicate profile dir", err)
	}
}

// With CLAUDE_REMOTE_PROFILES unset, the single default profile MUST leave the
// environment and the trust config exactly as they were before profiles.
func TestSingleDefaultInherits(t *testing.T) {
	tests := []struct {
		name     string
		fallback string
		env      []string
	}{
		{name: "explicit CLAUDE_CONFIG_DIR=~/.claude", fallback: home + "/.claude",
			env: []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=" + home + "/.claude"}},
		{name: "CLAUDE_HOME only", fallback: "/ch", env: []string{"PATH=/bin", "CLAUDE_HOME=/ch"}},
		{name: "empty dir", fallback: "", env: []string{"PATH=/bin"}},
		{name: "other CLAUDE_CONFIG_DIR", fallback: "/c", env: []string{"CLAUDE_CONFIG_DIR=/c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_REMOTE_PROFILES", "")
			s, err := FromEnv(tt.fallback, home)
			if err != nil {
				t.Fatal(err)
			}
			p := s.Default()
			in := append([]string(nil), tt.env...)
			got := p.Env(tt.env)
			if !reflect.DeepEqual(got, in) {
				t.Errorf("Env = %v, want the input unchanged %v", got, in)
			}
			if len(got) > 0 {
				got[0] = "mutated"
				if tt.env[0] == "mutated" {
					t.Error("Env returned its input, not a copy")
				}
			}
			if cf := p.ConfigFile(); cf != "" {
				t.Errorf("ConfigFile = %q, want \"\" (use the Manager's rule)", cf)
			}
		})
	}
}

func TestListedProfilesDoNotInherit(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=/w")
	s, err := FromEnv("/ignored", home)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Default().ConfigFile(); got != "/w/.claude.json" {
		t.Errorf("ConfigFile = %q, want /w/.claude.json", got)
	}
	if got := s.Default().Env(nil); !reflect.DeepEqual(got, []string{"CLAUDE_CONFIG_DIR=/w"}) {
		t.Errorf("Env = %v", got)
	}
}

// A profile dir claude has not created yet MUST match ~/.claude the same way
// it will once it exists, or the match flips when claude creates it.
func TestUserClaudeNotYetCreated(t *testing.T) {
	real, link := symlinkedEmptyHome(t)
	for _, tc := range []struct{ name, dir, home string }{
		{"home is a symlink", real + "/.claude", link},
		{"dir is under a symlink", link + "/.claude", real},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prh("x", tc.dir, tc.home)
			if got, want := p.ConfigFile(), filepath.Join(tc.home, ".claude.json"); got != want {
				t.Errorf("ConfigFile = %q, want %q", got, want)
			}
			if got := p.Env([]string{"CLAUDE_CONFIG_DIR=/old"}); len(got) != 0 {
				t.Errorf("Env = %v, want CLAUDE_CONFIG_DIR removed and not set", got)
			}
		})
	}
	t.Run("other missing dir under the same home", func(t *testing.T) {
		if got, want := prh("x", real+"/.claude-dev", link).ConfigFile(), real+"/.claude-dev/.claude.json"; got != want {
			t.Errorf("ConfigFile = %q, want %q", got, want)
		}
	})
}

func TestParseDuplicateDirNotYetCreated(t *testing.T) {
	real, link := symlinkedEmptyHome(t)
	for _, spec := range []string{
		"work=" + real + "/.claude,dev=" + link + "/.claude",
		"work=" + real + "/a/b,dev=" + link + "/a/b",
	} {
		if _, err := Parse(spec, ""); err == nil || !strings.Contains(err.Error(), "duplicate profile dir") {
			t.Errorf("Parse(%q) error = %v, want a duplicate profile dir", spec, err)
		}
	}
	if _, err := Parse("work="+real+"/a,dev="+link+"/b", ""); err != nil {
		t.Errorf("distinct missing dirs: %v", err)
	}
}

func TestIsUserClaude(t *testing.T) {
	for _, tc := range []struct {
		p    Profile
		home string
		want bool
	}{
		{pr("work", home+"/.claude"), home, true},
		{pr("work", home+"/.claude/"), home, true},
		{pr("private", home+"/.claude-private"), home, false},
		{prh("work", home+"/.claude", ""), "", false},
		{Profile{Name: "default", Dir: home + "/.claude", home: home}, home, true},
	} {
		if got := tc.p.IsUserClaude(); got != tc.want {
			t.Errorf("%+v.IsUserClaude() = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// A profile keeps the home it was parsed with: changing $HOME before launch
// moves neither its env nor its config file.
func TestProfileKeepsItsParseTimeHome(t *testing.T) {
	list, err := Parse("work=/h/.claude,private=/h/.claude-private", "/h")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	work, private := list[0], list[1]
	if got := work.Env([]string{"CLAUDE_CONFIG_DIR=/old"}); len(got) != 0 {
		t.Errorf("work Env = %v, want CLAUDE_CONFIG_DIR removed", got)
	}
	if got := work.ConfigFile(); got != "/h/.claude.json" {
		t.Errorf("work ConfigFile = %q, want /h/.claude.json", got)
	}
	if got := private.Env(nil); !reflect.DeepEqual(got, []string{"CLAUDE_CONFIG_DIR=/h/.claude-private"}) {
		t.Errorf("private Env = %v", got)
	}
}

// ForConfigDir maps the CLAUDE_CONFIG_DIR a claude process runs with to the
// profile it is in: unset is ~/.claude, a dir is the profile with that dir,
// through symlinks too.
func TestForConfigDir(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude-private"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "priv-link")
	if err := os.Symlink(filepath.Join(home, ".claude-private"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,private=~/.claude-private")
	t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "private")
	s, err := FromEnv("", home)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dir, want string
		ok        bool
	}{
		{"", "work", true},
		{filepath.Join(home, ".claude-private"), "private", true},
		{link + "/", "private", true},
		{filepath.Join(home, ".claude"), "work", true},
		{filepath.Join(home, "other"), "", false},
	} {
		if p, ok := s.ForConfigDir(tc.dir); ok != tc.ok || p.Name != tc.want {
			t.Errorf("ForConfigDir(%q) = %q, %v; want %q, %v", tc.dir, p.Name, ok, tc.want, tc.ok)
		}
	}
}
