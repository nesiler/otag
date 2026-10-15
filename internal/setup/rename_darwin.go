package setup

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// RenameNoReplace moves from to to, failing with fs.ErrExist when to exists (never
// a hard link where the file system can rename exclusively).
func RenameNoReplace(from, to string) error { return renameNoReplace(from, to) }

// renameNoReplace moves from to to, failing with fs.ErrExist when to exists. On
// macOS this is one renamex_np(RENAME_EXCL) call, which works on every file system
// that supports rename (also exFAT, where hard links do not exist); only a file
// system without RENAME_EXCL falls back to a hard link and an unlink.
func renameNoReplace(from, to string) error {
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return linkRename(from, to)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
