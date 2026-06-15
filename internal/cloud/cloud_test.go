package cloud

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-remote-api/internal/session"
)

func writeCreds(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, ".credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"` + token + `","refreshToken":"r","expiresAt":1}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIsArchivedSession(t *testing.T) {
	tr, fa := true, false
	at := "2026-06-15T00:00:00Z"
	empty := ""
	cases := []struct {
		name string
		s    Session
		want bool
	}{
		{"archived true", Session{Archived: &tr}, true},
		{"archived false", Session{Archived: &fa}, false},
		{"is_archived true", Session{IsArchived: &tr}, true},
		{"archived_at set", Session{ArchivedAt: &at}, true},
		{"archived_at empty", Session{ArchivedAt: &empty}, false},
		{"none set", Session{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.IsArchivedSession(); got != c.want {
				t.Errorf("IsArchivedSession() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestListSendsAuthAndParses(t *testing.T) {
	const token = "tok-abc-123"
	var gotAuth, gotBeta, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotVer = r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"a","name":"one","connection_status":"connected","archived":false},
			{"id":"b","name":"two","connection_status":"disconnected","archived":true}
		]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, CredentialsPath: writeCreds(t, token)}
	sessions, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
	if gotBeta != betaHeader || gotVer != apiVersion {
		t.Errorf("beta=%q version=%q, want %q/%q", gotBeta, gotVer, betaHeader, apiVersion)
	}
	if len(sessions) != 2 || !sessions[1].IsArchivedSession() || sessions[0].IsArchivedSession() {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
}

func TestListErrorStatusDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, CredentialsPath: writeCreds(t, "secret-token")}
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error leaked the token: %v", err)
	}
}

func TestArchivePostsToCorrectPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, CredentialsPath: writeCreds(t, "t")}
	if err := c.Archive(context.Background(), "sess-1"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/sessions/sess-1/archive" {
		t.Errorf("got %s %s, want POST /v1/sessions/sess-1/archive", gotMethod, gotPath)
	}
}

// --- reconciler ---

type fakeCloud struct {
	sessions []Session
	err      error
}

func (f *fakeCloud) List(context.Context) ([]Session, error) { return f.sessions, f.err }

type fakeManager struct {
	sessions []session.Session
	killed   []string
}

func (f *fakeManager) List() ([]session.Session, error) { return f.sessions, nil }
func (f *fakeManager) Kill(id string) (bool, error) {
	f.killed = append(f.killed, id)
	return true, nil
}

func TestReconcileQuitsOnlyArchivedOwnedScreens(t *testing.T) {
	tr := true
	cloudCl := &fakeCloud{sessions: []Session{
		{ID: "arch-1", Archived: &tr},
		{ID: "live-1"},
		{ID: "arch-not-local", Archived: &tr}, // archived but no local screen
	}}
	mgr := &fakeManager{sessions: []session.Session{
		{ID: "arch-1", Screen: "p-arch-1"},
		{ID: "live-1", Screen: "p-live-1"},
	}}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger()}
	r.reconcileOnce(context.Background())

	if len(mgr.killed) != 1 || mgr.killed[0] != "arch-1" {
		t.Fatalf("killed = %v, want [arch-1]", mgr.killed)
	}
}

func TestReconcileNoArchivedIsNoop(t *testing.T) {
	cloudCl := &fakeCloud{sessions: []Session{{ID: "live-1"}}}
	mgr := &fakeManager{sessions: []session.Session{{ID: "live-1"}}}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger()}
	r.reconcileOnce(context.Background())
	if len(mgr.killed) != 0 {
		t.Fatalf("killed = %v, want none", mgr.killed)
	}
}

func TestReconcileCloudErrorDoesNotKill(t *testing.T) {
	cloudCl := &fakeCloud{err: context.DeadlineExceeded}
	mgr := &fakeManager{sessions: []session.Session{{ID: "x"}}}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger()}
	r.reconcileOnce(context.Background())
	if len(mgr.killed) != 0 {
		t.Fatalf("killed on cloud error: %v", mgr.killed)
	}
}

// helpers

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
