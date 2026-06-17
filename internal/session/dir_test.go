package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDir(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "chapek-platform")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	aFile := filepath.Join(root, "file.txt")
	if err := os.WriteFile(aFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()

	t.Run("valid dir under root", func(t *testing.T) {
		m := &Manager{WorkspaceRoot: root}
		got, err := m.resolveDir(proj)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != filepath.Clean(proj) {
			t.Errorf("got %q want %q", got, proj)
		}
	})

	t.Run("dir outside root rejected", func(t *testing.T) {
		m := &Manager{WorkspaceRoot: root}
		if _, err := m.resolveDir(outside); !errors.Is(err, ErrInvalidDir) {
			t.Errorf("want ErrInvalidDir, got %v", err)
		}
	})

	t.Run("traversal escape rejected", func(t *testing.T) {
		m := &Manager{WorkspaceRoot: root}
		if _, err := m.resolveDir(filepath.Join(proj, "..", "..", "etc")); !errors.Is(err, ErrInvalidDir) {
			t.Errorf("want ErrInvalidDir, got %v", err)
		}
	})

	t.Run("nonexistent dir rejected", func(t *testing.T) {
		m := &Manager{WorkspaceRoot: root}
		if _, err := m.resolveDir(filepath.Join(root, "nope")); !errors.Is(err, ErrInvalidDir) {
			t.Errorf("want ErrInvalidDir, got %v", err)
		}
	})

	t.Run("file (not dir) rejected", func(t *testing.T) {
		m := &Manager{WorkspaceRoot: root}
		if _, err := m.resolveDir(aFile); !errors.Is(err, ErrInvalidDir) {
			t.Errorf("want ErrInvalidDir, got %v", err)
		}
	})

	t.Run("no workspace root allows any existing dir", func(t *testing.T) {
		m := &Manager{}
		if _, err := m.resolveDir(outside); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
