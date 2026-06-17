package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"claude-remote-api/internal/cloud"
	"claude-remote-api/internal/session"
	"claude-remote-api/internal/store"
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

	mgr := &session.Manager{Prefix: prefix, ClaudeBin: claudeBin, ScreenBin: screenBin, ClaudeHome: claudeHome,
		WorkspaceRoot: os.Getenv("CLAUDE_WORKSPACE_ROOT")}

	logger := auditLogger()

	// ctx is cancelled on SIGINT/SIGTERM to drive a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sync := startSessionSync(ctx, logger, mgr, claudeHome)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(token, mgr, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("starting", "addr", addr, "prefix", prefix)
	srvErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	var exitErr error
	select {
	case <-ctx.Done(): // signal received
		logger.Info("shutting_down")
	case exitErr = <-srvErr: // server failed to run
		logger.Error("server_error", "error", exitErr.Error())
	}

	// Drain in-flight requests, then stop the reconciler and checkpoint+close
	// the SQLite store.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if err := sync.Close(); err != nil {
		logger.Warn("store_close", "error", err.Error())
	}

	if exitErr != nil {
		os.Exit(1)
	}
}

// auditLogger builds the structured logger used for the audit trail.
// CLAUDE_REMOTE_LOG_FORMAT=json emits JSON (one object per line) for log
// shippers; anything else (default) emits human-readable key=value text.
func auditLogger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var h slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if strings.EqualFold(envOr("CLAUDE_REMOTE_LOG_FORMAT", "text"), "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// syncHandle owns the background reconciler's lifecycle and its store. Close
// waits for the reconciler goroutine to stop, then closes the store (which
// checkpoints the SQLite WAL). Safe to call on a nil handle.
type syncHandle struct {
	db   io.Closer
	done <-chan struct{}
}

func (h *syncHandle) Close() error {
	if h == nil {
		return nil
	}
	if h.done != nil {
		<-h.done // let the in-flight reconcile finish before closing the DB
	}
	if h.db != nil {
		return h.db.Close()
	}
	return nil
}

// startSessionSync launches the background reconciler that polls the Anthropic
// Sessions API and quits the screen of any session archived (or deleted)
// server-side. The reconciler stops when ctx is cancelled; the returned handle's
// Close waits for it and closes the store.
//
// It's enabled by default but degrades gracefully: if the OAuth credentials
// file is absent (e.g. a headless host with no logged-in claude), it logs once
// and does nothing. Disable explicitly with CLAUDE_REMOTE_SESSION_SYNC=off.
func startSessionSync(ctx context.Context, logger *slog.Logger, mgr *session.Manager, claudeHome string) *syncHandle {
	if !envBool("CLAUDE_REMOTE_SESSION_SYNC", true) {
		return nil
	}

	credsPath := envOr("CLAUDE_REMOTE_CREDENTIALS", filepath.Join(claudeHome, ".credentials.json"))
	if _, err := os.Stat(credsPath); err != nil {
		logger.Warn("session_sync_disabled", "reason", "no credentials file", "path", credsPath)
		return nil
	}

	interval := 30 * time.Second
	if v := os.Getenv("CLAUDE_REMOTE_SYNC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		} else {
			logger.Warn("session_sync", "msg", "bad CLAUDE_REMOTE_SYNC_INTERVAL, using default", "value", v)
		}
	}

	grace := 15 * time.Minute
	if v := os.Getenv("CLAUDE_REMOTE_ARCHIVE_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			grace = d
		} else {
			logger.Warn("session_sync", "msg", "bad CLAUDE_REMOTE_ARCHIVE_GRACE, using default", "value", v)
		}
	}

	client := &cloud.Client{
		BaseURL:         envOr("CLAUDE_REMOTE_CLOUD_BASE", cloud.DefaultBaseURL),
		CredentialsPath: credsPath,
	}
	rec := &cloud.Reconciler{
		Cloud:      client,
		Manager:    mgr,
		Interval:   interval,
		Grace:      grace,
		Log:        logger,
		MatchTitle: envBool("CLAUDE_REMOTE_MATCH_TITLE", false),
	}

	// Durable session mirror + ledger. Best-effort: on failure the reconciler
	// falls back to its in-memory grace clock (mirror is dropped).
	h := &syncHandle{}
	dbPath := envOr("CLAUDE_REMOTE_DB", defaultDBPath())
	if db, err := store.Open(dbPath); err != nil {
		logger.Warn("session_store_disabled", "reason", "open failed", "path", dbPath, "error", err.Error())
	} else {
		rec.Store = db
		h.db = db
		logger.Info("session_store_enabled", "path", dbPath)
	}

	logger.Info("session_sync_enabled", "interval", interval.String(), "grace", grace.String(), "credentials", credsPath, "match_title", rec.MatchTitle)
	done := make(chan struct{})
	h.done = done
	go func() {
		defer close(done)
		rec.Run(ctx)
	}()
	return h
}

