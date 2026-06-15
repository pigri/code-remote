package cloud

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"claude-remote-api/internal/session"
)

// cloudAPI is the slice of the cloud client the reconciler needs (for testing).
type cloudAPI interface {
	List(ctx context.Context) ([]Session, error)
	Get(ctx context.Context, id string) (Session, bool, error)
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
	Cloud    cloudAPI
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

	// Store persists the session mirror and the archive grace clock. Optional:
	// defaults to an in-memory store (grace clock only, lost on restart).
	Store Store

	muStore  sync.Mutex
	defStore Store
}

// store returns the configured Store, or a lazily-created in-memory one.
func (r *Reconciler) store() Store {
	if r.Store != nil {
		return r.Store
	}
	r.muStore.Lock()
	defer r.muStore.Unlock()
	if r.defStore == nil {
		r.defStore = newMemStore()
	}
	return r.defStore
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

	cloudByBridge := map[string]Session{} // server id -> session
	archivedTitle := map[titleKey]bool{}  // title+cwd -> all counterparts archived
	activeTitle := map[titleKey]bool{}    // title+cwd has a non-archived counterpart
	for _, s := range remote {
		if s.ID != "" {
			cloudByBridge[s.ID] = s
		}
		if !r.MatchTitle || s.Title == "" {
			continue
		}
		k := titleKey{s.Title, s.Cwd()}
		if s.IsArchivedSession() {
			if !activeTitle[k] {
				archivedTitle[k] = true
			}
		} else {
			activeTitle[k] = true
			delete(archivedTitle, k)
		}
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

	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	nowT := now()
	st := r.store()

	for _, ls := range local {
		cwd := cwdByID[ls.ID]
		bridge := bridgeByID[ls.ID]

		// Determine the server view + whether (and how) it's a quit target.
		// quit covers both archived and deleted (404) sessions.
		var cloudStatus, connStatus, match string
		quit, confirmFailed := false, false
		if bridge != "" {
			cs, ok := cloudByBridge[bridge]
			if !ok {
				// Not on the fetched page: confirm by id to tell a genuine
				// deletion (404) from a pagination gap or archived-beyond-100.
				got, found, gerr := r.Cloud.Get(ctx, bridge)
				switch {
				case gerr != nil:
					confirmFailed = true
					r.Log.Warn("session_sync", "phase", "confirm", "id", ls.ID, "error", gerr.Error())
				case !found:
					cs, ok = Session{SessionStatus: "deleted"}, true
					quit, match = true, "deleted"
				default:
					cs, ok = got, true
				}
			}
			if ok {
				cloudStatus, connStatus = cs.SessionStatus, cs.ConnectionStatus
				if !quit && cs.IsArchivedSession() {
					quit, match = true, "bridge_id"
				}
			}
		}
		if !quit && !confirmFailed && r.MatchTitle && ls.Title != "" && archivedTitle[titleKey{ls.Title, cwd}] {
			quit, match = true, "title_cwd"
			if cloudStatus == "" {
				cloudStatus = "archived"
			}
		}

		// Mirror the joined view.
		if err := st.UpsertSession(SessionRecord{
			UUID: ls.ID, Screen: ls.Screen, Title: ls.Title, Cwd: cwd,
			LocalStatus: ls.Status, CloudStatus: cloudStatus, ConnectionStatus: connStatus,
			BridgeSessionID: bridge, CreatedAt: ls.CreatedAt, Archived: quit,
		}); err != nil {
			r.Log.Warn("session_sync", "phase", "mirror", "id", ls.ID, "error", err.Error())
		}

		// On an inconclusive confirm, do nothing (don't quit, don't reset).
		if confirmFailed {
			continue
		}
		if !quit {
			_ = st.ClearArchiveClock(ls.ID)
			continue
		}

		// Grace: quit only after observed archived/deleted for >= Grace.
		first, ok, ferr := st.FirstSeenArchived(ls.ID)
		if ferr != nil {
			r.Log.Warn("session_sync", "phase", "grace", "id", ls.ID, "error", ferr.Error())
			continue
		}
		if !ok {
			first = nowT
			_ = st.SetFirstSeenArchived(ls.ID, nowT)
		}
		if waited := nowT.Sub(first); waited < r.Grace {
			r.Log.Info("archive_pending", "id", ls.ID, "title", ls.Title, "match", match,
				"remaining", (r.Grace - waited).Round(time.Second).String())
			continue
		}

		existed, err := r.Manager.Kill(ls.ID)
		if err != nil {
			r.Log.Error("auto_archive", "id", ls.ID, "screen", ls.Screen, "error", err.Error())
			continue
		}
		_ = st.MarkArchived(ls.ID, nowT)
		if existed {
			reason := "archived_on_server"
			if match == "deleted" {
				reason = "deleted_on_server"
			}
			r.Log.Info("auto_archive", "id", ls.ID, "screen", ls.Screen, "title", ls.Title,
				"match", match, "reason", reason)
		}
	}
}
