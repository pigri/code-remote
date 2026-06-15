// Package store is a SQLite-backed mirror of code-remote sessions plus the
// archive grace clock. It uses the pure-Go modernc.org/sqlite driver, so the
// binaries still cross-compile with CGO_ENABLED=0.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"claude-remote-api/internal/cloud"
)

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
	sqlDB.SetMaxOpenConns(1) // serialize writers; avoids SQLITE_BUSY
	if _, err := sqlDB.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
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
	_, err := d.db.Exec(schema)
	return err
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

func (d *DB) logEvent(uuid, kind, detail string) error {
	_, err := d.db.Exec(`INSERT INTO events (uuid, kind, detail, at) VALUES (?, ?, ?, ?)`,
		uuid, kind, detail, time.Now().Unix())
	return err
}

// Session is a row from the mirror, for read-back (e.g. crctl/API visibility).
type Session struct {
	UUID, Screen, Title, Cwd                            string
	LocalStatus, CloudStatus, ConnectionStatus, Bridge  string
	CreatedAt                                           string
	FirstSeenArchived, ArchivedAt, ResumedAt, UpdatedAt sql.NullInt64
}

// AllSessions returns every mirrored session, most-recently-updated first.
func (d *DB) AllSessions() ([]Session, error) {
	rows, err := d.db.Query(`
SELECT uuid, screen, title, cwd, local_status, cloud_status, connection_status, bridge_session_id,
       created_at, first_seen_archived, archived_at, resumed_at, updated_at
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
			&s.ResumedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
