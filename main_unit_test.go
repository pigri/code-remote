package main

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
	h := startSessionSync(context.Background(), discardLogger(), nil, "")
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
	if h := startSessionSync(context.Background(), discardLogger(), nil, ""); h != nil {
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
	t.Setenv("CLAUDE_REMOTE_DB", filepath.Join(dir, "mirror.db"))
	t.Setenv("CLAUDE_REMOTE_SYNC_INTERVAL", "50ms")
	t.Setenv("CLAUDE_REMOTE_ARCHIVE_GRACE", "1m")

	// A pre-cancelled context: the reconciler runs one ctx-bound reconcile that
	// fails fast (no network) and returns immediately, so this stays hermetic.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := startSessionSync(ctx, discardLogger(), &session.Manager{Prefix: "test-sync"}, "")
	if h == nil {
		t.Fatal("startSessionSync enabled = nil, want handle")
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mirror.db")); err != nil {
		t.Errorf("store db not created: %v", err)
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
	t.Setenv("CLAUDE_REMOTE_DB", filepath.Join(dir, "mirror.db"))
	t.Setenv("CLAUDE_REMOTE_SYNC_INTERVAL", "garbage") // -> warn + default
	t.Setenv("CLAUDE_REMOTE_ARCHIVE_GRACE", "garbage") // -> warn + default

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := startSessionSync(ctx, discardLogger(), &session.Manager{Prefix: "test-sync"}, "")
	if h == nil {
		t.Fatal("startSessionSync = nil, want handle")
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
