package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nesiler/otag/internal/db"
)

// StatusLine is one `otag statusline` invocation: record the payload's quota, then run
// the user's own statusLine command with the same payload and print its output.
type StatusLine struct {
	Account   string // registry account name
	Harness   string // claude | "" (the default)
	ConfigDir string // Claude config dir whose settings.json holds the user's statusLine
	Command   string // the user's statusLine command; "" = look it up (Claude only)
	DataDir   string
	Payload   []byte
	Now       time.Time
	// OpenDB opens the database with a short busy timeout; nil = no database write.
	OpenDB func(ctx context.Context) (*db.DB, error)
	Env    []string // environment of the wrapped command
	Getenv func(string) string
	Log    func(format string, args ...any)
	Stdout io.Writer
	Stderr io.Writer
}

// Claude Code runs the statusLine after every assistant message. While the values do
// not change, the status file is rewritten at most every statusFileEvery (it only moves
// observed_at forward) and the database at most every statusLineDBEvery.
const (
	statusFileEvery   = 30 * time.Second
	statusLineDBEvery = time.Minute
)

// Run never fails; problems go to Log.
func (s StatusLine) Run(ctx context.Context) {
	if s.Getenv != nil && s.Getenv(RecursionEnv) != "" {
		return // the wrapper wrapped itself
	}
	if s.Now.IsZero() {
		s.Now = time.Now()
	}
	s.Now = s.Now.UTC()
	if s.Log == nil {
		s.Log = func(string, ...any) {}
	}
	s.record(ctx)

	cmd := s.Command
	if cmd == "" {
		dir := s.ConfigDir
		if dir == "" && s.Getenv != nil {
			dir = s.Getenv("CLAUDE_CONFIG_DIR")
		}
		if dir == "" && s.Getenv != nil && s.Getenv("HOME") != "" {
			dir = filepath.Join(s.Getenv("HOME"), ".claude")
		}
		cmd = ClaudeStatusLineCommand(dir, PayloadProjectDir(s.Payload))
	}
	// A settings statusLine may be the wrapper itself (the user opted their own
	// sessions in): run what it wraps, never otag again.
	if cmd = UnwrapCommand(cmd); cmd == "" {
		return
	}
	Passthrough(ctx, cmd, s.Payload, s.Stdout, s.Stderr, s.Env)
}

func (s StatusLine) record(ctx context.Context) {
	if s.Account == "" || len(strings.TrimSpace(string(s.Payload))) == 0 {
		return
	}
	harness, source, ws, err := ParseStatusline(s.Harness, s.Payload, s.Now)
	if err != nil {
		s.Log("%s: parse payload: %v", s.Account, err)
		return
	}
	if len(ws) == 0 {
		return // before the first API response, or not a subscription
	}
	path := StatusFilePath(s.DataDir, s.Account)
	// Claude Code may run statusLines of one account concurrently: the file is
	// published under a per-account lock and never replaced by an older observation.
	unlock, err := lockFile(ctx, path+".lock", time.Second)
	if err != nil {
		s.Log("%s: %v", s.Account, err)
		return
	}
	defer unlock()
	f := NewStatusFile(s.Account, harness, source, s.Now, ws)
	prev, ok := ReadStatusFile(path)
	if ok && prev.ObservedAt.After(s.Now) {
		return // a newer observation is already published (and recorded)
	}
	same := ok && prev.Source == source && prev.SameWindows(f)
	if same && !s.Now.Before(prev.ObservedAt) && s.Now.Sub(prev.ObservedAt) < statusFileEvery {
		return // nothing new to say
	}
	if same && s.Now.Sub(prev.RecordedAt) < statusLineDBEvery && !prev.RecordedAt.IsZero() {
		f.RecordedAt = prev.RecordedAt // the collector touches the row later
	} else if s.OpenDB != nil {
		switch err := s.writeDB(ctx, source, ws); {
		case err == nil:
			f.RecordedAt = s.Now
		case errors.Is(err, db.ErrNotMigrated):
			// The database needs a migration (or does not exist yet): the statusLine
			// never takes that write lock. The file is enough; the daemon's collector,
			// which does migrate, ingests it.
			if ok {
				f.RecordedAt = prev.RecordedAt
			}
		default:
			s.Log("%s: record: %v", s.Account, err)
		}
	}
	if err := WriteStatusFile(path, f); err != nil {
		s.Log("%s: write %s: %v", s.Account, path, err)
	}
}

// lockFile takes an exclusive flock on path (created 0600), waiting at most wait.
func lockFile(ctx context.Context, path string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s StatusLine) writeDB(ctx context.Context, source string, ws []Window) error {
	d, err := s.OpenDB(ctx)
	if err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		id, _, err := AccountID(ctx, tx, s.Account)
		if err != nil {
			return err
		}
		_, err = RecordStatusline(ctx, tx, id, source, s.Now, ws)
		return err
	})
}
