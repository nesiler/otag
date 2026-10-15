package daemon

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/notify"
)

// blockingCollector hangs like an unreachable endpoint or a Keychain dialog until it
// is cancelled.
type blockingCollector struct{ started, done atomic.Int32 }

func (c *blockingCollector) Collect(ctx context.Context) error {
	c.started.Add(1)
	<-ctx.Done()
	c.done.Add(1)
	return ctx.Err()
}

// A hanging collector delays neither the run loops nor the shutdown.
func TestSlowCollectorDoesNotBlockTicks(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	shown := make(chan string, 10)
	c := &blockingCollector{}
	cfg := DefaultConfig()
	cfg.Tick = 20 * time.Millisecond
	dm := &Daemon{DB: d, Config: cfg, Collectors: []QuotaCollector{c},
		Desktop: func(_ context.Context, title, _ string) error { shown <- title; return nil }}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- dm.Run(rctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for c.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := notify.Add(ctx, d, notify.New{Kind: "test", Title: "while collecting"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-shown:
		if got != "while collecting" {
			t.Fatalf("shown %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ticks stopped while a collector hangs")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return while a collector hangs")
	}
	if c.done.Load() != 1 {
		t.Fatal("collector not cancelled at shutdown")
	}
}

// deliver judges a notification's age by the daemon's clock.
func TestDeliverUsesDaemonClock(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	shown := 0
	dm := &Daemon{DB: d, Config: DefaultConfig(), Now: func() time.Time { return time.Now().Add(time.Hour) },
		Desktop: func(context.Context, string, string) error { shown++; return nil }}
	_, _ = notify.Add(ctx, d, notify.New{Kind: "test", Title: "old by the daemon's clock"})
	if err := dm.deliver(ctx); err != nil {
		t.Fatal(err)
	}
	if shown != 0 {
		t.Fatal("a notification older than 15 minutes by the daemon's clock was shown")
	}
}

// Titles and bodies are clipped at a rune boundary and escaped; the script compiles.
func TestOsascriptScript(t *testing.T) {
	body := strings.Repeat("a", maxBody-1) + "ğüş" // the cut falls inside "ğ"
	title := `run "x" \ ` + strings.Repeat("é", 100)
	script := osascriptScript(title, body+"\nline two")
	if !utf8.ValidString(script) {
		t.Fatalf("invalid UTF-8: %q", script)
	}
	if !strings.Contains(script, `"`+strings.Repeat("a", maxBody-1)+`…"`) {
		t.Fatalf("body not clipped at the rune boundary: %q", script)
	}
	if !strings.Contains(script, `subtitle "run \"x\" \\ é`) || strings.Contains(script, "\n") {
		t.Fatalf("title not escaped: %q", script)
	}
	if n := strings.Count(script, "é"); n > maxTitle/2 {
		t.Fatalf("title not clipped (%d of 100 runes kept): %q", n, script)
	}
	if clip("ab", 5) != "ab" || clip("ğğ", 3) != "ğ…" || clip("ğ", 1) != "…" {
		t.Fatal("clip")
	}
	if _, err := exec.LookPath("osacompile"); err != nil {
		t.Skip("osacompile not available")
	}
	out, err := exec.Command("osacompile", "-o", filepath.Join(t.TempDir(), "n.scpt"), "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("osacompile: %v %s\n%s", err, out, script)
	}
}
