package event

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nesiler/otag/internal/db"
)

func TestRecordDedupe(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	e := Event{Source: "hook:claude", DedupeKey: "evt-1", Kind: "x", EntityType: "system", EntityID: "1"}
	for i, want := range []bool{true, false} {
		got, err := Record(ctx, d, e)
		if err != nil || got != want {
			t.Fatalf("call %d: inserted=%v err=%v", i, got, err)
		}
	}
	// No dedupe key: never deduplicated.
	e.DedupeKey = ""
	for range 2 {
		if ok, err := Record(ctx, d, e); !ok || err != nil {
			t.Fatalf("inserted=%v err=%v", ok, err)
		}
	}
	var n int
	_ = d.QueryRow(`SELECT count(*) FROM events`).Scan(&n)
	if n != 3 {
		t.Fatalf("events = %d", n)
	}
}
