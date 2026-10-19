package cloud

import (
	"sync"
	"time"
)

// SessionRecord is the mirrored view of one code-remote session, combining local
// (screen) and server (cloud) facts. Persisted by a Store for visibility and to
// survive restarts.
type SessionRecord struct {
	UUID             string // claude session id (our screen suffix)
	Screen           string
	Title            string
	Cwd              string
	LocalStatus      string // Detached | Attached (from screen)
	CloudStatus      string // session_status: archived | idle | running | ...
	ConnectionStatus string // connected | disconnected
	BridgeSessionID  string // server session id, when known
	CreatedAt        string
	Archived         bool
	// LaunchGen is the session's launch generation (see Store.LaunchGens)
	// when the facts above were read. The upsert is dropped if the session
	// has been launched since.
	LaunchGen int64
}

// Store persists the session mirror and the archive grace clock. The reconciler
// always has one (an in-memory store by default); a SQLite-backed implementation
// adds durability and cross-process visibility.
type Store interface {
	// UpsertSession records/updates the mirrored view of a session.
	UpsertSession(rec SessionRecord) error
	// FirstSeenArchived returns when a session was first observed archived.
	FirstSeenArchived(id string) (t time.Time, ok bool, err error)
	// SetFirstSeenArchived starts the grace clock for a session, unless it
	// has been launched since generation gen.
	SetFirstSeenArchived(id string, t time.Time, gen int64) error
	// ClearArchiveClock resets the grace clock (session no longer archived).
	ClearArchiveClock(id string) error
	// MarkArchived records that the session's screen was quit, clearing the clock.
	MarkArchived(id string, t time.Time) error
	// LastBridge returns the most recently recorded bridge session id for a
	// local session, or "" if unknown. Used to confirm deletion after the live
	// registry has reset bridgeSessionId to null.
	LastBridge(id string) (string, error)
	// LaunchGens returns each known session's launch generation, which every
	// launch (create, resume, restart) increments. A session missing from the
	// map is at generation 0.
	LaunchGens() (map[string]int64, error)
}

// memStore is the default in-process Store: it tracks the grace clock and drops
// the mirror (UpsertSession is a no-op). Used when no durable store is wired.
type memStore struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newMemStore() *memStore { return &memStore{seen: map[string]time.Time{}} }

func (m *memStore) UpsertSession(SessionRecord) error { return nil }

func (m *memStore) FirstSeenArchived(id string) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.seen[id]
	return t, ok, nil
}

func (m *memStore) SetFirstSeenArchived(id string, t time.Time, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen[id] = t
	return nil
}

func (m *memStore) ClearArchiveClock(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.seen, id)
	return nil
}

func (m *memStore) MarkArchived(id string, _ time.Time) error { return m.ClearArchiveClock(id) }

func (m *memStore) LastBridge(string) (string, error) { return "", nil }

func (m *memStore) LaunchGens() (map[string]int64, error) { return nil, nil }
