package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// upgradeHome builds a claude home with a session log (so the session is
// resumable) and, when version/status are given, a process-registry entry.
func upgradeHome(t *testing.T, home, id, version, status string) {
	t.Helper()
	proj := filepath.Join(home, "projects", "repo")
	sdir := filepath.Join(home, "sessions")
	for _, d := range []string{proj, sdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := `{"type":"user","cwd":"` + t.TempDir() + `","sessionId":"` + id + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if version == "" {
		return
	}
	reg := `{"sessionId":"` + id + `","version":"` + version + `","status":"` + status + `"}`
	if err := os.WriteFile(filepath.Join(sdir, id+".json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fastStop(t *testing.T) {
	t.Helper()
	origT, origP := stopTimeout, stopPoll
	stopTimeout, stopPoll = 200*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { stopTimeout, stopPoll = origT, origP })
}

func TestVersion(t *testing.T) {
	fs := &fakeScreen{version: "2.1.287"}
	withFakeScreen(t, fs)
	m := &Manager{ClaudeBin: "claude"}
	if got := m.Version(); got != "2.1.287" {
		t.Errorf("Version = %q, want 2.1.287", got)
	}
	fs.version = "" // --version fails
	if got := m.Version(); got != "" {
		t.Errorf("Version(failing) = %q, want empty", got)
	}

	// A claude that never answers is cut off rather than hanging the caller.
	orig, origExec := versionTimeout, execCommand
	versionTimeout = 50 * time.Millisecond
	execCommand = func(string, ...string) *exec.Cmd { return exec.Command("sleep", "30") }
	t.Cleanup(func() { versionTimeout, execCommand = orig, origExec })
	start := time.Now()
	if got := m.Version(); got != "" || time.Since(start) > 5*time.Second {
		t.Errorf("Version(hanging) = %q after %v, want empty promptly", got, time.Since(start))
	}
}

func TestRestart(t *testing.T) {
	const id = "6fd0b321-a454-4b40-9aed-131afe120d36"
	fastStop(t)

	t.Run("running session is killed then resumed under the same id", func(t *testing.T) {
		home := t.TempDir()
		upgradeHome(t, home, id, "", "")
		fs := &fakeScreen{created: []string{"test-rc-" + id}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home}

		s, err := m.Restart(id)
		if err != nil || s.ID != id {
			t.Fatalf("Restart = %+v, %v", s, err)
		}
		if len(fs.killed) != 1 || len(fs.launches) != 1 {
			t.Fatalf("killed=%v launches=%v, want one of each", fs.killed, fs.launches)
		}
		if want := "--resume " + id + " --remote-control " + id; !strings.Contains(fs.launches[0], want) {
			t.Errorf("relaunch argv %q missing %q", fs.launches[0], want)
		}
	})

	t.Run("resumes the live conversation when claude switched ids", func(t *testing.T) {
		const liveID = "c0ffee00-1111-4222-8333-444455556666"
		home := t.TempDir()
		upgradeHome(t, home, id, "", "")     // stale log of the id it was launched with
		upgradeHome(t, home, liveID, "", "") // log of the conversation it's in now
		// The fake lists the screen as pid 10000; its claude child (pid 4242) is
		// registered under the NEW conversation id.
		reg := `{"sessionId":"` + liveID + `","pid":4242,"version":"2.1.278","status":"idle"}`
		if err := os.WriteFile(filepath.Join(home, "sessions", "4242.json"), []byte(reg), 0o644); err != nil {
			t.Fatal(err)
		}
		fs := &fakeScreen{created: []string{"test-rc-" + id}, ps: "4242 10000\n"}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home}

		s, err := m.Restart(id)
		if err != nil || s.ID != id {
			t.Fatalf("Restart = %+v, %v", s, err)
		}
		// Same screen + Remote Control name, but the live conversation.
		want := "test-rc-" + id + " claude --resume " + liveID + " --remote-control " + id
		if len(fs.launches) != 1 || !strings.Contains(fs.launches[0], want) {
			t.Errorf("relaunch argv %v, want it to contain %q", fs.launches, want)
		}
	})

	t.Run("stopped session is just resumed", func(t *testing.T) {
		home := t.TempDir()
		upgradeHome(t, home, id, "", "")
		fs := &fakeScreen{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home}
		if _, err := m.Restart(id); err != nil || len(fs.killed) != 0 || len(fs.launches) != 1 {
			t.Fatalf("Restart(stopped) err=%v killed=%v launches=%v", err, fs.killed, fs.launches)
		}
	})

	t.Run("no log: refuses without killing", func(t *testing.T) {
		fs := &fakeScreen{created: []string{"test-rc-" + id}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: t.TempDir()}
		if _, err := m.Restart(id); !errors.Is(err, ErrNotResumable) {
			t.Fatalf("Restart(no log) err = %v, want ErrNotResumable", err)
		}
		if len(fs.killed) != 0 || len(fs.created) != 1 {
			t.Errorf("session was touched: killed=%v created=%v", fs.killed, fs.created)
		}
	})

	t.Run("kill fails", func(t *testing.T) {
		fs := &fakeScreen{created: []string{"test-rc-" + id}, failKill: true}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen"}
		if _, err := m.Restart(id); err == nil || len(fs.launches) != 0 {
			t.Fatalf("Restart(kill fails) err=%v launches=%v, want error and no relaunch", err, fs.launches)
		}
	})

	t.Run("invalid id", func(t *testing.T) {
		if _, err := (&Manager{}).Restart("nope"); !errors.Is(err, ErrNotResumable) {
			t.Fatalf("Restart(bad id) err = %v, want ErrNotResumable", err)
		}
	})
}

func TestUpgrade(t *testing.T) {
	const (
		idle     = "11111111-1111-4111-8111-111111111111"
		busy     = "22222222-2222-4222-8222-222222222222"
		current  = "33333333-3333-4333-8333-333333333333"
		unknown  = "44444444-4444-4444-8444-444444444444"
		shell    = "66666666-6666-4666-8666-666666666666"
		waiting  = "77777777-7777-4777-8777-777777777777"
		oldV     = "2.1.285"
		newV     = "2.1.287"
		prefixed = "test-rc-"
	)
	fastStop(t)

	// registry is each session's entry in claude's process registry
	// (unknown has none). setup starts the given sessions; every fake exec costs
	// ~1s under -race, so subtests run only the sessions they assert on.
	registry := map[string][2]string{
		idle: {oldV, "idle"}, busy: {oldV, "busy"}, current: {newV, "idle"}, unknown: {"", ""},
		shell: {oldV, "shell"}, waiting: {oldV, "waiting"},
	}
	setup := func(t *testing.T, ids ...string) (*Manager, *fakeScreen) {
		home := t.TempDir()
		fs := &fakeScreen{version: oldV, updateTo: newV}
		for _, id := range ids {
			upgradeHome(t, home, id, registry[id][0], registry[id][1])
			fs.created = append(fs.created, prefixed+id)
		}
		withFakeScreen(t, fs)
		return &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home}, fs
	}
	actions := func(res UpgradeResult) map[string]string {
		got := map[string]string{}
		for _, o := range res.Sessions {
			got[o.ID] = o.Action
		}
		return got
	}

	t.Run("restarts outdated idle sessions only", func(t *testing.T) {
		m, fs := setup(t, idle, busy, current, unknown)
		res, err := m.Upgrade(true, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Before != oldV || res.After != newV || !strings.Contains(res.Output, "Successfully updated") {
			t.Errorf("result = %+v", res)
		}
		got := actions(res)
		want := map[string]string{idle: ActionRestarted, busy: ActionSkipped, current: ActionSkipped, unknown: ActionRestarted}
		for id, a := range want {
			if got[id] != a {
				t.Errorf("session %s: action %q, want %q", id, got[id], a)
			}
		}
		if len(fs.killed) != 2 || len(fs.created) != 4 {
			t.Errorf("killed=%d running=%d, want 2 restarted and all 4 running", len(fs.killed), len(fs.created))
		}
	})

	t.Run("force also restarts busy, never up-to-date", func(t *testing.T) {
		m, _ := setup(t, busy, current)
		res, err := m.Upgrade(true, true)
		if err != nil {
			t.Fatal(err)
		}
		got := actions(res)
		if got[busy] != ActionRestarted || got[current] != ActionSkipped {
			t.Errorf("actions = %v", got)
		}
	})

	t.Run("every non-idle status is skipped, with the status as the reason", func(t *testing.T) {
		m, fs := setup(t, shell, waiting)
		res, err := m.Upgrade(true, false)
		if err != nil {
			t.Fatal(err)
		}
		reasons := map[string]string{}
		for _, o := range res.Sessions {
			if o.Action != ActionSkipped {
				t.Errorf("session %s: action %q, want skipped", o.ID, o.Action)
			}
			reasons[o.ID] = o.Reason
		}
		if !strings.HasPrefix(reasons[shell], "shell") || !strings.HasPrefix(reasons[waiting], "waiting") {
			t.Errorf("reasons = %v, want each to lead with the session's status", reasons)
		}
		if len(fs.killed) != 0 {
			t.Errorf("killed = %v, want none", fs.killed)
		}
	})

	t.Run("force restarts every non-idle status", func(t *testing.T) {
		m, _ := setup(t, shell, waiting)
		res, err := m.Upgrade(true, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := actions(res); got[shell] != ActionRestarted || got[waiting] != ActionRestarted {
			t.Errorf("actions = %v, want both restarted", got)
		}
	})

	t.Run("a named session already on the new version is skipped", func(t *testing.T) {
		for _, force := range []bool{false, true} {
			m, fs := setup(t, current)
			res, err := m.Upgrade(true, force, current)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Sessions) != 1 || res.Sessions[0].Action != ActionSkipped ||
				!strings.Contains(res.Sessions[0].Reason, newV) {
				t.Errorf("force=%v: sessions = %+v, want one skipped as already on %s", force, res.Sessions, newV)
			}
			if len(fs.killed) != 0 {
				t.Errorf("force=%v: killed = %v, want none", force, fs.killed)
			}
		}
	})

	t.Run("ids limit the restart to those sessions", func(t *testing.T) {
		m, fs := setup(t, idle, unknown)
		const stoppedID = "55555555-5555-4555-8555-555555555555"
		res, err := m.Upgrade(true, false, idle, stoppedID)
		if err != nil {
			t.Fatal(err)
		}
		got := actions(res)
		if len(got) != 2 || got[idle] != ActionRestarted || got[stoppedID] != ActionSkipped {
			t.Errorf("actions = %v, want idle restarted, stopped skipped, unknown untouched", got)
		}
		if len(fs.killed) != 1 {
			t.Errorf("killed = %v, want only the named session", fs.killed)
		}
		if _, err := m.Upgrade(true, false, "nope"); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Upgrade(bad id) err = %v, want ErrInvalidID", err)
		}
	})

	t.Run("no restart leaves sessions alone", func(t *testing.T) {
		m, fs := setup(t, idle)
		res, err := m.Upgrade(false, false)
		if err != nil || len(res.Sessions) != 0 || len(fs.killed) != 0 {
			t.Fatalf("res=%+v err=%v killed=%v", res, err, fs.killed)
		}
	})

	t.Run("failed update restarts nothing", func(t *testing.T) {
		m, fs := setup(t, idle)
		fs.failUpdate = true
		res, err := m.Upgrade(true, true)
		if err == nil || !strings.Contains(res.Output, "network unreachable") || len(fs.killed) != 0 {
			t.Fatalf("res=%+v err=%v killed=%v, want error + output and no restarts", res, err, fs.killed)
		}
	})

	t.Run("restart failure is reported per session", func(t *testing.T) {
		m, fs := setup(t, idle)
		fs.failKill = true
		res, err := m.Upgrade(true, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := actions(res); got[idle] != ActionFailed {
			t.Errorf("actions = %v, want idle failed", got)
		}
	})
}

func TestPidAlive(t *testing.T) {
	if pidAlive(0) {
		t.Error("pidAlive(0) = true")
	}
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive(self) = false")
	}
	// A reaped child's pid is gone.
	cmd := helperCmd("", 0)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if pid := cmd.Process.Pid; pidAlive(pid) {
		t.Logf("pid %s reused; skipping", strconv.Itoa(pid))
	}
}

