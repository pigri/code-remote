package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-remote-api/internal/cloud"
	"claude-remote-api/internal/session"
)

func TestDefaultPath(t *testing.T) {
	t.Run("respects XDG_DATA_HOME", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/xdg")
		if got, want := DefaultPath(), "/xdg/code-remote/code-remote.db"; got != want {
			t.Errorf("DefaultPath = %q, want %q", got, want)
		}
	})
	t.Run("falls back to ~/.local/share", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		if got, want := DefaultPath(), filepath.Join(".local", "share", "code-remote", "code-remote.db"); !strings.HasSuffix(got, want) {
			t.Errorf("DefaultPath = %q, want suffix %q", got, want)
		}
	})
}

func TestRecordAndStoppedSessions(t *testing.T) {
	d := openTemp(t)
	const running, stopped = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"

	if err := d.Record(running, "p-"+running, "live one", "/repo", "Detached", "now"); err != nil {
		t.Fatal(err)
	}
	if err := d.Record(stopped, "p-"+stopped, "stopped one", "/repo", "Detached", "now"); err != nil {
		t.Fatal(err)
	}

	// Only `stopped` is absent from the running set -> it's the resumable one.
	live := []session.Session{{ID: running}}
	got, err := d.StoppedSessions(live, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != stopped || got[0].Status != "Stopped" {
		t.Fatalf("StoppedSessions = %+v, want just %s marked Stopped", got, stopped)
	}
	if got[0].Screen != "p-"+stopped || got[0].Title != "stopped one" {
		t.Errorf("StoppedSessions row not populated from mirror: %+v", got[0])
	}

	// keep predicate filters candidates out.
	none, err := d.StoppedSessions(live, func(string) bool { return false })
	if err != nil || len(none) != 0 {
		t.Errorf("StoppedSessions(keep=false) = %+v, %v, want empty", none, err)
	}
}

func TestRecordClearsArchiveAndStampsResumed(t *testing.T) {
	d := openTemp(t)
	const id = "33333333-3333-3333-3333-333333333333"

	// Reconciler archives the session (mirror row + archived_at set).
	if err := d.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "p-" + id, Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkArchived(id, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}

	// Resuming records it live: archived_at cleared, resumed_at stamped.
	if err := d.Record(id, "p-"+id, "back", "/repo", "Detached", "now"); err != nil {
		t.Fatal(err)
	}
	rows, err := d.AllSessions()
	if err != nil || len(rows) != 1 {
		t.Fatalf("AllSessions = %d, err=%v", len(rows), err)
	}
	if rows[0].ArchivedAt.Valid {
		t.Error("archived_at should be cleared after Record")
	}
	if !rows[0].ResumedAt.Valid {
		t.Error("resumed_at should be stamped when re-recording an archived session")
	}
	if rows[0].LocalStatus != "Detached" {
		t.Errorf("local_status = %q, want Detached", rows[0].LocalStatus)
	}
}

func TestOpenParentIsFile(t *testing.T) {
	// A regular file where a directory is expected makes MkdirAll fail.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(f, "sub", "code-remote.db")); err == nil {
		t.Fatal("Open under a file path = nil error, want error")
	}
}

func TestQueriesForAbsentSession(t *testing.T) {
	d := openTemp(t)

	if b, err := d.LastBridge("nope"); err != nil || b != "" {
		t.Errorf("LastBridge(absent) = %q, %v, want empty/nil", b, err)
	}
	if _, ok, err := d.FirstSeenArchived("nope"); err != nil || ok {
		t.Errorf("FirstSeenArchived(absent) ok=%v err=%v, want false/nil", ok, err)
	}
	if rows, err := d.AllSessions(); err != nil || len(rows) != 0 {
		t.Errorf("AllSessions(empty) = %d rows, %v, want 0/nil", len(rows), err)
	}
}
