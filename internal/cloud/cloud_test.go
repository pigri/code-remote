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
	cases := []struct {
		status string
		want   bool
	}{
		{"archived", true},
		{"Archived", true},
		{"idle", false},
		{"running", false},
		{"requires_action", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			if got := (Session{SessionStatus: c.status}).IsArchivedSession(); got != c.want {
				t.Errorf("IsArchivedSession(%q) = %v, want %v", c.status, got, c.want)
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
			{"id":"session_a","title":"one","session_status":"idle","connection_status":"connected","session_context":{"cwd":"/x"}},
			{"id":"session_b","title":"two","session_status":"archived","connection_status":"connected","session_context":{"cwd":"/y"}}
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
	if sessions[1].Cwd() != "/y" {
		t.Errorf("Cwd() = %q, want /y", sessions[1].Cwd())
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
	regs     []session.Registration
	killed   []string
}

func (f *fakeManager) List() ([]session.Session, error)               { return f.sessions, nil }
func (f *fakeManager) Registrations() ([]session.Registration, error) { return f.regs, nil }
func (f *fakeManager) Kill(id string) (bool, error) {
	f.killed = append(f.killed, id)
	return true, nil
}

// Title+Cwd join: archived cloud sessions map to our screens via the registry's
// cwd, and only archived ones get quit.
func TestReconcileQuitsArchivedByTitleCwd(t *testing.T) {
	cloudCl := &fakeCloud{sessions: []Session{
		archived("test-2", "/repo"),
		idle("test-keep", "/repo"),
	}}
	mgr := &fakeManager{
		sessions: []session.Session{
			{ID: "uuid-arch", Screen: "p-uuid-arch", Title: "test-2"},
			{ID: "uuid-keep", Screen: "p-uuid-keep", Title: "test-keep"},
		},
		regs: []session.Registration{
			{SessionID: "uuid-arch", Cwd: "/repo"},
			{SessionID: "uuid-keep", Cwd: "/repo"},
		},
	}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger(), MatchTitle: true}
	r.ReconcileOnce(context.Background())

	if len(mgr.killed) != 1 || mgr.killed[0] != "uuid-arch" {
		t.Fatalf("killed = %v, want [uuid-arch]", mgr.killed)
	}
}

// Title matching is off by default: a title-only archived match (no bridge id)
// must NOT be quit, since titles are mutable.
func TestReconcileTitleIgnoredByDefault(t *testing.T) {
	cloudCl := &fakeCloud{sessions: []Session{archived("test-2", "/repo")}}
	mgr := &fakeManager{
		sessions: []session.Session{{ID: "uuid-arch", Title: "test-2"}},
		regs:     []session.Registration{{SessionID: "uuid-arch", Cwd: "/repo"}}, // no bridge id
	}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger()} // MatchTitle defaults false
	r.ReconcileOnce(context.Background())
	if len(mgr.killed) != 0 {
		t.Fatalf("killed = %v, want none (title match off by default)", mgr.killed)
	}
}

// Precise join: a non-null bridgeSessionId matching an archived server id.
func TestReconcileQuitsArchivedByBridgeID(t *testing.T) {
	cloudCl := &fakeCloud{sessions: []Session{
		{ID: "session_X", SessionStatus: "archived"}, // no title
	}}
	mgr := &fakeManager{
		sessions: []session.Session{{ID: "uuid-1", Screen: "p-uuid-1"}}, // no title either
		regs:     []session.Registration{{SessionID: "uuid-1", BridgeSessionID: "session_X"}},
	}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger()}
	r.ReconcileOnce(context.Background())
	if len(mgr.killed) != 1 || mgr.killed[0] != "uuid-1" {
		t.Fatalf("killed = %v, want [uuid-1]", mgr.killed)
	}
}

// Safety: if any cloud session sharing Title+Cwd is still active, don't quit.
func TestReconcileSkipsWhenActiveCounterpartExists(t *testing.T) {
	cloudCl := &fakeCloud{sessions: []Session{
		archived("dup", "/repo"),
		idle("dup", "/repo"), // same title+cwd, still active
	}}
	mgr := &fakeManager{
		sessions: []session.Session{{ID: "uuid-dup", Title: "dup"}},
		regs:     []session.Registration{{SessionID: "uuid-dup", Cwd: "/repo"}},
	}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger(), MatchTitle: true}
	r.ReconcileOnce(context.Background())
	if len(mgr.killed) != 0 {
		t.Fatalf("killed = %v, want none (active counterpart exists)", mgr.killed)
	}
}

// Different cwd must not match even with the same title.
func TestReconcileTitleRequiresMatchingCwd(t *testing.T) {
	cloudCl := &fakeCloud{sessions: []Session{archived("same", "/other")}}
	mgr := &fakeManager{
		sessions: []session.Session{{ID: "uuid-1", Title: "same"}},
		regs:     []session.Registration{{SessionID: "uuid-1", Cwd: "/repo"}},
	}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger(), MatchTitle: true}
	r.ReconcileOnce(context.Background())
	if len(mgr.killed) != 0 {
		t.Fatalf("killed = %v, want none (cwd mismatch)", mgr.killed)
	}
}

func TestReconcileNoArchivedIsNoop(t *testing.T) {
	cloudCl := &fakeCloud{sessions: []Session{idle("live", "/repo")}}
	mgr := &fakeManager{
		sessions: []session.Session{{ID: "uuid-1", Title: "live"}},
		regs:     []session.Registration{{SessionID: "uuid-1", Cwd: "/repo"}},
	}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger()}
	r.ReconcileOnce(context.Background())
	if len(mgr.killed) != 0 {
		t.Fatalf("killed = %v, want none", mgr.killed)
	}
}

func TestReconcileCloudErrorDoesNotKill(t *testing.T) {
	cloudCl := &fakeCloud{err: context.DeadlineExceeded}
	mgr := &fakeManager{sessions: []session.Session{{ID: "x", Title: "t"}}}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger()}
	r.ReconcileOnce(context.Background())
	if len(mgr.killed) != 0 {
		t.Fatalf("killed on cloud error: %v", mgr.killed)
	}
}

func archived(title, cwd string) Session { return mkSession(title, cwd, "archived") }
func idle(title, cwd string) Session     { return mkSession(title, cwd, "idle") }
func mkSession(title, cwd, status string) Session {
	s := Session{Title: title, SessionStatus: status}
	s.SessionContext.Cwd = cwd
	return s
}

// helpers

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
