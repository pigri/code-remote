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

// fakeScreen stands in for the `screen` binary. It records `-dmS` creations and
// serves them back through `-ls`, so Create/List/Get/Kill can be exercised
// deterministically without screen installed.
type fakeScreen struct {
	created    []string // screen names from -dmS
	extra      string   // trailing `screen -ls` lines (foreign sessions, footer)
	failCreate bool
	failKill   bool
}

func (f *fakeScreen) command(name string, args ...string) *exec.Cmd {
	out, exit := "", 0
	switch {
	case len(args) > 0 && args[0] == "-ls":
		var b strings.Builder
		b.WriteString("There are screens on:\n")
		for i, n := range f.created {
			b.WriteString("\t" + strconv.Itoa(10000+i) + "." + n + "\t(06/15/2026 08:41:26 AM)\t(Detached)\n")
		}
		b.WriteString(f.extra)
		out = b.String()
	case len(args) > 1 && args[0] == "-dmS":
		if f.failCreate {
			out, exit = "cannot create session", 1
		} else {
			f.created = append(f.created, args[1]) // args[1] is <prefix>-<id>
		}
	case len(args) > 0 && args[0] == "-S": // kill: -S <name> -X quit
		if f.failKill {
			out, exit = "No screen session found", 1
		}
	}
	return helperCmd(out, exit)
}

// helperCmd builds a command that re-execs this test binary as a stub process
// which prints HELPER_OUT and exits with HELPER_EXIT (the classic os/exec seam).
func helperCmd(out string, exit int) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = []string{"GO_WANT_HELPER_PROCESS=1", "HELPER_OUT=" + out, "HELPER_EXIT=" + strconv.Itoa(exit)}
	return cmd
}

// TestHelperProcess is not a real test: when GO_WANT_HELPER_PROCESS is set it
// impersonates `screen`, emitting canned output and exit code, then exits.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Stdout.WriteString(os.Getenv("HELPER_OUT"))
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT"))
	os.Exit(code)
}

// withFakeScreen installs fs as the exec seam for the duration of the test.
func withFakeScreen(t *testing.T, fs *fakeScreen) {
	t.Helper()
	orig := execCommand
	execCommand = fs.command
	t.Cleanup(func() { execCommand = orig })
}

func TestListViaScreen(t *testing.T) {
	fs := &fakeScreen{
		created: []string{"test-rc-6fd0b321-a454-4b40-9aed-131afe120d36"},
		extra:   "\t99999.some-foreign-session\t(06/15/2026 09:00:00 AM)\t(Detached)\n1 Socket.\n",
	}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc"}
	got, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "6fd0b321-a454-4b40-9aed-131afe120d36" {
		t.Fatalf("List = %+v, want the one prefixed session", got)
	}
}

