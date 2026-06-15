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

	r.reconcileOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce lists server sessions, and quits the screen of any local
// session the server has archived.
func (r *Reconciler) reconcileOnce(ctx context.Context) {
	remote, err := r.Cloud.List(ctx)
	if err != nil {
		r.Log.Warn("session_sync", "phase", "list_cloud", "error", err.Error())
		return
	}
	archived := make(map[string]bool, len(remote))
	for _, s := range remote {
		if s.IsArchivedSession() {
			archived[s.ID] = true
		}
	}
	if len(archived) == 0 {
		return
	}

	local, err := r.Manager.List()
	if err != nil {
		r.Log.Warn("session_sync", "phase", "list_local", "error", err.Error())
		return
	}
	for _, ls := range local {
		if !archived[ls.ID] {
			continue
		}
		existed, err := r.Manager.Kill(ls.ID)
		if err != nil {
			r.Log.Error("auto_archive", "id", ls.ID, "screen", ls.Screen, "error", err.Error())
			continue
		}
		if existed {
			r.Log.Info("auto_archive", "id", ls.ID, "screen", ls.Screen, "reason", "archived_on_server")
		}
	}
}
