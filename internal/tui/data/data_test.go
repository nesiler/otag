package data_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/tuitest"
)

var otagBin string

func TestMain(m *testing.M) {
	bin, dir, err := tuitest.BuildOtag()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	otagBin = bin
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestLoad(t *testing.T) {
	ctx := context.Background()
	e := tuitest.New(t, otagBin)
	ready := e.AddTask("alpha", "Ready one", "--priority", "2")
	draft := e.AddTask("alpha", "A draft", "--draft")
	blocked := e.AddTask("alpha", "Blocked one")
	e.OK("task", "block", blocked, "--reason", "waits for the API")
	live := e.AddTask("alpha", "Live one")
	liveRun := e.Start(live)
	finished := e.AddTask("alpha", "Finished one")
	finishedRun := e.Start(finished, "--profile", "finish")
	e.WaitRun(finishedRun, func(status, _, task string) bool { return status == run.Ended && task == "done" })
	review := e.AddTask("alpha", "Review of the live one", "--review-of", live)
	cancelled := e.AddTask("beta", "Cancelled one")
	e.OK("task", "cancel", cancelled, "--reason", "not needed")

	d := e.DB()
	dataDir := e.Vars["OTAG_DATA_DIR"]
	now := time.Now()
	s, err := data.Load(ctx, d, dataDir, data.Request{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Projects) != 2 || s.Projects[0].Name != "alpha" || s.Projects[1].Name != "beta" {
		t.Fatalf("projects %+v", s.Projects)
	}
	ids := func(l any) map[string]string {
		out := map[string]string{}
		switch l := l.(type) {
		case []run.Run:
			for _, r := range l {
				out[r.ID] = r.Status
			}
		default:
			for _, tk := range s.Open {
				out[tk.ID] = tk.Status
			}
		}
		return out
	}
	open := ids(nil)
	for id, status := range map[string]string{ready: "ready", draft: "draft", blocked: "blocked", live: "running",
		review: "ready"} {
		if open[id] != status {
			t.Errorf("open %s: %q, want %q (%v)", id, open[id], status, open)
		}
	}
	if open[finished] != "" || open[cancelled] != "" {
		t.Errorf("closed tasks in Open: %v", open)
	}
	if len(s.Deliverable) != 1 || s.Deliverable[0].ID != finished {
		t.Errorf("deliverable %+v", s.Deliverable)
	}
	if s.Closed != nil {
		t.Errorf("closed tasks without ShowClosed: %+v", s.Closed)
	}
	if s.ReviewOf[review] != live {
		t.Errorf("review edges %v", s.ReviewOf)
	}
	runs := ids(s.Runs)
	if runs[liveRun] == "" || runs[finishedRun] != run.Ended {
		t.Errorf("runs %v", runs)
	}
	if s.RunCount[live] != 1 || s.RunCount[finished] != 1 || s.RunCount[ready] != 0 {
		t.Errorf("run counts %v", s.RunCount)
	}
	if len(s.Needs.Blocked) != 1 || s.Needs.Blocked[0].Task != blocked {
		t.Errorf("needs %+v", s.Needs)
	}
	if s.Daemon || s.ReadOnly || len(s.Profiles) != 5 || len(s.Unavailable) != 0 { // tuitest's five
		t.Errorf("daemon %v read-only %v profiles %d unavailable %v", s.Daemon, s.ReadOnly, len(s.Profiles), s.Unavailable)
	}

	// A second attempt: the newest run is listed, the history only on request.
	e.OK("run", "stop", liveRun, "--reason", "again")
	second := e.Start(live)
	s, err = data.Load(ctx, d, dataDir, data.Request{ShowClosed: true, History: []string{live, ready}}, now)
	if err != nil {
		t.Fatal(err)
	}
	runs = ids(s.Runs)
	if runs[second] == "" || runs[liveRun] != "" || s.RunCount[live] != 2 {
		t.Errorf("after a retry: runs %v counts %v", runs, s.RunCount)
	}
	if h := s.History[live]; len(h) != 2 || h[0].ID != second || h[1].ID != liveRun {
		t.Errorf("history %+v", h)
	}
	if _, ok := s.History[ready]; ok {
		t.Errorf("history of a task without runs: %+v", s.History)
	}
	closed := map[string]string{}
	for _, tk := range s.Closed {
		closed[tk.ID] = tk.Status
	}
	if closed[finished] != "done" || closed[cancelled] != "cancelled" {
		t.Errorf("closed %v", closed)
	}

	// Liveness: one call for the active runs, keyed by run id.
	l := data.Liveness(ctx, e.Tmux, s.Runs)
	if l[second].State != tmux.Alive {
		t.Errorf("liveness %+v", l)
	}
	if _, ok := l[finishedRun]; ok {
		t.Errorf("liveness of an ended run: %+v", l[finishedRun])
	}
}

func TestWatcher(t *testing.T) {
	ctx := context.Background()
	e := tuitest.New(t, otagBin)
	w, err := data.NewWatcher(ctx, e.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	changed := func() bool {
		t.Helper()
		c, err := w.Changed(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if changed() {
		t.Fatal("first call reported a change")
	}
	if changed() {
		t.Fatal("change without a commit")
	}
	e.AddTask("alpha", "From another connection") // cli.Main opens its own database handle
	if !changed() {
		t.Fatal("commit not seen")
	}
	if changed() {
		t.Fatal("one commit seen twice")
	}
}
