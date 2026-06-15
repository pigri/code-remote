package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	token := os.Getenv("CLAUDE_REMOTE_API_TOKEN")
	if token == "" {
		log.Fatal("CLAUDE_REMOTE_API_TOKEN is required (this endpoint launches processes; refusing to run unauthenticated)")
	}

	addr := envOr("CLAUDE_REMOTE_API_ADDR", ":8080")
	prefix := envOr("CLAUDE_REMOTE_SESSION_PREFIX", "pigri-dev-remote")

	// Resolve the binaries to absolute paths up front: screen execs claude in a
	// fresh environment, so passing an absolute path avoids PATH surprises and
	// fails fast here if either is missing.
	screenBin, err := exec.LookPath(envOr("SCREEN_BIN", "screen"))
	if err != nil {
		log.Fatalf("screen binary not found: %v", err)
	}
	claudeBin, err := exec.LookPath(envOr("CLAUDE_BIN", "claude"))
	if err != nil {
		log.Fatalf("claude binary not found: %v", err)
	}

	// ~/.claude holds the per-session logs we read titles from. CLAUDE_HOME
	// overrides it (e.g. if HOME isn't the claude user's home).
	claudeHome := envOr("CLAUDE_HOME", "")
	if claudeHome == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			claudeHome = filepath.Join(home, ".claude")
		}
	}

	mgr := &Manager{Prefix: prefix, ClaudeBin: claudeBin, ScreenBin: screenBin, ClaudeHome: claudeHome}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(token, mgr),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("claude-remote-api listening on %s (session prefix %q)", addr, prefix)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// newHandler builds the fully-wired HTTP handler (routes + bearer auth) for the
// given session manager. Shared by main() and the e2e tests.
func newHandler(token string, mgr *Manager) http.Handler {
	srv := &server{mgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /sessions", srv.create)
	mux.HandleFunc("GET /sessions", srv.list)
	mux.HandleFunc("GET /sessions/{id}", srv.get)
	mux.HandleFunc("DELETE /sessions/{id}", srv.delete)
	return authMiddleware(token, mux)
}

type server struct{ mgr *Manager }

func (s *server) create(w http.ResponseWriter, _ *http.Request) {
	sess, err := s.mgr.Create()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	sessions, err := s.mgr.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sessions == nil {
		sessions = []Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.mgr.ValidID(id) {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	sess, ok, err := s.mgr.Get(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.mgr.ValidID(id) {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	existed, err := s.mgr.Kill(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !existed {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "stopped"})
}

// authMiddleware enforces a constant-time bearer-token check on every route
// except the unauthenticated health probe.
func authMiddleware(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
