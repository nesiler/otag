package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/apperr"
)

// LockFile is the single-instance lock in the data dir; it holds the daemon's pid.
func LockFile(dataDir string) string { return filepath.Join(dataDir, "daemon.lock") }

// Lock is the held single-instance lock (flock on <data>/daemon.lock).
type Lock struct {
	f    *os.File
	Path string
}

// Acquire takes the lock or fails with CONFLICT when another daemon holds it. The
// file is kept on release (removing it would race with a starting daemon).
func Acquire(dataDir string) (*Lock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := LockFile(dataDir)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	// A status probe holds a shared lock for an instant: retry briefly.
	for i := 0; ; i++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || i == 10 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			_, pid, _ := Probe(dataDir)
			return nil, apperr.New(apperr.Conflict, "otag daemon is already running (pid %d, lock %s)", pid, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &Lock{f: f, Path: path}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = l.f.Truncate(0)
	err := l.f.Close() // closing the last descriptor releases the flock
	l.f = nil
	return err
}

// Probe reports whether a daemon holds the lock, and its pid.
func Probe(dataDir string) (running bool, pid int, err error) {
	f, err := os.Open(LockFile(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, 0, nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, 0, err
	}
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, _ = strconv.Atoi(strings.TrimSpace(string(b[:n])))
	return true, pid, nil
}
