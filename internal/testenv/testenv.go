// Package testenv keeps a test binary off the caller's real files.
package testenv

import (
	"os"
	"path/filepath"
	"testing"
)

// Run points every location the code under test falls back to (HOME, the XDG
// data dir, the session DB, claude's config dir) at a throwaway dir, runs the
// tests and exits. Call it from TestMain.
func Run(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "code-remote-test-")
	if err != nil {
		os.Stderr.WriteString("testenv: " + err.Error() + "\n")
		return 1
	}
	defer os.RemoveAll(dir)
	if err := Isolate(dir); err != nil {
		os.Stderr.WriteString("testenv: " + err.Error() + "\n")
		return 1
	}
	return m.Run()
}

// Isolate sets the environment so that nothing resolves outside dir, and no
// profile or session setting of the caller's reaches the tests.
func Isolate(dir string) error {
	set := map[string]string{
		"HOME":             dir,
		"XDG_DATA_HOME":    filepath.Join(dir, "data"),
		"CLAUDE_REMOTE_DB": filepath.Join(dir, "code-remote.db"),
	}
	for k, v := range set {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_HOME", "CLAUDE_REMOTE_PROFILES",
		"CLAUDE_REMOTE_DEFAULT_PROFILE", "CLAUDE_REMOTE_CREDENTIALS", "CLAUDE_REMOTE_SESSION_PREFIX"} {
		if err := os.Unsetenv(k); err != nil {
			return err
		}
	}
	return nil
}
