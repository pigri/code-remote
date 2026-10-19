package session

import (
	"os"
	"path/filepath"
)

// DefaultClaudeHome is the claude config dir to read session logs and the
// process registry from: CLAUDE_HOME, else CLAUDE_CONFIG_DIR (where claude
// itself keeps them when it is set), else ~/.claude. "" when no home is known.
func DefaultClaudeHome() string {
	if v := os.Getenv("CLAUDE_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".claude")
	}
	return ""
}
