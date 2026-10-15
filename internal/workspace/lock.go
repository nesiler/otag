package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// fetchLockWait bounds how long a start waits for another process's base fetch of the
// same repository.
const fetchLockWait = 2 * time.Minute

// lockRepo takes an exclusive lock for the repository at projectPath under lockDir
// (<data>/locks/repo-<hash of its git common dir>.lock), shared by every otag process,
// and returns its release. Two starts that fetch the same base otherwise race on its
// remote-tracking ref: the later update finds the ref moved and git refuses it. An
// empty lockDir takes no lock.
func lockRepo(ctx context.Context, lockDir, projectPath string) (func(), error) {
	if lockDir == "" {
		return func() {}, nil
	}
	common, err := commonDir(ctx, projectPath)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(common))
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(lockDir, "repo-"+hex.EncodeToString(sum[:8])+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(fetchLockWait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s for the base fetch: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
