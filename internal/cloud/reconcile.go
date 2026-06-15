package cloud

import (
	"context"
	"log/slog"
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
// The server never exposes our --session-id UUID, so we map server→local two
// ways: (1) precise — the registry's bridgeSessionId equals the server id;
// (2) fallback — Title+Cwd match. The fallback only fires when EVERY server
// session sharing that Title+Cwd is archived, so a session with a live cloud
// counterpart is never killed.
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
		if s.Title == "" {
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

	for _, ls := range local {
		match := ""
		if bid := bridgeByID[ls.ID]; bid != "" && archivedBridge[bid] {
			match = "bridge_id"
		} else if ls.Title != "" && archivedTitle[titleKey{ls.Title, cwdByID[ls.ID]}] {
			match = "title_cwd"
		}
		if match == "" {
			continue
		}
		existed, err := r.Manager.Kill(ls.ID)
		if err != nil {
			r.Log.Error("auto_archive", "id", ls.ID, "screen", ls.Screen, "error", err.Error())
			continue
		}
		if existed {
			r.Log.Info("auto_archive", "id", ls.ID, "screen", ls.Screen, "title", ls.Title, "match", match, "reason", "archived_on_server")
		}
	}
}
