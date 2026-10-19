package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"claude-remote-api/internal/profile"
	"claude-remote-api/internal/session"
)

func TestHumanizeAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name, ts, want string
	}{
		{"empty", "", "-"},
		{"unparseable", "not-a-time", "-"},
		{"seconds -> just now", now.Add(-30 * time.Second).Format(time.RFC3339), "just now"},
		{"minutes", now.Add(-5 * time.Minute).Format(time.RFC3339), "5m ago"},
		{"hours", now.Add(-3 * time.Hour).Format(time.RFC3339), "3h ago"},
		{"days", now.Add(-49 * time.Hour).Format(time.RFC3339), "2d ago"},
		{"future clock skew", now.Add(2 * time.Hour).Format(time.RFC3339), "just now"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := humanizeAge(c.ts); got != c.want {
				t.Errorf("humanizeAge(%q) = %q, want %q", c.ts, got, c.want)
			}
		})
	}
}

// captureStdout returns whatever fn writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

type fakeBackend struct {
	sessions    []session.Session
	created     session.Session
	createdDir  string
	createdProf string
	resumed     session.Session
	resumedID   string
	resumedIDs  []string
	movedDir    string           // dir passed to resume / restart
	movedProf   string           // profile passed to resume / restart
	resumeErrs  map[string]error // per-id failures, checked before resumeErr
	removed     string
	listErr     error
	createErr   error
	resumeErr   error
	removeErr   error

	restartedID string
	restartErr  error
	upgradeRes  session.UpgradeResult
	upgradeErr  error
	gotRestart  bool
	gotForce    bool
	gotIDs      []string

	profileList session.ProfileList
	profilesErr error
}

func (f *fakeBackend) profiles() (session.ProfileList, error) { return f.profileList, f.profilesErr }
func (f *fakeBackend) list() ([]session.Session, error)       { return f.sessions, f.listErr }
func (f *fakeBackend) create(dir, prof string) (session.Session, error) {
	f.createdDir, f.createdProf = dir, prof
	return f.created, f.createErr
}
func (f *fakeBackend) resume(id, dir, prof string) (session.Session, error) {
	f.movedDir, f.movedProf = dir, prof
	f.resumedID = id
	f.resumedIDs = append(f.resumedIDs, id)
	if err := f.resumeErrs[id]; err != nil {
		return session.Session{}, err
	}
	return f.resumed, f.resumeErr
}
func (f *fakeBackend) restart(id, dir, prof string) (session.Session, error) {
	f.movedDir, f.movedProf = dir, prof
	f.restartedID = id
	return session.Session{ID: id, Screen: "p-" + id}, f.restartErr
}
func (f *fakeBackend) upgrade(restart, force bool, ids ...string) (session.UpgradeResult, error) {
	f.gotRestart, f.gotForce, f.gotIDs = restart, force, ids
	return f.upgradeRes, f.upgradeErr
}
func (f *fakeBackend) remove(id string) error { f.removed = id; return f.removeErr }

func TestListCommand(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := list(&fakeBackend{}); err != nil {
				t.Errorf("list empty: %v", err)
			}
		})
		if out == "" {
			t.Error("expected a 'no sessions' message")
		}
	})
	t.Run("with rows", func(t *testing.T) {
		be := &fakeBackend{sessions: []session.Session{
			{ID: "id1", Title: "t", Status: "Detached", Screen: "p-id1"},
			{ID: "id2", Status: "Attached", Screen: "p-id2"}, // untitled
		}}
		out := captureStdout(t, func() {
			if err := list(be); err != nil {
				t.Errorf("list: %v", err)
			}
		})
		if !contains(out, "id1") || !contains(out, "(untitled)") {
			t.Errorf("listing missing expected content:\n%s", out)
		}
	})
	t.Run("stopped session shows resume action", func(t *testing.T) {
		be := &fakeBackend{sessions: []session.Session{
			{ID: "run1", Status: "Detached", Screen: "p-run1"},
			{ID: "stop1", Status: "Stopped", Screen: "p-stop1"},
		}}
		out := captureStdout(t, func() {
			if err := list(be); err != nil {
				t.Errorf("list: %v", err)
			}
		})
		if !contains(out, "crctl resume stop1") {
			t.Errorf("stopped session should show resume action:\n%s", out)
		}
		if !contains(out, "screen -r p-run1") {
			t.Errorf("running session should show attach action:\n%s", out)
		}
	})
	t.Run("last active column", func(t *testing.T) {
		be := &fakeBackend{sessions: []session.Session{
			{ID: "run1", Status: "Detached", Screen: "p-run1",
				LastActive: time.Now().Add(-2 * time.Hour).Format(time.RFC3339)},
			{ID: "run2", Status: "Detached", Screen: "p-run2"}, // no timestamp -> "-"
		}}
		out := captureStdout(t, func() {
			if err := list(be); err != nil {
				t.Errorf("list: %v", err)
			}
		})
		if !contains(out, "LAST ACTIVE") {
			t.Errorf("header should include LAST ACTIVE column:\n%s", out)
		}
		if !contains(out, "2h ago") {
			t.Errorf("expected humanized age '2h ago':\n%s", out)
		}
	})
	t.Run("error", func(t *testing.T) {
		if err := list(&fakeBackend{listErr: errors.New("boom")}); err == nil {
			t.Error("list should surface backend error")
		}
	})
}

