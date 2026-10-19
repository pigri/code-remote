package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-remote-api/internal/cloud"
	"claude-remote-api/internal/session"
)

// accounts is a fake Sessions API serving one cloud per OAuth token. It
// records every request each account receives.
type accounts struct {
	mu     sync.Mutex
	clouds map[string][]cloud.Session // token -> the account's sessions
	seen   map[string][]string        // token -> "METHOD path" of each request
}

func newAccounts(t *testing.T, clouds map[string][]cloud.Session) *accounts {
	t.Helper()
	a := &accounts{clouds: clouds, seen: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	t.Setenv("CLAUDE_REMOTE_CLOUD_BASE", srv.URL)
	return a
}

func (a *accounts) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	a.mu.Lock()
	a.seen[token] = append(a.seen[token], r.Method+" "+r.URL.Path)
	sessions, ok := a.clouds[token]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "read-only fake", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/v1/sessions" {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": sessions})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	for _, s := range sessions {
		if s.ID == id {
			_ = json.NewEncoder(w).Encode(s)
			return
		}
	}
	http.NotFound(w, r)
}

func (a *accounts) requests(token string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.seen[token])
}

// credentials writes an OAuth token for the profile dir dir (relative to HOME).
func (h *profileHost) credentials(t *testing.T, dir, token string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h.user, dir), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"claudeAiOauth":{"accessToken":"` + token + `"}}`
	if err := os.WriteFile(filepath.Join(h.user, dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// quits is the screens h's fake screen has quit, one per line.
func (h *profileHost) quits() string {
	got, _ := os.ReadFile(h.killed)
	return string(got)
}

// recordProfile stores each session's profile, as a launch would.
func (h *profileHost) recordProfile(t *testing.T, profiles map[string]string) {
	t.Helper()
	for id, p := range profiles {
		if err := h.db.Record(id, "crtest-"+id, "", "", "Detached", "", p); err != nil {
			t.Fatal(err)
		}
	}
}

func syncLogger(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(&lockedWriter{w: buf}, nil))
}

type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func archived(id string) cloud.Session { return cloud.Session{ID: id, SessionStatus: "archived"} }
func running(id string) cloud.Session  { return cloud.Session{ID: id, SessionStatus: "running"} }

// profileOf is the profile a reconciler's view holds.
func profileOf(t *testing.T, rec *cloud.Reconciler) string {
	t.Helper()
	v, ok := rec.Manager.(*session.ProfileView)
	if !ok {
		t.Fatalf("reconciler manager = %T, want a profile view", rec.Manager)
	}
	return v.Profile()
}

// Each profile's sessions sync with that profile's account only: one account's
// cloud is never asked about, and never causes a quit of, another's sessions.
func TestSessionSyncPerProfile(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "",
		map[string]int{sessX: 4242, sessW: 4343})
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "on")
	t.Setenv("CLAUDE_REMOTE_ARCHIVE_GRACE", "0s")
	fakePS(t, "  7777  4242 claude\\n  8888  4343 claude\\n")
	h.register(t, ".claude-private", sessX, sessX, 7777, "session_private")
	h.register(t, ".claude", sessW, sessW, 8888, "session_work")
	h.recordProfile(t, map[string]string{sessX: "private", sessW: "work"})
	h.credentials(t, ".claude", "tok-work")
	h.credentials(t, ".claude-private", "tok-private")
	// The work account archived its session; the private one's still runs.
	clouds := newAccounts(t, map[string][]cloud.Session{
		"tok-work":    {archived("session_work")},
		"tok-private": {running("session_private")},
	})

	recs := sessionReconcilers(discardLogger(), h.mgr, h.db)
	var names []string
	for _, rec := range recs {
		names = append(names, profileOf(t, rec))
		rec.ReconcileOnce(context.Background())
	}
	if !slices.Equal(names, []string{"work", "private"}) {
		t.Fatalf("reconcilers for %v, want work and private", names)
	}
	if got, want := h.quits(), "crtest-"+sessW+"\n"; got != want {
		t.Errorf("quit %q, want only %q", got, want)
	}
	for token, other := range map[string]string{"tok-work": "session_private", "tok-private": "session_work"} {
		reqs := clouds.requests(token)
		if !slices.Contains(reqs, "GET /v1/sessions") {
			t.Errorf("%s requests = %q, want its account listed", token, reqs)
		}
		for _, r := range reqs {
			if strings.Contains(r, other) || !strings.HasPrefix(r, "GET ") {
				t.Errorf("%s request %q: it reads only, and only its own sessions", token, r)
			}
		}
	}
	if reqs := clouds.requests(""); len(reqs) != 0 {
		t.Errorf("unauthenticated requests %q", reqs)
	}
}

// A profile without credentials is logged once and skipped; the others still
// sync, and their log lines name their profile.
func TestSessionSyncSkipsAProfileWithoutCredentials(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "private",
		map[string]int{sessX: 4242, sessW: 4343})
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "on")
	t.Setenv("CLAUDE_REMOTE_ARCHIVE_GRACE", "0s")
	fakePS(t, "  7777  4242 claude\\n  8888  4343 claude\\n")
	h.register(t, ".claude-private", sessX, sessX, 7777, "session_private")
	h.register(t, ".claude", sessW, sessW, 8888, "session_work")
	h.recordProfile(t, map[string]string{sessX: "private", sessW: "work"})
	h.credentials(t, ".claude", "tok-work") // the default, private, has none
	newAccounts(t, map[string][]cloud.Session{"tok-work": {archived("session_work")}})

	var buf strings.Builder
	recs := sessionReconcilers(syncLogger(&buf), h.mgr, h.db)
	if len(recs) != 1 || profileOf(t, recs[0]) != "work" {
		t.Fatalf("reconcilers = %d, want work's only", len(recs))
	}
	recs[0].ReconcileOnce(context.Background())
	if got, want := h.quits(), "crtest-"+sessW+"\n"; got != want {
		t.Errorf("quit %q, want %q", got, want)
	}

	var warnings, enabled []string
	for _, line := range strings.Split(buf.String(), "\n") {
		switch {
		case strings.Contains(line, "msg=session_sync_disabled"):
			warnings = append(warnings, line)
		case strings.Contains(line, "msg=session_sync_enabled"):
			enabled = append(enabled, line)
		}
	}
	privCreds := filepath.Join(h.user, ".claude-private", ".credentials.json")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "profile=private") || !strings.Contains(warnings[0], privCreds) {
		t.Errorf("warnings = %q, want one naming private and %s", warnings, privCreds)
	}
	if len(enabled) != 1 || !strings.Contains(enabled[0], "profile=work") {
		t.Errorf("enabled = %q, want one naming work", enabled)
	}
}

// With one profile, the sync is as before: one reconciler on the whole
// manager, with CLAUDE_REMOTE_CREDENTIALS taking over the credentials, and no
// profile in its log lines.
func TestSessionSyncSingleProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "on")
	creds := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(creds, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := &session.Manager{Prefix: "crtest", ClaudeHome: home}

	t.Run("its home has no credentials", func(t *testing.T) {
		if recs := sessionReconcilers(discardLogger(), mgr, nil); len(recs) != 0 {
			t.Errorf("reconcilers = %d, want none", len(recs))
		}
	})
	t.Run("CLAUDE_REMOTE_CREDENTIALS", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_CREDENTIALS", creds)
		var buf strings.Builder
		recs := sessionReconcilers(syncLogger(&buf), mgr, nil)
		if len(recs) != 1 || recs[0].Manager != mgr {
			t.Fatalf("reconcilers = %+v, want one on the manager", recs)
		}
		if c := recs[0].Cloud.(*cloud.Client); c.CredentialsPath != creds {
			t.Errorf("credentials = %q, want %q", c.CredentialsPath, creds)
		}
		if strings.Contains(buf.String(), "profile=") {
			t.Errorf("log %q names a profile", buf.String())
		}
	})
	t.Run("its home's credentials", func(t *testing.T) {
		own := filepath.Join(home, ".credentials.json")
		if err := os.WriteFile(own, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		recs := sessionReconcilers(discardLogger(), mgr, nil)
		if len(recs) != 1 || recs[0].Cloud.(*cloud.Client).CredentialsPath != own {
			t.Errorf("reconcilers = %+v, want one with %s", recs, own)
		}
	})
}

// Shutdown waits for every profile's reconciler, so the store outlives them.
func TestSessionSyncWaitsForEveryReconciler(t *testing.T) {
	for _, slow := range []string{"work", "private"} {
		t.Run(slow, func(t *testing.T) {
			h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", nil)
			t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "on")
			h.credentials(t, ".claude", "tok-work")
			h.credentials(t, ".claude-private", "tok-private")

			release := make(chan struct{})
			var mu sync.Mutex
			var ran []string
			orig := runReconciler
			t.Cleanup(func() { runReconciler = orig })
			runReconciler = func(rec *cloud.Reconciler, ctx context.Context) {
				p := profileOf(t, rec)
				mu.Lock()
				ran = append(ran, p)
				mu.Unlock()
				<-ctx.Done()
				if p == slow {
					<-release
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			sh := startSessionSync(ctx, discardLogger(), h.mgr, h.db)
			cancel()
			short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancelShort()
			if sh.wait(short) {
				t.Fatalf("wait returned while %s's reconciler still ran", slow)
			}
			close(release)
			if err := sh.Close(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			slices.Sort(ran)
			if !slices.Equal(ran, []string{"private", "work"}) {
				t.Errorf("ran %v, want both profiles", ran)
			}
		})
	}
}

// One reconcile reads the registries and the process table once.
func TestReconcileReadsTheProcessTableOnce(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	dir := t.TempDir()
	count := filepath.Join(dir, "ps-calls")
	script := "#!/bin/sh\necho x >> '" + count + "'\nprintf '  7777  4242 claude\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.register(t, ".claude-private", sessX, sessX, 7777, "")
	h.recordProfile(t, map[string]string{sessX: "private"})

	v, err := h.mgr.ForProfile("private")
	if err != nil {
		t.Fatal(err)
	}
	rec := &cloud.Reconciler{Cloud: notFoundCloud{}, Manager: v, Store: h.db, Log: discardLogger()}
	rec.ReconcileOnce(context.Background())
	got, _ := os.ReadFile(count)
	if n := strings.Count(string(got), "x"); n != 1 {
		t.Errorf("ps ran %d times in one reconcile, want once", n)
	}
}
