package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const convX = "c0ffee00-1111-4222-8333-444455556666"

// convFiles is a conversation's files under a profile dir, by path relative
// to it, with their contents.
type convFiles map[string]string

// writeConv writes files under home, each with mode 0o600 unless modes names
// another, and every dir with 0o750.
func writeConv(t *testing.T, home string, files convFiles, modes map[string]fs.FileMode) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		mode := fs.FileMode(0o600)
		if m, ok := modes[rel]; ok {
			mode = m
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
}

// tree is every entry under dir, by path relative to it: a file's content,
// or "/" for a dir. Nil when dir does not exist.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			out[rel] = "/"
			return nil
		}
		data, err := os.ReadFile(p)
		out[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sidecarConv is a transcript with a sidecar dir of subagent transcripts and
// tool results, under projects/repo.
func sidecarConv(conv string) convFiles {
	base := filepath.Join("projects", "repo", conv)
	return convFiles{
		base + ".jsonl": `{"type":"user","cwd":"/x","sessionId":"` + conv + `"}` + "\n" +
			`{"type":"custom-title","customTitle":"private title","sessionId":"` + conv + `"}` + "\n",
		filepath.Join(base, "subagents", "agent-1.jsonl"): "sub\n",
		filepath.Join(base, "tool-results", "a.txt"):      "result\n",
	}
}

func TestCopyConvWithSidecar(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	files := sidecarConv(convX)
	sub := filepath.Join("projects", "repo", convX, "subagents", "agent-1.jsonl")
	writeConv(t, src, files, map[string]fs.FileMode{sub: 0o640})
	before := tree(t, src)

	undo, err := copyConv(t, src, dst, "repo", convX)
	if err != nil {
		t.Fatalf("copyConv: %v", err)
	}
	if got := tree(t, dst); !mapsEqual(got, before) {
		t.Errorf("target = %v, want %v", got, before)
	}
	if got := tree(t, src); !mapsEqual(got, before) {
		t.Errorf("source after the copy = %v, want it intact: %v", got, before)
	}
	for rel := range files {
		sfi, _ := os.Stat(filepath.Join(src, rel))
		dfi, err := os.Stat(filepath.Join(dst, rel))
		if err != nil || dfi.Mode() != sfi.Mode() {
			t.Errorf("%s: mode %v (%v), want %v", rel, dfi.Mode(), err, sfi.Mode())
		}
	}
	dfi, _ := os.Stat(filepath.Join(dst, "projects", "repo", convX))
	if dfi.Mode().Perm() != 0o750 {
		t.Errorf("sidecar dir mode %v, want 0750", dfi.Mode().Perm())
	}

	undo()
	if got := tree(t, dst); len(got) != 0 {
		t.Errorf("target after undo = %v, want what the copy made removed", got)
	}
}

func TestCopyConvWithoutSidecar(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	log := filepath.Join("projects", "repo", convX+".jsonl")
	writeConv(t, src, convFiles{log: "x\n"}, nil)
	if _, err := copyConv(t, src, dst, "repo", convX); err != nil {
		t.Fatal(err)
	}
	if got := tree(t, filepath.Join(dst, "projects", "repo")); !mapsEqual(got, map[string]string{convX + ".jsonl": "x\n"}) {
		t.Errorf("target = %v, want the transcript only", got)
	}
}

func TestCopyConvRefusesTraversal(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeConv(t, src, sidecarConv(convX), nil)
	// What a traversal would reach, so a copy that followed it would succeed.
	writeConv(t, src, convFiles{filepath.Join("projects", convX+".jsonl"): "up\n", convX + ".jsonl": "top\n"}, nil)
	for _, c := range [][2]string{
		{"..", convX}, {"../projects", convX}, {"repo/..", convX}, {".", convX}, {"", convX}, {"a/b", convX},
		{"repo", "../" + convX}, {"repo", ".."}, {"repo", "."}, {"repo", ""}, {"repo", "x/" + convX},
	} {
		if _, err := copyConv(t, src, dst, c[0], c[1]); err == nil {
			t.Errorf("copyConv(slug %q, conv %q) = nil, want a refusal", c[0], c[1])
		}
	}
	if got := tree(t, dst); len(got) != 0 {
		t.Errorf("target = %v, want nothing written", got)
	}
}

// asideFiles is the tree under the aside dir c reported, by path relative to it.
func asideFiles(t *testing.T, dst string, c *convCopy) map[string]string {
	t.Helper()
	if c.asideDir == "" || filepath.Dir(filepath.Dir(c.asideDir)) != dst || filepath.Base(filepath.Dir(c.asideDir)) != "crctl-aside" {
		t.Fatalf("aside dir = %q, want <target>/crctl-aside/<stamp>", c.asideDir)
	}
	return tree(t, c.asideDir)
}

// withoutAside is tree(dst) less the crctl-aside dir.
func withoutAside(t *testing.T, dst string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, v := range tree(t, dst) {
		if k != "crctl-aside" && !strings.HasPrefix(k, "crctl-aside"+string(filepath.Separator)) {
			out[k] = v
		}
	}
	return out
}

func TestCopyConvMovesADifferentTargetAside(t *testing.T) {
	log := filepath.Join("projects", "repo", convX+".jsonl")
	side := filepath.Join("projects", "repo", convX, "tool-results", "a.txt")
	for _, rel := range []string{log, side} {
		for _, size := range []string{"other size", "same size"} {
			t.Run(filepath.Base(rel)+", "+size, func(t *testing.T) {
				src, dst := t.TempDir(), t.TempDir()
				writeConv(t, src, sidecarConv(convX), nil)
				other := "other\n"
				if size == "same size" {
					other = strings.Repeat("x", len(sidecarConv(convX)[rel]))
				}
				writeConv(t, dst, convFiles{rel: other}, nil)
				c, err := planCopy(src, dst, "repo", convX)
				if err != nil {
					t.Fatal(err)
				}
				undo, err := c.run()
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				if got, want := withoutAside(t, dst), tree(t, src); !mapsEqual(got, want) {
					t.Errorf("target = %v, want the source's files: %v", got, want)
				}
				if got := asideFiles(t, dst, c)[rel]; got != other {
					t.Errorf("aside %s = %q, want the old file", rel, got)
				}
				if c.asided != 1 {
					t.Errorf("asided = %d, want 1", c.asided)
				}
				undo()
				got := tree(t, dst)
				if got[rel] != other || got["crctl-aside"] != "" {
					t.Errorf("target after undo = %v, want the old file back and no aside dir", got)
				}
			})
		}
	}
}

// Every differing file is moved aside in one move, and undo puts each back.
func TestCopyConvMovesSeveralFilesAside(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeConv(t, src, sidecarConv(convX), nil)
	old := convFiles{}
	for rel := range sidecarConv(convX) {
		old[rel] = "old " + rel + "\n"
	}
	writeConv(t, dst, old, nil)
	before := tree(t, dst)
	c, err := planCopy(src, dst, "repo", convX)
	if err != nil {
		t.Fatal(err)
	}
	undo, err := c.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c.asided != len(old) {
		t.Errorf("asided = %d, want %d", c.asided, len(old))
	}
	for rel, body := range old {
		if got := asideFiles(t, dst, c)[rel]; got != body {
			t.Errorf("aside %s = %q, want %q", rel, got, body)
		}
		if got, _ := os.ReadFile(filepath.Join(dst, rel)); string(got) != sidecarConv(convX)[rel] {
			t.Errorf("target %s = %q, want the source's", rel, got)
		}
	}
	undo()
	if got := tree(t, dst); !mapsEqual(got, before) {
		t.Errorf("target after undo = %v, want it as before: %v", got, before)
	}
}

// A file where a dir is needed, and a dir where a file goes, are moved aside
// too.
func TestCopyConvMovesTheWrongTypeAside(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  convFiles
		rel  string // what lands in the aside dir, by its top path
	}{
		{"file for the project dir", convFiles{filepath.Join("projects", "repo"): "file\n"}, filepath.Join("projects", "repo")},
		{"file for the sidecar dir", convFiles{filepath.Join("projects", "repo", convX): "file\n"}, filepath.Join("projects", "repo", convX)},
		{"dir for the transcript", convFiles{filepath.Join("projects", "repo", convX+".jsonl", "x"): "in\n"}, filepath.Join("projects", "repo", convX+".jsonl", "x")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			writeConv(t, src, sidecarConv(convX), nil)
			// writeConv cannot put a file where a dir is: build it by hand.
			for rel, body := range tc.old {
				if tc.name == "dir for the transcript" {
					writeConv(t, dst, convFiles{rel: body}, nil)
					continue
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dst, rel), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := tree(t, dst)
			c, err := planCopy(src, dst, "repo", convX)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			undo, err := c.run()
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got, want := withoutAside(t, dst), tree(t, src); !mapsEqual(got, want) {
				t.Errorf("target = %v, want the source's files: %v", got, want)
			}
			if got, ok := asideFiles(t, dst, c)[tc.rel]; !ok || (got != "/" && got != tc.old[tc.rel]) {
				t.Errorf("aside = %v, want %s in it", asideFiles(t, dst, c), tc.rel)
			}
			undo()
			if got := tree(t, dst); !mapsEqual(got, before) {
				t.Errorf("target after undo = %v, want it as before: %v", got, before)
			}
		})
	}
}

