package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	sessions   []session.Session
	created    session.Session
	createdDir string
	resumed    session.Session
	resumedID  string
	removed    string
	listErr    error
	createErr  error
	resumeErr  error
	removeErr  error

	restartedID string
	restartErr  error
	upgradeRes  session.UpgradeResult
	upgradeErr  error
	gotRestart  bool
	gotForce    bool
	gotIDs      []string
}

func (f *fakeBackend) list() ([]session.Session, error) { return f.sessions, f.listErr }
func (f *fakeBackend) create(dir string) (session.Session, error) {
	f.createdDir = dir
	return f.created, f.createErr
}
func (f *fakeBackend) resume(id string) (session.Session, error) {
	f.resumedID = id
	return f.resumed, f.resumeErr
}
func (f *fakeBackend) restart(id string) (session.Session, error) {
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
		be, err := pickBackend()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := be.(*localBackend); !ok {
			t.Errorf("got %T, want *localBackend", be)
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
		s, err := be.create("/work")
		if err != nil || s.ID != "s2" {
			t.Fatalf("create = %+v, %v", s, err)
		}
		if gotMethod != http.MethodPost || !contains(gotBody, `"dir":"/work"`) {
			t.Errorf("method=%q body=%q", gotMethod, gotBody)
		}
	})
	t.Run("restart", func(t *testing.T) {
		s, err := be.restart("s3")
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
		t.Setenv("CLAUDE_HOME", "/custom/home")
		if got := claudeHome(); got != "/custom/home" {
			t.Errorf("claudeHome = %q, want /custom/home", got)
		}
		t.Setenv("CLAUDE_HOME", "")
		if got := claudeHome(); !contains(got, ".claude") {
			t.Errorf("claudeHome fallback = %q, want a path ending in .claude", got)
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
