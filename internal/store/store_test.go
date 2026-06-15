package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"claude-remote-api/internal/cloud"
	"claude-remote-api/internal/session"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "code-remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestStoreUpsertAndGraceClock(t *testing.T) {
	d := openTemp(t)

	rec := cloud.SessionRecord{UUID: "u1", Screen: "p-u1", Title: "test", Cwd: "/repo",
		LocalStatus: "Detached", CloudStatus: "archived", ConnectionStatus: "connected",
		BridgeSessionID: "session_X", CreatedAt: "now", Archived: true}
	if err := d.UpsertSession(rec); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := d.FirstSeenArchived("u1"); ok {
		t.Fatal("clock should be unset initially")
	}
	now := time.Unix(1_000_000, 0)
	if err := d.SetFirstSeenArchived("u1", now); err != nil {
		t.Fatal(err)
	}
	got, ok, err := d.FirstSeenArchived("u1")
	if err != nil || !ok || !got.Equal(now) {
		t.Fatalf("FirstSeenArchived = %v ok=%v err=%v, want %v", got, ok, err, now)
	}

	if err := d.ClearArchiveClock("u1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.FirstSeenArchived("u1"); ok {
		t.Fatal("clock should be cleared")
	}

	// Upsert preserves the (now-empty) ledger and the bridge id when blank.
	rec.BridgeSessionID = ""
	if err := d.UpsertSession(rec); err != nil {
		t.Fatal(err)
	}
	sessions, err := d.AllSessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("AllSessions = %d rows, err=%v", len(sessions), err)
	}
	if sessions[0].Bridge != "session_X" {
		t.Errorf("bridge id should be preserved on blank upsert, got %q", sessions[0].Bridge)
	}

	if err := d.MarkArchived("u1", now); err != nil {
		t.Fatal(err)
	}
	sessions, _ = d.AllSessions()
	if !sessions[0].ArchivedAt.Valid {
		t.Error("archived_at should be set after MarkArchived")
	}
}

// --- integration: reconciler backed by the real SQLite store ---

type fakeCloud struct{ sessions []cloud.Session }

func (f *fakeCloud) List(context.Context) ([]cloud.Session, error) { return f.sessions, nil }

type fakeManager struct {
	sessions []session.Session
	regs     []session.Registration
	killed   []string
}

func (f *fakeManager) List() ([]session.Session, error)               { return f.sessions, nil }
func (f *fakeManager) Registrations() ([]session.Registration, error) { return f.regs, nil }
func (f *fakeManager) Kill(id string) (bool, error) {
	f.killed = append(f.killed, id)
	return true, nil
}

func TestReconcilerWithSQLiteStorePersistsGrace(t *testing.T) {
	d := openTemp(t)
	cl := &fakeCloud{sessions: []cloud.Session{{ID: "session_X", SessionStatus: "archived", Title: "t"}}}
	mgr := &fakeManager{
		sessions: []session.Session{{ID: "u1", Screen: "p-u1", Title: "t"}},
		regs:     []session.Registration{{SessionID: "u1", Cwd: "/repo", BridgeSessionID: "session_X"}},
	}
	clk := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &cloud.Reconciler{Cloud: cl, Manager: mgr, Log: logger, Grace: 15 * time.Minute,
		Now: func() time.Time { return clk }, Store: d}

	r.ReconcileOnce(context.Background()) // start clock; mirror written
	if len(mgr.killed) != 0 {
		t.Fatalf("killed within grace: %v", mgr.killed)
	}
	if _, ok, _ := d.FirstSeenArchived("u1"); !ok {
		t.Fatal("grace clock should be persisted in SQLite")
	}
	if rows, _ := d.AllSessions(); len(rows) != 1 || rows[0].CloudStatus != "archived" {
		t.Fatalf("mirror not written correctly: %+v", rows)
	}

	clk = clk.Add(16 * time.Minute) // past grace
	r.ReconcileOnce(context.Background())
	if len(mgr.killed) != 1 || mgr.killed[0] != "u1" {
		t.Fatalf("killed = %v, want [u1] after grace", mgr.killed)
	}
	if rows, _ := d.AllSessions(); !rows[0].ArchivedAt.Valid {
		t.Error("archived_at should be recorded after auto-quit")
	}
}