func TestCreateCommandParsesDir(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no dir", nil, ""},
		{"--dir space", []string{"--dir", "/x"}, "/x"},
		{"-d space", []string{"-d", "/y"}, "/y"},
		{"--dir=", []string{"--dir=/z"}, "/z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			be := &fakeBackend{created: session.Session{ID: "id1", Screen: "p-id1"}}
			out := captureStdout(t, func() {
				if err := create(be, c.args); err != nil {
					t.Fatalf("create: %v", err)
				}
			})
			if be.createdDir != c.want {
				t.Errorf("dir = %q, want %q", be.createdDir, c.want)
			}
			if !contains(out, "started id1") {
				t.Errorf("missing confirmation:\n%s", out)
			}
		})
	}
}

func TestCreateCommandError(t *testing.T) {
	if err := create(&fakeBackend{createErr: errors.New("nope")}, nil); err == nil {
		t.Error("create should surface backend error")
	}
}

// TestRunDispatch drives run() end-to-end against a remote httptest backend so
// the command routing (ls/new/rm/help/unknown/errors) is exercised without screen.
func TestRunDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"sessions":[{"id":"s1","screen":"p-s1"}]}`))
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"s2","screen":"p-s2"}`))
		default: // DELETE
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	t.Setenv("CLAUDE_REMOTE_API_URL", srv.URL)
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")

	t.Run("ls", func(t *testing.T) {
		_ = captureStdout(t, func() {
			if err := run([]string{"ls"}); err != nil {
				t.Errorf("run ls: %v", err)
			}
		})
	})
	t.Run("new", func(t *testing.T) {
		_ = captureStdout(t, func() {
			if err := run([]string{"new", "--dir", "/w"}); err != nil {
				t.Errorf("run new: %v", err)
			}
		})
	})
	t.Run("rm success prints", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := run([]string{"rm", "s1"}); err != nil {
				t.Errorf("run rm: %v", err)
			}
		})
		if !contains(out, "s1 stopped") {
			t.Errorf("rm output = %q, want 's1 stopped'", out)
		}
	})
	t.Run("resume success prints", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := run([]string{"resume", "s2"}); err != nil {
				t.Errorf("run resume: %v", err)
			}
		})
		if !contains(out, "resumed s2") {
			t.Errorf("resume output = %q, want 'resumed s2'", out)
		}
	})
	t.Run("restart success prints", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := run([]string{"restart", "s2"}); err != nil {
				t.Errorf("run restart: %v", err)
			}
		})
		if !contains(out, "restarted s2") {
			t.Errorf("restart output = %q, want 'restarted s2'", out)
		}
	})
	t.Run("restart missing id", func(t *testing.T) {
		if err := run([]string{"restart"}); err == nil {
			t.Error("run restart without id should error")
		}
	})
	t.Run("upgrade", func(t *testing.T) {
		_ = captureStdout(t, func() {
			if err := run([]string{"upgrade"}); err != nil {
				t.Errorf("run upgrade: %v", err)
			}
		})
	})
	t.Run("resume missing id", func(t *testing.T) {
		if err := run([]string{"resume"}); err == nil {
			t.Error("run resume without id should error")
		}
	})
	t.Run("rm missing id", func(t *testing.T) {
		if err := run([]string{"rm"}); err == nil {
			t.Error("run rm without id should error")
		}
	})
	t.Run("unknown command", func(t *testing.T) {
		if err := run([]string{"frobnicate"}); err == nil {
			t.Error("unknown command should error")
		}
	})
	t.Run("help", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := run([]string{"help"}); err != nil {
				t.Errorf("run help: %v", err)
			}
		})
		if !contains(out, "crctl") {
			t.Error("help should print usage")
		}
	})
}

func TestRunPickBackendError(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_URL", "https://api.example") // remote mode...
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "")                  // ...but no token
	if err := run([]string{"ls"}); err == nil {
		t.Error("run should surface pickBackend error")
	}
}

func TestPickBackend(t *testing.T) {
	t.Run("local by default", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_API_URL", "")
		db := filepath.Join(t.TempDir(), "test.db")
		t.Setenv("CLAUDE_REMOTE_DB", db)
		be, err := pickBackend()
		if err != nil {
			t.Fatal(err)
		}
		lb, ok := be.(*localBackend)
		if !ok {
			t.Fatalf("got %T, want *localBackend", be)
		}
		if lb.stopped == nil {
			t.Fatal("local backend has no session store")
		}
		if c, ok := lb.stopped.(io.Closer); ok {
			t.Cleanup(func() { c.Close() })
		}
		if _, err := os.Stat(db); err != nil {
			t.Errorf("pickBackend did not open the session DB at CLAUDE_REMOTE_DB: %v", err)
		}
	})
	t.Run("local has the single default profile", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_API_URL", "")
		t.Setenv("CLAUDE_REMOTE_PROFILES", "")
		t.Setenv("CLAUDE_REMOTE_DB", filepath.Join(t.TempDir(), "test.db"))
		home := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		be, err := pickBackend()
		if err != nil {
			t.Fatal(err)
		}
		lb := be.(*localBackend)
		if c, ok := lb.stopped.(io.Closer); ok {
			t.Cleanup(func() { c.Close() })
		}
		ps := lb.mgr.Profiles
		if d := ps.Default(); d.Name != "default" || d.Dir != home || ps.Multi() {
			t.Errorf("Profiles default = %v multi=%v, want default at %s, single", d, ps.Multi(), home)
		}
		if cf := ps.Default().ConfigFile(); cf != "" {
			t.Errorf("single default ConfigFile = %q, want \"\" (inherit the Manager's rule)", cf)
		}
		if lb.mgr.ClaudeHome != home {
			t.Errorf("ClaudeHome = %q, want %q", lb.mgr.ClaudeHome, home)
		}
	})
	t.Run("local with bad profiles errors", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_API_URL", "")
		t.Setenv("CLAUDE_REMOTE_DB", filepath.Join(t.TempDir(), "test.db"))
		for _, tc := range []struct{ profiles, def string }{{"work", ""}, {"work=/w", "private"}} {
			t.Setenv("CLAUDE_REMOTE_PROFILES", tc.profiles)
			t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", tc.def)
			be, err := pickBackend()
			if err == nil {
				if lb, ok := be.(*localBackend); ok {
					if c, ok := lb.stopped.(io.Closer); ok {
						c.Close()
					}
				}
				t.Errorf("pickBackend with CLAUDE_REMOTE_PROFILES=%q DEFAULT=%q: want error", tc.profiles, tc.def)
			}
		}
	})
	t.Run("remote with token", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_API_URL", "https://api.example/")
		t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
		be, err := pickBackend()
		if err != nil {
			t.Fatal(err)
		}
		hb, ok := be.(*httpBackend)
		if !ok || hb.base != "https://api.example" { // trailing slash trimmed
			t.Errorf("got %#v, want httpBackend base without trailing slash", be)
		}
	})
	t.Run("remote without token errors", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_API_URL", "https://api.example")
		t.Setenv("CLAUDE_REMOTE_API_TOKEN", "")
		if _, err := pickBackend(); err == nil {
			t.Error("remote mode without token should error")
		}
	})
}

