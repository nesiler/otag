//go:build !darwin

package setup

// RenameNoReplace moves from to to, failing with fs.ErrExist when to exists.
func RenameNoReplace(from, to string) error { return renameNoReplace(from, to) }

// renameNoReplace moves from to to, failing with fs.ErrExist when to exists (otag
// runs on macOS; elsewhere a hard link and an unlink).
func renameNoReplace(from, to string) error { return linkRename(from, to) }
