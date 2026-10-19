package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"claude-remote-api/internal/cloud"
	"claude-remote-api/internal/profile"
	"claude-remote-api/internal/session"
	"claude-remote-api/internal/store"
)

func TestEnvOr(t *testing.T) {
	t.Run("unset returns default", func(t *testing.T) {
		os.Unsetenv("CR_TEST_ENVOR")
		if got := envOr("CR_TEST_ENVOR", "def"); got != "def" {
			t.Errorf("envOr unset = %q, want def", got)
		}
	})
	t.Run("set returns value", func(t *testing.T) {
		t.Setenv("CR_TEST_ENVOR", "val")
		if got := envOr("CR_TEST_ENVOR", "def"); got != "val" {
			t.Errorf("envOr set = %q, want val", got)
		}
	})
	t.Run("empty falls back to default", func(t *testing.T) {
		t.Setenv("CR_TEST_ENVOR", "")
		if got := envOr("CR_TEST_ENVOR", "def"); got != "def" {
			t.Errorf("envOr empty = %q, want def", got)
		}
	})
}

func TestEnvBool(t *testing.T) {
	cases := []struct {
		val  string
		def  bool
		want bool
	}{
		{"1", false, true}, {"true", false, true}, {"on", false, true}, {"yes", false, true},
		{"TRUE", false, true}, {" on ", false, true}, // case-insensitive + trimmed
		{"0", true, false}, {"false", true, false}, {"off", true, false}, {"no", true, false},
		{"", true, true}, {"", false, false}, // unset/empty -> default
		{"maybe", true, true}, {"maybe", false, false}, // unrecognized -> default
	}
	for _, c := range cases {
		t.Run(c.val+"_def_"+map[bool]string{true: "t", false: "f"}[c.def], func(t *testing.T) {
			t.Setenv("CR_TEST_ENVBOOL", c.val)
			if got := envBool("CR_TEST_ENVBOOL", c.def); got != c.want {
				t.Errorf("envBool(%q, %v) = %v, want %v", c.val, c.def, got, c.want)
			}
		})
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns whatever
// was written. auditLogger binds os.Stdout at call time, so fn must both build
// the logger and log through it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func TestAuditLoggerFormat(t *testing.T) {
	t.Run("default is text", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_LOG_FORMAT", "text")
		out := captureStdout(t, func() { auditLogger().Info("hi", "k", "v") })
		if strings.HasPrefix(strings.TrimSpace(out), "{") {
			t.Errorf("text format looks like JSON: %q", out)
		}
		if !strings.Contains(out, "k=v") {
			t.Errorf("text format missing key=value: %q", out)
		}
	})
	t.Run("json when configured", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_LOG_FORMAT", "JSON") // case-insensitive
		out := captureStdout(t, func() { auditLogger().Info("hi", "k", "v") })
		if !strings.HasPrefix(strings.TrimSpace(out), "{") || !strings.Contains(out, `"k":"v"`) {
			t.Errorf("json format not emitted: %q", out)
		}
	})
}

func TestDefaultDBPath(t *testing.T) {
	t.Run("honors XDG_DATA_HOME", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/tmp/xdg-data")
		want := filepath.Join("/tmp/xdg-data", "code-remote", "code-remote.db")
		if got := defaultDBPath(); got != want {
			t.Errorf("defaultDBPath = %q, want %q", got, want)
		}
	})
	t.Run("falls back to ~/.local/share", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		want := filepath.Join(".local", "share", "code-remote", "code-remote.db")
		if got := defaultDBPath(); !strings.HasSuffix(got, want) {
			t.Errorf("defaultDBPath = %q, want suffix %q", got, want)
		}
	})
}

func TestClientIP(t *testing.T) {
	t.Run("strips port", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.7:44321"
		if got := clientIP(r); got != "203.0.113.7" {
			t.Errorf("clientIP = %q, want 203.0.113.7", got)
		}
	})
	t.Run("returns whole addr when no port", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "unix-socket"
		if got := clientIP(r); got != "unix-socket" {
			t.Errorf("clientIP = %q, want unix-socket", got)
		}
	})
}

