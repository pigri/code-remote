package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrustDir(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	// A big integer and an HTML-ish string: both must survive the rewrite as is.
	orig := `{"numStartups":157,"big":12345678901234567890,"tip":"a<b>&c",` +
		`"projects":{"/repo":{"allowedTools":["x"],"hasTrustDialogAccepted":false},"/other":{"lastCost":1.5}}}`
	if err := os.WriteFile(cfg, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ClaudeConfig: cfg}

	if m.DirTrusted("/repo") || m.DirTrusted("/new") {
		t.Fatal("DirTrusted = true before trusting")
	}
	for _, dir := range []string{"/repo", "/new"} {
		if err := m.TrustDir(dir); err != nil {
			t.Fatalf("TrustDir(%s): %v", dir, err)
		}
		if !m.DirTrusted(dir) {
			t.Errorf("DirTrusted(%s) = false after TrustDir", dir)
		}
	}
	if m.DirTrusted("/other") {
		t.Error("TrustDir trusted a directory it wasn't asked to")
	}

	data, _ := os.ReadFile(cfg)
	for _, want := range []string{`12345678901234567890`, `"a<b>&c"`, `"numStartups": 157`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config lost %s:\n%s", want, data)
		}
	}
	var got struct {
		Projects map[string]struct {
			AllowedTools []string `json:"allowedTools"`
			LastCost     float64  `json:"lastCost"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, data)
	}
	if len(got.Projects["/repo"].AllowedTools) != 1 || got.Projects["/other"].LastCost != 1.5 {
		t.Errorf("project settings lost: %+v", got.Projects)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(cfg + ".lock"); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

func TestTrustDirCreatesAndRefuses(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{ClaudeConfig: filepath.Join(dir, "new.json")}
	if err := m.TrustDir("/repo"); err != nil || !m.DirTrusted("/repo") {
		t.Fatalf("TrustDir on a missing config: %v", err)
	}

	// A config that doesn't parse is left exactly as it was.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Manager{ClaudeConfig: bad}).TrustDir("/repo"); err == nil {
		t.Error("TrustDir(unparseable config) = nil error")
	}
	if data, _ := os.ReadFile(bad); string(data) != "{not json" {
		t.Errorf("unparseable config was rewritten: %q", data)
	}

	if err := (&Manager{}).TrustDir("/repo"); err == nil {
		t.Error("TrustDir with no config location = nil error")
	}
}

func TestTrustDirLock(t *testing.T) {
	origWait, origStale := trustLockWait, trustLockStale
	t.Cleanup(func() { trustLockWait, trustLockStale = origWait, origStale })
	trustLockWait = 150 * time.Millisecond

	cfg := filepath.Join(t.TempDir(), ".claude.json")
	m := &Manager{ClaudeConfig: cfg}
	if err := os.Mkdir(cfg+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	// Held by someone else: give up rather than write over them.
	if err := m.TrustDir("/repo"); err == nil || m.DirTrusted("/repo") {
		t.Fatalf("TrustDir under a held lock = %v, want error and no write", err)
	}
	// Abandoned: taken over.
	trustLockStale = 0
	if err := m.TrustDir("/repo"); err != nil || !m.DirTrusted("/repo") {
		t.Fatalf("TrustDir under a stale lock: %v", err)
	}
}

func TestLaunchTrustsOrFlagsDir(t *testing.T) {
	const id = "6fd0b321-a454-4b40-9aed-131afe120d36"
	for _, trust := range []bool{false, true} {
		home, root := t.TempDir(), evalDir(t)
		upgradeHome(t, home, id, "", "")
		withFakeScreen(t, &fakeScreen{})
		m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", ClaudeHome: home,
			WorkspaceRoot: root, ClaudeConfig: filepath.Join(home, "config.json"), TrustDirs: trust}

		s, err := m.ResumeIn(id, root)
		if err != nil {
			t.Fatalf("ResumeIn: %v", err)
		}
		if s.NeedsTrust == trust || m.DirTrusted(root) != trust {
			t.Errorf("TrustDirs=%v: NeedsTrust=%v trusted=%v", trust, s.NeedsTrust, m.DirTrusted(root))
		}

		c, err := m.Create(root)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if c.NeedsTrust == trust {
			t.Errorf("TrustDirs=%v: Create NeedsTrust=%v", trust, c.NeedsTrust)
		}
	}
}
