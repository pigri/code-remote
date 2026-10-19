// Package store is a SQLite-backed mirror of code-remote sessions plus the
// archive grace clock. It uses the pure-Go modernc.org/sqlite driver, so the
// binaries still cross-compile with CGO_ENABLED=0.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
	"modernc.org/sqlite/lib"

	"claude-remote-api/internal/cloud"
	"claude-remote-api/internal/session"
)

// DefaultPath is where the SQLite mirror lives when CLAUDE_REMOTE_DB is unset:
// $XDG_DATA_HOME/code-remote/code-remote.db (or ~/.local/share/... as a
// fallback). Shared by the API server and crctl so both read the same mirror.
func DefaultPath() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "code-remote", "code-remote.db")
}

// beforeAddColumn, when set, runs between a migration's column check and its
// ALTER TABLE. Tests use it to add the column from another connection.
var beforeAddColumn func(column, def string)

// DB is a SQLite session store. It implements cloud.Store.
type DB struct{ db *sql.DB }

// Open opens (creating parent dirs + schema) the SQLite database at path.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Serializes this process's statements. The server and crctl each open the
	// database, so locks still contend across processes: busy_timeout waits.
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(fmt.Sprintf(`PRAGMA busy_timeout=%d`, busyTimeout.Milliseconds())); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	if err := switchToWAL(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	d := &DB{db: sqlDB}
	if err := d.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

const busyTimeout = 5 * time.Second

// walSwitchTimeout bounds how long switchToWAL retries. Tests shrink it.
var walSwitchTimeout = busyTimeout

// switchToWAL puts the database in WAL mode. On a database not yet in WAL
// mode, SQLite upgrades a read transaction to a write transaction to switch,
// and returns SQLITE_BUSY at once, without the busy handler, when another
// connection holds the write lock. So retry on SQLITE_BUSY until
// walSwitchTimeout has passed. One attempt can itself wait up to busyTimeout
// for another lock, so the switch can take up to about
// walSwitchTimeout+busyTimeout.
func switchToWAL(db *sql.DB) error {
	deadline := time.Now().Add(walSwitchTimeout)
	for {
		_, err := db.Exec(`PRAGMA journal_mode=WAL`)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isBusy reports whether err is SQLITE_BUSY or one of its extended codes.
func isBusy(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == sqlite3.SQLITE_BUSY
}

func (d *DB) Close() error { return d.db.Close() }

func (d *DB) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS sessions (
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
	updated_at          INTEGER
);
CREATE TABLE IF NOT EXISTS events (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	uuid   TEXT,
	kind   TEXT,
	detail TEXT,
	at     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_events_uuid ON events(uuid);`
	if _, err := d.db.Exec(schema); err != nil {
		return err
	}
	// These columns were added after the first release; CREATE TABLE IF NOT
	// EXISTS won't add them to an existing database. A profile of '' means
	// none was recorded.
	const hasColumn = `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name=?`
	for _, col := range []struct{ name, def string }{
		{"conv_id", "TEXT"},
		{"profile", "TEXT NOT NULL DEFAULT ''"},
	} {
		var n int
		if err := d.db.QueryRow(hasColumn, col.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if beforeAddColumn != nil {
				beforeAddColumn(col.name, col.def)
			}
			if _, err := d.db.Exec(`ALTER TABLE sessions ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
				// The server and crctl open the same database. If the other one
				// added the column since the check, SQLite reports a duplicate.
				if d.db.QueryRow(hasColumn, col.name).Scan(&n) != nil || n == 0 {
					return err
				}
			}
		}
	}
	return nil
}

// SetConversation notes the claude conversation id a session is currently in.
// It starts out equal to the session's uuid but drifts when claude moves the
// session onto a new conversation (e.g. /clear); resuming must follow it. A
// no-op for sessions the mirror doesn't know.
func (d *DB) SetConversation(uuid, conv string) error {
	_, err := d.db.Exec(
		`UPDATE sessions SET conv_id=? WHERE uuid=? AND COALESCE(conv_id,'') != ?`, conv, uuid, conv)
	return err
}

