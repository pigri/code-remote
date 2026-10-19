package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-remote-api/internal/profile"
	"claude-remote-api/internal/session"
)

// --- run() lifecycle ---

func TestRunMissingToken(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "")
	if code := run(context.Background()); code != 1 {
		t.Errorf("run without token = %d, want 1", code)
	}
}

func TestRunMissingBinaries(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	t.Run("screen missing", func(t *testing.T) {
		t.Setenv("SCREEN_BIN", "/no/such/screen-binary-xyz")
		if code := run(context.Background()); code != 1 {
			t.Errorf("run with missing screen = %d, want 1", code)
		}
	})
	t.Run("claude missing", func(t *testing.T) {
		t.Setenv("SCREEN_BIN", os.Args[0]) // exists + executable (the test binary)
		t.Setenv("CLAUDE_BIN", "/no/such/claude-binary-xyz")
		if code := run(context.Background()); code != 1 {
			t.Errorf("run with missing claude = %d, want 1", code)
		}
	})
}

func TestRunBadProfiles(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	t.Setenv("SCREEN_BIN", os.Args[0])
	t.Setenv("CLAUDE_BIN", os.Args[0])
	t.Setenv("CLAUDE_REMOTE_API_ADDR", "127.0.0.1:0")
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "off")
	t.Setenv("CLAUDE_REMOTE_DB", filepath.Join(t.TempDir(), "test.db"))
	for _, tc := range []struct{ name, profiles, def string }{
		{"bad spec", "work", ""},
		{"unknown default", "work=/w", "private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_REMOTE_PROFILES", tc.profiles)
			t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", tc.def)
			// Pre-cancelled: a run that ignored the bad config would serve and exit 0.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if code := run(ctx); code != 1 {
				t.Errorf("run with CLAUDE_REMOTE_PROFILES=%q = %d, want 1", tc.profiles, code)
			}
		})
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	t.Setenv("SCREEN_BIN", os.Args[0]) // any existing executable resolves LookPath
	t.Setenv("CLAUDE_BIN", os.Args[0])
	t.Setenv("CLAUDE_REMOTE_API_ADDR", "127.0.0.1:0") // ephemeral port
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "off")     // no reconciler
	db := filepath.Join(t.TempDir(), "test.db")
	t.Setenv("CLAUDE_REMOTE_DB", db)

	// Pre-cancelled context: run binds the listener then immediately shuts down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := run(ctx); code != 0 {
		t.Errorf("run graceful shutdown = %d, want 0", code)
	}
	if _, err := os.Stat(db); err != nil {
		t.Errorf("run did not open the session DB at CLAUDE_REMOTE_DB: %v", err)
	}
}

// --- handler error/success branches via an injectable manager ---

type stubMgr struct {
	createSess  session.Session
	createErr   error
	listSess    []session.Session
	listErr     error
	getSess     session.Session
	getOK       bool
	getErr      error
	killExisted bool
	killErr     error
	resumeSess  session.Session
	resumeErr   error
	restartSess session.Session
	restartErr  error
	gotDir      string // dir passed to ResumeIn / RestartIn
	gotProfile  string // profile passed to Create / ResumeIn / RestartIn
	upgradeRes  session.UpgradeResult
	upgradeErr  error
	gotRestart  bool // restart flag passed to Upgrade
	gotForce    bool // force flag passed to Upgrade
	gotIDs      []string
}

