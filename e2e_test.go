//go:build e2e

// End-to-end test: drives the real HTTP handler against real `screen` with a
// stub `claude`. Run with: go test -tags e2e ./...
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-remote-api/internal/profile"
	"claude-remote-api/internal/session"
	"claude-remote-api/internal/store"
)

const e2eToken = "e2e-token"

// stub claude: derives the session id from its args, writes a custom-title
// record where the API expects it, then sleeps so the screen session stays up.
const stubClaude = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (stub)"; exit 0; }
id="$2"   # invoked as: claude --session-id <id> --remote-control <id>
mkdir -p "$CLAUDE_HOME/projects/e2e"
printf '{"type":"custom-title","customTitle":"e2e title","sessionId":"%s"}\n' "$id" > "$CLAUDE_HOME/projects/e2e/$id.jsonl"
exec sleep 600
`

func TestE2ELifecycle(t *testing.T) {
	screenBin, err := exec.LookPath("screen")
	if err != nil {
		t.Skip("screen not installed; skipping e2e")
	}

	home := t.TempDir()
	t.Setenv("CLAUDE_HOME", home) // inherited by screen -> stub
	stub := filepath.Join(home, "claude")
	if err := os.WriteFile(stub, []byte(stubClaude), 0o755); err != nil {
		t.Fatal(err)
	}

	prefix := fmt.Sprintf("crapi-e2e-%d", os.Getpid())
	mgr := &session.Manager{Prefix: prefix, ClaudeBin: stub, ScreenBin: screenBin, ClaudeHome: home}
	t.Cleanup(func() {
		if ss, _ := mgr.List(); ss != nil {
			for _, s := range ss {
				_, _ = mgr.Kill(s.ID)
			}
		}
	})

	ts := httptest.NewServer(newHandler(e2eToken, mgr, nil, nil))
	defer ts.Close()

	// create
	var created session.Session
	if code := req(t, ts, http.MethodPost, "/sessions", &created); code != http.StatusCreated {
		t.Fatalf("POST /sessions = %d", code)
	}
	if !mgr.ValidID(created.ID) {
		t.Fatalf("created id is not a uuid: %q", created.ID)
	}
	if created.Screen != prefix+"-"+created.ID {
		t.Fatalf("screen name = %q, want %q", created.Screen, prefix+"-"+created.ID)
	}

	// list contains it
	var listed struct {
		Sessions []session.Session `json:"sessions"`
	}
	if code := req(t, ts, http.MethodGet, "/sessions", &listed); code != http.StatusOK {
		t.Fatalf("GET /sessions = %d", code)
	}
	if !containsID(listed.Sessions, created.ID) {
		t.Fatalf("listing missing %s: %+v", created.ID, listed.Sessions)
	}

	// title (written by the stub) propagates through to GET
	var got session.Session
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req(t, ts, http.MethodGet, "/sessions/"+created.ID, &got)
		if got.Title == "e2e title" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.Title != "e2e title" {
		t.Errorf("title = %q, want %q", got.Title, "e2e title")
	}
	if got.Status != "Detached" {
		t.Errorf("status = %q, want Detached", got.Status)
	}

	// delete, then confirm it's gone
	if code := req(t, ts, http.MethodDelete, "/sessions/"+created.ID, nil); code != http.StatusOK {
		t.Fatalf("DELETE = %d", code)
	}
	code := http.StatusOK
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if code = req(t, ts, http.MethodGet, "/sessions/"+created.ID, nil); code == http.StatusNotFound {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code != http.StatusNotFound {
		t.Errorf("after delete, GET = %d, want 404", code)
	}
}

func TestE2EAuthRequired(t *testing.T) {
	mgr := &session.Manager{Prefix: "crapi-e2e-auth"}
	ts := httptest.NewServer(newHandler(e2eToken, mgr, nil, nil))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions") // no Authorization header
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth GET /sessions = %d, want 401", resp.StatusCode)
	}
}

// stubProfileClaude writes its log where a real claude would, given its
// environment: under ${CLAUDE_CONFIG_DIR:-$HOME/.claude}. The title names that
// dir, and each launch appends to the conversation's log.
const stubProfileClaude = `#!/bin/sh
[ "$1" = "--version" ] && { echo "0.0.0 (stub)"; exit 0; }
id="$2"   # claude --session-id <id> ... or claude --resume <id> ...
dir="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
mkdir -p "$dir/projects/e2e"
printf '{"type":"custom-title","customTitle":"in %s","sessionId":"%s"}\n' "$(basename "$dir")" "$id" >> "$dir/projects/e2e/$id.jsonl"
exec sleep 600
`

func TestE2EProfilesLifecycle(t *testing.T) {
	screenBin, err := exec.LookPath("screen")
	if err != nil {
		t.Skip("screen not installed; skipping e2e")
	}
	home := t.TempDir()
	t.Setenv("HOME", home) // inherited by screen -> stub
	work, private := filepath.Join(home, ".claude"), filepath.Join(home, ".claude-private")
	for _, d := range []string{work, private} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The server's own CLAUDE_CONFIG_DIR must reach no child.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "parent"))
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,private=~/.claude-private")
	t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "")
	set, err := profile.FromEnv("", home)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(home, "claude")
	if err := os.WriteFile(stub, []byte(stubProfileClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(home, "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	prefix := fmt.Sprintf("crapi-e2e-prof-%d", os.Getpid())
	mgr := &session.Manager{Prefix: prefix, ClaudeBin: stub, ScreenBin: screenBin, Profiles: set, Store: db}
	t.Cleanup(func() {
		if ss, _ := mgr.List(); ss != nil {
			for _, s := range ss {
				_, _ = mgr.Kill(s.ID)
			}
		}
	})
	ts := httptest.NewServer(newHandler(e2eToken, mgr, nil, db))
	defer ts.Close()

	// waitLog waits for the stub to have written id's log under dir.
	waitLog := func(dir, id string) {
		t.Helper()
		log := filepath.Join(dir, "projects", "e2e", id+".jsonl")
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if _, err := os.Stat(log); err == nil {
				return
			}
		}
		t.Fatalf("no log at %s", log)
	}
	listed := func() map[string]session.Session {
		t.Helper()
		var l struct {
			Sessions []session.Session `json:"sessions"`
		}
		if code := reqBody(t, ts, http.MethodGet, "/sessions", "", &l); code != http.StatusOK {
			t.Fatalf("GET /sessions = %d", code)
		}
		out := map[string]session.Session{}
		for _, s := range l.Sessions {
			out[s.ID] = s
		}
		return out
	}

	var w, p session.Session
	if code := reqBody(t, ts, http.MethodPost, "/sessions", "", &w); code != http.StatusCreated || w.Profile != "work" {
		t.Fatalf("POST /sessions = %d %+v, want 201 in work, the default", code, w)
	}
	if code := reqBody(t, ts, http.MethodPost, "/sessions", `{"profile":"private"}`, &p); code != http.StatusCreated || p.Profile != "private" {
		t.Fatalf("POST /sessions private = %d %+v, want 201 in private", code, p)
	}
	waitLog(work, w.ID)
	waitLog(private, p.ID)
	if _, err := os.Stat(filepath.Join(home, "parent")); !os.IsNotExist(err) {
		t.Errorf("a child wrote under the server's CLAUDE_CONFIG_DIR: %v", err)
	}

	got := listed()
	if s := got[w.ID]; s.Profile != "work" || s.Title != "in .claude" {
		t.Errorf("work session = %+v, want profile work, title from ~/.claude", s)
	}
	if s := got[p.ID]; s.Profile != "private" || s.Title != "in .claude-private" {
		t.Errorf("private session = %+v, want profile private, title from ~/.claude-private", s)
	}
	for id, want := range map[string]string{w.ID: "work", p.ID: "private"} {
		if rec, err := db.Profile(id); err != nil || rec != want {
			t.Errorf("stored profile of %s = %q, %v, want %q", id, rec, err, want)
		}
	}

	if code := reqBody(t, ts, http.MethodPost, "/sessions", `{"profile":"nope"}`, nil); code != http.StatusBadRequest {
		t.Errorf("POST /sessions unknown profile = %d, want 400", code)
	}

	// Stop the private session: it lists as stopped, still in private.
	if code := reqBody(t, ts, http.MethodDelete, "/sessions/"+p.ID, "", nil); code != http.StatusOK {
		t.Fatalf("DELETE = %d", code)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got[p.ID] = listed()[p.ID]; got[p.ID].Status == "Stopped" {
			break
		}
	}
	if s := got[p.ID]; s.Status != "Stopped" || s.Profile != "private" || s.Title != "in .claude-private" {
		t.Errorf("stopped session = %+v, want Stopped in private with its title", s)
	}

	if code := reqBody(t, ts, http.MethodPost, "/sessions/"+p.ID+"/resume", `{"profile":"work"}`, nil); code != http.StatusConflict {
		t.Errorf("resume into another profile = %d, want 409", code)
	}
	var r session.Session
	if code := reqBody(t, ts, http.MethodPost, "/sessions/"+p.ID+"/resume", "", &r); code != http.StatusCreated || r.Profile != "private" {
		t.Fatalf("resume = %d %+v, want 201 in private", code, r)
	}
	log := filepath.Join(private, "projects", "e2e", p.ID+".jsonl")
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if data, _ := os.ReadFile(log); strings.Count(string(data), "\n") == 2 {
			break
		}
	}
	if data, _ := os.ReadFile(log); strings.Count(string(data), "\n") != 2 {
		t.Errorf("private log after resume:\n%s\nwant a second launch appended in private", data)
	}
	if _, err := os.Stat(filepath.Join(work, "projects", "e2e", p.ID+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("resume wrote the private session under work: %v", err)
	}
}

func req(t *testing.T, ts *httptest.Server, method, path string, out any) int {
	t.Helper()
	return reqBody(t, ts, method, path, "", out)
}

// reqBody is req with a JSON request body ("" for none).
func reqBody(t *testing.T, ts *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	var rb io.Reader
	if body != "" {
		rb = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, ts.URL+path, rb)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+e2eToken)
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func containsID(ss []session.Session, id string) bool {
	for _, s := range ss {
		if s.ID == id {
			return true
		}
	}
	return false
}
