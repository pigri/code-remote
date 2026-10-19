package session

import (
	"os"
	"testing"
)

// TestMain clears the caller's claude config location: configPath and the
// home lookup read it, so a test run from inside a claude session would
// otherwise trust directories in that session's real config.
func TestMain(m *testing.M) {
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Unsetenv("CLAUDE_HOME")
	os.Exit(m.Run())
}