func (s *stubMgr) ValidID(string) bool { return true } // let requests reach the manager
func (s *stubMgr) Create(_, prof string) (session.Session, error) {
	s.gotProfile = prof
	return s.createSess, s.createErr
}
func (s *stubMgr) ResumeIn(_, dir, prof string) (session.Session, error) {
	s.gotDir, s.gotProfile = dir, prof
	return s.resumeSess, s.resumeErr
}
func (s *stubMgr) RestartIn(_, dir, prof string) (session.Session, error) {
	s.gotDir, s.gotProfile = dir, prof
	return s.restartSess, s.restartErr
}
func (s *stubMgr) Upgrade(restart, force bool, ids ...string) (session.UpgradeResult, error) {
	s.gotRestart, s.gotForce, s.gotIDs = restart, force, ids
	return s.upgradeRes, s.upgradeErr
}
func (s *stubMgr) List() ([]session.Session, error) { return s.listSess, s.listErr }
func (s *stubMgr) ListAll(session.StoppedLister) ([]session.Session, error) {
	return s.listSess, s.listErr
}
func (s *stubMgr) Get(string) (session.Session, bool, error) {
	return s.getSess, s.getOK, s.getErr
}
func (s *stubMgr) Kill(string) (bool, error) { return s.killExisted, s.killErr }

func stubHandler(m sessionManager) http.Handler { return newHandler(testToken, m, nil, nil) }

func send(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

const stubID = "6fd0b321-a454-4b40-9aed-131afe120d36"

func TestHandlerSuccessPaths(t *testing.T) {
	t.Run("create 201", func(t *testing.T) {
		m := &stubMgr{createSess: session.Session{ID: stubID, Screen: "p-" + stubID}}
		rr := send(t, stubHandler(m), http.MethodPost, "/sessions", "")
		if rr.Code != http.StatusCreated {
			t.Errorf("create = %d, want 201 (%s)", rr.Code, rr.Body)
		}
	})
	t.Run("get 200", func(t *testing.T) {
		m := &stubMgr{getOK: true, getSess: session.Session{ID: stubID}}
		rr := send(t, stubHandler(m), http.MethodGet, "/sessions/"+stubID, "")
		if rr.Code != http.StatusOK {
			t.Errorf("get = %d, want 200", rr.Code)
		}
	})
	t.Run("delete 200", func(t *testing.T) {
		m := &stubMgr{killExisted: true}
		rr := send(t, stubHandler(m), http.MethodDelete, "/sessions/"+stubID, "")
		if rr.Code != http.StatusOK {
			t.Errorf("delete = %d, want 200", rr.Code)
		}
	})
	t.Run("resume 201", func(t *testing.T) {
		m := &stubMgr{resumeSess: session.Session{ID: stubID, Screen: "p-" + stubID}}
		rr := send(t, stubHandler(m), http.MethodPost, "/sessions/"+stubID+"/resume", "")
		if rr.Code != http.StatusCreated {
			t.Errorf("resume = %d, want 201 (%s)", rr.Code, rr.Body)
		}
	})
}

func TestResumeHandlerErrors(t *testing.T) {
	cases := []struct {
		name string
		mgr  *stubMgr
		path string
		want int
	}{
		{"already running -> 409", &stubMgr{resumeErr: session.ErrAlreadyRunning}, "/sessions/" + stubID + "/resume", http.StatusConflict},
		{"not resumable -> 404", &stubMgr{resumeErr: session.ErrNotResumable}, "/sessions/" + stubID + "/resume", http.StatusNotFound},
		{"backend error -> 500", &stubMgr{resumeErr: errors.New("boom")}, "/sessions/" + stubID + "/resume", http.StatusInternalServerError},
		{"bad dir -> 400", &stubMgr{resumeErr: session.ErrInvalidDir}, "/sessions/" + stubID + "/resume", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := send(t, stubHandler(c.mgr), http.MethodPost, c.path, "")
			if rr.Code != c.want {
				t.Errorf("resume = %d, want %d (%s)", rr.Code, c.want, rr.Body)
			}
		})
	}
}

func TestResumeHandlerInvalidID(t *testing.T) {
	m := &badIDMgr{stubMgr: stubMgr{}}
	rr := send(t, stubHandler(m), http.MethodPost, "/sessions/not-a-uuid/resume", "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("resume(bad id) = %d, want 400 (%s)", rr.Code, rr.Body)
	}
}

// badIDMgr rejects every id so the handler's ValidID guard can be exercised.
type badIDMgr struct{ stubMgr }

