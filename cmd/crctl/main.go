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
)

// backend is the set of operations crctl needs, satisfied by either the local
// screen manager or the remote HTTP API.
type backend interface {
	list() ([]session.Session, error)
	create() (session.Session, error)
	remove(id string) error
}

func main() {
	cmd := "ls"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage()
		return
	}

	be, err := pickBackend()
	if err != nil {
		fail(err)
	}

	switch cmd {
	case "ls", "list":
		err = list(be)
	case "new", "create":
		err = create(be)
	case "rm", "stop", "delete":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: crctl rm <id>")
		} else {
			err = be.remove(os.Args[2])
			if err == nil {
				fmt.Printf("%s stopped\n", os.Args[2])
			}
		}
	default:
		err = fmt.Errorf("unknown command %q (try: ls, new, rm)", cmd)
	}
	if err != nil {
		fail(err)
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
	return &localBackend{mgr: &session.Manager{
		Prefix:     envOr("CLAUDE_REMOTE_SESSION_PREFIX", "pigri-dev-remote"),
		ScreenBin:  resolveBin(envOr("SCREEN_BIN", "screen")),
		ClaudeBin:  resolveBin(envOr("CLAUDE_BIN", "claude")),
		ClaudeHome: claudeHome(),
	}}, nil
}

// ---- local backend (direct screen/claude) ----

type localBackend struct{ mgr *session.Manager }

func (b *localBackend) list() ([]session.Session, error) { return b.mgr.List() }
func (b *localBackend) create() (session.Session, error) { return b.mgr.Create() }
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
	if err := b.do(http.MethodGet, "/sessions", &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

func (b *httpBackend) create() (session.Session, error) {
	var s session.Session
	err := b.do(http.MethodPost, "/sessions", &s)
	return s, err
}

func (b *httpBackend) remove(id string) error {
	return b.do(http.MethodDelete, "/sessions/"+id, nil)
}

func (b *httpBackend) do(method, path string, out any) error {
	req, err := http.NewRequest(method, b.base+path, nil)
	if err != nil {
		return err
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
	fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tATTACH")
	for _, s := range ss {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\tscreen -r %s\n", s.ID, title, s.Status, s.Screen)
	}
	return w.Flush()
}

func create(be backend) error {
	s, err := be.create()
	if err != nil {
		return err
	}
	fmt.Printf("started %s\n  attach: screen -r %s\n", s.ID, s.Screen)
	return nil
}

// ---- helpers ----

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
  crctl new           start a new detached claude session
  crctl rm <id>       stop a session

Runs LOCALLY by default (drives screen/claude directly; no API or token).
Set CLAUDE_REMOTE_API_URL to use a remote API instead:

Env:
  CLAUDE_REMOTE_API_URL    use the HTTP API at this base URL (remote mode)
  CLAUDE_REMOTE_API_TOKEN  bearer token (required in remote mode)
  CLAUDE_REMOTE_SESSION_PREFIX, CLAUDE_BIN, SCREEN_BIN, CLAUDE_HOME  (local mode)
`)
}