// authedPost issues an authenticated POST with the given raw body.
func authedPost(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestCreateInvalidJSON(t *testing.T) {
	rr := authedPost(t, testHandler(), "{not valid json")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("POST bad JSON = %d, want 400 (body: %s)", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "invalid JSON body") {
		t.Errorf("missing error message: %s", rr.Body)
	}
}

func TestCreateInvalidDir(t *testing.T) {
	// testHandler's manager has no WorkspaceRoot, so any requested dir is
	// rejected with ErrInvalidDir -> 400 before screen is ever invoked.
	rr := authedPost(t, testHandler(), `{"dir":"whatever"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("POST with dir = %d, want 400 (body: %s)", rr.Code, rr.Body)
	}
}

func TestStartSessionSyncDisabled(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "off")
	h := startSessionSync(context.Background(), discardLogger(), &session.Manager{}, nil)
	if h != nil {
		t.Fatalf("startSessionSync disabled = %v, want nil", h)
	}
	// Close on a nil handle must be a no-op, not a panic.
	if err := h.Close(); err != nil {
		t.Errorf("nil handle Close = %v, want nil", err)
	}
}

func TestStartSessionSyncNoCredentials(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "on")
	t.Setenv("CLAUDE_REMOTE_CREDENTIALS", filepath.Join(t.TempDir(), "does-not-exist.json"))
	if h := startSessionSync(context.Background(), discardLogger(), &session.Manager{}, nil); h != nil {
		t.Fatalf("startSessionSync without creds = %v, want nil", h)
	}
}

func TestStartSessionSyncEnabled(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "on")
	t.Setenv("CLAUDE_REMOTE_CREDENTIALS", creds)
	t.Setenv("CLAUDE_REMOTE_SYNC_INTERVAL", "50ms")
	t.Setenv("CLAUDE_REMOTE_ARCHIVE_GRACE", "1m")

	// The store is owned by run() now; here we pass one in and confirm the
	// reconciler accepts it.
	db, err := store.Open(filepath.Join(dir, "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A pre-cancelled context: the reconciler runs one ctx-bound reconcile that
	// fails fast (no network) and returns immediately, so this stays hermetic.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := startSessionSync(ctx, discardLogger(), &session.Manager{Prefix: "test-sync"}, db)
	if h == nil {
		t.Fatal("startSessionSync enabled = nil, want handle")
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

func TestStartSessionSyncBadDurations(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(creds, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "on")
	t.Setenv("CLAUDE_REMOTE_CREDENTIALS", creds)
	t.Setenv("CLAUDE_REMOTE_SYNC_INTERVAL", "garbage") // -> warn + default
	t.Setenv("CLAUDE_REMOTE_ARCHIVE_GRACE", "garbage") // -> warn + default

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := startSessionSync(ctx, discardLogger(), &session.Manager{Prefix: "test-sync"}, nil)
	if h == nil {
		t.Fatal("startSessionSync = nil, want handle")
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A worker that already finished is reported as finished even when the
// deadline has passed too.
func TestSyncHandleWaitPrefersDone(t *testing.T) {
	done := make(chan struct{})
	close(done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &syncHandle{done: done}
	for i := 0; i < 200; i++ {
		if !h.wait(ctx) {
			t.Fatalf("wait = false on try %d for a finished worker", i)
		}
	}
	if !(*syncHandle)(nil).wait(ctx) {
		t.Error("nil handle wait = false, want true")
	}
}

func TestSyncTarget(t *testing.T) {
	t.Run("single profile: the manager and its home", func(t *testing.T) {
		mgr := &session.Manager{ClaudeHome: "/h/.claude"}
		home, view, err := syncTarget(mgr)
		if err != nil || home != "/h/.claude" || view != reconcileManager(mgr) {
			t.Errorf("syncTarget = %q, %T; want the claude home and the manager itself", home, view)
		}
	})
	t.Run("several profiles: the default's dir and sessions", func(t *testing.T) {
		user := t.TempDir()
		t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,private=~/.claude-private")
		t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "private")
		set, err := profile.FromEnv("", user)
		if err != nil {
			t.Fatal(err)
		}
		home, view, err := syncTarget(&session.Manager{ClaudeHome: filepath.Join(user, ".claude"), Profiles: set})
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(user, ".claude-private"); home != want {
			t.Errorf("home = %q, want the default profile's %q", home, want)
		}
		if v, ok := view.(*session.ProfileView); !ok || v.Profile() != "private" {
			t.Errorf("view = %#v, want the view of private", view)
		}
	})
}

// notFoundCloud is an account whose cloud knows no session.
type notFoundCloud struct{}

func (notFoundCloud) List(context.Context) ([]cloud.Session, error) { return nil, nil }
func (notFoundCloud) Get(context.Context, string) (cloud.Session, bool, error) {
	return cloud.Session{}, false, nil
}

// profileHost is a temp HOME with configured profiles, a store, and a script
// screen running some sessions, which logs each session it quits.
type profileHost struct {
	user, killed string
	db           *store.DB
	mgr          *session.Manager
}

// newProfileHost configures spec as CLAUDE_REMOTE_PROFILES with def as the
// default, and runs a screen per entry of screens (session id -> screen pid).
func newProfileHost(t *testing.T, spec, def string, screens map[string]int) *profileHost {
	t.Helper()
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CLAUDE_REMOTE_PROFILES", spec)
	t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", def)
	set, err := profile.FromEnv("", user)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir := t.TempDir()
	h := &profileHost{user: user, killed: filepath.Join(dir, "killed"), db: db}
	ls := "There are screens on:\\n"
	for id, pid := range screens {
		ls += "\\t" + strconv.Itoa(pid) + ".crtest-" + id + "\\t(Detached)\\n"
	}
	script := "#!/bin/sh\ncase \"$1\" in\n-ls) printf '" + ls + "' ;;\n" +
		"-S) echo \"$2\" >> '" + h.killed + "' ;;\nesac\nexit 0\n"
	screen := filepath.Join(dir, "screen")
	if err := os.WriteFile(screen, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	h.mgr = &session.Manager{Prefix: "crtest", ScreenBin: screen, ClaudeHome: filepath.Join(user, ".claude"),
		Profiles: set, Store: db}
	return h
}

// register writes a registry entry for session id, as claude process pid,
// under the profile dir dir (relative to HOME). An empty bridge is null.
func (h *profileHost) register(t *testing.T, dir, file, id string, pid int, bridge string) {
	t.Helper()
	sdir := filepath.Join(h.user, dir, "sessions")
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := "null"
	if bridge != "" {
		b = `"` + bridge + `"`
	}
	reg := `{"sessionId":"` + id + `","pid":` + strconv.Itoa(pid) + `,"bridgeSessionId":` + b + `}`
	if err := os.WriteFile(filepath.Join(sdir, file+".json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reconcile runs one pass of the session sync on the default profile's view,
// against a cloud that knows no session, and returns the screens it quit.
func (h *profileHost) reconcile(t *testing.T) string {
	t.Helper()
	_, view, err := syncTarget(h.mgr)
	if err != nil {
		t.Fatal(err)
	}
	rec := &cloud.Reconciler{Cloud: notFoundCloud{}, Manager: view, Store: h.db, Grace: 0, Log: discardLogger()}
	rec.ReconcileOnce(context.Background())
	got, _ := os.ReadFile(h.killed)
	return string(got)
}

// fakePS puts a ps on PATH that prints out, as `ps -e -o pid=,ppid=,args=`.
func fakePS(t *testing.T, out string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nprintf '"+out+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const (
	sessX = "6fd0b321-a454-4b40-9aed-131afe120d36"
	sessW = "a9c1cf1e-ce20-4833-9eeb-7acf5c327506"
	sessZ = "3c4b2a1e-0f9d-4e8c-b7a6-5d4c3b2a1f0e"
)

// A session whose claude registered in another profile's registry stays out
// of the sync's view, even when its own profile resolves to the default: its
// last bridge belongs to the other account, where the default's cloud would
// read it as deleted.
func TestReconcilerLeavesAnotherProfilesSession(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "",
		map[string]int{sessX: 99999998, sessW: 99999999})
	// Both registries have reset their bridge, so the store's last one is all
	// the reconciler has.
	h.register(t, ".claude-private", sessX, sessX, 0, "")
	h.register(t, ".claude", sessW, sessW, 0, "")
	for id, bridge := range map[string]string{sessX: "session_private", sessW: "session_work"} {
		// x's row says work, yet its claude runs in private.
		if err := h.db.Record(id, "crtest-"+id, "", "", "Detached", "", "work"); err != nil {
			t.Fatal(err)
		}
		if err := h.db.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "crtest-" + id, BridgeSessionID: bridge}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := h.reconcile(t), "crtest-"+sessW+"\n"; got != want {
		t.Errorf("killed = %q, want only %q, the work session gone from the work cloud", got, want)
	}
}

// A stale registry entry in the default profile, left by a crashed claude
// whose pid a private session's claude now has, does not pull that session
// into the default's sync, where its stale bridge would read as deleted.
func TestReconcilerIgnoresAStaleEntryWithAReusedPID(t *testing.T) {
	// work is listed last, so a merge that lets the later profile win picks
	// the stale entry.
	h := newProfileHost(t, "private=~/.claude-private,work=~/.claude", "work", map[string]int{sessX: 4242})
	fakePS(t, "  7777  4242 claude --resume "+sessX+"\\n")
	h.register(t, ".claude-private", sessX, sessX, 7777, "")
	h.register(t, ".claude", sessZ, sessZ, 7777, "session_stale")
	if err := h.db.Record(sessX, "crtest-"+sessX, "", "", "Detached", "", "private"); err != nil {
		t.Fatal(err)
	}
	if got := h.reconcile(t); got != "" {
		t.Errorf("killed = %q, want nothing: the session runs in private", got)
	}
}

// A session whose profile is only a guess (a legacy row and no transcript,
// so the ~/.claude rule picks work) is not tied to a stale work entry that
// shares its claude's pid with a live private entry: the guess cannot tell
// the two apart, so neither is its registration and nothing is quit.
func TestReconcilerIgnoresAStaleEntryInAGuessedProfile(t *testing.T) {
	h := newProfileHost(t, "private=~/.claude-private,work=~/.claude", "work", map[string]int{sessX: 4242})
	fakePS(t, "  7777  4242 claude --resume "+sessX+"\\n")
	h.register(t, ".claude-private", sessX, sessX, 7777, "")
	h.register(t, ".claude", sessZ, sessZ, 7777, "session_stale")
	if err := h.db.Record(sessX, "crtest-"+sessX, "", "", "Detached", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := h.reconcile(t); got != "" {
		t.Errorf("killed = %q, want nothing: the guess picked a stale entry", got)
	}
}

// With an unambiguous live registration, a session's profile is the one it
// registered in, everywhere: in the listing and in each profile's view.
func TestListShowsTheRegisteredProfile(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	h.register(t, ".claude-private", sessX, sessX, 0, "")
	// A legacy row: no profile recorded, and no transcript to find one by.
	if err := h.db.Record(sessX, "crtest-"+sessX, "", "", "Detached", "", ""); err != nil {
		t.Fatal(err)
	}

	rr := send(t, newHandler(testToken, h.mgr, nil, h.db), http.MethodGet, "/sessions", "")
	var body struct{ Sessions []session.Session }
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /sessions = %d %s: %v", rr.Code, rr.Body, err)
	}
	if l := body.Sessions; len(l) != 1 || l[0].Profile != "private" {
		t.Errorf("GET /sessions = %+v, want the session in private", l)
	}
	for name, want := range map[string]int{"private": 1, "work": 0} {
		v, err := h.mgr.ForProfile(name)
		if err != nil {
			t.Fatal(err)
		}
		if l, _ := v.List(); len(l) != want {
			t.Errorf("%s view List = %+v, want %d sessions", name, l, want)
		}
	}
}

func TestRunRefusesCredentialsWithProfiles(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	t.Setenv("SCREEN_BIN", os.Args[0])
	t.Setenv("CLAUDE_BIN", os.Args[0])
	t.Setenv("CLAUDE_REMOTE_API_ADDR", "127.0.0.1:0")
	t.Setenv("CLAUDE_REMOTE_SESSION_SYNC", "off")
	t.Setenv("CLAUDE_REMOTE_DB", filepath.Join(t.TempDir(), "test.db"))
	t.Setenv("CLAUDE_REMOTE_PROFILES", "work=/w,private=/p")
	t.Setenv("CLAUDE_REMOTE_CREDENTIALS", "/creds.json")
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	// Pre-cancelled: a run that ignored the setting would serve and exit 0.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := run(ctx); code != 1 {
		t.Errorf("run = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "CLAUDE_REMOTE_CREDENTIALS") || !strings.Contains(buf.String(), "CLAUDE_REMOTE_PROFILES") {
		t.Errorf("log = %q, want it to name CLAUDE_REMOTE_CREDENTIALS and CLAUDE_REMOTE_PROFILES", buf.String())
	}
}
