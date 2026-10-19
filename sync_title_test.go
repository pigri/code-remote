package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"claude-remote-api/internal/cloud"
)

// A resumed session is not quit by title over the cloud session of the claude
// it replaced: that one shares its title and cwd and is archived, and the new
// claude has not bridged yet.
func TestReconcilerSparesAResumedSessionOverAnArchivedTitle(t *testing.T) {
	for _, grace := range []time.Duration{0, 15 * time.Minute} {
		t.Run(grace.String(), func(t *testing.T) {
			h, rec, _, now := titleHost(t, grace, "archived")
			for range 2 {
				rec.ReconcileOnce(context.Background())
				if got := h.quits(); got != "" {
					t.Fatalf("quit %q, a resumed session not bridged yet", got)
				}
				*now = now.Add(grace + time.Minute)
			}
		})
	}
}

// The launch before the resume saw the counterpart active; that sighting is
// not the resumed launch's.
func TestReconcilerSparesAResumedSessionOverACounterpartSeenBefore(t *testing.T) {
	for _, grace := range []time.Duration{0, 15 * time.Minute} {
		t.Run(grace.String(), func(t *testing.T) {
			h, rec, cl, now := titleHost(t, grace, "running")
			rec.ReconcileOnce(context.Background())
			h.recordProfile(t, map[string]string{sessX: "work"}) // the resume
			(*cl)[0].SessionStatus = "archived"
			for range 2 {
				rec.ReconcileOnce(context.Background())
				if got := h.quits(); got != "" {
					t.Fatalf("quit %q over a sighting by the launch it replaced", got)
				}
				*now = now.Add(grace + time.Minute)
			}
		})
	}
}

// A session whose title counterpart this launch saw active is quit by title
// once that counterpart is archived.
func TestReconcilerQuitsByTitleACounterpartSeenActive(t *testing.T) {
	for _, grace := range []time.Duration{0, 15 * time.Minute} {
		t.Run(grace.String(), func(t *testing.T) {
			h, rec, cl, now := titleHost(t, grace, "running")
			rec.ReconcileOnce(context.Background())
			if got := h.quits(); got != "" {
				t.Fatalf("quit %q while its counterpart runs", got)
			}
			(*cl)[0].SessionStatus = "archived"
			for range 2 {
				if h.quits() == "" {
					rec.ReconcileOnce(context.Background())
				}
				*now = now.Add(grace + time.Minute)
			}
			if got, want := h.quits(), "crtest-"+sessX+"\n"; got != want {
				t.Errorf("quit %q, want %q", got, want)
			}
		})
	}
}

// After a title quit, a tick that runs before the session's next launch is
// recorded, as one can while the new screen starts, does not quit it again over
// the same counterpart.
func TestReconcilerQuitsByTitleOnceUntilTheNextLaunch(t *testing.T) {
	h, rec, cl, _ := titleHost(t, 0, "running")
	rec.ReconcileOnce(context.Background())
	(*cl)[0].SessionStatus = "archived"
	rec.ReconcileOnce(context.Background())
	if got, want := h.quits(), "crtest-"+sessX+"\n"; got != want {
		t.Fatalf("quit %q, want %q", got, want)
	}
	rec.ReconcileOnce(context.Background())
	if got, want := h.quits(), "crtest-"+sessX+"\n"; got != want {
		t.Errorf("quit %q after a second tick, want only the first quit", got)
	}
}

// titleHost is session sessX, titled and resumed in the work profile, its new
// claude registered unbridged in /repo, and a reconciler matching by title
// against a cloud holding one counterpart with that title and cwd in status.
func titleHost(t *testing.T, grace time.Duration, status string) (*profileHost, *cloud.Reconciler, *cloudOf, *time.Time) {
	t.Helper()
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	fakePS(t, "  7777  4242 claude --resume "+sessX+"\\n")
	h.titled(t, sessX, "fix the tests")
	h.recordProfile(t, map[string]string{sessX: "work"})
	h.registerIn(t, sessX, 7777, "/repo")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cs := cloud.Session{ID: "session_old", Title: "fix the tests", SessionStatus: status}
	cs.SessionContext.Cwd = "/repo"
	rec, cl := h.syncer(t, cs, grace, &now)
	rec.MatchTitle = true
	return h, rec, cl, &now
}

// titled gives session id the custom title title in the work profile.
func (h *profileHost) titled(t *testing.T, id, title string) {
	t.Helper()
	dir := filepath.Join(h.user, ".claude", "projects", "-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"custom-title","customTitle":"` + title + `","sessionId":"` + id + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// registerIn writes an unbridged registry entry for session id, as claude
// process pid running in cwd, in the work profile.
func (h *profileHost) registerIn(t *testing.T, id string, pid int, cwd string) {
	t.Helper()
	sdir := filepath.Join(h.user, ".claude", "sessions")
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg := `{"sessionId":"` + id + `","pid":` + strconv.Itoa(pid) + `,"cwd":"` + cwd + `","bridgeSessionId":null}`
	if err := os.WriteFile(filepath.Join(sdir, strconv.Itoa(pid)+".json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
}
