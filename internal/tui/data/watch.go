package data

import (
	"context"
	"database/sql"

	"github.com/nesiler/otag/internal/db"
)

// Watcher notices commits to the database. It pins one connection and reads
// PRAGMA data_version on it, which changes whenever any other connection commits: the
// CLI, hooks, the daemon, and the TUI's own backend writes (other pool connections).
// One read costs microseconds and no IO beyond the shared-memory file.
type Watcher struct {
	conn    *sql.Conn
	last    int64
	started bool
}

// NewWatcher pins a connection of d for the watcher's lifetime.
func NewWatcher(ctx context.Context, d *db.DB) (*Watcher, error) {
	c, err := d.Conn(ctx)
	if err != nil {
		return nil, err
	}
	return &Watcher{conn: c}, nil
}

// Changed reports whether another connection committed since the last call; the
// first call only records the starting version. Not safe for concurrent use.
func (w *Watcher) Changed(ctx context.Context) (bool, error) {
	var v int64
	if err := w.conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&v); err != nil {
		return false, err
	}
	changed := w.started && v != w.last
	w.last, w.started = v, true
	return changed, nil
}

// Close returns the pinned connection to the pool.
func (w *Watcher) Close() error { return w.conn.Close() }
