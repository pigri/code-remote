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
	if dirTrusted(cfg, "/repo") || dirTrusted(cfg, "/new") {
		t.Fatal("dirTrusted = true before trusting")
	}
	for _, dir := range []string{"/repo", "/new"} {
		if err := trustDir(cfg, dir); err != nil {
			t.Fatalf("trustDir(%s): %v", dir, err)
		}
		if !dirTrusted(cfg, dir) {
			t.Errorf("dirTrusted(%s) = false after trustDir", dir)
		}
	}
	if dirTrusted(cfg, "/other") {
		t.Error("trustDir trusted a directory it wasn't asked to")
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
	cfg := filepath.Join(dir, "new.json")
	if err := trustDir(cfg, "/repo"); err != nil || !dirTrusted(cfg, "/repo") {
		t.Fatalf("trustDir on a missing config: %v", err)
	}

	// A config that doesn't parse is left exactly as it was.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := trustDir(bad, "/repo"); err == nil {
		t.Error("trustDir(unparseable config) = nil error")
	}
	if data, _ := os.ReadFile(bad); string(data) != "{not json" {
		t.Errorf("unparseable config was rewritten: %q", data)
	}

	if err := trustDir("", "/repo"); err == nil {
		t.Error("trustDir with no config location = nil error")
	}
}

func TestTrustDirLock(t *testing.T) {
	origWait, origStale := trustLockWait, trustLockStale
	t.Cleanup(func() { trustLockWait, trustLockStale = origWait, origStale })
	trustLockWait = 150 * time.Millisecond

	cfg := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.Mkdir(cfg+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	// Held by someone else: give up rather than write over them.
	if err := trustDir(cfg, "/repo"); err == nil || dirTrusted(cfg, "/repo") {
		t.Fatalf("trustDir under a held lock = %v, want error and no write", err)
	}
	// Abandoned: taken over.
	trustLockStale = 0
	if err := trustDir(cfg, "/repo"); err != nil || !dirTrusted(cfg, "/repo") {
		t.Fatalf("trustDir under a stale lock: %v", err)
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

		s, err := m.ResumeIn(id, root, "")
		if err != nil {
			t.Fatalf("ResumeIn: %v", err)
		}
		if trusted := dirTrusted(m.ClaudeConfig, root); s.NeedsTrust == trust || trusted != trust {
			t.Errorf("TrustDirs=%v: NeedsTrust=%v trusted=%v", trust, s.NeedsTrust, trusted)
		}

		c, err := m.Create(root, "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if c.NeedsTrust == trust {
			t.Errorf("TrustDirs=%v: Create NeedsTrust=%v", trust, c.NeedsTrust)
		}
	}
}

// A Manager only touches the config it was given: the caller's own
// CLAUDE_CONFIG_DIR is resolved at startup (DefaultClaudeConfig), never here.
func TestTrustDirIgnoresCallerConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	m := &Manager{}
	p, _ := m.lookupProfile("")
	if err := trustDir(m.configPath(p), "/repo"); err == nil {
		t.Error("TrustDir with no configured location = nil error")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf("TrustDir wrote the caller's config: stat err = %v", err)
	}
}

func TestDefaultClaudeConfig(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/config")
	if got, want := DefaultClaudeConfig(), "/custom/config/.claude.json"; got != want {
		t.Errorf("DefaultClaudeConfig() = %q, want %q", got, want)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := DefaultClaudeConfig(); got != "" {
		t.Errorf("DefaultClaudeConfig() without CLAUDE_CONFIG_DIR = %q, want \"\" (beside ClaudeHome)", got)
	}
}
