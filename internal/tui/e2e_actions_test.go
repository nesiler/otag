package tui_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/tui/tuitest"
)

// clickText clicks the first cell of text on the current screen.
func (o *outer) clickText(text string) {
	o.t.Helper()
	s, _ := o.waitFor(text, 5*time.Second, contains(text))
	x, y, _ := find(s, text)
	o.click(x, y)
}

// selectTask clicks the row titled title until task id is selected (the message box
// follows the selection): a reload can move the rows between a capture and a click.
func (o *outer) selectTask(title, id string) {
	o.t.Helper()
	for i := 0; i < 20; i++ {
		o.clickText(title)
		for j := 0; j < 10; j++ {
			if strings.Contains(o.screen(), "to: "+id+" ▾") {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	o.t.Fatalf("could not select %s (%s):\n%s", title, id, o.screen())
}

// escape presses Esc alone: a key sent right after it would read as alt+key.
func (o *outer) escape() {
	o.t.Helper()
	o.keys("Escape")
	time.Sleep(200 * time.Millisecond)
}

// typeText types literal text into the TUI.
func (o *outer) typeText(s string) {
	o.t.Helper()
	o.keys("-l", s)
}

// E2E 2, 3 and the message box: n adds a task, s starts a run of it with the
// project's default profile, the message box sends to the run, S stops it.
func TestE2EAddStartSendStop(t *testing.T) {
	e := tuitest.New(t, otagBin)
	e.AddTask("alpha", "Existing task")
	o := startTUI(t, e, 160, 45)
	o.waitFor("the tree", 10*time.Second, contains("Existing task"))

	// E2E 2: n → title → ctrl+s; the row appears and is selected.
	o.keys("n")
	o.waitFor("the dialog", 5*time.Second, contains("New task in alpha", "[ Add task ]"))
	o.typeText("Ship the widget")
	o.keys("C-s")
	o.waitFor("the new task", 5*time.Second, func(s string) bool {
		return strings.Contains(s, "Ship the widget") && !strings.Contains(s, "[ Add task ]")
	})
	var id string
	for _, l := range strings.Split(e.OK("--project", "alpha", "task", "list"), "\n") {
		if strings.Contains(l, "Ship the widget") {
			id = strings.Fields(l)[0]
		}
	}
	if id == "" {
		t.Fatal("the task is not in the database")
	}

	// E2E 3: s → the profile picker (manual, the project's default) → Enter.
	o.keys("1", "s")
	o.waitFor("the picker", 5*time.Second, contains("Start a run of "+id, "› manual"))
	o.keys("Enter")
	o.waitFor("the result", 15*time.Second, contains("Run started", "started "+id+"-r1"))
	o.escape()
	o.waitFor("the run row and its preview", 10*time.Second, contains("r1 manual", id+"-r1 · manual", "fake: start"))
	e.WaitRun(id+"-r1", func(status, _, _ string) bool { return status == "running" || status == "idle" })

	// The message box sends to the selected run through otag send: dispatched (proof to
	// come), delivered or queued,
	// never typed into the pane.
	o.keys("m")
	o.waitFor("the message box", 5*time.Second, contains("Enter send"))
	o.typeText("hello there")
	o.keys("Enter")
	o.waitFor("the send outcome", 10*time.Second, func(s string) bool {
		return strings.Contains(s, "info #") && (strings.Contains(s, ": delivered") || strings.Contains(s, ": dispatched") ||
			strings.Contains(s, ": queued"))
	})
	time.Sleep(500 * time.Millisecond) // a few preview captures
	if s := o.screen(); strings.Contains(s, "hello there") {
		t.Fatalf("the message reached the pane as keys (or stayed in the box):\n%s", s)
	}
	o.escape()

	// S → Stop.
	o.keys("S")
	o.waitFor("the stop dialog", 5*time.Second, contains("Stop "+id+"-r1", "[ Stop ]"))
	o.typeText("enough")
	o.keys("C-s")
	e.WaitRun(id+"-r1", func(status, ended, _ string) bool { return status == "ended" && ended == "stopped" })
	o.waitFor("the stop toast", 5*time.Second, contains(id+"-r1: ended (stopped)"))
}

// E2E 4 and 9: a question shows as needing the human; r replies through the message
// box and the task leaves waiting. Then S → Park on another asking run, a reply
// that waits queued, and u resumes it.
func TestE2EReplyParkResume(t *testing.T) {
	e := tuitest.New(t, otagBin)
	ask := e.AddTask("alpha", "Ask me", "--profile", "asker")
	park := e.AddTask("alpha", "Park me", "--profile", "asker")
	e.Start(ask)
	e.Start(park)
	e.WaitTask(ask, func(s string) bool { return s == "waiting" })
	e.WaitTask(park, func(s string) bool { return s == "waiting" })
	o := startTUI(t, e, 160, 45)
	o.waitFor("the questions", 10*time.Second, contains("Ask me", "Park me", "⚑ 2 needs you"))

	// E2E 4: select the task, r, type, Enter.
	o.selectTask("Ask me", ask)
	o.keys("r")
	o.waitFor("reply mode", 5*time.Second, contains("reply #"))
	o.typeText("green")
	o.keys("Enter")
	o.waitFor("the reply outcome", 10*time.Second, contains("answer #"))
	e.WaitTask(ask, func(s string) bool { return s != "waiting" })
	o.escape()

	// E2E 9: S on the other asking run offers Park.
	o.selectTask("Park me", park)
	o.keys("S")
	o.waitFor("the stop dialog", 5*time.Second, contains("[ Park ]"))
	o.clickText("[ Park ]")
	runs := e.Runs(park)
	e.WaitRun(runs[0].ID, func(status, ended, _ string) bool { return status == "ended" && ended == "parked" })
	if st := e.Task(park).Status; st != "waiting" {
		t.Fatalf("parked task is %s", st)
	}
	// The answer waits queued (no live run); u resumes the task with it.
	o.selectTask("Park me", park)
	o.keys("r")
	o.waitFor("reply mode", 5*time.Second, contains("reply #"))
	o.typeText("blue")
	o.keys("Enter")
	o.waitFor("the queued answer", 10*time.Second, contains("answer #"))
	o.escape()
	o.keys("u")
	o.waitFor("the resume toast", 15*time.Second, contains("resumed task "+park))
	if rs := e.Runs(park); len(rs) != 2 || rs[0].Status == "ended" {
		t.Fatalf("runs after resume: %+v", rs)
	}
}

// E2E 8: review accept and reject, then deliver: the worktree is removed and the
// result says so.
func TestE2EReviewDeliver(t *testing.T) {
	e := tuitest.New(t, otagBin)
	acc := e.AddTask("beta", "Accept me", "--profile", "finish")
	rej := e.AddTask("beta", "Reject me", "--profile", "finish")
	del := e.AddTask("alpha", "Deliver me", "--profile", "finish")
	e.Start(del)
	for _, id := range []string{acc, rej} { // one run at a time in beta's directory
		e.Start(id)
		e.WaitTask(id, func(s string) bool { return s == "review" })
		e.WaitRun(e.Runs(id)[0].ID, func(status, _, _ string) bool { return status == "ended" })
	}
	e.WaitTask(del, func(s string) bool { return s == "done" })
	worktree := e.Runs(del)[0].Workspace.Path
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("no worktree before the delivery: %v", err)
	}
	o := startTUI(t, e, 160, 45)
	o.waitFor("the tasks", 10*time.Second, contains("Accept me", "Reject me", "Deliver me", "Ready to deliver"))

	o.selectTask("Accept me", acc)
	o.keys("v")
	o.waitFor("the review dialog", 5*time.Second, contains("Review "+acc, "[ Accept ]"))
	o.typeText("looks good")
	o.keys("C-s")
	e.WaitTask(acc, func(s string) bool { return s == "done" })
	o.waitFor("the review toast", 5*time.Second, contains("accept task "+acc))

	o.selectTask("Reject me", rej)
	o.keys("v")
	o.waitFor("the review dialog", 5*time.Second, contains("Review "+rej, "[ Reject ]"))
	o.typeText("tests fail")
	o.clickText("[ Reject ]")
	e.WaitTask(rej, func(s string) bool { return s == "ready" })

	o.selectTask("Deliver me", del)
	o.keys("D")
	s, _ := o.waitFor("the deliver dialog", 5*time.Second, contains("Deliver "+del, "[ ] delete the branch if it is merged"))
	if !strings.Contains(s, "branch feature/"+del) {
		t.Errorf("no branch in the dialog:\n%s", s)
	}
	o.keys("C-s")
	o.waitFor("the result", 15*time.Second, contains("Delivered "+del, "delivered "+del, ": removed", "branch feature/"+del+": kept"))
	e.WaitTask(del, func(s string) bool { return s == "delivered" })
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
}
