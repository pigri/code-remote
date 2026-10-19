package session

import (
	"os"
	"path/filepath"
)

// DefaultClaudeHome is the claude config dir to read session logs and the
// process registry from: CLAUDE_CONFIG_DIR, which the launched claude inherits
// and writes them to, else CLAUDE_HOME, else ~/.claude. "" when no home is known.
func DefaultClaudeHome() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("CLAUDE_HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".claude")
	}
	return ""
}
