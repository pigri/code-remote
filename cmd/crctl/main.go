// crctl is a small local client for the claude-remote API: list, create, and
// stop detached claude sessions, and see which screen maps to which claude
// session/title.
//
//	crctl ls                 # list sessions (default)
//	crctl new                # start a new session
//	crctl rm <id>            # stop a session
//
// Config (env):
//
//	CLAUDE_REMOTE_API_URL    base URL (default http://127.0.0.1:8080)
//	CLAUDE_REMOTE_API_TOKEN  bearer token (required)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type session struct {
	ID     string `json:"id"`
	Screen string `json:"screen"`
	Title  string `json:"title"`
	PID    string `json:"pid"`
	Status string `json:"status"`
}

func main() {
	cmd := "ls"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	var err error
	switch cmd {
	case "ls", "list":
		err = list()
	case "new", "create":
		err = create()
	case "rm", "stop", "delete":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: crctl rm <id>")
		} else {
			err = remove(os.Args[2])
		}
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "crctl:", err)
		os.Exit(1)
	}
}

func list() error {
	var resp struct {
		Sessions []session `json:"sessions"`
	}
	if err := apiJSON(http.MethodGet, "/sessions", nil, &resp); err != nil {
		return err
	}
	if len(resp.Sessions) == 0 {
		fmt.Println("No sessions running.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tATTACH")
	for _, s := range resp.Sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\tscreen -r %s\n", s.ID, title, s.Status, s.Screen)
	}
	return w.Flush()
}

func create() error {
	var s session
	if err := apiJSON(http.MethodPost, "/sessions", nil, &s); err != nil {
		return err
	}
	fmt.Printf("started %s\n  attach: screen -r %s\n", s.ID, s.Screen)
	return nil
}

func remove(id string) error {
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := apiJSON(http.MethodDelete, "/sessions/"+id, nil, &resp); err != nil {
		return err
	}
	fmt.Printf("%s %s\n", resp.ID, resp.Status)
	return nil
}

// apiJSON performs an authenticated request and decodes the JSON response into
// out (may be nil). Non-2xx responses are surfaced as errors with the server's
// message when present.
func apiJSON(method, path string, body, out any) error {
	base := envOr("CLAUDE_REMOTE_API_URL", "http://127.0.0.1:8080")
	token := os.Getenv("CLAUDE_REMOTE_API_TOKEN")
	if token == "" {
		return fmt.Errorf("CLAUDE_REMOTE_API_TOKEN is not set")
	}

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if msg := jsonError(data); msg != "" {
			return fmt.Errorf("%s (%d)", msg, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func jsonError(data []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	return e.Error
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Print(`crctl - local client for the claude-remote API

Usage:
  crctl ls            list running sessions (default)
  crctl new           start a new detached claude session
  crctl rm <id>       stop a session

Env:
  CLAUDE_REMOTE_API_URL    base URL (default http://127.0.0.1:8080)
  CLAUDE_REMOTE_API_TOKEN  bearer token (required)
`)
}