// defaultDBPath is $XDG_DATA_HOME/code-remote (or ~/.local/share/code-remote)
// /code-remote.db.
func defaultDBPath() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "code-remote", "code-remote.db")
}

// newHandler builds the fully-wired HTTP handler (audit log + routes + bearer
// auth) for the given session manager. Shared by main() and the e2e tests.
func newHandler(token string, mgr *session.Manager, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv := &server{mgr: mgr, log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /sessions", srv.create)
	mux.HandleFunc("GET /sessions", srv.list)
	mux.HandleFunc("GET /sessions/{id}", srv.get)
	mux.HandleFunc("DELETE /sessions/{id}", srv.delete)
	return auditMiddleware(logger, authMiddleware(token, mux))
}

type server struct {
	mgr *session.Manager
	log *slog.Logger
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	// Optional JSON body: {"dir": "<path under CLAUDE_WORKSPACE_ROOT>"}.
	var body struct {
		Dir string `json:"dir"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	sess, err := s.mgr.Create(body.Dir)
	if err != nil {
		s.log.Error("session_create", "remote", clientIP(r), "error", err.Error(), "dir", body.Dir)
		if errors.Is(err, session.ErrInvalidDir) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("session_create", "remote", clientIP(r), "id", sess.ID, "screen", sess.Screen, "dir", body.Dir)
	writeJSON(w, http.StatusCreated, sess)
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	sessions, err := s.mgr.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sessions == nil {
		sessions = []session.Session{}
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
		s.log.Error("session_delete", "remote", clientIP(r), "id", id, "error", err.Error())
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !existed {
		s.log.Info("session_delete", "remote", clientIP(r), "id", id, "existed", false)
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	s.log.Info("session_delete", "remote", clientIP(r), "id", id, "existed", true)
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

// auditMiddleware emits one structured audit line per request — including
// rejected ones — capturing method, path, response status, latency, client IP,
// and the auth outcome. It is the outermost wrapper so unauthorized attempts
// (401s from authMiddleware) are logged too. The bearer token is never logged.
func auditMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		auth := "ok"
		switch {
		case r.URL.Path == "/healthz":
			auth = "n/a"
		case rec.status == http.StatusUnauthorized:
			auth = "denied"
		}

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
			"auth", auth,
		}
		if ff := r.Header.Get("X-Forwarded-For"); ff != "" {
			attrs = append(attrs, "forwarded_for", ff)
		}
		logger.Info("request", attrs...)
	})
}

// statusRecorder captures the response status code for the audit log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

// clientIP is the request's source address without the port. Behind ngrok +
// Synapse this is 127.0.0.1; the real client is in the X-Forwarded-For header
// (logged separately, and only as trustworthy as the upstream that set it).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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

// envBool reads a boolean env var (on/off/true/false/1/0), returning def when
// unset or unrecognized.
func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	default:
		return def
	}
}