// When the aside dir cannot be made, the move fails as a conflict and the
// target is untouched.
func TestCopyConvAsideFailureIsAConflict(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeConv(t, src, sidecarConv(convX), nil)
	log := filepath.Join("projects", "repo", convX+".jsonl")
	writeConv(t, dst, convFiles{log: "other\n", "crctl-aside": "in the way\n"}, nil)
	before := tree(t, dst)
	c, err := planCopy(src, dst, "repo", convX)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.run(); !errors.Is(err, ErrMoveConflict) {
		t.Fatalf("run err = %v, want ErrMoveConflict", err)
	}
	if got := tree(t, dst); !mapsEqual(got, before) {
		t.Errorf("target = %v, want it unchanged: %v", got, before)
	}
}

func TestCopyConvAcceptsAnIdenticalTarget(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	files := sidecarConv(convX)
	writeConv(t, src, files, nil)
	writeConv(t, dst, files, nil)
	before := tree(t, dst)
	c, err := planCopy(src, dst, "repo", convX)
	if err != nil {
		t.Fatal(err)
	}
	undo, err := c.run()
	if err != nil {
		t.Fatalf("run onto an identical copy: %v", err)
	}
	if c.asided != 0 || c.asideDir != "" {
		t.Errorf("asided %d into %q, want nothing moved aside", c.asided, c.asideDir)
	}
	undo()
	if got := tree(t, dst); !mapsEqual(got, before) {
		t.Errorf("target after undo = %v, want the copy that was there kept: %v", got, before)
	}
}

