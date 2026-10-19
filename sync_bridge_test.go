package main

import (
	"context"
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