func (b *badIDMgr) ValidID(string) bool { return false }

func TestHandlerInternalErrors(t *testing.T) {
	boom := errors.New("backend exploded")
	cases := []struct {
		name, method, path string
		mgr                *stubMgr
	}{
		{"create 500", http.MethodPost, "/sessions", &stubMgr{createErr: boom}},
		{"list 500", http.MethodGet, "/sessions", &stubMgr{listErr: boom}},
		{"get 500", http.MethodGet, "/sessions/" + stubID, &stubMgr{getErr: boom}},
		{"delete 500", http.MethodDelete, "/sessions/" + stubID, &stubMgr{killErr: boom}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := send(t, stubHandler(c.mgr), c.method, c.path, "")
			if rr.Code != http.StatusInternalServerError {
				t.Errorf("%s = %d, want 500 (%s)", c.name, rr.Code, rr.Body)
			}
		})
	}
}

func TestRestartHandler(t *testing.T) {
	path := "/sessions/" + stubID + "/restart"
	cases := []struct {
		name string
		mgr  sessionManager
		path string
		want int
	}{
		{"restart 200", &stubMgr{restartSess: session.Session{ID: stubID}}, path, http.StatusOK},
		{"not resumable -> 404", &stubMgr{restartErr: session.ErrNotResumable}, path, http.StatusNotFound},
		{"backend error -> 500", &stubMgr{restartErr: errors.New("boom")}, path, http.StatusInternalServerError},
		{"bad id -> 400", &badIDMgr{}, "/sessions/not-a-uuid/restart", http.StatusBadRequest},
		{"bad dir -> 400", &stubMgr{restartErr: session.ErrInvalidDir}, path, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := send(t, stubHandler(c.mgr), http.MethodPost, c.path, "")
			if rr.Code != c.want {
				t.Errorf("restart = %d, want %d (%s)", rr.Code, c.want, rr.Body)
			}
		})
	}
}

func TestUpgradeHandler(t *testing.T) {
	t.Run("defaults to restart without force", func(t *testing.T) {
		m := &stubMgr{upgradeRes: session.UpgradeResult{Before: "1.0.0", After: "1.0.1",
			Sessions: []session.RestartOutcome{{ID: stubID, Action: session.ActionRestarted}}}}
		rr := send(t, stubHandler(m), http.MethodPost, "/upgrade", "")
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"after":"1.0.1"`) {
			t.Fatalf("upgrade = %d %s, want 200 with versions", rr.Code, rr.Body)
		}
		if !m.gotRestart || m.gotForce {
			t.Errorf("Upgrade(restart=%v, force=%v), want true/false", m.gotRestart, m.gotForce)
		}
	})
	t.Run("body opts out of restart and sets force", func(t *testing.T) {
		m := &stubMgr{}
		rr := send(t, stubHandler(m), http.MethodPost, "/upgrade", `{"restart":false,"force":true}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("upgrade = %d, want 200 (%s)", rr.Code, rr.Body)
		}
		if m.gotRestart || !m.gotForce {
			t.Errorf("Upgrade(restart=%v, force=%v), want false/true", m.gotRestart, m.gotForce)
		}
	})
	t.Run("body limits restarts to named sessions", func(t *testing.T) {
		m := &stubMgr{}
		rr := send(t, stubHandler(m), http.MethodPost, "/upgrade", `{"sessions":["`+stubID+`"]}`)
		if rr.Code != http.StatusOK || len(m.gotIDs) != 1 || m.gotIDs[0] != stubID {
			t.Fatalf("upgrade = %d ids=%v, want 200 with the one id", rr.Code, m.gotIDs)
		}
	})
	t.Run("invalid session id -> 400", func(t *testing.T) {
		m := &stubMgr{upgradeErr: session.ErrInvalidID}
		rr := send(t, stubHandler(m), http.MethodPost, "/upgrade", `{"sessions":["nope"]}`)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("upgrade(bad id) = %d, want 400", rr.Code)
		}
	})
	t.Run("invalid JSON -> 400", func(t *testing.T) {
		rr := send(t, stubHandler(&stubMgr{}), http.MethodPost, "/upgrade", "{nope")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("upgrade(bad json) = %d, want 400", rr.Code)
		}
	})
	t.Run("update failure -> 500 with output", func(t *testing.T) {
		m := &stubMgr{upgradeErr: errors.New("claude update: exit status 1"),
			upgradeRes: session.UpgradeResult{Output: "network unreachable"}}
		rr := send(t, stubHandler(m), http.MethodPost, "/upgrade", "")
		if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "network unreachable") {
			t.Errorf("upgrade(fail) = %d %s, want 500 carrying updater output", rr.Code, rr.Body)
		}
	})
}

