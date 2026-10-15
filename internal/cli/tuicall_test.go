package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/task"
)

// tuiCall runs a mutation like the command line: the same human text, the error code,
// and --request-id makes a repeat a replay.
func TestTuiCall(t *testing.T) {
	h := newHarness(t)
	a := &app{env: Env{Getenv: func(k string) string { return h.env[k] }, Cwd: h.root}}
	if _, err := a.openDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.db.Close() }()
	ctx := context.Background()

	o := a.tuiCall(ctx, "--request-id", "tui-1", "task", "add", "--project", "alpha", "--priority", "2", "--", "-Write docs")
	tk, ok := o.Data.(*task.Task)
	if o.Err != nil || !ok || tk.Title != "-Write docs" || tk.Priority != 2 {
		t.Fatalf("task add: %+v %+v", o.Err, o.Data)
	}
	if want := h.ok("task", "show", tk.ID); o.Human != want {
		t.Fatalf("human\n%s\nwant\n%s", o.Human, want)
	}
	// The same request id again: the stored result, no second task.
	again := a.tuiCall(ctx, "--request-id", "tui-1", "task", "add", "--project", "alpha", "--priority", "2", "--", "-Write docs")
	if again.Err != nil || again.Data.(*task.Task).ID != tk.ID || !strings.Contains(again.Human, "replayed result of request tui-1") {
		t.Fatalf("replay: %+v %q", again.Err, again.Human)
	}
	if n := strings.Count(h.ok("--project", "alpha", "task", "list"), "Write docs"); n != 1 {
		t.Fatalf("%d tasks", n)
	}
	// Without one, each call gets its own: two calls, two tasks.
	a.tuiCall(ctx, "task", "add", "--project", "alpha", "--", "twice")
	a.tuiCall(ctx, "task", "add", "--project", "alpha", "--", "twice")
	if n := strings.Count(h.ok("--project", "alpha", "task", "list"), "twice"); n != 2 {
		t.Fatalf("%d tasks", n)
	}
	// The human text is the CLI's, byte for byte.
	h.ok("task", "block", tk.ID, "--reason", "x")
	o = a.tuiCall(ctx, "task", "ready", tk.ID)
	h.ok("task", "block", tk.ID, "--reason", "x")
	if want := h.ok("task", "ready", tk.ID); o.Err != nil || o.Human != want {
		t.Fatalf("ready: %+v\n%q\nwant\n%q", o.Err, o.Human, want)
	}
	// Errors carry the CLI's code and message.
	o = a.tuiCall(ctx, "task", "edit", tk.ID, "--title", "x", "--version", "1")
	if o.Err == nil || o.Err.Code != "CONFLICT" || !strings.Contains(o.Err.Message, "expected 1") {
		t.Fatalf("conflict: %+v", o.Err)
	}
	if o = a.tuiCall(ctx, "frobnicate"); o.Err == nil || o.Err.Code != "USAGE" {
		t.Fatalf("unknown command: %+v", o.Err)
	}
	// A message to the human: the send output of the CLI.
	o = a.tuiCall(ctx, "send", "--project", "alpha", "--kind", "info", "--", "human", "note to self")
	if o.Err != nil || !strings.Contains(o.Human, "info #") || !strings.Contains(o.Human, "→ human: skipped") {
		t.Fatalf("send: %+v %q", o.Err, o.Human)
	}
}

// The TUI's commands do not depend on where it was started: no cwd, no OTAG_PROJECT,
// no OTAG_RUN_* (cmdTui refuses to run inside a run). Started inside alpha, [Mark read]
// (`inbox`) still reads every project's messages, like the Inbox tab shows them.
func TestTuiCallNeutralEnv(t *testing.T) {
	h := newHarness(t)
	h.ok("send", "--project", "beta", "--kind", "info", "--", "human", "beta note")
	h.ok("send", "--project", "alpha", "--kind", "info", "--", "human", "alpha note")
	h.env["OTAG_PROJECT"] = "alpha"
	h.env["OTAG_RUN_TOKEN"] = "stale"
	a := &app{env: Env{Getenv: func(k string) string { return h.env[k] }, Cwd: h.alpha}}
	if _, err := a.openDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.db.Close() }()
	ctx := context.Background()
	o := a.tuiCall(ctx, "inbox")
	if o.Err != nil || !strings.Contains(o.Human, "beta note") || !strings.Contains(o.Human, "alpha note") {
		t.Fatalf("inbox: %+v\n%s", o.Err, o.Human)
	}
	delete(h.env, "OTAG_PROJECT")
	delete(h.env, "OTAG_RUN_TOKEN")
	if left := h.ok("inbox", "--peek"); !strings.Contains(left, "no messages") {
		t.Fatalf("left unread:\n%s", left)
	}
	// Without --project nothing picks a project for it.
	h.env["OTAG_PROJECT"] = "alpha"
	if o = a.tuiCall(ctx, "task", "add", "--", "x"); o.Err == nil || o.Err.Code != "NO_PROJECT" {
		t.Fatalf("task add without --project: %+v", o.Err)
	}
	if o = a.tuiCall(ctx, "--project", "beta", "task", "add", "--", "x"); o.Err != nil ||
		o.Data.(*task.Task).Project != "beta" {
		t.Fatalf("task add --project beta: %+v %+v", o.Err, o.Data)
	}
}

func TestWaitFor(t *testing.T) {
	var wg sync.WaitGroup
	ctx := context.Background()
	if !waitFor(ctx, &wg, time.Millisecond) {
		t.Fatal("an empty group")
	}
	wg.Add(1)
	if waitFor(ctx, &wg, 10*time.Millisecond) {
		t.Fatal("a running call")
	}
	// ctrl+c (the context) ends the wait at once.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	start := time.Now()
	if waitFor(cctx, &wg, time.Minute) || time.Since(start) > 5*time.Second {
		t.Fatal("a cancelled wait")
	}
	wg.Done()
}

func TestHasFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"--request-id", "x", "send"}, true},
		{[]string{"-request-id=x", "send"}, true},
		{[]string{"send", "--", "--request-id"}, false},
		{[]string{"send", "--project", "a"}, false},
	} {
		if got := hasFlag(c.args, "request-id"); got != c.want {
			t.Errorf("hasFlag(%q) = %v", c.args, got)
		}
	}
}