func TestHTTPBackend(t *testing.T) {
	const token = "tok-xyz"
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"sessions":[{"id":"s1","screen":"p-s1"}]}`))
		case r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"s2","screen":"p-s2"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	be := &httpBackend{base: srv.URL, token: token}

	t.Run("list", func(t *testing.T) {
		ss, err := be.list()
		if err != nil || len(ss) != 1 || ss[0].ID != "s1" {
			t.Fatalf("list = %+v, %v", ss, err)
		}
		if gotAuth != "Bearer "+token || gotPath != "/sessions" {
			t.Errorf("auth=%q path=%q", gotAuth, gotPath)
		}
	})
	t.Run("create with dir sends body", func(t *testing.T) {
		s, err := be.create("/work", "")
		if err != nil || s.ID != "s2" {
			t.Fatalf("create = %+v, %v", s, err)
		}
		if gotMethod != http.MethodPost || !contains(gotBody, `"dir":"/work"`) {
			t.Errorf("method=%q body=%q", gotMethod, gotBody)
		}
	})
	t.Run("restart", func(t *testing.T) {
		s, err := be.restart("s3", "", "")
		if err != nil || s.ID != "s2" {
			t.Fatalf("restart = %+v, %v", s, err)
		}
		if gotMethod != http.MethodPost || gotPath != "/sessions/s3/restart" {
			t.Errorf("method=%q path=%q", gotMethod, gotPath)
		}
	})
	t.Run("upgrade sends flags", func(t *testing.T) {
		if _, err := be.upgrade(false, true); err != nil {
			t.Fatal(err)
		}
		if gotPath != "/upgrade" || !contains(gotBody, `"restart":false`) || !contains(gotBody, `"force":true`) {
			t.Errorf("path=%q body=%q", gotPath, gotBody)
		}
		if _, err := be.upgrade(true, false, "s9"); err != nil || !contains(gotBody, `"sessions":["s9"]`) {
			t.Errorf("upgrade with ids: err=%v body=%q", err, gotBody)
		}
	})
	t.Run("remove", func(t *testing.T) {
		if err := be.remove("s3"); err != nil {
			t.Fatal(err)
		}
		if gotMethod != http.MethodDelete || gotPath != "/sessions/s3" {
			t.Errorf("method=%q path=%q", gotMethod, gotPath)
		}
	})
}

func TestHTTPBackendErrorBodies(t *testing.T) {
	t.Run("json error message", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"session not found"}`))
		}))
		defer srv.Close()
		err := (&httpBackend{base: srv.URL, token: "t"}).remove("x")
		if err == nil || !contains(err.Error(), "session not found") {
			t.Errorf("err = %v, want it to include the API message", err)
		}
	})
	t.Run("bare status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		if err := (&httpBackend{base: srv.URL, token: "t"}).remove("x"); err == nil {
			t.Error("non-2xx should error even without a body")
		}
	})
	t.Run("connection failure", func(t *testing.T) {
		// Nothing listening -> transport error path.
		if err := (&httpBackend{base: "http://127.0.0.1:1", token: "t"}).remove("x"); err == nil {
			t.Error("expected a transport error")
		}
	})
}

func TestLocalBackendRemoveInvalidID(t *testing.T) {
	be := &localBackend{mgr: &session.Manager{Prefix: "p"}}
	if err := be.remove("not-a-uuid"); err == nil {
		t.Error("remove with invalid id should error before touching screen")
	}
}