func TestResumeRestartPassDir(t *testing.T) {
	for _, verb := range []string{"resume", "restart"} {
		m := &stubMgr{}
		rr := send(t, stubHandler(m), http.MethodPost, "/sessions/"+stubID+"/"+verb, `{"dir":"work/repo"}`)
		if rr.Code >= 300 || m.gotDir != "work/repo" {
			t.Errorf("%s = %d, dir %q; want 2xx with dir work/repo (%s)", verb, rr.Code, m.gotDir, rr.Body)
		}
		rr = send(t, stubHandler(&stubMgr{}), http.MethodPost, "/sessions/"+stubID+"/"+verb, `{not json`)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s(bad body) = %d, want 400", verb, rr.Code)
		}
	}
}

// run must not close the store under a conversation sync still in flight.
func TestRunWaitsForConversationSync(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	t.Setenv("SCREEN_BIN", os.Args[0])
	t.Setenv("CLAUDE_BIN", os.Args[0])
	t.Setenv("CLAUDE_REMOTE_API_ADDR", "127.0.0.1:0")
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "off")
	t.Setenv("CLAUDE_REMOTE_DB", filepath.Join(t.TempDir(), "test.db"))

	entered, release := make(chan struct{}), make(chan struct{})
	storeErr := make(chan error, 1)
	orig := syncConversations
	t.Cleanup(func() { syncConversations = orig })
	var once sync.Once
	syncConversations = func(m *session.Manager) {
		once.Do(func() {
			close(entered)
			<-release
			// The pass's own write, made after shutdown has begun.
			w := m.Store.(interface{ SetConversation(id, conv string) error })
			storeErr <- w.SetConversation("00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002")
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no conversation sync pass started")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("run returned while a conversation sync was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("run = %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after the sync pass finished")
	}
	if err := <-storeErr; err != nil {
		t.Errorf("the in-flight pass could not write to the store: %v", err)
	}
}

// A server that cannot listen makes run exit 1, with the store open and the
// background workers running.
func TestRunExitsWhenServerFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	t.Setenv("SCREEN_BIN", os.Args[0])
	t.Setenv("CLAUDE_BIN", os.Args[0])
	t.Setenv("CLAUDE_REMOTE_API_ADDR", ln.Addr().String()) // already taken
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "off")
	t.Setenv("CLAUDE_REMOTE_DB", filepath.Join(t.TempDir(), "test.db"))

	done := make(chan int, 1)
	go func() { done <- run(context.Background()) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("run with the port taken = %d, want 1", code)
		}
	case <-time.After(3 * time.Second): // under run's 5s shutdown deadline
		t.Fatal("run did not return promptly after the server failed")
	}
}

func TestHandlersPassProfile(t *testing.T) {
	for _, path := range []string{"/sessions", "/sessions/" + stubID + "/resume", "/sessions/" + stubID + "/restart"} {
		m := &stubMgr{}
		rr := send(t, stubHandler(m), http.MethodPost, path, `{"dir":"repo","profile":"private"}`)
		if rr.Code >= 300 || m.gotProfile != "private" {
			t.Errorf("POST %s = %d, profile %q; want 2xx with profile private (%s)", path, rr.Code, m.gotProfile, rr.Body)
		}
	}
}

