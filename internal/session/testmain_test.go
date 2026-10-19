package session

import (
	"errors"
	"testing"

	"claude-remote-api/internal/testenv"
)

// TestMain keeps every test off the real /proc: a process's environment is
// unreadable unless a test fakes it (withEnviron).
func TestMain(m *testing.M) {
	procEnviron = func(string) ([]string, error) { return nil, errors.New("no environ in tests") }
	testenv.Run(m)
}
