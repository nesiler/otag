package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/tmux"
)

func TestTuiRefusals(t *testing.T) {
	h := newHarness(t)
	// The harness's stdin and stdout are buffers, not terminals.
	if code, _, errs := h.run("tui"); code != ExitUsage || !strings.Contains(errs, "needs a terminal") {
		t.Fatalf("no terminal: %d %s", code, errs)
	}
	if code, e := h.json("tui"); code != ExitUsage || !strings.Contains(e.Error.Message, "no --json") {
		t.Fatalf("--json: %d %+v", code, e.Error)
	}
	if code, _, errs := h.run("tui", "extra"); code != ExitUsage || !strings.Contains(errs, "usage: otag tui") {
		t.Fatalf("argument: %d %s", code, errs)
	}
	// Inside a run the child gate refuses it before anything else.
	h.env["OTAG_RUN_ID"] = "al-1-r1"
	if code, _, errs := h.run("tui"); code == 0 || !strings.Contains(errs, "FORBIDDEN") {
		t.Fatalf("inside a run: %d %s", code, errs)
	}
}

func TestInsideSocket(t *testing.T) {
	own := tmux.SocketPath("otag")
	for env, want := range map[string]bool{
		own + ",123,0":                  true,
		tmux.SocketPath("other") + ",1": false,
		"":                              false,
		own + "x,1,0":                   false,
	} {
		if got := insideSocket(env, "otag"); got != want {
			t.Errorf("insideSocket(%q) = %v", env, got)
		}
	}
}

// tuiText returns what the command prints on the command line.
func TestTuiText(t *testing.T) {
	h := newHarness(t)
	h.cwd = h.alpha
	_, e := h.json("task", "add", "Write docs")
	var tk struct{ ID string }
	_ = json.Unmarshal(e.Data, &tk)
	want := h.ok("task", "show", tk.ID)

	a := &app{env: Env{Getenv: func(k string) string { return h.env[k] }, Cwd: h.cwd}}
	if _, err := a.openDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.db.Close() }()
	got, err := a.tuiText(context.Background(), "task", "show", tk.ID)
	if err != nil || got != want {
		t.Fatalf("task show: %v\n%s\nwant\n%s", err, got, want)
	}
	if got, err := a.tuiText(context.Background(), "--project", "alpha", "status"); err != nil ||
		got != h.ok("--project", "alpha", "status") {
		t.Fatalf("status: %v\n%s", err, got)
	}
	if _, err := a.tuiText(context.Background(), "task", "show", "al-nope"); err == nil {
		t.Fatal("no error for a missing task")
	}
	if _, err := a.tuiText(context.Background(), "frobnicate"); err == nil {
		t.Fatal("no error for an unknown command")
	}
}
