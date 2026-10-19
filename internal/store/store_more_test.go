package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestProfile(t *testing.T) {
	d := openTemp(t)
	const id = "11111111-1111-1111-1111-111111111111"
	if got, err := d.Profile(id); err != nil || got != "" {
		t.Fatalf("Profile(unknown) = %q, %v, want empty", got, err)
	}
	if err := d.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "p-" + id}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Profile(id); err != nil || got != "" {
		t.Fatalf("Profile(reconciler row) = %q, %v, want empty", got, err)
	}
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", "private"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Profile(id); err != nil || got != "private" {
		t.Fatalf("Profile = %q, %v, want private", got, err)
	}
}

// SetProfile changes only the profile: the row's bridge, status and archive
// state stay as they were.
func TestSetProfile(t *testing.T) {
	d := openTemp(t)
	const id = "11111111-1111-1111-1111-111111111111"
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", "work"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "p-" + id, Title: "t", LocalStatus: "Detached", BridgeSessionID: "session_b", LaunchGen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetProfile(id, "private"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Profile(id); err != nil || got != "private" {
		t.Errorf("Profile = %q, %v, want private", got, err)
	}
	if b, err := d.LastBridge(id); err != nil || b != "session_b" {
		t.Errorf("LastBridge = %q, %v, want session_b kept", b, err)
	}
	if err := d.SetProfile("22222222-2222-2222-2222-222222222222", "private"); err != nil {
		t.Errorf("SetProfile(unknown) = %v, want a no-op", err)
	}
	if all, _ := d.AllSessions(); len(all) != 1 {
		t.Errorf("rows = %d after SetProfile of an unknown session, want 1", len(all))
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
	if err := d.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "p-" + id, Title: "t", BridgeSessionID: "session_p", LaunchGen: 1}); err != nil {
		t.Fatal(err)
	}
	if b, err := d.LastBridge(id); err != nil || b != "session_p" {
		t.Fatalf("LastBridge after the upsert = %q, %v, want session_p", b, err)
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

// oldSchemaDB returns the path of a database created before column existed.
func oldSchemaDB(t *testing.T, column string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`ALTER TABLE sessions DROP COLUMN ` + column); err != nil {
		t.Fatal(err)
	}
	d.Close()
	return path
}

// Another process (the server or crctl) can add a column between Open's check
// and its ALTER; Open must still succeed.
func TestMigrateColumnAddedConcurrently(t *testing.T) {
	for _, column := range []string{"conv_id", "profile", "launch_gen"} {
		t.Run(column, func(t *testing.T) {
			path := oldSchemaDB(t, column)
			added := false
			beforeAddColumn = func(col, def string) {
				if col != column {
					t.Errorf("hook called for %s, want only %s", col, column)
					return
				}
				other, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				if _, err := other.Exec(`ALTER TABLE sessions ADD COLUMN ` + col + ` ` + def); err != nil {
					t.Fatal(err)
				}
				added = true
			}
			t.Cleanup(func() { beforeAddColumn = nil })

			d, err := Open(path)
			if err != nil {
				t.Fatalf("Open after a concurrent migration: %v", err)
			}
			defer d.Close()
			if !added {
				t.Fatal("hook never ran")
			}
			const id = "11111111-1111-1111-1111-111111111111"
			if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", "private"); err != nil {
				t.Fatal(err)
			}
			if err := d.SetConversation(id, "c"); err != nil {
				t.Fatal(err)
			}
			if got, _ := d.Conversation(id); got != "c" {
				t.Errorf("Conversation = %q, want c", got)
			}
			if all, err := d.AllSessions(); err != nil || len(all) != 1 || all[0].Profile != "private" {
				t.Errorf("AllSessions = %+v, %v, want one session with profile private", all, err)
			}
		})
	}
}

// An ALTER that fails for any other reason still fails Open.
func TestMigrateAlterErrorWithoutColumn(t *testing.T) {
	path := oldSchemaDB(t, "conv_id")
	beforeAddColumn = func(col, _ string) {
		if col != "conv_id" {
			return
		}
		other, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		if _, err := other.Exec(`DROP TABLE sessions`); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeAddColumn = nil })

	if d, err := Open(path); err == nil {
		d.Close()
		t.Fatal("Open succeeded although the ALTER failed and conv_id is missing")
	}
}

// The server and crctl open the same database. Opens that overlap must wait
// for each other's locks rather than fail with "database is locked". Each
// round starts with no connection open, as after a restart, so the opens
// race again.
func TestConcurrentOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-remote.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	const rounds, n = 30, 8
	for round := range rounds {
		start := make(chan struct{})
		errs := make(chan error, n)
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				db, err := Open(path)
				if err == nil {
					err = db.Close()
				}
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent Open: %v", round, err)
			}
		}
	}
}

