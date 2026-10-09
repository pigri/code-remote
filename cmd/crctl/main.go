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
//	crctl resume <id>        # relaunch a stopped session (--dir D to move it)
//	crctl resume all         # relaunch every stopped session
//	crctl restart <id>       # stop + resume a session under the same id (--dir D to move it)
//	crctl upgrade --all      # update claude, restart all sessions onto the new version
//	crctl upgrade <id>...    # update claude, restart just these sessions
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
	"slices"
	"sort"
	"strconv"
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
	resume(id, dir string) (session.Session, error)
	restart(id, dir string) (session.Session, error)
	upgrade(restart, force bool, ids ...string) (session.UpgradeResult, error)
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

	// --trust (anywhere on the line): mark the session's directory as a trusted
	// folder for claude before launching into it.
	trust := false
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool {
		if a == "--trust" {
			trust = true
		}
		return a == "--trust"
	})

	be, err := pickBackend()
	if err != nil {
		return err
	}
	if trust {
		lb, ok := be.(*localBackend)
		if !ok {
			return fmt.Errorf("--trust is local-only; in remote mode set CLAUDE_REMOTE_TRUST_DIRS=1 on the API server")
		}
		lb.mgr.TrustDirs = true
	}

	switch cmd {
	case "ls", "list":
		return list(be)
	case "new", "create":
		return create(be, args[1:])
	case "resume":
		dir, rest, err := dirFlag(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: crctl resume (all | <id> [--dir D])")
		}
		if a := rest[0]; a == "all" || a == "--all" || a == "-a" {
			if dir != "" {
				return fmt.Errorf("--dir moves one session; it can't be combined with `resume all`")
			}
			return resumeAll(be)
		}
		s, err := be.resume(rest[0], dir)
		if err != nil {
			return err
		}
		fmt.Printf("resumed %s\n  attach: screen -r %s\n", s.ID, s.Screen)
		trustHint(s)
		return nil
	case "restart":
		dir, rest, err := dirFlag(args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 1 {
			return fmt.Errorf("usage: crctl restart <id> [--dir D]")
		}
		s, err := be.restart(rest[0], dir)
		if err != nil {
			return err
		}
		fmt.Printf("restarted %s\n  attach: screen -r %s\n", s.ID, s.Screen)
		trustHint(s)
		return nil
	case "upgrade", "update":
		return upgrade(be, args[1:])
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
		return fmt.Errorf("unknown command %q (try: ls, new, resume, restart, upgrade, rm)", cmd)
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
		// Local mode runs as the invoking user, who can already start claude
		// anywhere; the workspace root only narrows --dir when it is set.
		WorkspaceRoot: envOr("CLAUDE_WORKSPACE_ROOT", "/"),
		TrustDirs:     isTrue(os.Getenv("CLAUDE_REMOTE_TRUST_DIRS")),
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

func (b *localBackend) list() ([]session.Session, error) { return b.mgr.ListAll(b.stopped) }
func (b *localBackend) create(dir string) (session.Session, error) {
	return b.mgr.Create(localDir(dir))
}
func (b *localBackend) resume(id, dir string) (session.Session, error) {
	return b.mgr.ResumeIn(id, localDir(dir))
}
func (b *localBackend) restart(id, dir string) (session.Session, error) {
	return b.mgr.RestartIn(id, localDir(dir))
}

// localDir makes a --dir given on the command line absolute, so a relative one
// means "relative to where crctl was run" (the manager would otherwise anchor
// it to the workspace root). Remote mode leaves that to the server.
func localDir(dir string) string {
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}
func (b *localBackend) upgrade(restart, force bool, ids ...string) (session.UpgradeResult, error) {
	return b.mgr.Upgrade(restart, force, ids...)
}
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
	err := b.do(http.MethodPost, "/sessions", dirBody(dir), &s)
	return s, err
}

func (b *httpBackend) resume(id, dir string) (session.Session, error) {
	var s session.Session
	err := b.do(http.MethodPost, "/sessions/"+id+"/resume", dirBody(dir), &s)
	return s, err
}

func (b *httpBackend) restart(id, dir string) (session.Session, error) {
	var s session.Session
	err := b.do(http.MethodPost, "/sessions/"+id+"/restart", dirBody(dir), &s)
	return s, err
}

// dirBody is the optional {"dir": ...} request body; nil when no dir is given.
func dirBody(dir string) any {
	if dir == "" {
		return nil
	}
	return map[string]string{"dir": dir}
}

func (b *httpBackend) upgrade(restart, force bool, ids ...string) (session.UpgradeResult, error) {
	var res session.UpgradeResult
	in := map[string]any{"restart": restart, "force": force}
	if len(ids) > 0 {
		in["sessions"] = ids
	}
	// The updater downloads a new binary; allow far longer than a normal call.
	err := b.doTimeout(upgradeTimeout, http.MethodPost, "/upgrade", in, &res)
	return res, err
}

func (b *httpBackend) remove(id string) error {
	return b.do(http.MethodDelete, "/sessions/"+id, nil, nil)
}

// upgradeTimeout is the client-side cap on a remote `crctl upgrade`.
const upgradeTimeout = 10 * time.Minute

func (b *httpBackend) do(method, path string, in, out any) error {
	return b.doTimeout(30*time.Second, method, path, in, out)
}

func (b *httpBackend) doTimeout(timeout time.Duration, method, path string, in, out any) error {
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
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
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
	// Most recently active first; sessions with no known activity go last.
	// LastActive is RFC3339 in UTC, so it orders as a string.
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].LastActive > ss[j].LastActive })
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tVERSION\tLAST ACTIVE\tACTION")
	outdated := 0
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
		version := s.Version
		if version == "" {
			version = "-"
		}
		if s.Outdated {
			version += " (outdated)"
			outdated++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, title, s.Status, version, humanizeAge(s.LastActive), action)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if outdated > 0 {
		fmt.Printf("\n%d session(s) running an outdated claude; run `crctl upgrade --all` to restart them on the installed version.\n", outdated)
	}
	return nil
}