func TestHelpers(t *testing.T) {
	t.Run("resolveBin", func(t *testing.T) {
		if got := resolveBin("no-such-binary-xyz-123"); got != "no-such-binary-xyz-123" {
			t.Errorf("resolveBin(missing) = %q, want the name unchanged", got)
		}
		if got := resolveBin("sh"); got == "" {
			t.Error("resolveBin(sh) returned empty")
		}
	})
	t.Run("claudeHome", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		t.Setenv("CLAUDE_HOME", "/custom/home")
		if got := claudeHome(); got != "/custom/home" {
			t.Errorf("claudeHome = %q, want /custom/home", got)
		}
		t.Setenv("CLAUDE_HOME", "")
		t.Setenv("CLAUDE_CONFIG_DIR", "/custom/config")
		if got := claudeHome(); got != "/custom/config" {
			t.Errorf("claudeHome with CLAUDE_CONFIG_DIR = %q, want /custom/config", got)
		}
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		if h, err := os.UserHomeDir(); err == nil {
			if got, want := claudeHome(), filepath.Join(h, ".claude"); got != want {
				t.Errorf("claudeHome fallback = %q, want %q", got, want)
			}
		}
	})
	t.Run("envOr", func(t *testing.T) {
		t.Setenv("CRCTL_TEST_ENV", "")
		if got := envOr("CRCTL_TEST_ENV", "def"); got != "def" {
			t.Errorf("envOr empty = %q, want def", got)
		}
		t.Setenv("CRCTL_TEST_ENV", "set")
		if got := envOr("CRCTL_TEST_ENV", "def"); got != "set" {
			t.Errorf("envOr set = %q, want set", got)
		}
	})
	t.Run("usage", func(t *testing.T) {
		if out := captureStdout(t, usage); !contains(out, "crctl") {
			t.Error("usage output missing program name")
		}
	})
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestUpgradeCommand(t *testing.T) {
	t.Run("prints versions and per-session outcomes", func(t *testing.T) {
		be := &fakeBackend{upgradeRes: session.UpgradeResult{Before: "1.0.0", After: "1.0.1",
			Sessions: []session.RestartOutcome{
				{ID: "s1", Title: "work", Action: session.ActionRestarted},
				{ID: "s2", Action: session.ActionSkipped, Reason: "busy"},
			}}}
		out := captureStdout(t, func() {
			if err := upgrade(be, []string{"--all"}); err != nil {
				t.Errorf("upgrade: %v", err)
			}
		})
		for _, want := range []string{"1.0.0 -> 1.0.1", "restarted", "skipped", "busy", "(untitled)"} {
			if !contains(out, want) {
				t.Errorf("output %q missing %q", out, want)
			}
		}
		if !be.gotRestart || be.gotForce {
			t.Errorf("flags restart=%v force=%v, want true/false", be.gotRestart, be.gotForce)
		}
	})
	t.Run("flags", func(t *testing.T) {
		be := &fakeBackend{upgradeRes: session.UpgradeResult{Before: "1.0.1", After: "1.0.1"}}
		out := captureStdout(t, func() {
			if err := upgrade(be, []string{"--force"}); err != nil {
				t.Errorf("upgrade: %v", err)
			}
		})
		if be.gotRestart || !be.gotForce || !contains(out, "already up to date") || !contains(out, "crctl upgrade --all") {
			t.Errorf("restart=%v force=%v out=%q", be.gotRestart, be.gotForce, out)
		}
	})
	t.Run("session ids limit the restart", func(t *testing.T) {
		be := &fakeBackend{}
		_ = captureStdout(t, func() {
			if err := upgrade(be, []string{"s1", "--force", "s2"}); err != nil {
				t.Errorf("upgrade: %v", err)
			}
		})
		if len(be.gotIDs) != 2 || be.gotIDs[0] != "s1" || be.gotIDs[1] != "s2" || !be.gotForce {
			t.Errorf("ids=%v force=%v, want [s1 s2] with force", be.gotIDs, be.gotForce)
		}
		if !be.gotRestart {
			t.Error("ids should imply restart")
		}
		if err := upgrade(&fakeBackend{}, []string{"--all", "s1"}); err == nil {
			t.Error("--all with ids should error")
		}
	})
	t.Run("no sessions", func(t *testing.T) {
		out := captureStdout(t, func() { _ = upgrade(&fakeBackend{}, []string{"-a"}) })
		if !contains(out, "claude updated") || !contains(out, "No sessions running.") {
			t.Errorf("out = %q", out)
		}
	})
	t.Run("failed restart is an error", func(t *testing.T) {
		be := &fakeBackend{upgradeRes: session.UpgradeResult{
			Sessions: []session.RestartOutcome{{ID: "s1", Action: session.ActionFailed, Reason: "boom"}}}}
		_ = captureStdout(t, func() {
			if err := upgrade(be, []string{"--all"}); err == nil {
				t.Error("upgrade with a failed restart should error")
			}
		})
	})
	t.Run("update error and bad flag", func(t *testing.T) {
		if err := upgrade(&fakeBackend{upgradeErr: io.ErrUnexpectedEOF}, nil); err == nil {
			t.Error("upgrade should surface backend error")
		}
		if err := upgrade(&fakeBackend{}, []string{"--bogus"}); err == nil {
			t.Error("unknown flag should error")
		}
	})
}