// openWhileLocked runs lock on another connection to path, then Open in the
// background. The lock is released after 200 ms; Open must wait for it and
// then succeed.
func openWhileLocked(t *testing.T, path string, lock ...string) {
	t.Helper()
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1) // the lock and its COMMIT share one connection
	// The COMMIT waits out the shared lock Open's retries take.
	for _, q := range append([]string{`PRAGMA busy_timeout=5000`}, lock...) {
		if _, err := other.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	done := make(chan error, 1)
	go func() {
		db, err := Open(path)
		if err == nil {
			err = db.Close()
		}
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Open returned while another connection held %q: %v", lock, err)
	default:
	}
	if _, err := other.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Open while another connection held %q: %v", lock, err)
	}
}

// rollbackLock leaves a database in rollback-journal mode, as an older
// release left it, and takes a write lock on it with begin.
func rollbackLock(begin string) []string {
	return []string{`PRAGMA journal_mode=DELETE`, `CREATE TABLE t (x)`, begin, `INSERT INTO t VALUES (1)`}
}

// Every statement of Open, the WAL switch included, waits for an exclusive
// lock held by another process.
func TestOpenWaitsForExclusiveLock(t *testing.T) {
	openWhileLocked(t, filepath.Join(t.TempDir(), "code-remote.db"), rollbackLock(`BEGIN EXCLUSIVE`)...)
}

// The WAL switch upgrades a read transaction to a write transaction, and
// SQLite does not call the busy handler for that upgrade. Open must still wait
// for a reserved lock held by another process.
func TestOpenWaitsForReservedLock(t *testing.T) {
	openWhileLocked(t, filepath.Join(t.TempDir(), "code-remote.db"), rollbackLock(`BEGIN IMMEDIATE`)...)
}

// On a database already in WAL mode, the migration waits through busy_timeout
// for another process's write transaction.
func TestOpenWaitsForWALWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-remote.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`ALTER TABLE sessions DROP COLUMN conv_id`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	openWhileLocked(t, path, `BEGIN IMMEDIATE`, `INSERT INTO events (kind) VALUES ('lock')`)
}

type codedErr int

func (c codedErr) Error() string { return fmt.Sprintf("sqlite code %d", int(c)) }
func (c codedErr) Code() int     { return int(c) }

