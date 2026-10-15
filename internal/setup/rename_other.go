//go:build !darwin

package setup

// renameNoReplace moves from to to, failing with fs.ErrExist when to exists (otag
// runs on macOS; elsewhere a hard link and an unlink).
func renameNoReplace(from, to string) error { return linkRename(from, to) }
