package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"claude-remote-api/internal/cloud"
)

// cloudOf is an account whose cloud holds sessions; any other id reads as
// deleted.
type cloudOf []cloud.Session

func (c cloudOf) List(context.Context) ([]cloud.Session, error) { return c, nil }
func (c cloudOf) Get(_ context.Context, id string) (cloud.Session, bool, error) {
	for _, s := range c {
		if s.ID == id {
			return s, true, nil
		}
	}
	return cloud.Session{}, false, nil
}

// syncer is a reconciler of the work profile's view against a cloud holding
// cl, with a clock that reads *now.
func (h *profileHost) syncer(t *testing.T, cl cloud.Session, grace time.Duration, now *time.Time) (*cloud.Reconciler, *cloudOf) {
	t.Helper()
	view, err := h.mgr.ForProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	c := &cloudOf{cl}
	return &cloud.Reconciler{Cloud: c, Manager: view, Store: h.db, Grace: grace,
		Now: func() time.Time { return *now }, Log: discardLogger()}, c
}

// A resumed session is not quit over the bridge of the claude it replaced: the
// entry that claude left behind names an archived cloud session, and the new
// claude registers under the same id, first not at all, then not yet bridged.
func TestReconcilerSparesAResumedSessionOverAStaleBridge(t *testing.T) {
	for _, grace := range []time.Duration{0, 15 * time.Minute} {
		t.Run(grace.String(), func(t *testing.T) {
			h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
			fakePS(t, "  7777  4242 claude --resume "+sessX+"\\n")
			h.register(t, ".claude", "5555", sessX, 5555, "session_old")
			if err := h.db.UpsertSession(cloud.SessionRecord{UUID: sessX, Screen: "crtest-" + sessX, BridgeSessionID: "session_old"}); err != nil {
				t.Fatal(err)
			}
			h.recordProfile(t, map[string]string{sessX: "work"}) // the resume
			now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
			rec, _ := h.syncer(t, archived("session_old"), grace, &now)

			rec.ReconcileOnce(context.Background())
			if got := h.quits(); got != "" {
				t.Fatalf("first pass quit %q, before the new claude registered", got)
			}
			h.register(t, ".claude", "7777", sessX, 7777, "")
			now = now.Add(grace + time.Minute)
			rec.ReconcileOnce(context.Background())
			if got := h.quits(); got != "" {
				t.Fatalf("second pass quit %q, once the new claude registered unbridged", got)
			}
		})
	}
}

// A session whose live entry names an archived cloud session is quit.
func TestReconcilerQuitsOnTheLiveBridge(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	fakePS(t, "  7777  4242 claude\\n")
	h.register(t, ".claude", "7777", sessX, 7777, "session_live")
	h.recordProfile(t, map[string]string{sessX: "work"})
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rec, _ := h.syncer(t, archived("session_live"), 0, &now)

	rec.ReconcileOnce(context.Background())
	if got, want := h.quits(), "crtest-"+sessX+"\n"; got != want {
		t.Errorf("quit %q, want %q", got, want)
	}
}

// Once its cloud session is deleted, claude resets the live entry's bridge;
// the bridge it carried while live is still enough to quit the session.
func TestReconcilerQuitsOnTheBridgeTheLiveEntryCarried(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	fakePS(t, "  7777  4242 claude\\n")
	h.register(t, ".claude", "7777", sessX, 7777, "session_live")
	h.recordProfile(t, map[string]string{sessX: "work"})
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rec, cl := h.syncer(t, running("session_live"), 0, &now)

	rec.ReconcileOnce(context.Background())
	if got := h.quits(); got != "" {
		t.Fatalf("quit %q while its cloud session runs", got)
	}
	h.register(t, ".claude", "7777", sessX, 7777, "")
	*cl = nil
	rec.ReconcileOnce(context.Background())
	if got, want := h.quits(), "crtest-"+sessX+"\n"; got != want {
		t.Errorf("quit %q, want %q", got, want)
	}
}