func TestListAllAnnotatesVersion(t *testing.T) {
	const (
		oldID  = "11111111-1111-4111-8111-111111111111"
		newID  = "22222222-2222-4222-8222-222222222222"
		noReg  = "33333333-3333-4333-8333-333333333333"
		liveID = "c0ffee00-1111-4222-8333-444455556666" // conversation oldID drifted onto
	)
	home := t.TempDir()
	upgradeHome(t, home, oldID, "", "")
	upgradeHome(t, home, liveID, "", "")
	upgradeHome(t, home, newID, "2.1.287", "idle")
	upgradeHome(t, home, noReg, "", "")
	// oldID's claude (pid 4242, child of screen pid 10000) is registered under liveID.
	reg := `{"sessionId":"` + liveID + `","pid":4242,"version":"2.1.278","status":"idle"}`
	if err := os.WriteFile(filepath.Join(home, "sessions", "4242.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the two logs' mtimes distinguishable: the live one is the recent one.
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(home, "projects", "repo", oldID+".jsonl"), stale, stale); err != nil {
		t.Fatal(err)
	}

	fs := &fakeScreen{version: "2.1.287", ps: "4242 10000\n",
		created: []string{"test-rc-" + oldID, "test-rc-" + newID, "test-rc-" + noReg}}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home}

	all, err := m.ListAll(nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListAll = %+v, %v", all, err)
	}
	byID := map[string]Session{}
	for _, s := range all {
		byID[s.ID] = s
	}
	if s := byID[oldID]; s.Version != "2.1.278" || !s.Outdated {
		t.Errorf("old session = %+v, want version 2.1.278 and outdated", s)
	}
	if ts, _ := time.Parse(time.RFC3339, byID[oldID].LastActive); time.Since(ts) > time.Hour {
		t.Errorf("old session LastActive = %q, want the live conversation's (recent) time", byID[oldID].LastActive)
	}
	if s := byID[newID]; s.Version != "2.1.287" || s.Outdated {
		t.Errorf("current session = %+v, want version 2.1.287 and not outdated", s)
	}
	if s := byID[noReg]; s.Version != "" || s.Outdated {
		t.Errorf("unregistered session = %+v, want no version", s)
	}
}

