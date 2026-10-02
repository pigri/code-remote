package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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

func TestRunServesAndShutsDown(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	t.Setenv("SCREEN_BIN", os.Args[0]) // any existing executable resolves LookPath
	t.Setenv("CLAUDE_BIN", os.Args[0])
	t.Setenv("CLAUDE_REMOTE_API_ADDR", "127.0.0.1:0") // ephemeral port
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "off")     // no reconciler/store

	// Pre-cancelled context: run binds the listener then immediately shuts down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := run(ctx); code != 0 {
		t.Errorf("run graceful shutdown = %d, want 0", code)
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
	upgradeRes  session.UpgradeResult
	upgradeErr  error
	gotRestart  bool // restart flag passed to Upgrade
	gotForce    bool // force flag passed to Upgrade
	gotIDs      []string
}

func (s *stubMgr) ValidID(string) bool { return true } // let requests reach the manager
func (s *stubMgr) Create(string) (session.Session, error) {
	return s.createSess, s.createErr
}
func (s *stubMgr) Resume(string) (session.Session, error) {
	return s.resumeSess, s.resumeErr
}
func (s *stubMgr) Restart(string) (session.Session, error) {
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