func TestIsBusy(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{codedErr(5), true},   // SQLITE_BUSY
		{codedErr(261), true}, // SQLITE_BUSY_RECOVERY
		{codedErr(517), true}, // SQLITE_BUSY_SNAPSHOT
		{fmt.Errorf("set pragmas: %w", codedErr(5)), true},
		{codedErr(6), false},  // SQLITE_LOCKED
		{codedErr(26), false}, // SQLITE_NOTADB
		{errors.New("database is locked"), false},
		{nil, false},
	} {
		if got := isBusy(tc.err); got != tc.want {
			t.Errorf("isBusy(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Only SQLITE_BUSY is retried: Open of a file that is not a database fails at
// once.
func TestOpenNotADatabaseFailsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code-remote.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("not a database\n", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d, err := Open(path)
	if err == nil {
		d.Close()
		t.Fatal("Open of a file that is not a database succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Open took %v to fail (%v), want well under the %v busy timeout", elapsed, err, busyTimeout)
	}
}

// With another connection's write lock never released, the WAL switch gives
// up after walSwitchTimeout and Open returns SQLITE_BUSY.
func TestOpenGivesUpAfterWALSwitchTimeout(t *testing.T) {
	walSwitchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { walSwitchTimeout = busyTimeout })
	path := filepath.Join(t.TempDir(), "code-remote.db")
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	for _, q := range append([]string{`PRAGMA busy_timeout=5000`}, rollbackLock(`BEGIN IMMEDIATE`)...) {
		if _, err := other.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	limit := walSwitchTimeout + time.Second
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		db, err := Open(path)
		if err == nil {
			err = db.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !isBusy(err) {
			t.Errorf("Open with the lock held = %v, want SQLITE_BUSY", err)
		}
		if elapsed := time.Since(start); elapsed > limit {
			t.Errorf("Open gave up after %v, want under %v", elapsed, limit)
		}
	case <-time.After(limit):
		other.Exec(`COMMIT`)
		<-done
		t.Fatalf("Open still blocked after %v with walSwitchTimeout %v", limit, walSwitchTimeout)
	}
}

// A pass of the reconciler that read a session before its last launch writes
// neither its view nor its archive clock; one that read it since writes both.
func TestLaunchDropsAnEarlierPassWrites(t *testing.T) {
	d := openTemp(t)
	const id = "11111111-1111-1111-1111-111111111111"
	gen := func() int64 {
		t.Helper()
		gens, err := d.LaunchGens()
		if err != nil {
			t.Fatal(err)
		}
		return gens[id]
	}
	if g := gen(); g != 0 {
		t.Fatalf("generation of an unknown session = %d, want 0", g)
	}
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	read := gen()
	if read != 1 {
		t.Fatalf("generation after the first launch = %d, want 1", read)
	}
	if err := d.Record(id, "p-"+id, "t", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	if g := gen(); g != 2 {
		t.Fatalf("generation after a relaunch = %d, want 2", g)
	}

	at := time.Unix(1000, 0)
	if err := d.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "p-" + id, BridgeSessionID: "session_old", LaunchGen: read}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetFirstSeenArchived(id, at, read); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.LastBridge(id); b != "" {
		t.Errorf("LastBridge = %q from a pass before the relaunch, want none", b)
	}
	if _, ok, _ := d.FirstSeenArchived(id); ok {
		t.Error("archive clock started by a pass before the relaunch")
	}

	if err := d.UpsertSession(cloud.SessionRecord{UUID: id, Screen: "p-" + id, BridgeSessionID: "session_new", LaunchGen: 2}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetFirstSeenArchived(id, at, 2); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.LastBridge(id); b != "session_new" {
		t.Errorf("LastBridge = %q, want session_new", b)
	}
	if first, ok, _ := d.FirstSeenArchived(id); !ok || !first.Equal(at) {
		t.Errorf("FirstSeenArchived = %v, %v, want %v", first, ok, at)
	}

	// A pass that read a session the store did not know yet is at generation
	// 0; the first launch recorded after that read drops its writes too.
	const fresh = "22222222-2222-2222-2222-222222222222"
	if err := d.Record(fresh, "p-"+fresh, "t", "/repo", "Detached", "now", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertSession(cloud.SessionRecord{UUID: fresh, Screen: "p-" + fresh, BridgeSessionID: "session_old"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetFirstSeenArchived(fresh, at, 0); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.LastBridge(fresh); b != "" {
		t.Errorf("LastBridge = %q from a pass before the first launch, want none", b)
	}
	if _, ok, _ := d.FirstSeenArchived(fresh); ok {
		t.Error("archive clock started by a pass before the first launch")
	}
}

// A database written before the launch_gen column existed gains it on Open:
// its rows are at generation 0, the reconciler still writes them, and the
// next launch moves a session to generation 1.
func TestMigrateAddsLaunchGen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The schema as Open created it before the launch_gen column.
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
	conv_id             TEXT,
	profile             TEXT NOT NULL DEFAULT ''
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
	(uuid, screen, title, cwd, local_status, cloud_status, connection_status, bridge_session_id, created_at, updated_at, conv_id, profile)
VALUES
	('11111111-1111-1111-1111-111111111111', 'p-1', 'one', '/repo', 'Detached', '', '', 'session_a', 'now', 2, 'c1', 'work'),
	('22222222-2222-2222-2222-222222222222', 'p-2', 'two', '/home', 'Detached', 'archived', '', 'session_x', 'then', 1, NULL, '');`
	if _, err := old.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	old.Close()

	const one, two = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	for i := range 2 {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d of old db: %v", i+1, err)
		}
		gens, err := d.LaunchGens()
		if err != nil || len(gens) != 2 || gens[one] != 0 || gens[two] != 0 {
			t.Fatalf("Open #%d: LaunchGens = %v, %v, want both rows at 0", i+1, gens, err)
		}
		if b, _ := d.LastBridge(one); b != "session_a" {
			t.Errorf("Open #%d: LastBridge = %q, want session_a kept", i+1, b)
		}
		var n int
		if err := d.db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='launch_gen' AND "notnull"=1 AND dflt_value='0'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("Open #%d: launch_gen columns = %d, %v, want one NOT NULL DEFAULT 0", i+1, n, err)
		}
		d.Close()
	}

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.UpsertSession(cloud.SessionRecord{UUID: two, Screen: "p-2", BridgeSessionID: "session_y"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.LastBridge(two); b != "session_y" {
		t.Errorf("LastBridge = %q after an upsert at generation 0, want session_y", b)
	}
	if err := d.Record(one, "p-1", "", "", "Detached", "", ""); err != nil {
		t.Fatal(err)
	}
	if gens, _ := d.LaunchGens(); gens[one] != 1 {
		t.Errorf("generation after a launch = %d, want 1", gens[one])
	}
}
