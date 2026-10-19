package profile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const home = "/home/u"

func TestParse(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		spec    string
		home    string
		want    []Profile
		wantErr string
	}{
		{name: "one", spec: "work=/a", home: home, want: []Profile{{"work", "/a"}}},
		{name: "ordered", spec: "work=/a,private=/b", home: home,
			want: []Profile{{"work", "/a"}, {"private", "/b"}}},
		{name: "tilde", spec: "work=~,dev=~/.claude-dev", home: home,
			want: []Profile{{"work", home}, {"dev", home + "/.claude-dev"}}},
		{name: "cleaned", spec: "work=/a/./b/../c/", home: home, want: []Profile{{"work", "/a/c"}}},
		{name: "relative made absolute", spec: "work=rel", home: home,
			want: []Profile{{"work", filepath.Join(cwd, "rel")}}},
		{name: "spaces trimmed", spec: " work = /a , dev=/b ", home: home,
			want: []Profile{{"work", "/a"}, {"dev", "/b"}}},
		{name: "name charset", spec: "a_b-9=/a", home: home, want: []Profile{{"a_b-9", "/a"}}},
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
		{name: "tilde user form not expanded", spec: "work=~bob", home: home,
			want: []Profile{{"work", filepath.Join(cwd, "~bob")}}},
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
			wantNames: []string{"default"}, wantDef: Profile{"default", "/x/.claude"}},
		{name: "unset keeps an empty fallback", fallback: "",
			wantNames: []string{"default"}, wantDef: Profile{"default", ""}},
		{name: "default profile ignored when profiles unset", def: "nope", fallback: "/x",
			wantNames: []string{"default"}, wantDef: Profile{"default", "/x"}},
		{name: "first listed is the default", profiles: "work=~/.claude,private=/p", fallback: "/ignored",
			wantNames: []string{"work", "private"}, wantDef: Profile{"work", home + "/.claude"}, wantMulti: true},
		{name: "default picked by name", profiles: "work=/w,private=/p", def: "private",
			wantNames: []string{"work", "private"}, wantDef: Profile{"private", "/p"}, wantMulti: true},
		{name: "one listed profile is still multi", profiles: "work=/w",
			wantNames: []string{"work"}, wantDef: Profile{"work", "/w"}, wantMulti: true},
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
			if got := s.Default(); got != tt.wantDef {
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
	if p, ok := s.Get("private"); !ok || p != (Profile{"private", "/p"}) {
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
			if got := (Profile{Name: "x", Dir: tt.dir}).ConfigFile(tt.home); got != tt.want {
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
			got := (Profile{Name: "x", Dir: tt.dir}).Env(tt.env, home)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Env() = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.env, in) {
				t.Errorf("Env() modified its input: %v, was %v", tt.env, in)
			}
		})
	}
}