func TestResumeAllCommand(t *testing.T) {
	t.Run("resumes only stopped sessions", func(t *testing.T) {
		be := &fakeBackend{sessions: []session.Session{
			{ID: "s1", Title: "live", Status: "Detached"},
			{ID: "s2", Title: "old", Status: "Stopped"},
			{ID: "s3", Status: "Stopped"},
		}}
		out := captureStdout(t, func() {
			if err := resumeAll(be); err != nil {
				t.Errorf("resumeAll: %v", err)
			}
		})
		if got := strings.Join(be.resumedIDs, ","); got != "s2,s3" {
			t.Errorf("resumed ids = %q, want s2,s3", got)
		}
		if !contains(out, "s2") || !contains(out, "resumed") || !contains(out, "(untitled)") || contains(out, "s1") {
			t.Errorf("output = %q", out)
		}
	})
	t.Run("nothing stopped", func(t *testing.T) {
		be := &fakeBackend{sessions: []session.Session{{ID: "s1", Status: "Detached"}}}
		out := captureStdout(t, func() {
			if err := resumeAll(be); err != nil {
				t.Errorf("resumeAll: %v", err)
			}
		})
		if len(be.resumedIDs) != 0 || !contains(out, "No stopped sessions") {
			t.Errorf("resumed %v, output = %q", be.resumedIDs, out)
		}
	})
	t.Run("a failure doesn't stop the rest", func(t *testing.T) {
		be := &fakeBackend{
			sessions: []session.Session{
				{ID: "s1", Status: "Stopped"},
				{ID: "s2", Status: "Stopped"},
			},
			resumeErrs: map[string]error{"s1": errors.New("boom")},
		}
		var err error
		out := captureStdout(t, func() { err = resumeAll(be) })
		if err == nil || !contains(err.Error(), "1 session(s) failed") {
			t.Errorf("err = %v, want 1 failed", err)
		}
		if len(be.resumedIDs) != 2 || !contains(out, "boom") {
			t.Errorf("resumed %v, output = %q", be.resumedIDs, out)
		}
	})
	t.Run("list error", func(t *testing.T) {
		if err := resumeAll(&fakeBackend{listErr: errors.New("boom")}); err == nil {
			t.Error("resumeAll should surface the list error")
		}
	})
}

func TestListShowsOutdated(t *testing.T) {
	be := &fakeBackend{sessions: []session.Session{
		{ID: "s1", Screen: "p-s1", Status: "Detached", Version: "2.1.278", Outdated: true},
		{ID: "s2", Screen: "p-s2", Status: "Detached", Version: "2.1.287"},
		{ID: "s3", Screen: "p-s3", Status: "Stopped"},
	}}
	out := captureStdout(t, func() {
		if err := list(be); err != nil {
			t.Errorf("list: %v", err)
		}
	})
	for _, want := range []string{"VERSION", "2.1.278 (outdated)", "2.1.287", "1 session(s) running an outdated claude", "crctl upgrade --all"} {
		if !contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
	if contains(out, "2.1.287 (outdated)") {
		t.Errorf("current session flagged outdated: %q", out)
	}
}

func TestResumeRestartDirFlag(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(data)
		_, _ = w.Write([]byte(`{"id":"s2","screen":"p-s2"}`))
	}))
	defer srv.Close()
	t.Setenv("CLAUDE_REMOTE_API_URL", srv.URL)
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")

	for _, c := range []struct {
		args     []string
		wantPath string
		wantBody string // substring; "" = empty body
	}{
		{[]string{"restart", "s2", "--dir", "/w/repo"}, "/sessions/s2/restart", `"dir":"/w/repo"`},
		{[]string{"restart", "--dir=/w/repo", "s2"}, "/sessions/s2/restart", `"dir":"/w/repo"`},
		{[]string{"resume", "s2", "-d", "/w/repo"}, "/sessions/s2/resume", `"dir":"/w/repo"`},
		{[]string{"restart", "s2"}, "/sessions/s2/restart", ""},
	} {
		gotPath, gotBody = "", ""
		_ = captureStdout(t, func() {
			if err := run(c.args); err != nil {
				t.Errorf("run %v: %v", c.args, err)
			}
		})
		if gotPath != c.wantPath || !contains(gotBody, c.wantBody) || (c.wantBody == "" && gotBody != "") {
			t.Errorf("run %v: path=%q body=%q, want %q with %q", c.args, gotPath, gotBody, c.wantPath, c.wantBody)
		}
	}

	for _, args := range [][]string{
		{"restart"},
		{"restart", "s2", "--dir"},
		{"resume", "all", "--dir", "/w"},
		{"resume", "s1", "s2"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run %v = nil error, want usage error", args)
		}
	}
}

func TestLocalDir(t *testing.T) {
	if got := localDir(""); got != "" {
		t.Errorf("localDir(\"\") = %q, want empty", got)
	}
	if got := localDir("/abs/x"); got != "/abs/x" {
		t.Errorf("localDir(abs) = %q", got)
	}
	wd, _ := os.Getwd()
	if got := localDir("sub"); got != filepath.Join(wd, "sub") {
		t.Errorf("localDir(rel) = %q, want under %q", got, wd)
	}
}

func TestListOrdersByLastActive(t *testing.T) {
	be := &fakeBackend{sessions: []session.Session{
		{ID: "old", Screen: "p-old", Status: "Detached", LastActive: "2026-01-01T10:00:00Z"},
		{ID: "never", Screen: "p-never", Status: "Detached"},
		{ID: "new", Screen: "p-new", Status: "Stopped", LastActive: "2026-01-02T09:00:00Z"},
	}}
	out := captureStdout(t, func() {
		if err := list(be); err != nil {
			t.Errorf("list: %v", err)
		}
	})
	iNew, iOld, iNever := strings.Index(out, "\nnew "), strings.Index(out, "\nold "), strings.Index(out, "\nnever ")
	if iNew < 0 || !(iNew < iOld && iOld < iNever) {
		t.Errorf("order new/old/never at %d/%d/%d, want ascending:\n%s", iNew, iOld, iNever, out)
	}
}

