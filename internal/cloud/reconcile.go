package cloud

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"claude-remote-api/internal/session"
)

// lister is the slice of the cloud client the reconciler needs (for testing).
type lister interface {
	List(ctx context.Context) ([]Session, error)
}

// screenManager is the slice of session.Manager the reconciler needs.
type screenManager interface {
	List() ([]session.Session, error)
	Kill(id string) (bool, error)
	Registrations() ([]session.Registration, error)
}

// Reconciler periodically reconciles server-side session state with local
// screens: any session the server reports as archived has its screen quit. It
// only ever acts on sessions the manager already owns (prefix-scoped), so it
// cannot touch unrelated screens.
type Reconciler struct {
	Cloud    lister
	Manager  screenManager
	Interval time.Duration
	Log      *slog.Logger

	// MatchTitle enables the title+cwd fallback for sessions that lack a
	// bridgeSessionId in the local registry (never-bridged empties). It is
	// off by default because titles are user-mutable (a rename breaks the
	// join) and not unique; bridgeSessionId is the stable, rename-proof key.
	MatchTitle bool

	// Grace is how long a session must be *continuously observed* archived
	// before its screen is quit. The API exposes no archive timestamp, so we
	// clock it locally: the timer starts when we first see a session archived
	// and resets if it's unarchived. Zero means quit on first observation.
	Grace time.Duration

	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // local session id -> first observed archived
}

// Run reconciles once immediately, then on every tick until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	r.ReconcileOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ReconcileOnce(ctx)
		}
	}
}

type titleKey struct{ title, cwd string }

// ReconcileOnce lists server sessions and quits the screen of any local session
// the server has archived.
//
// The server never exposes our --session-id UUID, so we map server→local by the
// registry's bridgeSessionId == server id (stable, rename-proof). When
// MatchTitle is set, sessions lacking a bridgeSessionId fall back to a Title+Cwd
// match that only fires when EVERY server session sharing that Title+Cwd is
// archived, so a session with a live cloud counterpart is never killed.
func (r *Reconciler) ReconcileOnce(ctx context.Context) {
	remote, err := r.Cloud.List(ctx)
	if err != nil {
		r.Log.Warn("session_sync", "phase", "list_cloud", "error", err.Error())
		return
	}

	archivedBridge := map[string]bool{}  // server id -> archived
	archivedTitle := map[titleKey]bool{} // title+cwd -> all archived
	activeTitle := map[titleKey]bool{}   // title+cwd has a non-archived counterpart
	for _, s := range remote {
		if s.ID != "" && s.IsArchivedSession() {
			archivedBridge[s.ID] = true
		}
		if !r.MatchTitle || s.Title == "" {
			continue
		}
		k := titleKey{s.Title, s.Cwd()}
		if s.IsArchivedSession() {
			if _, seen := activeTitle[k]; !seen {
				archivedTitle[k] = true
			}
		} else {
			activeTitle[k] = true
			delete(archivedTitle, k)
		}
	}
	if len(archivedBridge) == 0 && len(archivedTitle) == 0 {
		// Nothing archived -> every grace clock resets.
		r.mu.Lock()
		r.seen = nil
		r.mu.Unlock()
		return
	}

	regs, err := r.Manager.Registrations()
	if err != nil {
		r.Log.Warn("session_sync", "phase", "registrations", "error", err.Error())
	}
	cwdByID := make(map[string]string, len(regs))
	bridgeByID := make(map[string]string, len(regs))
	for _, reg := range regs {
		cwdByID[reg.SessionID] = reg.Cwd
		bridgeByID[reg.SessionID] = reg.BridgeSessionID
	}

	local, err := r.Manager.List()
	if err != nil {
		r.Log.Warn("session_sync", "phase", "list_local", "error", err.Error())
		return
	}

	// Collect owned screens the server considers archived.
	type candidate struct {
		ls    session.Session
		match string
	}
	var candidates []candidate
	for _, ls := range local {
		switch {
		case bridgeByID[ls.ID] != "" && archivedBridge[bridgeByID[ls.ID]]:
			candidates = append(candidates, candidate{ls, "bridge_id"})
		case r.MatchTitle && ls.Title != "" && archivedTitle[titleKey{ls.Title, cwdByID[ls.ID]}]:
			candidates = append(candidates, candidate{ls, "title_cwd"})
		}
	}

	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	nowT := now()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]time.Time{}
	}

	current := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		current[c.ls.ID] = true
		first, ok := r.seen[c.ls.ID]
		if !ok {
			r.seen[c.ls.ID] = nowT
			first = nowT
		}
		if waited := nowT.Sub(first); waited < r.Grace {
			r.Log.Info("archive_pending", "id", c.ls.ID, "title", c.ls.Title,
				"remaining", (r.Grace - waited).Round(time.Second).String())
			continue
		}
		existed, err := r.Manager.Kill(c.ls.ID)
		if err != nil {
			r.Log.Error("auto_archive", "id", c.ls.ID, "screen", c.ls.Screen, "error", err.Error())
			continue
		}
		delete(r.seen, c.ls.ID)
		if existed {
			r.Log.Info("auto_archive", "id", c.ls.ID, "screen", c.ls.Screen, "title", c.ls.Title,
				"match", c.match, "reason", "archived_on_server")
		}
	}

	// Reset the grace clock for sessions no longer archived (or gone).
	for id := range r.seen {
		if !current[id] {
			delete(r.seen, id)
		}
	}
}
