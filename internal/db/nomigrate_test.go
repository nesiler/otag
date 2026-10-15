package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// NoMigrate opens a current database as is, and refuses (without writing) a missing
// one or one with an older schema.
func TestOpenNoMigrate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "otag.db")
	if _, err := OpenWith(ctx, path, Options{NoMigrate: true}); !errors.Is(err, ErrNotMigrated) {
		t.Fatalf("missing db: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("NoMigrate created the file")
	}
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	latest := d.Latest
	_ = d.Close()

	d, err = OpenWith(ctx, path, Options{NoMigrate: true})
	if err != nil || d.Version != latest || d.ReadOnly() {
		t.Fatalf("current db: %v %+v", err, d)
	}
	if _, err := d.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if _, err := OpenWith(ctx, path, Options{NoMigrate: true}); !errors.Is(err, ErrNotMigrated) {
		t.Fatalf("old schema: %v", err)
	}
	ro, err := sql.Open("sqlite", dsn(path, true, 1000))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if v, err := userVersion(ctx, ro); err != nil || v != 1 {
		t.Fatalf("NoMigrate migrated: v%d %v", v, err)
	}
}
