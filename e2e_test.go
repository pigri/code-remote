//go:build e2e

// End-to-end test: drives the real HTTP handler against real `screen` with a
// stub `claude`. Run with: go test -tags e2e ./...
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"claude-remote-api/internal/session"
)

const e2eToken = "e2e-token"

// stub claude: derives the session id from its args, writes a custom-title
// record where the API expects it, then sleeps so the screen session stays up.
const stubClaude = `#!/bin/sh
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

	ts := httptest.NewServer(newHandler(e2eToken, mgr, nil))
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
	ts := httptest.NewServer(newHandler(e2eToken, mgr, nil))
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

func req(t *testing.T, ts *httptest.Server, method, path string, out any) int {
	t.Helper()
	r, err := http.NewRequest(method, ts.URL+path, nil)
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
