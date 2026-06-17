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
		want, _ := filepath.EvalSymlinks(proj) // resolveDir returns the symlink-resolved path
		if got != want {
			t.Errorf("got %q want %q", got, want)
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

	t.Run("no workspace root is rejected (fail closed)", func(t *testing.T) {
		m := &Manager{}
		if _, err := m.resolveDir(outside); !errors.Is(err, ErrInvalidDir) {
			t.Errorf("want ErrInvalidDir when no root configured, got %v", err)
		}
	})

	t.Run("symlink inside root pointing outside is rejected", func(t *testing.T) {
		link := filepath.Join(root, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		m := &Manager{WorkspaceRoot: root}
		if _, err := m.resolveDir(link); !errors.Is(err, ErrInvalidDir) {
			t.Errorf("want ErrInvalidDir for symlink escape, got %v", err)
		}
	})
}
