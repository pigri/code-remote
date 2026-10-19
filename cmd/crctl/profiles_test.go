package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claude-remote-api/internal/session"
)

func TestProfilesCommand(t *testing.T) {
	be := &fakeBackend{profileList: session.ProfileList{
		Profiles: []session.ProfileName{{Name: "work"}, {Name: "private", Default: true}},
		Default:  "private",
	}}
	var err error
	out := captureStdout(t, func() { err = profiles(be, nil) })
	if err != nil || out != "  work\n* private\n" {
		t.Errorf("profiles = %q, %v; want work, then private marked default", out, err)
	}
	if err := profiles(be, []string{"extra"}); err == nil {
		t.Error("profiles with an argument = nil error, want a usage error")
	}
	if err := profiles(&fakeBackend{profilesErr: errors.New("boom")}, nil); err == nil || !contains(err.Error(), "boom") {
		t.Errorf("profiles with a failing backend = %v, want boom", err)
	}
}

func TestProfilesCommandLocal(t *testing.T) {
	t.Setenv("CLAUDE_REMOTE_API_URL", "") // local mode, whatever the caller's shell has
	t.Setenv("HOME", t.TempDir())
	t.Run("several", func(t *testing.T) {
		t.Setenv("CLAUDE_REMOTE_PROFILES", "work=~/.claude,private=~/.claude-private")
		t.Setenv("CLAUDE_REMOTE_DEFAULT_PROFILE", "private")
		var err error
		out := captureStdout(t, func() { err = run([]string{"profiles"}) })
		if err != nil || out != "  work\n* private\n" {
			t.Errorf("local profiles = %q, %v; want work, then private marked default", out, err)
		}
		if contains(out, ".claude") {
			t.Errorf("local profiles = %q, want names only", out)
		}
	})
	t.Run("single", func(t *testing.T) {
		var err error
		out := captureStdout(t, func() { err = run([]string{"profiles"}) })
		if err != nil || out != "* default\n" {
			t.Errorf("local profiles = %q, %v; want the one profile default", out, err)
		}
	})
}

func TestProfilesCommandHTTP(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"profiles": []map[string]any{{"name": "work", "default": true}, {"name": "private", "default": false}},
			"default":  "work",
		})
	}))
	defer srv.Close()
	t.Setenv("CLAUDE_REMOTE_API_URL", srv.URL)
	t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
	var err error
	out := captureStdout(t, func() { err = run([]string{"profiles"}) })
	if err != nil || out != "* work\n  private\n" {
		t.Errorf("remote profiles = %q, %v; want work marked default, then private", out, err)
	}
	if gotMethod != http.MethodGet || gotPath != "/profiles" || gotAuth != "Bearer tok" {
		t.Errorf("request = %s %s auth %q, want GET /profiles with the token", gotMethod, gotPath, gotAuth)
	}
}

// An API server from before GET /profiles answers it 404, as plain text from
// its mux, or as JSON from a proxy in front of it.
func TestProfilesCommandOldServer(t *testing.T) {
	for name, reply := range map[string]http.HandlerFunc{
		"mux": http.NotFound,
		"json": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(reply)
			defer srv.Close()
			t.Setenv("CLAUDE_REMOTE_API_URL", srv.URL)
			t.Setenv("CLAUDE_REMOTE_API_TOKEN", "tok")
			var err error
			out := captureStdout(t, func() { err = run([]string{"profiles"}) })
			if err == nil || out != "" {
				t.Fatalf("profiles against an old server = %q, %v; want an error and no output", out, err)
			}
			msg := err.Error()
			if !contains(msg, "does not support profiles") || !contains(msg, "upgrade") ||
				contains(msg, "{") || contains(msg, "page not found") || strings.Contains(msg, "\n") {
				t.Errorf("profiles against an old server: err = %q, want one clear line saying to upgrade it", msg)
			}
		})
	}
	t.Run("other errors stay as they are", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}))
		defer srv.Close()
		_, err := (&httpBackend{base: srv.URL, token: "t"}).profiles()
		if err == nil || !contains(err.Error(), "boom") || contains(err.Error(), "does not support profiles") {
			t.Errorf("profiles on a 500 = %v, want the server's error", err)
		}
	})
}

// A --profile value that starts with "-" is an invalid profile name, in both
// forms of the flag, and reaches no backend.
func TestProfileFlagLeadingDash(t *testing.T) {
	cmds := map[string]func(backend, []string) error{"new": create, "resume": resume, "restart": restart}
	for name, cmd := range cmds {
		var errs []string
		for _, form := range [][]string{{"--profile", "-work"}, {"--profile=-work"}} {
			args := form
			if name != "new" {
				args = append([]string{"s1"}, form...)
			}
			be := &fakeBackend{}
			err := cmd(be, args)
			if err == nil || !contains(err.Error(), `invalid profile name "-work"`) {
				t.Errorf("%s %v: err = %v, want an invalid profile name", name, args, err)
				continue
			}
			if be.createdProf != "" || be.movedProf != "" || be.restartedID != "" || len(be.resumedIDs) != 0 {
				t.Errorf("%s %v reached the backend", name, args)
			}
			errs = append(errs, err.Error())
		}
		if len(errs) == 2 && errs[0] != errs[1] {
			t.Errorf("%s: --profile -work says %q, --profile=-work says %q; want the same", name, errs[0], errs[1])
		}
	}
}