func TestCopyConvCleansUpAPartialCopy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable file")
	}
	src, dst := t.TempDir(), t.TempDir()
	files := sidecarConv(convX)
	bad := filepath.Join("projects", "repo", convX, "tool-results", "z.txt") // copied after a.txt
	files[bad] = "unreadable\n"
	writeConv(t, src, files, map[string]fs.FileMode{bad: 0})
	if err := os.MkdirAll(filepath.Join(dst, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := copyConv(t, src, dst, "repo", convX); err == nil {
		t.Fatal("copyConv of an unreadable file = nil, want an error")
	}
	if got := tree(t, dst); !mapsEqual(got, map[string]string{"projects": "/"}) {
		t.Errorf("target = %v, want only the dir that was there before", got)
	}
}

// A copy that fails to put a file in place leaves no temp file behind.
func TestCopyConvLeavesNoTempFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeConv(t, src, sidecarConv(convX), nil)
	if err := os.MkdirAll(filepath.Join(dst, "projects", "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := planCopy(src, dst, "repo", convX)
	if err != nil {
		t.Fatal(err)
	}
	// A dir where the transcript goes, made after the plan, fails its link,
	// which comes after the sidecar's. It cannot be moved aside either: a file
	// holds the aside dir's place.
	if err := os.Mkdir(filepath.Join(dst, "projects", "repo", convX+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeConv(t, dst, convFiles{asideName: "in the way\n"}, nil)
	if _, err := c.run(); err == nil {
		t.Fatal("run onto a dir = nil error")
	}
	want := map[string]string{"projects": "/", "projects/repo": "/", "projects/repo/" + convX + ".jsonl": "/", asideName: "in the way\n"}
	if got := tree(t, dst); !mapsEqual(got, want) {
		t.Errorf("target = %v, want only the dir in the way: %v", got, want)
	}
}

// A file that appears in the target between the plan and the copy is never
// replaced: the same content is accepted and left to whoever made it, other
// content is moved aside once and the copy retried. One that keeps coming
// back is ErrMoveConflict.
func TestCopyConvNeverReplacesAFileThatAppears(t *testing.T) {
	log := filepath.Join("projects", "repo", convX+".jsonl")
	for _, c := range []struct {
		name, body string
		times      int // how often the file appears
	}{
		{"identical", sidecarConv(convX)[log], 1},
		{"different", "other\n", 1},
		{"different again", "other\n", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			writeConv(t, src, sidecarConv(convX), nil)
			cp, err := planCopy(src, dst, "repo", convX)
			if err != nil {
				t.Fatal(err)
			}
			orig := beforePublish
			t.Cleanup(func() { beforePublish = orig })
			n := 0
			beforePublish = func(to string) {
				if to == filepath.Join(dst, log) && n < c.times {
					n++
					writeConv(t, dst, convFiles{log: c.body}, nil)
				}
			}
			undo, err := cp.run()
			switch c.name {
			case "different again":
				if !errors.Is(err, ErrMoveConflict) {
					t.Fatalf("run err = %v, want ErrMoveConflict", err)
				}
				if got := tree(t, dst)[log]; got != c.body {
					t.Errorf("target %s = %q, want the file that appeared, as it was", log, got)
				}
				if got := tree(t, cp.asideDir)[log]; got != c.body {
					t.Errorf("aside %s = %q, want the first file that appeared", log, got)
				}
				rel, _ := filepath.Rel(dst, cp.asideDir)
				if !strings.Contains(err.Error(), rel) || strings.Contains(err.Error(), dst) {
					t.Errorf("err = %v, want it to name %s, not a profile dir", err, rel)
				}
				return
			case "different":
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				if got := asideFiles(t, dst, cp)[log]; got != c.body {
					t.Errorf("aside %s = %q, want the file that appeared", log, got)
				}
				if got := tree(t, dst)[log]; got != sidecarConv(convX)[log] {
					t.Errorf("target %s = %q, want the source's", log, got)
				}
			default:
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				if cp.asided != 0 {
					t.Errorf("asided = %d, want 0", cp.asided)
				}
				undo()
				want := map[string]string{"projects": "/", "projects/repo": "/", log: c.body}
				if got := tree(t, dst); !mapsEqual(got, want) {
					t.Errorf("target = %v, want only the file that appeared, as it was: %v", got, want)
				}
			}
		})
	}
}

// A file that differs at the plan and appears again before the copy: both
// copies end up in the aside dir, neither replaced.
func TestCopyConvKeepsBothAsideCopies(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	log := filepath.Join("projects", "repo", convX+".jsonl")
	writeConv(t, src, sidecarConv(convX), nil)
	writeConv(t, dst, convFiles{log: "planned\n"}, nil)
	cp, err := planCopy(src, dst, "repo", convX)
	if err != nil {
		t.Fatal(err)
	}
	orig := beforePublish
	t.Cleanup(func() { beforePublish = orig })
	once := false
	beforePublish = func(to string) {
		if to == filepath.Join(dst, log) && !once {
			once = true
			writeConv(t, dst, convFiles{log: "racer\n"}, nil)
		}
	}
	if _, err := cp.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	var bodies []string
	for rel, body := range asideFiles(t, dst, cp) {
		if body != "/" && strings.HasPrefix(rel, log) {
			bodies = append(bodies, body)
		}
	}
	slices.Sort(bodies)
	if !slices.Equal(bodies, []string{"planned\n", "racer\n"}) || cp.asided != 2 {
		t.Errorf("aside copies %q, asided %d; want planned and racer, 2", bodies, cp.asided)
	}
}

// A planned displacement whose file is gone by the time of the run has
// nothing to move aside.
func TestCopyConvSkipsADisplacementThatVanished(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	log := filepath.Join("projects", "repo", convX+".jsonl")
	writeConv(t, src, sidecarConv(convX), nil)
	writeConv(t, dst, convFiles{log: "other\n"}, nil)
	cp, err := planCopy(src, dst, "repo", convX)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dst, log)); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cp.asided != 0 || cp.asideDir != "" {
		t.Errorf("asided %d into %q, want nothing", cp.asided, cp.asideDir)
	}
	if got, want := tree(t, dst), tree(t, src); !mapsEqual(got, want) {
		t.Errorf("target = %v, want the source's files (and no aside dir): %v", got, want)
	}
}

// A displacement that fails after another succeeded puts the first back and
// leaves no aside dir.
func TestCopyConvRollsBackAPartialAside(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames out of a read-only dir")
	}
	src, dst := t.TempDir(), t.TempDir()
	log := filepath.Join("projects", "repo", convX+".jsonl")
	side := filepath.Join("projects", "repo", convX, "tool-results", "a.txt")
	writeConv(t, src, sidecarConv(convX), nil)
	writeConv(t, dst, convFiles{log: "old log\n", side: "old a\n"}, nil)
	before := tree(t, dst)
	cp, err := planCopy(src, dst, "repo", convX)
	if err != nil {
		t.Fatal(err)
	}
	// The sidecar file is displaced first; the transcript's parent refuses
	// the second rename.
	repo := filepath.Join(dst, "projects", "repo")
	if err := os.Chmod(repo, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err = cp.run()
	if err := os.Chmod(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, ErrMoveConflict) {
		t.Fatalf("run err = %v, want ErrMoveConflict", err)
	}
	if got := tree(t, dst); !mapsEqual(got, before) {
		t.Errorf("target = %v, want it as before: %v", got, before)
	}
}

// A dir that appears in the target between the plan and the copy is used,
// and left to whoever made it.
func TestCopyConvUsesADirThatAppears(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeConv(t, src, convFiles{filepath.Join("projects", "repo", convX+".jsonl"): "x\n"}, nil)
	cp, err := planCopy(src, dst, "repo", convX)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "projects", "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	undo, err := cp.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	undo()
	if got := tree(t, dst); !mapsEqual(got, map[string]string{"projects": "/", "projects/repo": "/"}) {
		t.Errorf("target after undo = %v, want the dirs that appeared kept", got)
	}
}

// A symlink in A is not copied: neither a sidecar entry nor the transcript.
func TestCopyConvRefusesSymlinks(t *testing.T) {
	log := filepath.Join("projects", "repo", convX+".jsonl")
	side := filepath.Join("projects", "repo", convX, "tool-results", "a.txt")
	for _, rel := range []string{side, log} {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			writeConv(t, src, sidecarConv(convX), nil)
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.WriteFile(target, []byte("outside\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(src, rel)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(src, rel)); err != nil {
				t.Fatal(err)
			}
			if _, err := copyConv(t, src, dst, "repo", convX); err == nil {
				t.Error("copyConv with a symlink = nil, want a refusal")
			}
			if got := tree(t, dst); len(got) != 0 {
				t.Errorf("target = %v, want nothing copied", got)
			}
		})
	}
}

