package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"claude-remote-api/internal/profile"
	"claude-remote-api/internal/session"
)

// profilesReply is the GET /profiles body.
type profilesReply struct {
	Profiles []profileEntry `json:"profiles"`
	Default  string         `json:"default"`
}

type profileEntry struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// multiProfileManager configures work and private, private the default, under
// a temp home; it returns the manager and the dirs that MUST NOT leak.
func multiProfileManager(t *testing.T) (*session.Manager, []string) {
	t.Helper()
	user := t.TempDir()
	t.Setenv("HOME", user)
	priv := filepath.Join(t.TempDir(), "claude-private")
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,private="+priv)
	t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "private")
	set, err := profile.FromEnv("", user)
	if err != nil {
		t.Fatal(err)
	}
	return &session.Manager{Prefix: "crtest", ClaudeHome: filepath.Join(user, ".claude"), Profiles: set},
		[]string{user, filepath.Join(user, ".claude"), priv, ".claude"}
}

func getProfiles(t *testing.T, mgr sessionManager) (profilesReply, string) {
	t.Helper()
	rr := send(t, stubHandler(mgr), http.MethodGet, "/profiles", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /profiles = %d, want 200 (body: %s)", rr.Code, rr.Body)
	}
	var got profilesReply
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET /profiles body %q: %v", rr.Body, err)
	}
	return got, rr.Body.String()
}

func TestProfilesRequiresAuth(t *testing.T) {
	mgr, _ := multiProfileManager(t)
	h := stubHandler(mgr)
	for _, token := range []string{"", "wrong"} {
		rr := do(t, h, http.MethodGet, "/profiles", token)
		if rr.Code != http.StatusUnauthorized || strings.Contains(rr.Body.String(), "private") {
			t.Errorf("GET /profiles with token %q = %d %q, want 401 naming no profile", token, rr.Code, rr.Body)
		}
	}
	if rr := do(t, h, http.MethodGet, "/profiles", testToken); rr.Code != http.StatusOK {
		t.Errorf("GET /profiles with the token = %d, want 200", rr.Code)
	}
}

func TestProfilesListsNamesInOrder(t *testing.T) {
	mgr, dirs := multiProfileManager(t)
	got, body := getProfiles(t, mgr)
	want := profilesReply{Profiles: []profileEntry{{"work", false}, {"private", true}}, Default: "private"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GET /profiles = %+v, want %+v", got, want)
	}
	for _, d := range dirs {
		if strings.Contains(body, d) {
			t.Errorf("GET /profiles body %q leaks the dir %q", body, d)
		}
	}
	var raw struct{ Profiles []map[string]any }
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	for _, p := range raw.Profiles {
		if len(p) != 2 {
			t.Errorf("profile entry %v has %d fields, want name and default only", p, len(p))
		}
	}
}

func TestProfilesSingleProfile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "claude-home")
	set, err := profile.FromEnv(home, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, mgr := range map[string]*session.Manager{
		"from env": {Prefix: "crtest", ClaudeHome: home, Profiles: set},
		"zero set": {Prefix: "crtest", ClaudeHome: home},
	} {
		got, body := getProfiles(t, mgr)
		if got.Default != "default" || len(got.Profiles) != 1 || got.Profiles[0].Name != "default" || !got.Profiles[0].Default {
			t.Errorf("%s: GET /profiles = %+v, want the one profile default, marked default", name, got)
		}
		if strings.Contains(body, home) {
			t.Errorf("%s: GET /profiles body %q leaks the dir %q", name, body, home)
		}
	}
}
