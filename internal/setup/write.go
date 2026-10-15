package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/apperr"
)

// New config files are 0644 (registry.toml and the project files hold no secrets of
// their own; an env table that does is the user's to protect). A file setup changes
// keeps its mode, and so does its backup (the old file itself, moved aside).
const newFileMode = 0o644

// writeHook, when set, runs at the named steps of a replace (tests: an editor saving
// at the worst moment).
var writeHook func(step, path string)

func hook(step, path string) {
	if writeHook != nil {
		writeHook(step, path)
	}
}

// Write writes files under the config lock (LockConfig) the caller holds. Editors do
// not take that lock, so a replace never trusts a check made earlier:
//
//   - create: a temporary file renamed into place without replacing (RENAME_EXCL):
//     a file there (another setup, an editor) is never replaced;
//   - append, replace: the new content goes to a temporary file first. Then the
//     current file is moved aside (rename, to the backup name or a hidden name) and
//     the moved file is hashed: if it is not the content the change was computed from
//     (File.Base), it is moved back and nothing is written. If it is, the new file is
//     renamed into place without replacing: a save that landed after the move
//     recreated the file, so the placement fails and both are kept (the save in
//     place, the old content aside). Only then is the aside copy removed (append) or
//     kept as the backup (replace; File.Backup gets the name used).
//
// The moved-aside file is never lost: every failure leaves it in place or under a
// name the error gives. A reader sees the old or the new file (or, for an instant,
// none), never a part.
func Write(files []File) error {
	for i := range files {
		f := &files[i]
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return err
		}
		if f.Action == Create {
			if err := CreateNew(f.Path, f.Content, newFileMode); err != nil {
				if errors.Is(err, fs.ErrExist) {
					return apperr.New(apperr.Conflict, "%s already exists (written meanwhile?): not replaced", f.Path)
				}
				return err
			}
			continue
		}
		if err := replace(f); err != nil {
			return err
		}
	}
	return nil
}

// replace puts f.Content at f.Path (append, replace) as Write describes.
func replace(f *File) error {
	st, err := os.Stat(f.Path)
	if err != nil {
		return err
	}
	tmp, err := writeTemp(f.Path, f.Content, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }() // a no-op once it is in place
	aside := f.Backup
	if aside == "" {
		aside = filepath.Join(filepath.Dir(f.Path), "."+filepath.Base(f.Path)+".otag-old")
	}
	hook("aside", f.Path)
	if aside, err = RenameUnique(f.Path, aside); err != nil {
		return err
	}
	old, err := os.ReadFile(aside)
	if err == nil && f.Base != "" && Sum(old) != f.Base {
		err = errChanged(f.Path)
	}
	if err != nil {
		if rerr := renameNoReplace(aside, f.Path); rerr != nil {
			return apperr.New(apperr.Conflict, "%v; %s was saved again meanwhile: that save is kept, and the version before it is %s",
				errText(err), f.Path, aside)
		}
		return err
	}
	hook("place", f.Path)
	if err := renameNoReplace(tmp, f.Path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return apperr.New(apperr.Conflict, "%s was saved while otag was writing it: your save is kept and otag's change was not written "+
				"(the version before your save is %s); run the command again", f.Path, aside)
		}
		if rerr := renameNoReplace(aside, f.Path); rerr != nil {
			return fmt.Errorf("%w (and %s is still at %s: %v)", err, f.Path, aside, rerr)
		}
		return err
	}
	if f.Backup != "" {
		f.Backup = aside
		return nil
	}
	return os.Remove(aside)
}

func errText(err error) string {
	if e, ok := apperr.As(err); ok {
		return e.Message
	}
	return err.Error()
}

// CreateNew writes data to path only if nothing is there: a temporary file in the
// same directory, renamed into place without replacing (an existing path is
// fs.ErrExist).
func CreateNew(path string, data []byte, perm os.FileMode) error {
	_, err := createAt(path, data, perm, false)
	return err
}

// CreateUnique is CreateNew at base, else base-2, base-3, ...; it returns the path used.
func CreateUnique(base string, data []byte, perm os.FileMode) (string, error) {
	return createAt(base, data, perm, true)
}

func createAt(base string, data []byte, perm os.FileMode, number bool) (string, error) {
	tmp, err := writeTemp(base, data, perm)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()
	if !number {
		return base, renameNoReplace(tmp, base)
	}
	return RenameUnique(tmp, base)
}

// RenameUnique moves from to base (else base-2, base-3, ...) without ever replacing
// a file there, and returns the new path.
func RenameUnique(from, base string) (string, error) {
	for n := 1; ; n++ {
		p := numbered(base, n)
		err := renameNoReplace(from, p)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return p, nil
	}
}

// linkRename is a no-replace rename through a hard link (an existing target is
// fs.ErrExist) and an unlink of the old name.
func linkRename(from, to string) error {
	if err := os.Link(from, to); err != nil {
		return err
	}
	return os.Remove(from)
}

func numbered(base string, n int) string {
	if n == 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, n)
}

func writeTemp(path string, data []byte, perm os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// WriteAtomic writes data to path through a temporary file in the same directory
// and a rename (replacing what is there).
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := writeTemp(path, data, perm)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// LockFile is the lock that serializes otag's changes to the config dir.
func LockFile(dir string) string { return filepath.Join(dir, ".otag-config.lock") }

// LockConfig takes the config dir's lock (an flock, waiting up to wait), so two
// `otag init` / `otag project` commands never read and write the config at once.
// The returned function releases it; the file stays.
func LockConfig(dir string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(LockFile(dir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, apperr.Retryable(apperr.Conflict, "another otag command is changing %s; try again", dir)
		}
		return nil, fmt.Errorf("lock %s: %w", LockFile(dir), err)
	}
	return func() { _ = f.Close() }, nil
}
