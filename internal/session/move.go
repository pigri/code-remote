package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claude-remote-api/internal/profile"
)

// ErrMoveConflict is returned (wrapped) by a move between profiles when a
// different copy of a file the move would copy is in the target profile and
// cannot be moved aside (see convCopy.run). Nothing is overwritten. Callers
// can errors.Is to surface it as a 409.
var ErrMoveConflict = errors.New("conversation already exists in the target profile")

// convCopy is a planned copy of one conversation's files from one profile
// dir into another: what is missing in the target, parents first and the
// transcript last. What is in the way there is displaced: moved aside before
// the copy.
type convCopy struct {
	root     string // the target profile dir
	displace []copyEntry
	dirs     []copyEntry
	files    []copyEntry

	asideDir  string   // set by run: where it moved displaced entries; "" if none
	asideMade []string // the dirs run made under asideDir, parents first
	asided    int      // regular files in them
}

// copyEntry is one dir or file of a convCopy, with the mode it gets.
type copyEntry struct {
	rel, src, dst string
	mode          fs.FileMode
}

// plainName reports whether name is one path element that stays where it is
// joined: not "", "." or "..", and with no separator.
func plainName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`)
}

// planCopy plans the copy of conversation conv from profile dir src into
// dst: its transcript projects/<slug>/<conv>.jsonl, and the sidecar dir
// projects/<slug>/<conv>/ when there is one. A target file that already
// exists with the same content is left as it is; one with other content, or
// of the wrong type, is displaced.
func planCopy(src, dst, slug, conv string) (*convCopy, error) {
	c := &convCopy{}
	return c, c.add(src, dst, slug, conv)
}

// add is planCopy into c.
func (c *convCopy) add(src, dst, slug, conv string) error {
	if !plainName(slug) || !plainName(conv) {
		return fmt.Errorf("not copying conversation %q of project %q: not a plain file name", conv, slug)
	}
	if src == "" || dst == "" {
		return errors.New("a move between profiles needs both profile dirs")
	}
	c.root = dst
	rel := filepath.Join("projects", slug)
	for _, d := range []string{"projects", rel} {
		if err := c.addDir(src, dst, d); err != nil {
			return err
		}
	}
	side := filepath.Join(rel, conv)
	if _, err := os.Lstat(filepath.Join(src, side)); err == nil {
		err := filepath.WalkDir(filepath.Join(src, side), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			r, _ := filepath.Rel(src, p)
			switch {
			case d.IsDir():
				return c.addDir(src, dst, r)
			case d.Type().IsRegular():
				return c.addFile(src, dst, r)
			}
			return fmt.Errorf("not copying %s: not a regular file", r)
		})
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return c.addFile(src, dst, filepath.Join(rel, conv+".jsonl"))
}

// missing reports whether err from a stat says the path is not there, or
// cannot be because a parent is not a dir.
func missing(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)
}

// displaceOnce plans to move aside what is at to, which is rel in the target.
func (c *convCopy) displaceOnce(rel, to string) {
	for _, d := range c.displace {
		if d.dst == to {
			return
		}
	}
	c.displace = append(c.displace, copyEntry{rel: rel, dst: to})
}

// addDir plans dir rel of src for a target that lacks it, or has something
// else there, which is displaced. A dir planned twice is made once (see run).
func (c *convCopy) addDir(src, dst, rel string) error {
	from, to := filepath.Join(src, rel), filepath.Join(dst, rel)
	sfi, err := os.Stat(from)
	if err != nil {
		return err
	}
	switch dfi, err := os.Stat(to); {
	case missing(err):
	case err != nil:
		return err
	case !dfi.IsDir():
		c.displaceOnce(rel, to)
	default:
		return nil
	}
	c.dirs = append(c.dirs, copyEntry{rel, from, to, sfi.Mode().Perm()})
	return nil
}

// addFile plans file rel of src for a target that lacks it, or has other
// content or another type there, which is displaced.
func (c *convCopy) addFile(src, dst, rel string) error {
	from, to := filepath.Join(src, rel), filepath.Join(dst, rel)
	sfi, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !sfi.Mode().IsRegular() {
		return fmt.Errorf("not copying %s: not a regular file", rel)
	}
	if _, err := os.Lstat(to); !missing(err) {
		same, err := sameFile(from, to)
		if err != nil {
			return err
		}
		if same {
			return nil
		}
		c.displaceOnce(rel, to)
	}
	c.files = append(c.files, copyEntry{rel, from, to, sfi.Mode().Perm()})
	return nil
}

// sameFile reports whether to is a regular file with the content of from.
func sameFile(from, to string) (bool, error) {
	sfi, err := os.Lstat(from)
	if err != nil {
		return false, err
	}
	switch dfi, err := os.Lstat(to); {
	case err != nil:
		return false, err
	case !dfi.Mode().IsRegular() || dfi.Size() != sfi.Size():
		return false, nil
	}
	a, err := os.ReadFile(from)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(to)
	if err != nil {
		return false, err
	}
	return bytes.Equal(a, b), nil
}

// asideName is the dir of the target profile that displaced entries go to,
// outside projects/ so claude does not list them as conversations. crctl
// never deletes from it.
const asideName = "crctl-aside"

// errDiffers is copyFile's: the file at its target has other content.
var errDiffers = errors.New("a different file is in the way")

// asideMove is one displaced entry: where it was and where it went.
type asideMove struct{ from, to string }

// reason is err without the paths it names, which are the profile dirs.
func reason(err error) error {
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		return pe.Err
	case errors.As(err, &le):
		return le.Err
	}
	return err
}

// makeAsideDir makes <root>/crctl-aside/<UTC stamp>, a fresh dir for each run
// so a rename into it never replaces anything.
func (c *convCopy) makeAsideDir() (string, error) {
	base := filepath.Join(c.root, asideName)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; ; i++ {
		d := filepath.Join(base, stamp)
		if i > 0 {
			d += "-" + strconv.Itoa(i)
		}
		err := os.Mkdir(d, 0o700)
		if err == nil || !errors.Is(err, fs.ErrExist) || i >= 100 {
			return d, err
		}
	}
}

// fileCount is the regular files at p: 1 for a file, those below it for a dir.
func fileCount(p string) int {
	n := 0
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// renameIfAbsent is renameNoReplace for a filesystem without the kernel's
// flag; it can lose a race that the flag cannot.
func renameIfAbsent(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrExist}
	}
	return os.Rename(from, to)
}

// moveAside renames from into the aside dir as rel, never onto an existing
// path: a taken name gets a numeric suffix. A from that is gone has nothing to
// move, and is reported as not moved. The dir is made on first use.
func (c *convCopy) moveAside(rel, from string) (moved *asideMove, err error) {
	fail := func(err error) error {
		return fmt.Errorf("%w: cannot move %s aside: %v", ErrMoveConflict, rel, reason(err))
	}
	if c.asideDir == "" {
		d, err := c.makeAsideDir()
		if err != nil {
			return nil, fail(err)
		}
		c.asideDir, c.asideMade = d, []string{d}
	}
	to := filepath.Join(c.asideDir, rel)
	if err := c.mkdirAside(filepath.Dir(to)); err != nil {
		return nil, fail(err)
	}
	n := fileCount(from)
	for i := 0; ; i++ {
		dst := to
		if i > 0 {
			dst += "." + strconv.Itoa(i)
		}
		err := renameNoReplace(from, dst)
		switch {
		case err == nil:
			c.asided += n
			return &asideMove{from, dst}, nil
		case errors.Is(err, fs.ErrNotExist):
			return nil, nil
		case !errors.Is(err, fs.ErrExist) || i >= 100:
			return nil, fail(err)
		}
	}
}

// mkdirAside makes dir, which is under the aside dir, noting each dir it makes.
func (c *convCopy) mkdirAside(dir string) error {
	var made []string
	for p := dir; p != c.asideDir; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		}
		made = append(made, p)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for i := len(made) - 1; i >= 0; i-- {
		c.asideMade = append(c.asideMade, made[i])
	}
	return nil
}

// tidyAside removes the aside dirs run made, when they are empty, and
// crctl-aside when it ends up empty. A dir that was displaced is never one of
// them. It returns the aside dir's path relative to the profile if something
// is left in it, else "".
func (c *convCopy) tidyAside() string {
	if c.asideDir == "" {
		return ""
	}
	for i := len(c.asideMade) - 1; i >= 0; i-- {
		_ = os.Remove(c.asideMade[i])
	}
	_ = os.Remove(filepath.Dir(c.asideDir))
	if _, err := os.Lstat(c.asideDir); err != nil {
		c.asideDir, c.asideMade = "", nil
		return ""
	}
	rel, _ := filepath.Rel(c.root, c.asideDir)
	return rel
}

// run carries out the copy. What is displaced (see planCopy) is first renamed
// into a fresh dir under the target profile, so nothing is overwritten or
// deleted; if that cannot be done the error is ErrMoveConflict. An entry that
// appeared in the target since the plan is never replaced or claimed: a dir is
// used, a file must have the same content (see copyFile) or is displaced, once.
// On failure run removes what it created and puts back what it moved aside,
// then returns the error, naming the aside dir if something could not be put
// back; on success, undo does the same.
func (c *convCopy) run() (undo func(), err error) {
	var madeFiles, madeDirs []string
	var moved []asideMove
	// remove undoes run in order: the files it made, then its dirs (a dir
	// may stand where a displaced file was), then the displaced entries.
	remove := func() string {
		for i := len(madeFiles) - 1; i >= 0; i-- {
			_ = os.Remove(madeFiles[i])
		}
		for i := len(madeDirs) - 1; i >= 0; i-- {
			_ = os.Remove(madeDirs[i])
		}
		for i := len(moved) - 1; i >= 0; i-- {
			_ = renameNoReplace(moved[i].to, moved[i].from)
		}
		return c.tidyAside()
	}
	fail := func(err error) (func(), error) {
		if left := remove(); left != "" {
			err = fmt.Errorf("%w (some files could not be put back; they are in %s)", err, left)
		}
		return nil, err
	}
	displace := func(rel, from string) error {
		m, err := c.moveAside(rel, from)
		if m != nil {
			moved = append(moved, *m)
		}
		return err
	}
	for _, d := range c.displace {
		if err := displace(d.rel, d.dst); err != nil {
			return fail(err)
		}
	}
	for _, d := range c.dirs {
		if err := os.Mkdir(d.dst, d.mode); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return fail(err)
		}
		madeDirs = append(madeDirs, d.dst)
		if err := os.Chmod(d.dst, d.mode); err != nil {
			return fail(err)
		}
	}
	for _, f := range c.files {
		created, err := copyFile(f)
		if errors.Is(err, errDiffers) {
			if err = displace(f.rel, f.dst); err == nil {
				created, err = copyFile(f)
			}
			if errors.Is(err, errDiffers) {
				err = fmt.Errorf("%w: %s keeps changing there", ErrMoveConflict, f.rel)
			}
		}
		if err != nil {
			return fail(err)
		}
		if created {
			madeFiles = append(madeFiles, f.dst)
		}
	}
	c.tidyAside()
	return func() { remove() }, nil
}

// beforePublish runs just before copyFile puts a file in place; a test seam.
var beforePublish = func(dst string) {}

// copyFile copies f with its mode. It writes a temp file beside f.dst and
// links it into place, which never replaces a file, so f.dst is never partial
// or overwritten. A file already at f.dst is kept when it has the same
// content (created is false) and is errDiffers otherwise.
func copyFile(f copyEntry) (created bool, err error) {
	in, err := os.Open(f.src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(f.dst), "."+filepath.Base(f.dst)+".tmp-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Chmod(f.mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	beforePublish(f.dst)
	if err := os.Link(tmp.Name(), f.dst); errors.Is(err, fs.ErrExist) {
		same, err := sameFile(f.src, f.dst)
		if err == nil && !same {
			err = errDiffers
		}
		return false, err
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// planMove plans the copy of session id's conversation conv, logged in profile
// from, into profile to, and returns it. When conv is not
// id, id's own log is copied too, when it has one: the session's title is set
// there. Its errors start with the sentinel's text, which the API's clients
// match on.
func (m *Manager) planMove(id string, from, to profile.Profile, conv string) (*convCopy, error) {
	log := logIn(from.Dir, conv)
	if log == "" {
		return nil, fmt.Errorf("%w: no claude session log for %s in profile %q", ErrNotResumable, conv, from.Name)
	}
	c := &convCopy{}
	err := c.add(from.Dir, to.Dir, filepath.Base(filepath.Dir(log)), conv)
	if own := logIn(from.Dir, id); err == nil && conv != id && own != "" {
		err = c.add(from.Dir, to.Dir, filepath.Base(filepath.Dir(own)), id)
	}
	if err != nil {
		return nil, fmt.Errorf("%w (moving to profile %q)", err, to.Name)
	}
	return c, nil
}
