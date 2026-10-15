// Package db opens otag's SQLite database and applies embedded migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nesiler/otag/internal/apperr"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// TimeFormat is the layout of every timestamp column.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// Now returns the current UTC time formatted like the DB defaults.
func Now() string { return time.Now().UTC().Format(TimeFormat) }

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB is an open otag database.
type DB struct {
	*sql.DB
	Path string
	// Version is the schema version found in the file; Latest is what this binary knows.
	Version, Latest int
	readOnly        bool
}

type migration struct {
	version int
	name    string
	sql     string
}

// Open opens (creating if needed) the database at path and migrates it to the latest schema.
// If the file's schema is newer than this binary, the DB is opened with mode=ro and every
// Tx call fails with SCHEMA_TOO_NEW.
func Open(ctx context.Context, path string) (*DB, error) {
	return OpenWith(ctx, path, Options{})
}

// Options tune a connection.
type Options struct {
	// BusyTimeout bounds how long a statement waits for a lock (default 10s). The hook
	// bridge uses a short one so a locked database never stalls a harness.
	BusyTimeout time.Duration
	// NoMigrate never creates or migrates the database: a missing file or an older
	// schema is ErrNotMigrated. For hot paths (the statusLine wrapper) that must never
	// take the migration's write lock.
	NoMigrate bool
}

// ErrNotMigrated is returned with Options.NoMigrate when the database is missing or
// has an older schema than this binary.
var ErrNotMigrated = errors.New("database missing or not migrated to this binary's schema")

// OpenWith is Open with options.
func OpenWith(ctx context.Context, path string, o Options) (*DB, error) {
	migs, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	return open(ctx, path, migs, o)
}

func open(ctx context.Context, path string, migs []migration, o Options) (*DB, error) {
	var err error
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = 10 * time.Second
	}
	busy := int(o.BusyTimeout / time.Millisecond)
	if path, err = filepath.Abs(path); err != nil {
		return nil, err
	}
	d := &DB{Path: path, Latest: len(migs)}

	// Probe an existing file read-only first, so a newer schema is never touched
	// (not even by the journal_mode pragma).
	_, statErr := os.Stat(path)
	if o.NoMigrate && statErr != nil {
		return nil, fmt.Errorf("%s: %w", path, ErrNotMigrated)
	}
	if statErr == nil {
		ro, err := sql.Open("sqlite", dsn(path, true, busy))
		if err != nil {
			return nil, err
		}
		v, err := userVersion(ctx, ro)
		if err != nil {
			_ = ro.Close()
			return nil, fmt.Errorf("read schema version: %w", wrapBusy(err))
		}
		if v > d.Latest {
			d.DB, d.Version, d.readOnly = ro, v, true
			return d, nil
		}
		_ = ro.Close()
		if o.NoMigrate {
			if v < d.Latest {
				return nil, fmt.Errorf("%s: schema v%d < v%d: %w", path, v, d.Latest, ErrNotMigrated)
			}
			sqldb, err := sql.Open("sqlite", dsn(path, false, busy))
			if err != nil {
				return nil, err
			}
			d.DB, d.Version = sqldb, v
			return d, nil
		}
	}

	if err := ensureFile(path); err != nil {
		return nil, err
	}
	sqldb, err := sql.Open("sqlite", dsn(path, false, busy))
	if err != nil {
		return nil, err
	}
	d.DB = sqldb
	if err := d.migrate(ctx, migs); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	if d.Version > d.Latest {
		// Upgraded by a newer binary between the probe and the write lock.
		_ = sqldb.Close()
		if d.DB, err = sql.Open("sqlite", dsn(path, true, busy)); err != nil {
			return nil, err
		}
		d.readOnly = true
	}
	return d, nil
}