// A session that drifted onto a new conversation keeps following it after it
// stops: Kill records the conversation, ListAll shows it, Resume reopens it.
func TestDriftedSessionLifecycle(t *testing.T) {
	const (
		id     = "6fd0b321-a454-4b40-9aed-131afe120d36"
		liveID = "c0ffee00-1111-4222-8333-444455556666"
	)
	fastStop(t)
	home := t.TempDir()
	upgradeHome(t, home, id, "", "")
	upgradeHome(t, home, liveID, "", "")
	proj := filepath.Join(home, "projects", "repo")
	title := func(conv, name string) {
		f, err := os.OpenFile(filepath.Join(proj, conv+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.WriteString(`{"type":"custom-title","customTitle":"` + name + `"}` + "\n")
	}
	title(id, "original title")
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(proj, id+".jsonl"), stale, stale); err != nil {
		t.Fatal(err)
	}
	reg := `{"sessionId":"` + liveID + `","pid":4242,"version":"2.1.278","status":"idle"}`
	if err := os.WriteFile(filepath.Join(home, "sessions", "4242.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}

	st := &fakeStore{stopped: []Session{{ID: id, Screen: "test-rc-" + id}}}
	fs := &fakeScreen{created: []string{"test-rc-" + id}, ps: "4242 10000 claude --resume " + id + "\n"}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home, Store: st}

	// Running: the listing shows the live conversation's activity, and keeps the
	// original title until the new conversation gets one of its own.
	st.stopped = nil
	all, err := m.ListAll(st)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAll(running) = %+v, %v", all, err)
	}
	if all[0].Title != "original title" {
		t.Errorf("running title = %q, want fallback to the original", all[0].Title)
	}
	title(liveID, "renamed since")
	if all, _ = m.ListAll(st); all[0].Title != "renamed since" {
		t.Errorf("running title = %q, want the live conversation's", all[0].Title)
	}

	// Stop it: the conversation it was in is remembered.
	st.convs = nil // forget what ListAll noted; Kill alone must record it
	m.convSeen = nil
	if existed, err := m.Kill(id); err != nil || !existed {
		t.Fatalf("Kill = %v, %v", existed, err)
	}
	if st.convs[id] != liveID {
		t.Fatalf("stored conversation = %q, want %q", st.convs[id], liveID)
	}

	// Stopped: listed with the live conversation's title and activity.
	st.stopped = []Session{{ID: id, Screen: "test-rc-" + id}}
	all, err = m.ListAll(st)
	if err != nil || len(all) != 1 || all[0].Status != "Stopped" {
		t.Fatalf("ListAll(stopped) = %+v, %v", all, err)
	}
	if all[0].Title != "renamed since" {
		t.Errorf("stopped title = %q, want the live conversation's", all[0].Title)
	}
	if ts, _ := time.Parse(time.RFC3339, all[0].LastActive); time.Since(ts) > time.Hour {
		t.Errorf("stopped LastActive = %q, want the live conversation's (recent) time", all[0].LastActive)
	}

	// Resume reopens the conversation it was in, under the original names.
	if _, err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	want := "test-rc-" + id + " claude --resume " + liveID + " --remote-control " + id
	if len(fs.launches) != 1 || !strings.Contains(fs.launches[0], want) {
		t.Errorf("resume argv %v, want it to contain %q", fs.launches, want)
	}
}

// With no registry entry, the conversation comes from the Store, then from the
// command line the process was launched with.
func TestRestartWithoutRegistry(t *testing.T) {
	const (
		id     = "6fd0b321-a454-4b40-9aed-131afe120d36"
		argvID = "c0ffee00-1111-4222-8333-444455556666"
		storID = "b1b1b1b1-1111-4222-8333-444455556666"
	)
	fastStop(t)
	setup := func(t *testing.T, st Recorder) (*Manager, *fakeScreen) {
		home := t.TempDir()
		for _, c := range []string{id, argvID, storID} {
			upgradeHome(t, home, c, "", "")
		}
		fs := &fakeScreen{created: []string{"test-rc-" + id},
			ps: "4242 10000 /usr/bin/claude --resume " + argvID + " --remote-control " + id + "\n"}
		withFakeScreen(t, fs)
		return &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home, Store: st}, fs
	}

	t.Run("command line", func(t *testing.T) {
		m, fs := setup(t, nil)
		if _, err := m.Restart(id); err != nil {
			t.Fatal(err)
		}
		if len(fs.launches) != 1 || !strings.Contains(fs.launches[0], "--resume "+argvID+" --remote-control "+id) {
			t.Errorf("relaunch argv %v, want --resume %s", fs.launches, argvID)
		}
	})
	t.Run("store wins over command line", func(t *testing.T) {
		st := &fakeStore{convs: map[string]string{id: storID}}
		m, fs := setup(t, st)
		if _, err := m.Restart(id); err != nil {
			t.Fatal(err)
		}
		if len(fs.launches) != 1 || !strings.Contains(fs.launches[0], "--resume "+storID+" --remote-control "+id) {
			t.Errorf("relaunch argv %v, want --resume %s", fs.launches, storID)
		}
	})
}

func TestSyncConversations(t *testing.T) {
	const (
		id     = "6fd0b321-a454-4b40-9aed-131afe120d36"
		liveID = "c0ffee00-1111-4222-8333-444455556666"
	)
	home := t.TempDir()
	upgradeHome(t, home, id, "", "")
	upgradeHome(t, home, liveID, "", "")
	reg := `{"sessionId":"` + liveID + `","pid":4242,"version":"2.1.278","status":"idle"}`
	if err := os.WriteFile(filepath.Join(home, "sessions", "4242.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{}
	withFakeScreen(t, &fakeScreen{created: []string{"test-rc-" + id}, ps: "4242 10000 claude\n"})
	m := &Manager{Prefix: "test-rc", ScreenBin: "screen", ClaudeHome: home, Store: st}
	m.SyncConversations()
	if st.convs[id] != liveID {
		t.Errorf("stored conversation = %q, want %q", st.convs[id], liveID)
	}
	(&Manager{}).SyncConversations() // no store: no-op
}