func TestTrustFlag(t *testing.T) {
	// Remote mode can't trust a folder on the server's behalf.
	t.Setenv("CLAUDE_REMOTE_API_URL", "http://127.0.0.1:1")
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	if err := run([]string{"restart", "s1", "--trust"}); err == nil || !contains(err.Error(), "local-only") {
		t.Errorf("run --trust (remote) = %v, want local-only error", err)
	}

	out := captureStdout(t, func() { trustHint(session.Session{Screen: "p-s1", NeedsTrust: true}) })
	if !contains(out, "screen -r p-s1") || !contains(out, "--trust") {
		t.Errorf("trustHint = %q", out)
	}
	if out := captureStdout(t, func() { trustHint(session.Session{Screen: "p-s1"}) }); out != "" {
		t.Errorf("trustHint(trusted) = %q, want nothing", out)
	}
}

func TestProfileFlag(t *testing.T) {
	for _, form := range [][]string{{"--profile", "work"}, {"--profile=work"}} {
		t.Run("new "+strings.Join(form, " "), func(t *testing.T) {
			be := &fakeBackend{created: session.Session{ID: "id1"}}
			_ = captureStdout(t, func() {
				if err := create(be, append([]string{"--dir", "/w"}, form...)); err != nil {
					t.Fatalf("create: %v", err)
				}
			})
			if be.createdProf != "work" || be.createdDir != "/w" {
				t.Errorf("create got dir=%q profile=%q, want /w and work", be.createdDir, be.createdProf)
			}
		})
		for _, c := range []struct {
			name string
			cmd  func(backend, []string) error
		}{{"resume", resume}, {"restart", restart}} {
			t.Run(c.name+" "+strings.Join(form, " "), func(t *testing.T) {
				be := &fakeBackend{}
				_ = captureStdout(t, func() {
					if err := c.cmd(be, append(slices.Clone(form), "s1")); err != nil {
						t.Fatalf("%s: %v", c.name, err)
					}
				})
				if be.movedProf != "work" {
					t.Errorf("%s got profile %q, want work", c.name, be.movedProf)
				}
			})
		}
	}
	t.Run("no flag means no profile", func(t *testing.T) {
		be := &fakeBackend{}
		_ = captureStdout(t, func() {
			if err := resume(be, []string{"s1"}); err != nil {
				t.Fatal(err)
			}
		})
		if be.movedProf != "" {
			t.Errorf("profile = %q, want empty", be.movedProf)
		}
	})
	for _, args := range [][]string{
		{"new", "--profile"},
		{"new", "--profile="},
		{"new", "--profile", ""},
		{"new", "--profile", "--dir", "/x"},
		{"resume", "s1", "--profile", "-d"},
		{"restart", "s1", "--profile", ""},
		{"new", "private"},
		{"new", "--dir", "/w", "extra"},
		{"resume", "s1", "--profile"},
		{"restart", "s1", "--profile="},
		{"resume", "all", "--profile", "work"},
		{"new", "--profile=private", "--profile"},
		{"new", "--profile", "private", "--profile"},
		{"restart", "s1", "--profile=private", "--profile"},
	} {
		be := &fakeBackend{}
		var err error
		switch args[0] {
		case "new":
			err = create(be, args[1:])
		case "resume":
			err = resume(be, args[1:])
		default:
			err = restart(be, args[1:])
		}
		if err == nil {
			t.Errorf("%v = nil error, want a usage error", args)
		}
		if be.createdProf != "" || be.movedProf != "" || len(be.resumedIDs) != 0 {
			t.Errorf("%v reached the backend", args)
		}
	}
}

func TestListProfileColumn(t *testing.T) {
	be := &fakeBackend{sessions: []session.Session{
		{ID: "w1", Title: "a", Status: "Detached", Screen: "p-w1", Profile: "work", LastActive: "2026-01-03T00:00:00Z"},
		{ID: "p1", Title: "b", Status: "Stopped", Screen: "p-p1", Profile: "private", LastActive: "2026-01-02T00:00:00Z"},
		{ID: "o1", Title: "c", Status: "Detached", Screen: "p-o1", LastActive: "2026-01-01T00:00:00Z"}, // server omitted it
	}}
	out := captureStdout(t, func() {
		if err := list(be); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	lines := strings.Split(out, "\n")
	header := strings.Fields(lines[0])
	if want := []string{"ID", "TITLE", "PROFILE", "STATUS"}; !slices.Equal(header[:4], want) {
		t.Fatalf("header starts %v, want %v:\n%s", header[:4], want, out)
	}
	for i, want := range [][]string{{"w1", "a", "work", "Detached"}, {"p1", "b", "private", "Stopped"}, {"o1", "c", "-", "Detached"}} {
		if got := strings.Fields(lines[i+1]); !slices.Equal(got[:4], want) {
			t.Errorf("row %d starts %v, want %v:\n%s", i, got[:4], want, out)
		}
	}
}

func TestHTTPBackendSendsProfile(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotBody = r.URL.Path, nil
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &gotBody); err != nil {
				t.Errorf("body %q: %v", data, err)
			}
		}
		_, _ = w.Write([]byte(`{"id":"s2","screen":"p-s2","profile":"work"}`))
	}))
	defer srv.Close()
	be := &httpBackend{base: srv.URL, token: "t"}
	for _, c := range []struct {
		name, wantPath string
		call           func() (session.Session, error)
		want           map[string]string
	}{
		{"create", "/sessions", func() (session.Session, error) { return be.create("", "work") }, map[string]string{"profile": "work"}},
		{"create with dir", "/sessions", func() (session.Session, error) { return be.create("/w", "work") }, map[string]string{"dir": "/w", "profile": "work"}},
		{"resume", "/sessions/s2/resume", func() (session.Session, error) { return be.resume("s2", "", "work") }, map[string]string{"profile": "work"}},
		{"restart", "/sessions/s2/restart", func() (session.Session, error) { return be.restart("s2", "/w", "work") }, map[string]string{"dir": "/w", "profile": "work"}},
		{"resume, no flags", "/sessions/s2/resume", func() (session.Session, error) { return be.resume("s2", "", "") }, nil},
	} {
		s, err := c.call()
		if err != nil || s.Profile != "work" {
			t.Errorf("%s = %+v, %v", c.name, s, err)
		}
		if gotPath != c.wantPath || !maps.Equal(gotBody, c.want) {
			t.Errorf("%s sent %s %v, want %s %v", c.name, gotPath, gotBody, c.wantPath, c.want)
		}
	}
}