// OpenReadOnly opens an existing database for a look that writes nothing at all
// (mode=ro, query_only, immutable): it is never created, migrated or chmod-ed, and
// not even a -wal or -shm file is made. immutable skips SQLite's locking and the WAL,
// so a concurrent writer's latest commits may be missing: callers use it for hints
// (the setup wizard before the user agreed to anything) and check again with Open.
func OpenReadOnly(path string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", dsn(path, true, 2000)+"&immutable=1")
}

// ensureFile creates the parent directory (0700) and the database file (0600), and
// tightens the mode of an existing database file. SQLite gives -wal/-shm the same mode.
func ensureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("create db file: %w", err)
	}
	_ = f.Close()
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return os.Chmod(path, 0o600)
	}
	return nil
}

// dsn builds a SQLite URI for path. The path is percent-escaped so '?', '#' and '%'
// in file names are not read as URI syntax.
func dsn(path string, readOnly bool, busyMS int) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyMS))
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
	}
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var migs []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("bad migration name %q", e.Name())
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		migs = append(migs, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i, m := range migs {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..n without gaps; got %s", m.name)
		}
	}
	return migs, nil
}

func userVersion(ctx context.Context, q Querier) (int, error) {
	var v int
	err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

func (d *DB) migrate(ctx context.Context, migs []migration) error {
	v, err := userVersion(ctx, d.DB)
	if err != nil {
		return fmt.Errorf("read schema version: %w", wrapBusy(err))
	}
	d.Version = v
	if v >= len(migs) {
		return nil
	}
	// Table rebuilds need foreign keys off; the pragma is a no-op inside a transaction,
	// so it is set on a dedicated connection around it and checked before commit.
	conn, err := d.Conn(ctx)
	if err != nil {
		return wrapBusy(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON") }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return wrapBusy(err)
	}
	defer func() { _ = tx.Rollback() }()
	// Re-read under the write lock: another process may have migrated meanwhile.
	if v, err = userVersion(ctx, tx); err != nil {
		return err
	}
	if v > len(migs) {
		d.Version = v
		return nil
	}
	for _, m := range migs[min(v, len(migs)):] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return err
	}
	if v < len(migs) {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migs))); err != nil {
			return err
		}
		v = len(migs)
	}
	if err := tx.Commit(); err != nil {
		return wrapBusy(err)
	}
	d.Version = v
	return nil
}

func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var bad []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		bad = append(bad, fmt.Sprintf("%s rowid %d -> %s", table, rowid.Int64, parent))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("migration broke foreign keys: %s", strings.Join(bad, "; "))
	}
	return nil
}

// ReadOnly reports whether writes are refused because the schema is newer than this binary.
func (d *DB) ReadOnly() bool { return d.readOnly }

// Tx runs fn inside a write transaction (BEGIN IMMEDIATE) and commits if fn succeeds.
func (d *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if d.readOnly {
		return apperr.New(apperr.SchemaTooNew,
			"database schema v%d is newer than this binary (v%d); upgrade otag", d.Version, d.Latest)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return wrapBusy(err)
	}
	defer func() { _ = tx.Rollback() }()
	// Another process may have migrated the file after we opened it.
	v, err := userVersion(ctx, tx)
	if err != nil {
		return wrapBusy(err)
	}
	if v > d.Latest {
		return apperr.New(apperr.SchemaTooNew,
			"database schema v%d is newer than this binary (v%d); upgrade otag", v, d.Latest)
	}
	if err := fn(tx); err != nil {
		return wrapBusy(err)
	}
	return wrapBusy(tx.Commit())
}

// wrapBusy turns SQLITE_BUSY/LOCKED into a retryable app error.
func wrapBusy(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return apperr.Retryable(apperr.Busy, "database is busy: %v", err)
		}
	}
	return err
}

// IsUniqueViolation reports whether err is a UNIQUE/PRIMARY KEY constraint failure.
func IsUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		c := se.Code()
		return c == sqlite3.SQLITE_CONSTRAINT_UNIQUE || c == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}