// Conversation returns the last conversation id noted for a session, or ""
// when none was recorded.
func (d *DB) Conversation(uuid string) (string, error) {
	var c sql.NullString
	err := d.db.QueryRow(`SELECT conv_id FROM sessions WHERE uuid=?`, uuid).Scan(&c)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return c.String, nil
}

// Cwd returns the working directory last recorded for a session, or "" when
// none is (or the session is unknown).
func (d *DB) Cwd(uuid string) (string, error) {
	var c sql.NullString
	err := d.db.QueryRow(`SELECT cwd FROM sessions WHERE uuid=?`, uuid).Scan(&c)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return c.String, nil
}

// UpsertSession records/updates the mirrored view, preserving the ledger columns
// (grace clock, archived_at, resumed_at).
func (d *DB) UpsertSession(r cloud.SessionRecord) error {
	const q = `
INSERT INTO sessions
	(uuid, screen, title, cwd, local_status, cloud_status, connection_status, bridge_session_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(uuid) DO UPDATE SET
	screen=excluded.screen,
	title=excluded.title,
	cwd=excluded.cwd,
	local_status=excluded.local_status,
	cloud_status=excluded.cloud_status,
	connection_status=excluded.connection_status,
	bridge_session_id=CASE WHEN excluded.bridge_session_id != '' THEN excluded.bridge_session_id ELSE sessions.bridge_session_id END,
	created_at=CASE WHEN excluded.created_at != '' THEN excluded.created_at ELSE sessions.created_at END,
	updated_at=excluded.updated_at;`
	_, err := d.db.Exec(q, r.UUID, r.Screen, r.Title, r.Cwd, r.LocalStatus, r.CloudStatus,
		r.ConnectionStatus, r.BridgeSessionID, r.CreatedAt, time.Now().Unix())
	return err
}

// Record marks a session as live — created or resumed. It upserts the mirror
// row and clears the archive markers, stamping resumed_at when the session had
// previously been archived. This is what makes a session discoverable as
// resumable after its screen is gone, independent of the cloud reconciler.
// profile names the profile the session runs under; "" records none and keeps
// the stored one.
func (d *DB) Record(uuid, screen, title, cwd, status, createdAt, profile string) error {
	// cloud_status/connection_status/bridge_session_id are seeded to '' so a
	// fresh row scans cleanly (AllSessions reads them as non-null strings); the
	// ON CONFLICT clause omits the statuses, preserving what the reconciler set,
	// but clears bridge_session_id: a (re)launched claude gets a new bridge, and
	// the old one, once gone server-side, would read as "deleted".
	const q = `
INSERT INTO sessions
	(uuid, screen, title, cwd, local_status, cloud_status, connection_status, bridge_session_id, created_at, updated_at, profile)
VALUES (?, ?, ?, ?, ?, '', '', '', ?, ?, ?)
ON CONFLICT(uuid) DO UPDATE SET
	screen=excluded.screen,
	profile=CASE WHEN excluded.profile != '' THEN excluded.profile ELSE sessions.profile END,
	title=CASE WHEN excluded.title != '' THEN excluded.title ELSE sessions.title END,
	cwd=CASE WHEN excluded.cwd != '' THEN excluded.cwd ELSE sessions.cwd END,
	local_status=excluded.local_status,
	bridge_session_id='',
	created_at=CASE WHEN excluded.created_at != '' THEN excluded.created_at ELSE sessions.created_at END,
	resumed_at=CASE WHEN sessions.archived_at IS NOT NULL THEN excluded.updated_at ELSE sessions.resumed_at END,
	archived_at=NULL,
	first_seen_archived=NULL,
	updated_at=excluded.updated_at;`
	now := time.Now().Unix()
	if _, err := d.db.Exec(q, uuid, screen, title, cwd, status, createdAt, now, profile); err != nil {
		return err
	}
	return d.logEvent(uuid, "record", status)
}

