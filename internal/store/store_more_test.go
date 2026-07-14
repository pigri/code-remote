package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenParentIsFile(t *testing.T) {
	// A regular file where a directory is expected makes MkdirAll fail.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(f, "sub", "code-remote.db")); err == nil {
		t.Fatal("Open under a file path = nil error, want error")
	}
}

func TestQueriesForAbsentSession(t *testing.T) {
	d := openTemp(t)

	if b, err := d.LastBridge("nope"); err != nil || b != "" {
		t.Errorf("LastBridge(absent) = %q, %v, want empty/nil", b, err)
	}
	if _, ok, err := d.FirstSeenArchived("nope"); err != nil || ok {
		t.Errorf("FirstSeenArchived(absent) ok=%v err=%v, want false/nil", ok, err)
	}
	if rows, err := d.AllSessions(); err != nil || len(rows) != 0 {
		t.Errorf("AllSessions(empty) = %d rows, %v, want 0/nil", len(rows), err)
	}
}
