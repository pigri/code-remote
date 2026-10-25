//go:build !linux

package session

// renameNoReplace renames from to to, and fails with an error that is
// fs.ErrExist rather than replace to.
func renameNoReplace(from, to string) error { return renameIfAbsent(from, to) }