func TestGetViaScreen(t *testing.T) {
	const id = "6fd0b321-a454-4b40-9aed-131afe120d36"
	fs := &fakeScreen{created: []string{"test-rc-" + id}}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc"}

	if s, ok, err := m.Get(id); err != nil || !ok || s.ID != id {
		t.Fatalf("Get(existing) = %+v ok=%v err=%v", s, ok, err)
	}
	if _, ok, err := m.Get("a9c1cf1e-ce20-4833-9eeb-7acf5c327506"); err != nil || ok {
		t.Fatalf("Get(absent) ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestCreateViaScreen(t *testing.T) {
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen"}

	// Success: create launches screen, then Get finds the freshly-listed session
	// (fakeScreen echoes the -dmS name back through -ls) -> enriched return.
	s, err := m.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !m.ValidID(s.ID) || s.Screen != "test-rc-"+s.ID {
		t.Fatalf("Create = %+v, want valid id + matching screen name", s)
	}
	if s.PID == "" || s.Status != "Detached" {
		t.Errorf("Create result not enriched from listing: %+v", s)
	}
}

func TestCreateScreenFails(t *testing.T) {
	fs := &fakeScreen{failCreate: true}
	withFakeScreen(t, fs)
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen"}
	if _, err := m.Create(""); err == nil {
		t.Fatal("Create with failing screen = nil error, want error")
	}
}

func TestCreateWithDir(t *testing.T) {
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	root := t.TempDir()
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", WorkspaceRoot: root}
	if _, err := m.Create("."); err != nil { // "." resolves to the workspace root
		t.Fatalf("Create with valid dir: %v", err)
	}
	if _, err := m.Create("../escape"); err == nil {
		t.Fatal("Create with escaping dir = nil, want ErrInvalidDir")
	}
}

func TestKillViaScreen(t *testing.T) {
	const id = "6fd0b321-a454-4b40-9aed-131afe120d36"
	t.Run("existing session", func(t *testing.T) {
		fs := &fakeScreen{created: []string{"test-rc-" + id}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen"}
		if existed, err := m.Kill(id); err != nil || !existed {
			t.Fatalf("Kill(existing) existed=%v err=%v, want true/nil", existed, err)
		}
	})
	t.Run("absent session", func(t *testing.T) {
		fs := &fakeScreen{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen"}
		if existed, err := m.Kill(id); err != nil || existed {
			t.Fatalf("Kill(absent) existed=%v err=%v, want false/nil", existed, err)
		}
	})
	t.Run("quit fails", func(t *testing.T) {
		fs := &fakeScreen{created: []string{"test-rc-" + id}, failKill: true}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen"}
		if existed, err := m.Kill(id); err == nil || !existed {
			t.Fatalf("Kill(quit-fails) existed=%v err=%v, want true + error", existed, err)
		}
	})
}

func TestResumeViaScreen(t *testing.T) {
	const id = "6fd0b321-a454-4b40-9aed-131afe120d36"

	// writeLog drops a claude session .jsonl carrying cwd under home.
	writeLog := func(t *testing.T, home, cwd string) {
		t.Helper()
		proj := filepath.Join(home, "projects", "repo")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"type":"user","cwd":"` + cwd + `","sessionId":"` + id + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("success restores session in recorded cwd", func(t *testing.T) {
		home := t.TempDir()
		cwd := t.TempDir() // must exist so Resume sets cmd.Dir
		writeLog(t, home, cwd)
		fs := &fakeScreen{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home}

		s, err := m.Resume(id)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if s.ID != id || s.Screen != "test-rc-"+id || s.Status != "Detached" {
			t.Fatalf("Resume = %+v, want enriched session for %s", s, id)
		}
	})

	t.Run("already running", func(t *testing.T) {
		fs := &fakeScreen{created: []string{"test-rc-" + id}}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen", ClaudeHome: t.TempDir()}
		if _, err := m.Resume(id); !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("Resume(running) err = %v, want ErrAlreadyRunning", err)
		}
	})

	t.Run("no log on disk", func(t *testing.T) {
		fs := &fakeScreen{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen", ClaudeHome: t.TempDir()}
		if _, err := m.Resume(id); !errors.Is(err, ErrNotResumable) {
			t.Fatalf("Resume(no log) err = %v, want ErrNotResumable", err)
		}
	})

	t.Run("invalid id", func(t *testing.T) {
		m := &Manager{Prefix: "test-rc", ScreenBin: "screen"}
		if _, err := m.Resume("not-a-uuid"); !errors.Is(err, ErrNotResumable) {
			t.Fatalf("Resume(bad id) err = %v, want ErrNotResumable", err)
		}
	})

	t.Run("no ClaudeHome skips disk check", func(t *testing.T) {
		fs := &fakeScreen{}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen"}
		if s, err := m.Resume(id); err != nil || s.ID != id {
			t.Fatalf("Resume(no home) = %+v err=%v, want success", s, err)
		}
	})

	t.Run("screen spawn fails", func(t *testing.T) {
		fs := &fakeScreen{failCreate: true}
		withFakeScreen(t, fs)
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen"}
		if _, err := m.Resume(id); err == nil {
			t.Fatal("Resume(spawn fails) = nil error, want error")
		}
	})
}

// fakeStore captures Record calls and serves canned StoppedSessions.
type fakeStore struct {
	recorded []string // uuids passed to Record
	lastCwd  string
	stopped  []Session
}

func (f *fakeStore) Record(uuid, _, _, cwd, _, _ string) error {
	f.recorded = append(f.recorded, uuid)
	f.lastCwd = cwd
	return nil
}

func (f *fakeStore) StoppedSessions(running []Session, keep func(string) bool) ([]Session, error) {
	var out []Session
	for _, s := range f.stopped {
		if keep != nil && !keep(s.ID) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func TestCreateRecordsToStore(t *testing.T) {
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	fst := &fakeStore{}
	root := t.TempDir()
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", WorkspaceRoot: root, Store: fst}

	s, err := m.Create(".") // "." resolves to the workspace root
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(fst.recorded) != 1 || fst.recorded[0] != s.ID {
		t.Fatalf("recorded = %v, want [%s]", fst.recorded, s.ID)
	}
	if fst.lastCwd != root {
		t.Errorf("recorded cwd = %q, want %q", fst.lastCwd, root)
	}
}

func TestListAll(t *testing.T) {
	const runningID = "6fd0b321-a454-4b40-9aed-131afe120d36"
	const stoppedID = "a9c1cf1e-ce20-4833-9eeb-7acf5c327506"

	fs := &fakeScreen{created: []string{"test-rc-" + runningID}}
	withFakeScreen(t, fs)

	// Session log on disk for the stopped id, so HasSessionLog keeps it.
	home := t.TempDir()
	proj := filepath.Join(home, "projects", "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"custom-title","customTitle":"resumable","sessionId":"` + stoppedID + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, stoppedID+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{Prefix: "test-rc", ScreenBin: "screen", ClaudeHome: home}

	t.Run("nil store returns only running", func(t *testing.T) {
		got, err := m.ListAll(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != runningID {
			t.Fatalf("ListAll(nil) = %+v, want just the running session", got)
		}
	})

	t.Run("merges resumable and drops sessions with no log", func(t *testing.T) {
		fst := &fakeStore{stopped: []Session{
			{ID: stoppedID, Screen: "test-rc-" + stoppedID},
			{ID: "deadbeef-0000-0000-0000-000000000000", Screen: "gone"}, // no log -> dropped
		}}
		got, err := m.ListAll(fst)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]Session{}
		for _, s := range got {
			byID[s.ID] = s
		}
		if len(byID) != 2 {
			t.Fatalf("ListAll = %d sessions, want running + one resumable: %+v", len(byID), got)
		}
		st, ok := byID[stoppedID]
		if !ok || st.Status != "Stopped" {
			t.Errorf("resumable session = %+v, want Status Stopped", st)
		}
		if st.Title != "resumable" { // enriched with the live title from disk
			t.Errorf("resumable title = %q, want live title 'resumable'", st.Title)
		}
	})
}

func TestListPopulatesLastActive(t *testing.T) {
	const id = "6fd0b321-a454-4b40-9aed-131afe120d36"
	fs := &fakeScreen{created: []string{"test-rc-" + id}}
	withFakeScreen(t, fs)

	home := t.TempDir()
	proj := filepath.Join(home, "projects", "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(proj, id+".jsonl")
	if err := os.WriteFile(log, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(log, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	m := &Manager{Prefix: "test-rc", ScreenBin: "screen", ClaudeHome: home}
	got, err := m.List()
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %+v, err=%v", got, err)
	}
	if got[0].LastActive != mtime.Format(time.RFC3339) {
		t.Errorf("LastActive = %q, want %q", got[0].LastActive, mtime.Format(time.RFC3339))
	}

	// No ClaudeHome -> no log lookup -> empty LastActive (not a crash).
	if la := (&Manager{Prefix: "test-rc", ScreenBin: "screen"}).lastActive(id); la != "" {
		t.Errorf("lastActive(no home) = %q, want empty", la)
	}
}

func TestTitleReadsFromDisk(t *testing.T) {
	const id = "6fd0b321-a454-4b40-9aed-131afe120d36"
	home := t.TempDir()
	proj := filepath.Join(home, "projects", "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"custom-title","customTitle":"my title","sessionId":"` + id + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{Prefix: "test-rc", ClaudeHome: home}
	if got := m.title(id); got != "my title" {
		t.Errorf("title = %q, want %q", got, "my title")
	}
	if got := m.title("a9c1cf1e-ce20-4833-9eeb-7acf5c327506"); got != "" {
		t.Errorf("title(no file) = %q, want empty", got)
	}
	if got := (&Manager{}).title(id); got != "" { // ClaudeHome empty -> ""
		t.Errorf("title(no home) = %q, want empty", got)
	}
}

func TestRegistrations(t *testing.T) {
	home := t.TempDir()
	sdir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(sdir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("1.json", `{"sessionId":"uuid-1","cwd":"/repo","bridgeSessionId":"session_X","status":"idle"}`)
	write("2.json", `{"sessionId":"uuid-2","cwd":"/other","bridgeSessionId":null,"status":"busy"}`)
	write("3.json", `{not valid json`)  // skipped
	write("4.json", `{"cwd":"/no-id"}`) // no sessionId -> skipped

	m := &Manager{ClaudeHome: home}
	regs, err := m.Registrations()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Registration{}
	for _, r := range regs {
		byID[r.SessionID] = r
	}
	if len(byID) != 2 {
		t.Fatalf("Registrations = %d valid, want 2: %+v", len(byID), regs)
	}
	if byID["uuid-1"].BridgeSessionID != "session_X" || byID["uuid-1"].Cwd != "/repo" {
		t.Errorf("uuid-1 = %+v", byID["uuid-1"])
	}
	if byID["uuid-2"].BridgeSessionID != "" { // null bridge -> empty
		t.Errorf("uuid-2 bridge = %q, want empty", byID["uuid-2"].BridgeSessionID)
	}

	if regs, err := (&Manager{}).Registrations(); err != nil || regs != nil { // no home -> nil
		t.Errorf("Registrations(no home) = %v, %v, want nil/nil", regs, err)
	}
}