// dirFlag pulls --dir D / -d D / --dir=D out of args, returning the directory
// ("" when not given) and the remaining arguments in order.
func dirFlag(args []string) (dir string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--dir" || args[i] == "-d":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s needs a directory", args[i])
			}
			dir = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--dir="):
			dir = strings.TrimPrefix(args[i], "--dir=")
		default:
			rest = append(rest, args[i])
		}
	}
	return dir, rest, nil
}

func create(be backend, args []string) error {
	dir, _, err := dirFlag(args)
	if err != nil {
		return err
	}
	s, err := be.create(dir)
	if err != nil {
		return err
	}
	fmt.Printf("started %s\n  attach: screen -r %s\n", s.ID, s.Screen)
	trustHint(s)
	return nil
}

// trustHint tells the user when a session was launched into a folder claude
// doesn't trust yet: it is sitting at the trust prompt until someone answers.
func trustHint(s session.Session) {
	if !s.NeedsTrust {
		return
	}
	fmt.Printf("  note: claude doesn't trust this folder yet, so the session is waiting at the\n"+
		"        trust prompt. Accept it with `screen -r %s`, or pass --trust next time.\n", s.Screen)
}

// resumeAll relaunches every stopped (resumable) session, printing what
// happened to each. One that fails doesn't stop the rest.
func resumeAll(be backend) error {
	ss, err := be.list()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tRESULT\tREASON")
	stopped, failed := 0, 0
	for _, s := range ss {
		if s.Status != "Stopped" {
			continue
		}
		stopped++
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		result, reason := "resumed", ""
		if _, err := be.resume(s.ID, ""); err != nil {
			result, reason = "failed", err.Error()
			failed++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.ID, title, result, reason)
	}
	if stopped == 0 {
		fmt.Println("No stopped sessions to resume.")
		return nil
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d session(s) failed to resume", failed)
	}
	return nil
}

// upgrade updates claude and then restarts running sessions onto the new
// version, printing what happened to each: every session with --all, or just
// the ones named by id. With neither it only updates claude.
func upgrade(be backend, args []string) error {
	const usage = "usage: crctl upgrade [--force] (--all | <id>...)"
	all, force := false, false
	var ids []string
	for _, a := range args {
		switch {
		case a == "--all" || a == "-a":
			all = true
		case a == "--force" || a == "-f":
			force = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("%s", usage)
		default:
			ids = append(ids, a)
		}
	}
	if all && len(ids) > 0 {
		return fmt.Errorf("--all can't be combined with session ids\n%s", usage)
	}
	restart := all || len(ids) > 0
	res, err := be.upgrade(restart, force, ids...)
	if err != nil {
		if res.Output != "" {
			fmt.Fprintln(os.Stderr, res.Output)
		}
		return err
	}
	switch {
	case res.Before != "" && res.Before == res.After:
		fmt.Printf("claude %s (already up to date)\n", res.After)
	case res.Before != "" && res.After != "":
		fmt.Printf("claude %s -> %s\n", res.Before, res.After)
	default:
		fmt.Println("claude updated")
	}
	if !restart {
		fmt.Println("No sessions restarted; use `crctl upgrade --all` (or pass session ids) to move running sessions onto this version.")
		return nil
	}
	if len(res.Sessions) == 0 {
		fmt.Println("No sessions running.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tRESULT\tREASON")
	failed := 0
	for _, o := range res.Sessions {
		title := o.Title
		if title == "" {
			title = "(untitled)"
		}
		if o.Action == session.ActionFailed {
			failed++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", o.ID, title, o.Action, o.Reason)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d session(s) failed to restart", failed)
	}
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

// isTrue reports whether an env value spells "on" (1, true, yes, ...).
func isTrue(v string) bool {
	b, _ := strconv.ParseBool(v)
	return b || strings.EqualFold(v, "yes") || strings.EqualFold(v, "on")
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
  crctl resume <id> [--dir D]
                      relaunch a stopped session by id (--dir moves it there)
  crctl resume all    relaunch every stopped session
  crctl restart <id> [--dir D]
                      stop a session and resume it under the same id; --dir
                      moves it into D (e.g. its git repo, for PR/branch tracking)
  crctl upgrade [--force] (--all | <id>...)
                      update claude, then restart running sessions onto the new
                      version (each resumes its original session): every
                      session with --all, or just the named ones. Sessions
                      already on the new version or not idle are skipped;
                      --force restarts non-idle ones too. With neither --all
                      nor ids, only claude is updated.
  crctl rm <id>       stop a session
  --trust             with new/resume/restart: mark the session's directory as a
                      trusted folder for claude first, so a detached session
                      doesn't wait at the trust prompt (or CLAUDE_REMOTE_TRUST_DIRS=1)

Runs LOCALLY by default (drives screen/claude directly; no API or token).
Set CLAUDE_REMOTE_API_URL to use a remote API instead:

Env:
  CLAUDE_REMOTE_API_URL    use the HTTP API at this base URL (remote mode)
  CLAUDE_REMOTE_API_TOKEN  bearer token (required in remote mode)
  CLAUDE_REMOTE_SESSION_PREFIX, CLAUDE_BIN, SCREEN_BIN, CLAUDE_HOME  (local mode)
`)
}
