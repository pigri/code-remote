package store

import (
	"database/sql"
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

	if err := d.Record(running, "p-"+running, "live one", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.Record(stopped, "p-"+stopped, "stopped one", "/repo", "Detached", "now", ""); err != nil {
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
	if err := d.Record(id, "p-"+id, "back", "/repo", "Detached", "now", ""); err != nil {
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

func TestConversation(t *testing.T) {
	d := openTemp(t)
	const id, conv = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"

	if got, err := d.Conversation(id); err != nil || got != "" {
		t.Fatalf("Conversation(unknown) = %q, %v, want empty", got, err)
	}
	// Unknown session: nothing to update, and no row is invented.
	if err := d.SetConversation(id, conv); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Conversation(id); got != "" {
		t.Fatalf("SetConversation created a row: %q", got)
	}

	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.SetConversation(id, conv); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Conversation(id); err != nil || got != conv {
		t.Fatalf("Conversation = %q, %v, want %q", got, err, conv)
	}
	// Re-recording the session (resume) must not forget the conversation.
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Conversation(id); got != conv {
		t.Errorf("Conversation after Record = %q, want %q", got, conv)
	}
}

// A database created before conv_id existed gains the column on Open.
func TestMigrateAddsConvID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`ALTER TABLE sessions DROP COLUMN conv_id`); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-1111-1111-1111-111111111111"
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen old db: %v", err)
	}
	defer d.Close()
	if err := d.SetConversation(id, "c"); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Conversation(id); got != "c" {
		t.Errorf("Conversation on migrated db = %q, want c", got)
	}
	if all, err := d.AllSessions(); err != nil || len(all) != 1 {
		t.Errorf("AllSessions on migrated db = %v, %v", all, err)
	}
}

func TestCwd(t *testing.T) {
	d := openTemp(t)
	const id = "11111111-1111-1111-1111-111111111111"
	if got, err := d.Cwd(id); err != nil || got != "" {
		t.Fatalf("Cwd(unknown) = %q, %v, want empty", got, err)
	}
	if err := d.Record(id, "p-"+id, "t", "/home", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	// A relaunch in another directory moves it; one with no dir keeps it.
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.Record(id, "p-"+id, "t", "", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Cwd(id); err != nil || got != "/repo" {
		t.Fatalf("Cwd = %q, %v, want /repo", got, err)
	}
}

// A session's profile survives every read path, and the reconciler's upserts
// leave it alone.
func TestProfileRoundTrip(t *testing.T) {
	d := openTemp(t)
	const id = "11111111-1111-1111-1111-111111111111"
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", "private"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "p-" + id, Title: "t"}); err != nil {
		t.Fatal(err)
	}

	all, err := d.AllSessions()
	if err != nil || len(all) != 1 || all[0].Profile != "private" {
		t.Fatalf("AllSessions = %+v, %v, want one row with profile private", all, err)
	}
	stopped, err := d.StoppedSessions(nil, nil)
	if err != nil || len(stopped) != 1 || stopped[0].Profile != "private" {
		t.Fatalf("StoppedSessions = %+v, %v, want one row with profile private", stopped, err)
	}

	// A Record that names no profile keeps the stored one.
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	if all, err := d.AllSessions(); err != nil || len(all) != 1 || all[0].Profile != "private" {
		t.Fatalf("AllSessions after Record without profile = %+v, %v, want profile private", all, err)
	}

	// A relaunch under another profile records it.
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", "work"); err != nil {
		t.Fatal(err)
	}
	if all, err := d.AllSessions(); err != nil || len(all) != 1 || all[0].Profile != "work" {
		t.Fatalf("AllSessions after relaunch = %+v, %v, want profile work", all, err)
	}
}

// A row the reconciler creates has no recorded profile until a Record names one.
func TestProfileOfReconcilerRow(t *testing.T) {
	d := openTemp(t)
	const id = "11111111-1111-1111-1111-111111111111"
	if err := d.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "p-" + id}); err != nil {
		t.Fatal(err)
	}
	if all, err := d.AllSessions(); err != nil || len(all) != 1 || all[0].Profile != "" {
		t.Fatalf("AllSessions = %+v, %v, want one row with no profile", all, err)
	}
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", "private"); err != nil {
		t.Fatal(err)
	}
	if all, err := d.AllSessions(); err != nil || len(all) != 1 || all[0].Profile != "private" {
		t.Fatalf("AllSessions after Record = %+v, %v, want profile private", all, err)
	}
}

// A database written before the profile column existed gains it on Open: its
// rows read back with no recorded profile, and a second Open changes nothing.
func TestMigrateAddsProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The schema as Open created it before the profile column.
	const oldSchema = `
CREATE TABLE sessions (
	uuid                TEXT PRIMARY KEY,
	screen              TEXT,
	title               TEXT,
	cwd                 TEXT,
	local_status        TEXT,
	cloud_status        TEXT,
	connection_status   TEXT,
	bridge_session_id   TEXT,
	created_at          TEXT,
	first_seen_archived INTEGER,
	archived_at         INTEGER,
	resumed_at          INTEGER,
	updated_at          INTEGER,
	conv_id             TEXT
);
CREATE TABLE events (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	uuid   TEXT,
	kind   TEXT,
	detail TEXT,
	at     INTEGER
);
CREATE INDEX idx_events_uuid ON events(uuid);
INSERT INTO sessions
	(uuid, screen, title, cwd, local_status, cloud_status, connection_status, bridge_session_id, created_at, updated_at, conv_id)
VALUES
	('11111111-1111-1111-1111-111111111111', 'p-1', 'one', '/repo', 'Detached', '', '', '', 'now', 2, 'c1'),
	('22222222-2222-2222-2222-222222222222', 'p-2', 'two', '/home', 'Detached', 'archived', '', 'session_x', 'then', 1, NULL);`
	if _, err := old.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	old.Close()

	for i := range 2 {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d of old db: %v", i+1, err)
		}
		all, err := d.AllSessions()
		if err != nil || len(all) != 2 {
			t.Fatalf("Open #%d: AllSessions = %+v, %v, want 2 rows", i+1, all, err)
		}
		for _, s := range all {
			if s.Profile != "" {
				t.Errorf("Open #%d: %s profile = %q, want none", i+1, s.UUID, s.Profile)
			}
		}
		if all[0].Title != "one" || all[1].Bridge != "session_x" {
			t.Errorf("Open #%d: rows not preserved: %+v", i+1, all)
		}
		stopped, err := d.StoppedSessions(nil, nil)
		if err != nil || len(stopped) != 2 || stopped[0].Profile != "" || stopped[1].Profile != "" {
			t.Errorf("Open #%d: StoppedSessions = %+v, %v, want 2 rows with no profile", i+1, stopped, err)
		}
		var n int
		if err := d.db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='profile' AND "notnull"=1 AND dflt_value=''''''`).Scan(&n); err != nil || n != 1 {
			t.Errorf("Open #%d: profile columns = %d, %v, want one NOT NULL DEFAULT ''", i+1, n, err)
		}
		d.Close()
	}
}