// TestLocalBackendPassesProfile checks that the profile reaches the Manager.
// Each call names a profile the Manager refuses before it would launch
// anything; without the profile it would go on to the (missing) screen.
func TestLocalBackendPassesProfile(t *testing.T) {
	work, private := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work="+work+",private="+private)
	profiles, err := profile.FromEnv("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "11111111-2222-4333-8444-555555555555"
	proj := filepath.Join(work, "projects", "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A different copy in private makes a move refuse before it launches.
	if err := os.MkdirAll(filepath.Join(private, "projects", "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "projects", "p", id+".jsonl"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	be := &localBackend{mgr: &session.Manager{
		Prefix: "p", ScreenBin: missing, ClaudeBin: missing,
		WorkspaceRoot: "/", Profiles: profiles,
	}}
	for _, c := range []struct {
		name string
		call func() (session.Session, error)
		want error
	}{
		{"create unknown", func() (session.Session, error) { return be.create("", "nope") }, session.ErrInvalidProfile},
		{"resume unknown", func() (session.Session, error) { return be.resume(id, "", "nope") }, session.ErrInvalidProfile},
		{"resume other", func() (session.Session, error) { return be.resume(id, "", "private") }, session.ErrMoveConflict},
		{"restart other", func() (session.Session, error) { return be.restart(id, "", "private") }, session.ErrMoveConflict},
	} {
		if _, err := c.call(); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

const (
	unknownHint   = "CLAUDE_REMOTE_PROFILES"
	moveHint      = "leave out --profile"
	oldServerHint = "upgrade the API server to move it"
	conflictHint  = "move that copy aside"
)

// saysOnce reports whether a profile-move error says "not supported" once:
// the Manager's text says it, so the hint MUST NOT repeat it.
func saysOnce(err error) bool { return strings.Count(err.Error(), "not supported") == 1 }

func TestProfileErrors(t *testing.T) {
	unknown := fmt.Errorf("%w: %q", session.ErrInvalidProfile, "nope")
	other := fmt.Errorf("%w: s1 is in profile %q, not %q, and moving it is not supported",
		session.ErrOtherProfile, "work", "private")
	conflict := fmt.Errorf("%w: projects/p/s1.jsonl differs there; not overwriting it", session.ErrMoveConflict)
	for _, c := range []struct {
		name     string
		be       *fakeBackend
		cmd      func(backend, []string) error
		args     []string
		want     error
		hint     string
		notHints []string
	}{
		{"new unknown", &fakeBackend{createErr: unknown}, create, []string{"--profile", "nope"}, session.ErrInvalidProfile, unknownHint, []string{moveHint}},
		{"resume unknown", &fakeBackend{resumeErr: unknown}, resume, []string{"s1", "--profile", "nope"}, session.ErrInvalidProfile, unknownHint, []string{moveHint}},
		{"resume other", &fakeBackend{resumeErr: other}, resume, []string{"s1", "--profile", "private"}, session.ErrOtherProfile, moveHint, []string{unknownHint}},
		{"restart other", &fakeBackend{restartErr: other}, restart, []string{"s1", "--profile", "private"}, session.ErrOtherProfile, oldServerHint, []string{unknownHint}},
		{"restart conflict", &fakeBackend{restartErr: conflict}, restart, []string{"s1", "--profile", "private"}, session.ErrMoveConflict, conflictHint, []string{unknownHint, moveHint, oldServerHint}},
		{"other error unchanged", &fakeBackend{restartErr: errors.New("boom")}, restart, []string{"s1"}, nil, "boom", []string{unknownHint, moveHint, conflictHint}},
	} {
		var err error
		_ = captureStdout(t, func() { err = c.cmd(c.be, c.args) })
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) || !contains(err.Error(), c.hint) {
			t.Errorf("%s: err = %v, want %v with %q", c.name, err, c.want, c.hint)
			continue
		}
		if c.want == session.ErrOtherProfile && !saysOnce(err) {
			t.Errorf("%s: err = %v, want \"not supported\" said once", c.name, err)
		}
		for _, h := range c.notHints {
			if contains(err.Error(), h) {
				t.Errorf("%s: err = %v, should not say %q", c.name, err, h)
			}
		}
	}
}

func TestProfileErrorsHTTP(t *testing.T) {
	var status int
	var msg string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}))
	defer srv.Close()
	t.Setenv("CLAUDE_REMOTE_API_URL", srv.URL)
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	for _, c := range []struct {
		name     string
		args     []string
		status   int
		msg      string
		want     error
		hint     string
		notHints []string
	}{
		{"new unknown", []string{"new", "--profile", "nope"}, 400, `unknown profile: "nope"`,
			session.ErrInvalidProfile, `unknown profile: "nope" (400)`, []string{moveHint}},
		{"restart unknown", []string{"restart", "s1", "--profile", "nope"}, 400, `unknown profile: "nope"`,
			session.ErrInvalidProfile, unknownHint, []string{moveHint}},
		{"resume other", []string{"resume", "s1", "--profile", "private"}, 409,
			`session is in another profile: s1 is in profile "work", not "private", and moving it is not supported`,
			session.ErrOtherProfile, moveHint, []string{unknownHint}},
		{"restart other", []string{"restart", "s1", "--profile", "private"}, 409,
			`session is in another profile: s1 is in profile "work", not "private", and moving it is not supported`,
			session.ErrOtherProfile, oldServerHint, []string{unknownHint, conflictHint}},
		{"restart conflict", []string{"restart", "s1", "--profile", "private"}, 409,
			session.ErrMoveConflict.Error() + ": projects/p/s1.jsonl differs there; not overwriting it (moving to profile \"private\")",
			session.ErrMoveConflict, conflictHint, []string{unknownHint, oldServerHint}},
		{"conflict text on a 400 is no conflict", []string{"restart", "s1", "--profile", "private"}, 400,
			session.ErrMoveConflict.Error(), nil, "(400)", []string{unknownHint, oldServerHint, conflictHint}},
		{"already running is no move", []string{"resume", "s1"}, 409, session.ErrAlreadyRunning.Error(),
			nil, session.ErrAlreadyRunning.Error() + " (409)", []string{unknownHint, moveHint}},
		{"bad dir is no profile", []string{"new", "--dir", "/x"}, 400, session.ErrInvalidDir.Error() + ": /x",
			nil, session.ErrInvalidDir.Error(), []string{unknownHint, moveHint}},
		{"other-profile text on a 400 is no move", []string{"restart", "s1", "--profile", "private"}, 400,
			session.ErrOtherProfile.Error() + ": s1", nil, "(400)", []string{unknownHint, moveHint}},
		{"unknown-profile text on a 409 is no unknown profile", []string{"resume", "s1", "--profile", "nope"}, 409,
			session.ErrInvalidProfile.Error() + `: "nope"`, nil, "(409)", []string{unknownHint, moveHint}},
		{"unknown session", []string{"restart", "s1", "--profile", "work"}, 404, "session not resumable: no session s1",
			nil, "no session s1 (404)", []string{unknownHint, moveHint}},
	} {
		status, msg = c.status, c.msg
		var err error
		_ = captureStdout(t, func() { err = run(c.args) })
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) || !contains(err.Error(), c.hint) {
			t.Errorf("%s: err = %v, want %v with %q", c.name, err, c.want, c.hint)
			continue
		}
		if c.want == nil && (errors.Is(err, session.ErrInvalidProfile) || errors.Is(err, session.ErrOtherProfile) ||
			errors.Is(err, session.ErrMoveConflict)) {
			t.Errorf("%s: err = %v reads as a profile error", c.name, err)
		}
		if c.want == session.ErrOtherProfile && !saysOnce(err) {
			t.Errorf("%s: err = %v, want \"not supported\" said once", c.name, err)
		}
		for _, h := range c.notHints {
			if contains(err.Error(), h) {
				t.Errorf("%s: err = %v, should not say %q", c.name, err, h)
			}
		}
	}
}

