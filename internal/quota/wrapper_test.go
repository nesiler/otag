package quota

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Only otag in command position is the wrapper; "otag statusline" as an argument of
// another command is the user's own output.
func TestWrapperOnlyInCommandPosition(t *testing.T) {
	for in, want := range map[string]string{
		`printf '%s %s' otag statusline`:                      `printf '%s %s' otag statusline`,
		`echo x; otag statusline --command y`:                 `echo x; otag statusline --command y`,
		`FOO=1 BAR=2 /opt/otag statusline --command 'echo y'`: "echo y",
		`exec otag statusline --command 'echo y'`:             "echo y",
		`env A=b otag statusline --command=z`:                 "z",
	} {
		if got := UnwrapCommand(in); got != want {
			t.Errorf("UnwrapCommand(%q) = %q, want %q", in, got, want)
		}
	}
	var out bytesWriter
	Passthrough(context.Background(), `printf '%s %s' otag statusline`, nil, &out, io.Discard, os.Environ())
	if out.String() != "otag statusline" {
		t.Fatalf("output %q", out.String())
	}
}

type bytesWriter struct {
	mu sync.Mutex
	b  []byte
}

func (w *bytesWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b = append(w.b, p...)
	return len(p), nil
}

func (w *bytesWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.b)
}

// A timed-out statusLine command is killed with its whole process group: a child that
// inherited stdout does not keep the reader waiting for EOF.
func TestPassthroughKillsProcessGroup(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	Passthrough(ctx, "printf hi; sleep 30 & wait", nil, w, io.Discard, os.Environ())
	_ = w.Close()
	done := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	select {
	case b := <-done:
		if string(b) != "hi" {
			t.Fatalf("output %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdout still open: a child of the timed-out command survived")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %s", d)
	}
}

// Observations published out of order (concurrent statusLine calls) never replace a
// newer status file with an older one.
func TestStatusFileKeepsNewestObservation(t *testing.T) {
	d := testDB(t)
	data := t.TempDir()
	payload := fixture(t, "claude-statusline.json")
	base := time.Unix(1791660000, 0)
	run := func(at time.Time) {
		s, _, _ := statusLine(t, d, data, payload, "")
		s.Now = at
		s.Run(context.Background())
	}
	run(base.Add(2 * time.Minute)) // the newer callback finishes first
	run(base)
	f, _ := ReadStatusFile(StatusFilePath(data, "c1"))
	if !f.ObservedAt.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("an older observation replaced the newer: %v", f.ObservedAt)
	}

	var wg sync.WaitGroup
	newest := base.Add(time.Hour)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run(base.Add(time.Duration(10+i) * time.Minute))
		}(i)
	}
	wg.Wait()
	run(newest.Add(-time.Hour + 5*time.Minute)) // older than all of them
	f, _ = ReadStatusFile(StatusFilePath(data, "c1"))
	if want := base.Add(29 * time.Minute); !f.ObservedAt.Equal(want) {
		t.Fatalf("observed %v, want the newest %v", f.ObservedAt, want)
	}
	if _, err := os.Stat(filepath.Join(data, "quota", "statusline-c1.json.lock")); err != nil {
		t.Fatal("no lock file")
	}
}
