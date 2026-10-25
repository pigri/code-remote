//go:build linux

package session

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames from to to, and fails with an error that is
// fs.ErrExist rather than replace to.
func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) {
		return renameIfAbsent(from, to) // a filesystem without the flag
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
