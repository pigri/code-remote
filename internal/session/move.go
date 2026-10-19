package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"claude-remote-api/internal/profile"
)

// ErrMoveConflict is returned (wrapped) by a move between profiles when the
// target profile already holds a different copy of a file the move would
// copy. Nothing is overwritten. Callers can errors.Is to surface it as a 409.
var ErrMoveConflict = errors.New("conversation already exists in the target profile")

// convCopy is a planned copy of one conversation's files from one profile
// dir into another: what is missing in the target, parents first and the
// transcript last.
type convCopy struct {
	dirs  []copyEntry
	files []copyEntry
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
// exists with the same content is left as it is; one with other content is
// ErrMoveConflict.
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

// addDir plans dir rel of src for a target that lacks it. A dir planned
// twice is made once (see run).
func (c *convCopy) addDir(src, dst, rel string) error {
	from, to := filepath.Join(src, rel), filepath.Join(dst, rel)
	sfi, err := os.Stat(from)
	if err != nil {
		return err
	}
	switch dfi, err := os.Stat(to); {
	case os.IsNotExist(err):
		c.dirs = append(c.dirs, copyEntry{rel, from, to, sfi.Mode().Perm()})
		return nil
	case err != nil:
		return err
	case !dfi.IsDir():
		return fmt.Errorf("%w: %s is not a directory there", ErrMoveConflict, rel)
	}
	return nil
}

// addFile plans file rel of src for a target that lacks it.
func (c *convCopy) addFile(src, dst, rel string) error {
	from, to := filepath.Join(src, rel), filepath.Join(dst, rel)
	sfi, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !sfi.Mode().IsRegular() {
		return fmt.Errorf("not copying %s: not a regular file", rel)
	}
	if _, err := os.Lstat(to); os.IsNotExist(err) {
		c.files = append(c.files, copyEntry{rel, from, to, sfi.Mode().Perm()})
		return nil
	}
	return sameFile(rel, from, to)
}

// sameFile is nil when file to has the content of from, else ErrMoveConflict
// naming rel.
func sameFile(rel, from, to string) error {
	differs := fmt.Errorf("%w: %s differs there; not overwriting it", ErrMoveConflict, rel)
	sfi, err := os.Lstat(from)
	if err != nil {
		return err
	}
	switch dfi, err := os.Lstat(to); {
	case err != nil:
		return err
	case !dfi.Mode().IsRegular() || dfi.Size() != sfi.Size():
		return differs
	}
	a, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(to)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return differs
	}
	return nil
}

// run carries out the copy. An entry that appeared in the target since the
// plan is never replaced or claimed: a dir is used, a file must have the same
// content (see copyFile). On failure run removes what it created and returns
// the error; on success, undo does that.
func (c *convCopy) run() (undo func(), err error) {
	var made []string
	remove := func() {
		for i := len(made) - 1; i >= 0; i-- {
			_ = os.Remove(made[i])
		}
	}
	for _, d := range c.dirs {
		if err := os.Mkdir(d.dst, d.mode); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			remove()
			return nil, err
		}
		made = append(made, d.dst)
		if err := os.Chmod(d.dst, d.mode); err != nil {
			remove()
			return nil, err
		}
	}
	for _, f := range c.files {
		created, err := copyFile(f)
		if err != nil {
			remove()
			return nil, err
		}
		if created {
			made = append(made, f.dst)
		}
	}
	return remove, nil
}

// beforePublish runs just before copyFile puts a file in place; a test seam.
var beforePublish = func(dst string) {}

// copyFile copies f with its mode. It writes a temp file beside f.dst and
// links it into place, which never replaces a file, so f.dst is never partial
// or overwritten. A file already at f.dst is kept when it has the same
// content (created is false) and is ErrMoveConflict otherwise.
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
		return false, sameFile(f.rel, f.src, f.dst)
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// planMove checks that session id's conversation conv, logged in profile
// from, can be copied into profile to, and returns the copy. When conv is not
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
