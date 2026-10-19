package testenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsolate(t *testing.T) {
	for _, k := range []string{"HOME", "XDG_DATA_HOME", "CLAUDE_REMOTE_DB"} {
		t.Setenv(k, "/real/"+k)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/real/config")
	t.Setenv("CLAUDE_HOME", "/real/home")
	dir := t.TempDir()
	if err := Isolate(dir); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HOME":             dir,
		"XDG_DATA_HOME":    filepath.Join(dir, "data"),
		"CLAUDE_REMOTE_DB": filepath.Join(dir, "code-remote.db"),
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s still set to %q", k, v)
		}
	}
}
