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

func TestCopyConvRefusesADifferentTarget(t *testing.T) {
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
				before := tree(t, dst)
				_, err := copyConv(t, src, dst, "repo", convX)
				if !errors.Is(err, ErrMoveConflict) {
					t.Fatalf("copyConv err = %v, want ErrMoveConflict", err)
				}
				if got := tree(t, dst); !mapsEqual(got, before) {
					t.Errorf("target = %v, want it unchanged: %v", got, before)
				}
			})
		}
	}
}

func TestCopyConvAcceptsAnIdenticalTarget(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	files := sidecarConv(convX)
	writeConv(t, src, files, nil)
	writeConv(t, dst, files, nil)
	before := tree(t, dst)
	undo, err := copyConv(t, src, dst, "repo", convX)
	if err != nil {
		t.Fatalf("copyConv onto an identical copy: %v", err)
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
	// which comes after the sidecar's.
	if err := os.Mkdir(filepath.Join(dst, "projects", "repo", convX+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.run(); err == nil {
		t.Fatal("run onto a dir = nil error")
	}
	want := map[string]string{"projects": "/", "projects/repo": "/", "projects/repo/" + convX + ".jsonl": "/"}
	if got := tree(t, dst); !mapsEqual(got, want) {
		t.Errorf("target = %v, want only the dir in the way: %v", got, want)
	}
}

// A file that appears in the target between the plan and the copy is never
// replaced: the same content is accepted and left to whoever made it, other
// content is ErrMoveConflict.
func TestCopyConvNeverReplacesAFileThatAppears(t *testing.T) {
	log := filepath.Join("projects", "repo", convX+".jsonl")
	for _, c := range []struct{ name, body string }{
		{"identical", sidecarConv(convX)[log]},
		{"different", "other\n"},
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
			beforePublish = func(to string) {
				if to == filepath.Join(dst, log) {
					writeConv(t, dst, convFiles{log: c.body}, nil)
				}
			}
			undo, err := cp.run()
			if c.name == "different" {
				if !errors.Is(err, ErrMoveConflict) {
					t.Fatalf("run err = %v, want ErrMoveConflict", err)
				}
			} else {
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				undo()
			}
			want := map[string]string{"projects": "/", "projects/repo": "/", log: c.body}
			if got := tree(t, dst); !mapsEqual(got, want) {
				t.Errorf("target = %v, want only the file that appeared, as it was: %v", got, want)
			}
		})
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

// A conflict that appears while the session stops is still refused: nothing
// is launched or overwritten, and the Store keeps A.
func TestMoveConflictAfterTheStop(t *testing.T) {
	m, fst, h := moveSetup(t)
	log := filepath.Join("projects", "repo", idA+".jsonl")
	fs := &fakeScreen{created: []string{"test-rc-private-" + idA}}
	fs.onKill = func() { writeConv(t, h.work, convFiles{log: "other\n"}, nil) }
	withFakeScreen(t, fs)
	if _, err := m.RestartIn(idA, "", "work"); !errors.Is(err, ErrMoveConflict) {
		t.Fatalf("err = %v, want ErrMoveConflict", err)
	}
	if len(fs.killed) != 1 || len(fs.launches) != 0 || fst.stored[idA] != "private" {
		t.Errorf("killed %v, launched %v, stored %q; want stopped, nothing launched, private kept",
			fs.killed, fs.launches, fst.stored[idA])
	}
	if got := tree(t, h.work); !mapsEqual(got, map[string]string{"projects": "/", "projects/repo": "/", log: "other\n"}) {
		t.Errorf("work = %v, want only the conflicting file", got)
	}
}

// A conflict found only when a file is put in place reads as one too: API
// clients tell it by the sentinel at the start of the text. The text names
// no profile dir.
func TestMoveConflictAtPublish(t *testing.T) {
	m, fst, h := moveSetup(t)
	log := filepath.Join("projects", "repo", idA+".jsonl")
	orig := beforePublish
	t.Cleanup(func() { beforePublish = orig })
	beforePublish = func(to string) {
		if to == filepath.Join(h.work, log) {
			writeConv(t, h.work, convFiles{log: "other\n"}, nil)
		}
	}
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
	if got := tree(t, h.work); !mapsEqual(got, map[string]string{"projects": "/", "projects/repo": "/", log: "other\n"}) {
		t.Errorf("work = %v, want only the file that appeared", got)
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
			writeConv(t, h.work, convFiles{filepath.Join("projects", "repo", idA+".jsonl"): "other\n"}, nil)
			before := tree(t, h.work)
			if _, err := m.RestartIn(idA, "", "work"); !errors.Is(err, ErrMoveConflict) {
				t.Fatalf("err = %v, want ErrMoveConflict", err)
			}
			if len(fs.killed)+len(fs.launches) != 0 || fst.stored[idA] != "private" {
				t.Errorf("killed %v, launched %v, stored %q; want the session left alone in private",
					fs.killed, fs.launches, fst.stored[idA])
			}
			if got := tree(t, h.work); !mapsEqual(got, before) {
				t.Errorf("work = %v, want it unchanged", got)
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
	for _, pre := range []string{"empty target", "identical copy in the target"} {
		t.Run(pre, func(t *testing.T) {
			m, fst, h := moveSetup(t)
			if pre != "empty target" {
				writeConv(t, h.work, sidecarConv(idA), nil)
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
