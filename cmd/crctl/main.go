// crctl is a client for claude-remote: list, create, and stop detached claude
// sessions.
//
// By default it runs LOCALLY — driving `screen`/`claude` directly, with no API
// process, token, or URL. Set CLAUDE_REMOTE_API_URL to talk to a remote API
// instead (bearer token required).
//
//	crctl ls                 # list sessions (default)
//	crctl new                # start a new session
//	crctl rm <id>            # stop a session
//
// Env:
//
//	CLAUDE_REMOTE_API_URL    if set, use the HTTP API at this base URL (remote mode)
//	CLAUDE_REMOTE_API_TOKEN  bearer token (required in remote mode)
//	CLAUDE_REMOTE_SESSION_PREFIX, CLAUDE_BIN, SCREEN_BIN, CLAUDE_HOME  (local mode)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"claude-remote-api/internal/session"
	"claude-remote-api/internal/store"
)

// backend is the set of operations crctl needs, satisfied by either the local
// screen manager or the remote HTTP API.
type backend interface {
	list() ([]session.Session, error)
	create(dir string) (session.Session, error)
	resume(id string) (session.Session, error)
	remove(id string) error
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fail(err)
	}
}

// run dispatches a crctl invocation (args after the program name). Split from
// main so the command routing is testable without spawning a process.
func run(args []string) error {
	cmd := "ls"
	if len(args) > 0 {
		cmd = args[0]
	}
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage()
		return nil
	}

	be, err := pickBackend()
	if err != nil {
		return err
	}

	switch cmd {
	case "ls", "list":
		return list(be)
	case "new", "create":
		return create(be, args[1:])
	case "resume":
		if len(args) < 2 {
			return fmt.Errorf("usage: crctl resume <id>")
		}
		s, err := be.resume(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("resumed %s\n  attach: screen -r %s\n", s.ID, s.Screen)
		return nil
	case "rm", "stop", "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: crctl rm <id>")
		}
		if err := be.remove(args[1]); err != nil {
			return err
		}
		fmt.Printf("%s stopped\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown command %q (try: ls, new, rm)", cmd)
	}
}

// pickBackend selects remote (HTTP) mode when CLAUDE_REMOTE_API_URL is set,
// otherwise local mode (drives screen/claude directly).
func pickBackend() (backend, error) {
	if base := os.Getenv("CLAUDE_REMOTE_API_URL"); base != "" {
		token := os.Getenv("CLAUDE_REMOTE_API_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("CLAUDE_REMOTE_API_TOKEN is required in remote mode (CLAUDE_REMOTE_API_URL is set)")
		}
		return &httpBackend{base: strings.TrimRight(base, "/"), token: token}, nil
	}
	mgr := &session.Manager{
		Prefix:     envOr("CLAUDE_REMOTE_SESSION_PREFIX", "pigri-dev-remote"),
		ScreenBin:  resolveBin(envOr("SCREEN_BIN", "screen")),
		ClaudeBin:  resolveBin(envOr("CLAUDE_BIN", "claude")),
		ClaudeHome: claudeHome(),
	}
	// Share the server's SQLite mirror so `new`/`resume` record sessions and
	// `ls` can surface resumable (stopped) ones. Best-effort: a store that won't
	// open just means no resumable tracking in local mode.
	be := &localBackend{mgr: mgr}
	if db, err := store.Open(envOr("CLAUDE_REMOTE_DB", store.DefaultPath())); err == nil {
		mgr.Store = db
		be.stopped = db
	}
	return be, nil
}

// ---- local backend (direct screen/claude) ----

type localBackend struct {
	mgr     *session.Manager
	stopped session.StoppedLister // nil when the store didn't open
}

func (b *localBackend) list() ([]session.Session, error)           { return b.mgr.ListAll(b.stopped) }
func (b *localBackend) create(dir string) (session.Session, error) { return b.mgr.Create(dir) }
func (b *localBackend) resume(id string) (session.Session, error)  { return b.mgr.Resume(id) }
func (b *localBackend) remove(id string) error {
	if !b.mgr.ValidID(id) {
		return fmt.Errorf("invalid session id")
	}
	existed, err := b.mgr.Kill(id)
	if err != nil {
		return err
	}
	if !existed {
		return fmt.Errorf("session not found")
	}
	return nil
}

// ---- remote backend (HTTP API) ----

type httpBackend struct{ base, token string }

func (b *httpBackend) list() ([]session.Session, error) {
	var resp struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := b.do(http.MethodGet, "/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

func (b *httpBackend) create(dir string) (session.Session, error) {
	var s session.Session
	var in any
	if dir != "" {
		in = map[string]string{"dir": dir}
	}
	err := b.do(http.MethodPost, "/sessions", in, &s)
	return s, err
}

func (b *httpBackend) resume(id string) (session.Session, error) {
	var s session.Session
	err := b.do(http.MethodPost, "/sessions/"+id+"/resume", nil, &s)
	return s, err
}

func (b *httpBackend) remove(id string) error {
	return b.do(http.MethodDelete, "/sessions/"+id, nil, nil)
}

func (b *httpBackend) do(method, path string, in, out any) error {
	var reqBody io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, b.base+path, reqBody)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error != "" {
			return fmt.Errorf("%s (%d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ---- commands ----

func list(be backend) error {
	ss, err := be.list()
	if err != nil {
		return err
	}
	if len(ss) == 0 {
		fmt.Println("No sessions running.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tLAST ACTIVE\tACTION")
	for _, s := range ss {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		// Stopped sessions have no live screen to attach to — show how to bring
		// them back instead.
		action := "screen -r " + s.Screen
		if s.Status == "Stopped" {
			action = "crctl resume " + s.ID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, title, s.Status, humanizeAge(s.LastActive), action)
	}
	return w.Flush()
}

func create(be backend, args []string) error {
	dir := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--dir" || args[i] == "-d":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--dir="):
			dir = strings.TrimPrefix(args[i], "--dir=")
		}
	}
	s, err := be.create(dir)
	if err != nil {
		return err
	}
	fmt.Printf("started %s\n  attach: screen -r %s\n", s.ID, s.Screen)
	return nil
}

// ---- helpers ----

// humanizeAge renders an RFC3339 timestamp as a compact relative age ("3m ago",
// "2h ago", "5d ago"). Returns "-" for an empty or unparseable value.
func humanizeAge(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now" // clock skew; don't print a negative age
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func resolveBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

func claudeHome() string {
	if v := os.Getenv("CLAUDE_HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".claude")
	}
	return ""
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "crctl:", err)
	os.Exit(1)
}

func usage() {
	fmt.Print(`crctl - client for claude-remote

Usage:
  crctl ls            list running sessions (default)
  crctl new [--dir D] start a new detached claude session (optional working dir)
  crctl resume <id>   relaunch a stopped session by id
  crctl rm <id>       stop a session

Runs LOCALLY by default (drives screen/claude directly; no API or token).
Set CLAUDE_REMOTE_API_URL to use a remote API instead:

Env:
  CLAUDE_REMOTE_API_URL    use the HTTP API at this base URL (remote mode)
  CLAUDE_REMOTE_API_TOKEN  bearer token (required in remote mode)
  CLAUDE_REMOTE_SESSION_PREFIX, CLAUDE_BIN, SCREEN_BIN, CLAUDE_HOME  (local mode)
`)
}