// An API server that predates profiles ignores "profile" and launches the
// session under its only account. crctl MUST NOT report that as success.
func TestHTTPBackendDroppedProfile(t *testing.T) {
	var reply string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	be := &httpBackend{base: srv.URL, token: "t"}
	calls := map[string]func(prof string) (session.Session, error){
		"create":  func(prof string) (session.Session, error) { return be.create("", prof) },
		"resume":  func(prof string) (session.Session, error) { return be.resume("s2", "", prof) },
		"restart": func(prof string) (session.Session, error) { return be.restart("s2", "", prof) },
	}
	for name, call := range calls {
		// A new session in the wrong account can go; an existing one resumed
		// or restarted is still the session it was, so stopping it fixes
		// nothing.
		advice, notAdvice := "crctl rm s2", "leave out --profile"
		if name != "create" {
			advice, notAdvice = notAdvice, advice
		}
		reply = `{"id":"s2","screen":"p-s2"}`
		_, err := call("private")
		if err == nil || !contains(err.Error(), "does not support profiles") || !contains(err.Error(), advice) || contains(err.Error(), notAdvice) {
			t.Errorf("%s with the profile omitted: err = %v, want a refusal advising %q, not %q", name, err, advice, notAdvice)
		}
		reply = `{"id":"s2","screen":"p-s2","profile":"default"}`
		_, err = call("private")
		if err == nil || !contains(err.Error(), `"default"`) || !contains(err.Error(), advice) || contains(err.Error(), notAdvice) {
			t.Errorf("%s under another profile: err = %v, want a refusal naming default and advising %q, not %q", name, err, advice, notAdvice)
		}
		reply = `{"id":"s2","screen":"p-s2"}`
		if _, err := call(""); err != nil {
			t.Errorf("%s without --profile on an old server: err = %v, want none", name, err)
		}
	}

	t.Setenv("CLAUDE_REMOTE_API_URL", srv.URL)
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	reply = `{"id":"s2","screen":"p-s2"}`
	var err error
	out := captureStdout(t, func() { err = run([]string{"new", "--profile", "private"}) })
	if err == nil || contains(out, "started") || !contains(err.Error(), "started") {
		t.Errorf("run new --profile on an old server: err = %v, out = %q; want the error to say it started", err, out)
	}
}
