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
// their own; an env table that does is the user's to protect); a file setup changes
// keeps its mode, a backup is 0600.
const newFileMode = 0o644

// Write writes files under the config lock (LockConfig) the caller holds:
//
//   - create: a temporary file linked into place, so an existing file (another
//     setup, an editor) is never replaced;
//   - append, replace: the file must still have the content the change was computed
//     from (File.Base), else nothing more is written; a replaced file is first copied
//     to a backup that never overwrites anything (File.Backup gets the name used);
//     then a temporary file with the old mode is renamed over it.
//
// A reader always sees the old or the new content, never a part.
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
		st, err := os.Stat(f.Path)
		if err != nil {
			return err
		}
		old, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		if f.Base != "" && Sum(old) != f.Base {
			return errChanged(f.Path)
		}
		if f.Backup != "" {
			if f.Backup, err = CreateUnique(f.Backup, old, 0o600); err != nil {
				return fmt.Errorf("back up %s: %w", f.Path, err)
			}
		}
		if err := WriteAtomic(f.Path, f.Content, st.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// CreateNew writes data to path only if nothing is there: a temporary file in the
// same directory, hard-linked to path (link never replaces; an existing path is
// fs.ErrExist), then the temporary name is removed.
func CreateNew(path string, data []byte, perm os.FileMode) error {
	tmp, err := writeTemp(path, data, perm)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return os.Link(tmp, path)
}

// CreateUnique is CreateNew at base, else base-2, base-3, ...; it returns the path used.
func CreateUnique(base string, data []byte, perm os.FileMode) (string, error) {
	for n := 1; ; n++ {
		p := numbered(base, n)
		err := CreateNew(p, data, perm)
		if !errors.Is(err, fs.ErrExist) {
			return p, err
		}
	}
}

// RenameUnique moves from to base (else base-2, base-3, ...) without ever replacing
// a file there, and returns the new path.
func RenameUnique(from, base string) (string, error) {
	for n := 1; ; n++ {
		p := numbered(base, n)
		err := os.Link(from, p)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return p, os.Remove(from)
	}
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