// StoppedSessions returns mirror rows that are not currently running — the
// resumable set. `running` is the live screen listing (their ids are excluded);
// `keep` filters candidates (e.g. "has an on-disk claude log"), skipped when nil.
// Rows come back as session.Session marked Status "Stopped".
func (d *DB) StoppedSessions(running []session.Session, keep func(id string) bool) ([]session.Session, error) {
	all, err := d.AllSessions()
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(running))
	for _, r := range running {
		live[r.ID] = true
	}
	var out []session.Session
	for _, s := range all {
		if live[s.UUID] {
			continue
		}
		if keep != nil && !keep(s.UUID) {
			continue
		}
		out = append(out, session.Session{
			ID:        s.UUID,
			Screen:    s.Screen,
			Title:     s.Title,
			Status:    "Stopped",
			CreatedAt: s.CreatedAt,
			Profile:   s.Profile,
		})
	}
	return out, nil
}

func (d *DB) FirstSeenArchived(id string) (time.Time, bool, error) {
	var sec sql.NullInt64
	err := d.db.QueryRow(`SELECT first_seen_archived FROM sessions WHERE uuid=?`, id).Scan(&sec)
	if err == sql.ErrNoRows || !sec.Valid {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(sec.Int64, 0), true, nil
}

func (d *DB) SetFirstSeenArchived(id string, t time.Time) error {
	_, err := d.db.Exec(`UPDATE sessions SET first_seen_archived=? WHERE uuid=?`, t.Unix(), id)
	return err
}

func (d *DB) ClearArchiveClock(id string) error {
	_, err := d.db.Exec(`UPDATE sessions SET first_seen_archived=NULL WHERE uuid=?`, id)
	return err
}

// MarkArchived records the auto-quit, clears the grace clock, and logs an event.
func (d *DB) MarkArchived(id string, t time.Time) error {
	if _, err := d.db.Exec(
		`UPDATE sessions SET archived_at=?, first_seen_archived=NULL WHERE uuid=?`, t.Unix(), id); err != nil {
		return err
	}
	return d.logEvent(id, "auto_archive", "")
}

// LastBridge returns the most recently recorded bridge session id for a session.
func (d *DB) LastBridge(id string) (string, error) {
	var b sql.NullString
	err := d.db.QueryRow(`SELECT bridge_session_id FROM sessions WHERE uuid=?`, id).Scan(&b)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return b.String, nil
}

func (d *DB) logEvent(uuid, kind, detail string) error {
	_, err := d.db.Exec(`INSERT INTO events (uuid, kind, detail, at) VALUES (?, ?, ?, ?)`,
		uuid, kind, detail, time.Now().Unix())
	return err
}

// Session is a row from the mirror, for read-back (e.g. crctl/API visibility).
type Session struct {
	UUID, Screen, Title, Cwd                            string
	LocalStatus, CloudStatus, ConnectionStatus, Bridge  string
	CreatedAt, Profile                                  string
	FirstSeenArchived, ArchivedAt, ResumedAt, UpdatedAt sql.NullInt64
}

// AllSessions returns every mirrored session, most-recently-updated first.
func (d *DB) AllSessions() ([]Session, error) {
	rows, err := d.db.Query(`
SELECT uuid, screen, title, cwd, local_status, cloud_status, connection_status, bridge_session_id,
       created_at, first_seen_archived, archived_at, resumed_at, updated_at, profile
FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.UUID, &s.Screen, &s.Title, &s.Cwd, &s.LocalStatus, &s.CloudStatus,
			&s.ConnectionStatus, &s.Bridge, &s.CreatedAt, &s.FirstSeenArchived, &s.ArchivedAt,
			&s.ResumedAt, &s.UpdatedAt, &s.Profile); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
