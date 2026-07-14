package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