func TestHandlersProfileErrors(t *testing.T) {
	invalid := fmt.Errorf("%w: %q", session.ErrInvalidProfile, "nope")
	other := fmt.Errorf("%w: moving it is not supported", session.ErrOtherProfile)
	for _, tc := range []struct {
		path string
		mgr  *stubMgr
		want int
	}{
		{"/sessions", &stubMgr{createErr: invalid}, http.StatusBadRequest},
		{"/sessions/" + stubID + "/resume", &stubMgr{resumeErr: invalid}, http.StatusBadRequest},
		{"/sessions/" + stubID + "/restart", &stubMgr{restartErr: invalid}, http.StatusBadRequest},
		{"/sessions/" + stubID + "/resume", &stubMgr{resumeErr: other}, http.StatusConflict},
		{"/sessions/" + stubID + "/restart", &stubMgr{restartErr: other}, http.StatusConflict},
	} {
		rr := send(t, stubHandler(tc.mgr), http.MethodPost, tc.path, `{"profile":"x"}`)
		if rr.Code != tc.want || !strings.Contains(rr.Body.String(), "profile") {
			t.Errorf("POST %s with %v = %d %s, want %d naming the profile", tc.path, tc.mgr, rr.Code, rr.Body, tc.want)
		}
	}
}

// crctl tells the profile errors apart by the start of the error text, so
// the handlers MUST write them unwrapped.
func TestHandlersProfileErrorText(t *testing.T) {
	invalid := fmt.Errorf("%w: %q", session.ErrInvalidProfile, "nope")
	other := fmt.Errorf("%w: moving it is not supported", session.ErrOtherProfile)
	for _, tc := range []struct {
		path string
		mgr  *stubMgr
		want error
	}{
		{"/sessions", &stubMgr{createErr: invalid}, session.ErrInvalidProfile},
		{"/sessions/" + stubID + "/resume", &stubMgr{resumeErr: invalid}, session.ErrInvalidProfile},
		{"/sessions/" + stubID + "/restart", &stubMgr{restartErr: invalid}, session.ErrInvalidProfile},
		{"/sessions/" + stubID + "/resume", &stubMgr{resumeErr: other}, session.ErrOtherProfile},
		{"/sessions/" + stubID + "/restart", &stubMgr{restartErr: other}, session.ErrOtherProfile},
	} {
		rr := send(t, stubHandler(tc.mgr), http.MethodPost, tc.path, `{"profile":"x"}`)
		var body struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("POST %s: body %s: %v", tc.path, rr.Body, err)
		}
		if !strings.HasPrefix(body.Error, tc.want.Error()) {
			t.Errorf("POST %s: error %q, want it to start with %q", tc.path, body.Error, tc.want.Error())
		}
	}
}

// Resuming or restarting a session that does not exist is 404, whichever
// profile the request names, and launches nothing.
func TestHandlersUnknownSessionWithProfile(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,private=~/.claude-private")
	set, err := profile.FromEnv("", user)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	launched := filepath.Join(dir, "launched")
	screen := filepath.Join(dir, "screen")
	script := "#!/bin/sh\nif [ \"$1\" = -dmS ]; then echo \"$2\" >> '" + launched + "'; fi\nexit 0\n"
	if err := os.WriteFile(screen, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	mgr := &session.Manager{Prefix: "crtest", ScreenBin: screen, ClaudeBin: "claude",
		ClaudeHome: filepath.Join(user, ".claude"), Profiles: set}
	for _, action := range []string{"resume", "restart"} {
		for _, prof := range []string{"work", "private", "nope"} {
			path := "/sessions/" + stubID + "/" + action
			rr := send(t, stubHandler(mgr), http.MethodPost, path, `{"profile":"`+prof+`"}`)
			if rr.Code != http.StatusNotFound {
				t.Errorf("POST %s with profile %s = %d %s, want 404", path, prof, rr.Code, rr.Body)
			}
		}
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Errorf("a launch was attempted: stat err = %v", err)
	}
}
