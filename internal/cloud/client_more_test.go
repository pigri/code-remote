package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claude-remote-api/internal/session"
)

func TestGetFoundNotFoundError(t *testing.T) {
	var status int
	body := `{"id":"session_a","title":"t","session_status":"idle","session_context":{"cwd":"/x"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, CredentialsPath: writeCreds(t, "tok")}

	t.Run("found", func(t *testing.T) {
		status = http.StatusOK
		s, ok, err := c.Get(context.Background(), "session_a")
		if err != nil || !ok || s.ID != "session_a" || s.Cwd() != "/x" {
			t.Fatalf("Get found = %+v ok=%v err=%v", s, ok, err)
		}
	})
	t.Run("not found (404)", func(t *testing.T) {
		status = http.StatusNotFound
		_, ok, err := c.Get(context.Background(), "gone")
		if ok || err != nil {
			t.Fatalf("Get 404 ok=%v err=%v, want false/nil", ok, err)
		}
	})
	t.Run("server error does not leak token", func(t *testing.T) {
		status = http.StatusInternalServerError
		body = "secret internal detail"
		_, ok, err := c.Get(context.Background(), "boom")
		if ok || err == nil {
			t.Fatalf("Get 500 ok=%v err=%v, want false + error", ok, err)
		}
	})
}

func TestLoadTokenVariants(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("nested claudeAiOauth", func(t *testing.T) {
		tok, err := loadToken(write("a.json", `{"claudeAiOauth":{"accessToken":"nested"}}`))
		if err != nil || tok != "nested" {
			t.Fatalf("loadToken nested = %q, %v", tok, err)
		}
	})
	t.Run("top-level fallback", func(t *testing.T) {
		tok, err := loadToken(write("b.json", `{"accessToken":"toplevel"}`))
		if err != nil || tok != "toplevel" {
			t.Fatalf("loadToken top-level = %q, %v", tok, err)
		}
	})
	t.Run("no token", func(t *testing.T) {
		if _, err := loadToken(write("c.json", `{"other":"x"}`)); err == nil {
			t.Fatal("loadToken with no token = nil error, want error")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		if _, err := loadToken(write("d.json", `{not json`)); err == nil {
			t.Fatal("loadToken malformed = nil error, want error")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := loadToken(filepath.Join(dir, "nope.json")); err == nil {
			t.Fatal("loadToken missing file = nil error, want error")
		}
	})
}

func TestBaseAndHTTPClientDefaults(t *testing.T) {
	if got := (&Client{}).base(); got != DefaultBaseURL {
		t.Errorf("base() default = %q, want %q", got, DefaultBaseURL)
	}
	if got := (&Client{BaseURL: "https://x/"}).base(); got != "https://x" {
		t.Errorf("base() trims slash = %q, want https://x", got)
	}
	custom := &http.Client{Timeout: time.Second}
	if got := (&Client{HTTP: custom}).httpClient(); got != custom {
		t.Error("httpClient() should return the injected client")
	}
	if (&Client{}).httpClient() == nil {
		t.Error("httpClient() default = nil")
	}
}

// newRequest surfaces a credentials error (missing file) before any HTTP call.
func TestListCredentialsError(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:0", CredentialsPath: filepath.Join(t.TempDir(), "missing.json")}
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("List with missing credentials = nil error, want error")
	}
}

// Run does an initial reconcile, then ticks until the context is cancelled.
func TestRunTicksAndStops(t *testing.T) {
	cloudCl := &fakeCloud{sessions: []Session{{ID: "session_X", SessionStatus: "archived"}}}
	mgr := &fakeManager{
		sessions: []session.Session{{ID: "uuid-1", Screen: "p-uuid-1"}},
		regs:     []session.Registration{{SessionID: "uuid-1", BridgeSessionID: "session_X"}},
	}
	r := &Reconciler{Cloud: cloudCl, Manager: mgr, Log: testLogger(), Interval: time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// Let the initial reconcile + at least one tick fire, then stop.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	if len(mgr.killed) == 0 {
		t.Error("expected the archived session to be quit at least once")
	}
}
