package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultClaudeHome(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home dir")
	}
	tests := []struct {
		name, claudeHome, configDir, want string
	}{
		{"CLAUDE_CONFIG_DIR wins", "/custom/home", "/custom/config", "/custom/config"},
		{"CLAUDE_HOME next", "/custom/home", "", "/custom/home"},
		{"~/.claude last", "", "", filepath.Join(userHome, ".claude")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_HOME", tt.claudeHome)
			t.Setenv("CLAUDE_CONFIG_DIR", tt.configDir)
			if got := DefaultClaudeHome(); got != tt.want {
				t.Errorf("DefaultClaudeHome() = %q, want %q", got, tt.want)
			}
		})
	}
}