// copyConv plans the copy and runs it.
func copyConv(t *testing.T, src, dst, slug, conv string) (undo func(), err error) {
	t.Helper()
	c, err := planCopy(src, dst, slug, conv)
	if err != nil {
		return nil, err
	}
	return c.run()
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// moveSetup is a private session idA with a titled transcript and a sidecar.
func moveSetup(t *testing.T) (*Manager, *fakeStore, homes) {
	t.Helper()
	fastStop(t)
	set, h := twoProfiles(t, "")
	writeConv(t, h.private, sidecarConv(idA), nil)
	fst := &fakeStore{stored: map[string]string{idA: "private"}}
	m := &Manager{Prefix: "test-rc", ClaudeBin: "claude", ScreenBin: "screen", Profiles: set, Store: fst}
	return m, fst, h
}

func TestMoveBetweenProfiles(t *testing.T) {
	for _, op := range []string{"restart running", "restart stopped", "resume"} {
		t.Run(op, func(t *testing.T) {
			m, fst, h := moveSetup(t)
			fs := &fakeScreen{parentEnv: true}
			if op == "restart running" {
				fs.created = []string{"test-rc-private-" + idA}
			}
			withFakeScreen(t, fs)
			t.Setenv("CLAUDE_CONFIG_DIR", h.private) // the caller's own, which the work child MUST NOT get
			want := tree(t, h.private)
			// What the launch sees: the copy in place, the Store still at A.
			var atLaunch map[string]string
			var storedAtLaunch string
			fs.onLaunch = func() { atLaunch, storedAtLaunch = tree(t, h.work), fst.stored[idA] }

			var s Session
			var err error
			if op == "resume" {
				s, err = m.ResumeIn(idA, "", "work")
			} else {
				s, err = m.RestartIn(idA, "", "work")
			}
			if err != nil {
				t.Fatalf("move: %v", err)
			}
			if op == "restart running" && !slices.Equal(fs.killed, []string{"test-rc-private-" + idA}) {
				t.Errorf("killed = %v, want the private screen stopped", fs.killed)
			}
			if !mapsEqual(atLaunch, want) || storedAtLaunch != "private" {
				t.Errorf("at launch: work = %v, stored %q; want the copy %v and private", atLaunch, storedAtLaunch, want)
			}
			if got := tree(t, h.private); !mapsEqual(got, want) {
				t.Errorf("private after the move = %v, want it intact", got)
			}
			name := "test-rc-work-" + idA
			args := "--resume " + idA + " --fork-session --remote-control=private title"
			if len(fs.launches) != 1 || launchedName(fs.launches[0]) != name || !strings.HasSuffix(fs.launches[0], args) {
				t.Fatalf("launches = %v, want one as %s ending in %q", fs.launches, name, args)
			}
			if v, set := envValue(fs.cmds[0].Env, "CLAUDE_CONFIG_DIR"); set {
				t.Errorf("work child has CLAUDE_CONFIG_DIR=%q, want it unset", v)
			}
			if s.Profile != "work" || s.Screen != name || fst.stored[idA] != "work" {
				t.Errorf("moved = %+v, stored %q; want work, %s", s, fst.stored[idA], name)
			}
			if c := fst.convs[idA]; c != "" && c != idA {
				t.Errorf("stored conversation = %q", c)
			}
		})
	}
}

// A move after claude moved the session onto another conversation keeps the
// title, which is set under the session id.
func TestMoveKeepsTheTitleOfTheSessionID(t *testing.T) {
	m, fst, _ := moveSetup(t)
	writeConv(t, m.profiles()[1].Dir, convFiles{filepath.Join("projects", "other", convX+".jsonl"): "{}\n"}, nil)
	fst.convs = map[string]string{idA: convX}
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	if _, err := m.ResumeIn(idA, "", "work"); err != nil {
		t.Fatal(err)
	}
	l, _ := m.List()
	if len(l) != 1 || l[0].Profile != "work" || l[0].Title != "private title" {
		t.Errorf("listed after the move = %+v, want it in work, titled \"private title\"", l)
	}
}

// A different copy that appears while the session stops is moved aside like
// any other; the Store keeps A until the launch.
func TestMoveAsideAfterTheStop(t *testing.T) {
	m, _, h := moveSetup(t)
	log := filepath.Join("projects", "repo", idA+".jsonl")
	fs := &fakeScreen{created: []string{"test-rc-private-" + idA}}
	fs.onKill = func() { writeConv(t, h.work, convFiles{log: "other\n"}, nil) }
	withFakeScreen(t, fs)
	s, err := m.RestartIn(idA, "", "work")
	if err != nil {
		t.Fatalf("err = %v, want the move to succeed", err)
	}
	if len(fs.launches) != 1 || s.MovedAside != 1 {
		t.Errorf("launched %v, moved aside %d; want one launch and one file aside", fs.launches, s.MovedAside)
	}
	if got := tree(t, h.work)[log]; got != sidecarConv(idA)[log] {
		t.Errorf("work %s = %q, want the source's", log, got)
	}
	if got := tree(t, s.AsideDir)[log]; got != "other\n" {
		t.Errorf("aside %s = %q, want the old file", log, got)
	}
}

// A move whose aside dir cannot be made fails as a conflict: API clients tell
// it by the sentinel at the start of the text. The text names no profile dir.
func TestMoveConflictWhenAsideFails(t *testing.T) {
	m, fst, h := moveSetup(t)
	log := filepath.Join("projects", "repo", idA+".jsonl")
	writeConv(t, h.work, convFiles{log: "other\n", "crctl-aside": "in the way\n"}, nil)
	before := tree(t, h.work)
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	_, err := m.ResumeIn(idA, "", "work")
	if !errors.Is(err, ErrMoveConflict) || !strings.HasPrefix(err.Error(), ErrMoveConflict.Error()) {
		t.Fatalf("err = %v, want it to start with %q", err, ErrMoveConflict)
	}
	if strings.Contains(err.Error(), h.work) || strings.Contains(err.Error(), h.private) {
		t.Errorf("err = %v names a profile dir", err)
	}
	if len(fs.launches) != 0 || fst.stored[idA] != "private" {
		t.Errorf("launched %v, stored %q; want nothing launched, private kept", fs.launches, fst.stored[idA])
	}
	if got := tree(t, h.work); !mapsEqual(got, before) {
		t.Errorf("work = %v, want it unchanged", got)
	}
}

// The conversation copied is the one resume uses: the Store's conv_id.
func TestMoveCopiesTheStoredConversation(t *testing.T) {
	m, fst, h := moveSetup(t)
	writeConv(t, h.private, convFiles{filepath.Join("projects", "repo", convX+".jsonl"): "{}\n"}, nil)
	fst.convs = map[string]string{idA: convX}
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	if _, err := m.ResumeIn(idA, "", "work"); err != nil {
		t.Fatal(err)
	}
	// The session id's own log comes along, for its title.
	if got, want := tree(t, h.work), tree(t, h.private); !mapsEqual(got, want) {
		t.Errorf("work = %v, want %s and %s copied: %v", got, convX, idA, want)
	}
	// Its title falls back to the one set under the session id, read in A.
	if want := "--resume " + convX + " --fork-session --remote-control=private title"; len(fs.launches) != 1 || !strings.HasSuffix(fs.launches[0], want) {
		t.Errorf("launches = %v, want one ending in %q", fs.launches, want)
	}
}

func TestMoveWithADir(t *testing.T) {
	m, fst, h := moveSetup(t)
	root := evalDir(t)
	m.WorkspaceRoot = root
	fs := &fakeScreen{created: []string{"test-rc-private-" + idA}}
	withFakeScreen(t, fs)
	if _, err := m.RestartIn(idA, root, "work"); err != nil {
		t.Fatal(err)
	}
	if len(fs.launches) != 1 || strings.Count(fs.launches[0], "--fork-session") != 1 ||
		launchedName(fs.launches[0]) != "test-rc-work-"+idA || fst.lastCwd != root || fst.stored[idA] != "work" {
		t.Errorf("launches %v, cwd %q, stored %q; want one fork in work from %s", fs.launches, fst.lastCwd, fst.stored[idA], root)
	}
	if tree(t, filepath.Join(h.work, "projects", "repo")) == nil {
		t.Error("no copy in work")
	}
}

func TestMoveRefusals(t *testing.T) {
	t.Run("unknown profile", func(t *testing.T) {
		m, fst, h := moveSetup(t)
		fs := &fakeScreen{created: []string{"test-rc-private-" + idA}}
		withFakeScreen(t, fs)
		if _, err := m.RestartIn(idA, "", "nope"); !errors.Is(err, ErrInvalidProfile) {
			t.Fatalf("err = %v, want ErrInvalidProfile", err)
		}
		if len(fs.killed)+len(fs.launches) != 0 || tree(t, filepath.Join(h.work, "projects")) != nil || fst.stored[idA] != "private" {
			t.Errorf("killed %v, launched %v; want nothing done", fs.killed, fs.launches)
		}
	})

	t.Run("own profile is a plain restart", func(t *testing.T) {
		m, fst, h := moveSetup(t)
		fs := &fakeScreen{created: []string{"test-rc-private-" + idA}}
		withFakeScreen(t, fs)
		if _, err := m.RestartIn(idA, "", "private"); err != nil {
			t.Fatal(err)
		}
		if len(fs.launches) != 1 || strings.Contains(fs.launches[0], "--fork-session") ||
			launchedName(fs.launches[0]) != "test-rc-private-"+idA || fst.stored[idA] != "private" {
			t.Errorf("launches = %v, stored %q; want a plain restart in private", fs.launches, fst.stored[idA])
		}
		if tree(t, filepath.Join(h.work, "projects")) != nil {
			t.Error("a plain restart copied the conversation")
		}
	})

	for _, op := range []string{"running", "stopped"} {
		t.Run("different copy in the target, "+op, func(t *testing.T) {
			m, fst, h := moveSetup(t)
			fs := &fakeScreen{}
			if op == "running" {
				fs.created = []string{"test-rc-private-" + idA}
			}
			withFakeScreen(t, fs)
			log := filepath.Join("projects", "repo", idA+".jsonl")
			writeConv(t, h.work, convFiles{log: "other\n"}, nil)
			s, err := m.RestartIn(idA, "", "work")
			if err != nil {
				t.Fatalf("err = %v, want the old copy moved aside", err)
			}
			if len(fs.launches) != 1 || fst.stored[idA] != "work" {
				t.Errorf("launched %v, stored %q; want it moved to work", fs.launches, fst.stored[idA])
			}
			if got := tree(t, h.work)[log]; got != sidecarConv(idA)[log] {
				t.Errorf("work %s = %q, want the source's", log, got)
			}
			if s.MovedAside != 1 || tree(t, s.AsideDir)[log] != "other\n" {
				t.Errorf("moved aside %d into %q; want the old file there", s.MovedAside, s.AsideDir)
			}
		})
	}
}

func TestMoveAcceptsAnIdenticalCopy(t *testing.T) {
	m, fst, h := moveSetup(t)
	writeConv(t, h.work, sidecarConv(idA), nil)
	fs := &fakeScreen{}
	withFakeScreen(t, fs)
	if _, err := m.ResumeIn(idA, "", "work"); err != nil {
		t.Fatalf("repeated move: %v", err)
	}
	if len(fs.launches) != 1 || fst.stored[idA] != "work" {
		t.Errorf("launches %v, stored %q; want it moved", fs.launches, fst.stored[idA])
	}
}

func TestFailedMoveLaunch(t *testing.T) {
	for _, pre := range []string{"empty target", "identical copy in the target", "different copy in the target"} {
		t.Run(pre, func(t *testing.T) {
			m, fst, h := moveSetup(t)
			switch pre {
			case "identical copy in the target":
				writeConv(t, h.work, sidecarConv(idA), nil)
			case "different copy in the target":
				writeConv(t, h.work, convFiles{filepath.Join("projects", "repo", idA+".jsonl"): "other\n"}, nil)
			}
			before, private := tree(t, h.work), tree(t, h.private)
			fs := &fakeScreen{failCreate: true}
			withFakeScreen(t, fs)
			if _, err := m.ResumeIn(idA, "", "work"); err == nil {
				t.Fatal("move with a failing launch = nil error")
			}
			if fst.stored[idA] != "private" || len(fst.recorded) != 0 {
				t.Errorf("stored %q, recorded %v; want the Store left at private", fst.stored[idA], fst.recorded)
			}
			if got := tree(t, h.work); !mapsEqual(got, before) {
				t.Errorf("work = %v, want it as before the move: %v", got, before)
			}
			if got := tree(t, h.private); !mapsEqual(got, private) {
				t.Errorf("private = %v, want it intact", got)
			}
		})
	}
}