// A session with no bridge, live or stored, is never quit, whatever the cloud
// holds.
func TestReconcilerNeverQuitsAnUnbridgedSession(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	fakePS(t, "  7777  4242 claude\\n")
	h.register(t, ".claude", "7777", sessX, 7777, "")
	h.recordProfile(t, map[string]string{sessX: "work"})
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rec, _ := h.syncer(t, archived("session_other"), 0, &now)

	for range 2 {
		rec.ReconcileOnce(context.Background())
		now = now.Add(time.Hour)
	}
	if got := h.quits(); got != "" {
		t.Errorf("quit %q, a session with no bridge", got)
	}
}

// psFails puts a ps on PATH that exits 1, as a failed fork or exec of it does,
// and makes only the pids in alive answer signal 0.
func (h *profileHost) psFails(t *testing.T, alive ...int) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.mgr.ProcAlive = func(pid int) bool { return slices.Contains(alive, pid) }
}

// When ps cannot be read, a dead claude's entry does not pass for live: its
// archived bridge would quit the session resumed since.
func TestReconcilerSparesAResumedSessionWhenPsFails(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	h.psFails(t)
	h.register(t, ".claude", "5555", sessX, 5555, "session_old")
	if err := h.db.UpsertSession(cloud.SessionRecord{UUID: sessX, Screen: "crtest-" + sessX, BridgeSessionID: "session_old"}); err != nil {
		t.Fatal(err)
	}
	h.recordProfile(t, map[string]string{sessX: "work"}) // the resume
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rec, _ := h.syncer(t, archived("session_old"), 0, &now)

	rec.ReconcileOnce(context.Background())
	if got := h.quits(); got != "" {
		t.Errorf("quit %q over a dead claude's bridge", got)
	}
}

// When ps cannot be read, a live claude's entry is still found by its id.
func TestReconcilerQuitsOnTheLiveBridgeWhenPsFails(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	h.psFails(t, 7777)
	h.register(t, ".claude", "5555", sessX, 5555, "")
	h.register(t, ".claude", "7777", sessX, 7777, "session_live")
	h.recordProfile(t, map[string]string{sessX: "work"})
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rec, _ := h.syncer(t, archived("session_live"), 0, &now)

	rec.ReconcileOnce(context.Background())
	if got, want := h.quits(), "crtest-"+sessX+"\n"; got != want {
		t.Errorf("quit %q, want %q", got, want)
	}
}

// A ps that runs and lists no claude decides alone: an entry whose pid it does
// not list is gone, whatever signal 0 says.
func TestReconcilerTrustsAPsThatListsNoClaude(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	fakePS(t, "  1  0 /sbin/init\\n")
	h.mgr.ProcAlive = func(int) bool { return true }
	h.register(t, ".claude", "5555", sessX, 5555, "session_old")
	h.recordProfile(t, map[string]string{sessX: "work"})
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rec, _ := h.syncer(t, archived("session_old"), 0, &now)

	rec.ReconcileOnce(context.Background())
	if got := h.quits(); got != "" {
		t.Errorf("quit %q over an entry ps does not list", got)
	}
}

// One tick with ps unreadable does not store a dead claude's bridge, which
// later healthy ticks would read back and quit the session over.
func TestReconcilerStoresNoStaleBridgeWhenPsFailsOnce(t *testing.T) {
	h := newProfileHost(t, "work=~/.claude,private=~/.claude-private", "", map[string]int{sessX: 4242})
	h.register(t, ".claude", "5555", sessX, 5555, "session_old")
	h.recordProfile(t, map[string]string{sessX: "work"}) // the resume
	grace := 15 * time.Minute
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rec, _ := h.syncer(t, archived("session_old"), grace, &now)

	h.psFails(t)
	rec.ReconcileOnce(context.Background())
	if b, _ := h.db.LastBridge(sessX); b == "session_old" {
		t.Fatalf("stored bridge %q from a dead claude's entry", b)
	}
	fakePS(t, "  7777  4242 claude --resume "+sessX+"\\n")
	h.register(t, ".claude", "7777", sessX, 7777, "")
	now = now.Add(grace + time.Minute)
	rec.ReconcileOnce(context.Background())
	if got := h.quits(); got != "" {
		t.Errorf("quit %q once ps read again", got)
	}
}
