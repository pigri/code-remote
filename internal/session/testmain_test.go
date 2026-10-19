package session

import (
	"errors"
	"testing"

	"claude-remote-api/internal/testenv"
)

// TestMain keeps every test off the real /proc: a process's environment and
// start are unreadable unless a test fakes them (withEnviron, withProcStart).
func TestMain(m *testing.M) {
	procEnviron = func(string) ([]string, error) { return nil, errors.New("no environ in tests") }
	procStart = func(string) (string, error) { return "", errors.New("no process start in tests") }
	testenv.Run(m)
}
