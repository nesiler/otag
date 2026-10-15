package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/fixture"
	"github.com/nesiler/otag/internal/tui/tabs"
)

// wantCall checks the arguments of the latest Options.Call.
func (h *harness) wantCall(want ...string) {
	h.t.Helper()
	if got := h.lastCall(); !slices.Equal(got, want) {
		h.t.Fatalf("call\n got %q\nwant %q", got, want)
	}
}

// dialogPart returns a cell of the open dialog's field id (button "" ) or button.
func (h *harness) dialogPart(field, button string, opt int) (int, int) {
	h.t.Helper()
	f := h.m.dlg.form
	_, hits := f.View(h.m.formWidth(), h.m.theme)
	for _, r := range layout(h.m).regions {
		if r.kind != regDlgPart {
			continue
		}
		i := 0
		for i < len(hits) && r.id != strconv.Itoa(i) {
			i++
		}
		ht := hits[i]
		switch {
		case button != "" && ht.Button >= 0 && f.Buttons[ht.Button].ID == button,
			field != "" && ht.Field >= 0 && f.Fields[ht.Field].ID == field && (opt < 0 || ht.Opt == opt):
			return r.rect.X, r.rect.Y
		}
	}
	h.t.Fatalf("no dialog part %q %q %d", field, button, opt)
	return 0, 0
}

func TestTaskAddDialog(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13") // any row of alpha
	h.key("n")
	if h.m.dlg == nil || h.m.dlg.form.Title != "New task in alpha" {
		t.Fatalf("dialog %+v", h.m.dlg)
	}
	// Required fields are checked here; nothing runs.
	h.key("ctrl+s")
	if len(h.calls) != 0 || h.m.dlg.form.Err != "title is required" {
		t.Fatalf("calls %v err %q", h.calls, h.m.dlg.form.Err)
	}
	h.typeText("-Fix it")
	h.key("tab")
	h.typeText("line one")
	h.key("alt+enter")
	h.typeText("two")
	h.key("tab")
	h.typeText("3")
	h.key("tab", "right", "right") // (project default) → opus → sonnet
	h.key("tab")
	h.typeText("al-16, al-12")
	h.key("tab", "space") // draft
	h.key("ctrl+s")
	h.wantCall("task", "add", "--project", "alpha", "--body", "line one\ntwo", "--priority", "3", "--profile", "sonnet",
		"--draft", "--blocked-by", "al-16", "--blocked-by", "al-12", "--", "-Fix it")
	if h.m.dlg != nil || !strings.HasPrefix(h.m.toast.text, "ok: task add") || h.m.toast.bad {
		t.Fatalf("dialog %v toast %+v", h.m.dlg, h.m.toast)
	}
	if len(h.m.log) < 2 || !strings.Contains(h.m.log[0], "otag task add --project alpha") ||
		!strings.Contains(h.m.log[0], "'-Fix it'") {
		t.Fatalf("log %q", h.m.log)
	}
	if len(h.pending("load")) == 0 && h.m.loading.inFlight {
		t.Fatal("no reload after the action")
	}
	// Esc closes a dialog without running anything.
	n := len(h.calls)
	h.key("n", "esc")
	if h.m.dlg != nil || len(h.calls) != n {
		t.Fatal("esc")
	}
}

// The new task is selected once a snapshot has it.
func TestTaskAddSelectsNewTask(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.result = func([]string) Outcome { return Outcome{Data: &task.Task{ID: "al-13"}, Human: "id: al-13"} }
	h.key("n")
	h.typeText("x")
	h.key("ctrl+s")
	h.fire("load")
	if h.selected() != "t:al-13" || h.m.want != "" {
		t.Fatalf("selected %s want %q", h.selected(), h.m.want)
	}
}

// A double submit runs the command once: the dialog is busy until the outcome.
func TestDoubleSubmit(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.key("s")
	h.hold = true
	h.key("enter")
	x, y := h.dialogPart("", "start", -1)
	h.click(x, y)
	h.key("ctrl+s")
	if len(h.calls) != 1 || !h.m.dlg.busy || !strings.Contains(h.m.dlg.form.Status, "run start al-13") {
		t.Fatalf("calls %v busy %v", h.calls, h.m.dlg.busy)
	}
	h.wantCall("--project", "alpha", "run", "start", "al-13", "--profile", "sonnet", "--no-attach")
	// The same action from elsewhere waits too.
	h.key("esc", "s", "enter")
	if len(h.calls) != 1 || !strings.Contains(h.m.toast.text, "already running") {
		t.Fatalf("calls %v toast %q", h.calls, h.m.toast.text)
	}
	h.release()
	if h.m.busy["run start al-13"] || h.m.inflight != 0 {
		t.Fatalf("busy %v", h.m.busy)
	}
}

// Every submit of the same arguments carries the same request id; other arguments
// get a new one.
func TestRequestIDs(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.result = func([]string) Outcome { return Outcome{Err: apperr.New(apperr.Unavailable, "tmux hiccup")} }
	h.selectRow("r:al-12-r2")
	h.key("S", "ctrl+s", "ctrl+s")
	h.typeText("why")
	h.key("ctrl+s")
	if len(h.reqs) != 3 || h.reqs[0] != h.reqs[1] || h.reqs[1] == h.reqs[2] {
		t.Fatalf("request ids %v", h.reqs)
	}
	if !strings.Contains(h.m.dlg.form.Err, "UNAVAILABLE: tmux hiccup") {
		t.Fatalf("error %q", h.m.dlg.form.Err)
	}
}

func TestEditDialogConflict(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.key("e")
	if h.m.dlg == nil || h.m.dlg.form.Get("title") != "Docs" || h.m.dlg.form.Get("blocked") != "al-16" {
		t.Fatalf("dialog %+v", h.m.dlg)
	}
	h.key("ctrl+s")
	if len(h.calls) != 0 || h.m.dlg.form.Err != "nothing changed" {
		t.Fatalf("unchanged: %v %q", h.calls, h.m.dlg.form.Err)
	}
	h.key("end")
	h.typeText("!")
	for range 4 {
		h.key("tab")
	}
	h.key("ctrl+u") // no longer blocked by al-16
	h.typeText("be-1")
	h.result = func([]string) Outcome {
		return Outcome{Err: apperr.New(apperr.Conflict, "task al-13 is at version 9, expected 7")}
	}
	h.key("ctrl+s")
	h.wantCall("--project", "alpha", "task", "edit", "al-13", "--title", "Docs!", "--add-dep", "blocks:be-1", "--rm-dep", "blocks:al-16",
		"--version", "7")
	if h.m.dlg == nil || !strings.Contains(h.m.dlg.form.Err, "changed meanwhile") || !h.m.dlg.stale {
		t.Fatalf("conflict: %+v", h.m.dlg)
	}
	// The reload brings the new version; the form keeps the user's values.
	h.snap = func() *data.Snapshot {
		s := fixture.Snapshot(false)
		s.Open[1].Version = 9
		return s
	}
	h.now = h.now.Add(loadGap)
	h.fire("load")
	h.result = nil
	h.key("ctrl+s")
	h.wantCall("--project", "alpha", "task", "edit", "al-13", "--title", "Docs!", "--add-dep", "blocks:be-1", "--rm-dep", "blocks:al-16",
		"--version", "9")
	if h.m.dlg != nil {
		t.Fatal("dialog open after success")
	}
}

func TestTaskStateActions(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.key("x")
	h.typeText("not needed")
	h.key("ctrl+s")
	h.wantCall("--project", "alpha", "task", "cancel", "al-13", "--reason", "not needed", "--version", "0")
	h.key("b")
	h.typeText("waits")
	h.key("ctrl+s")
	h.wantCall("--project", "alpha", "task", "block", "al-13", "--reason", "waits")
	h.key("R")
	h.wantCall("--project", "alpha", "task", "ready", "al-13")
	h.selectRow("t:al-14")
	h.key("u")
	h.wantCall("--project", "alpha", "run", "resume", "al-14")
	// Actions that need a task say so.
	n := len(h.calls)
	h.selectRow("p:alpha")
	h.key("x")
	if h.m.dlg != nil || len(h.calls) != n || !h.m.toast.bad {
		t.Fatalf("x on a project: %+v %q", h.m.dlg, h.m.toast.text)
	}
}

func TestStartPicker(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.result = func(args []string) Outcome {
		return Outcome{Data: &run.StartResult{Run: &run.Run{ID: "al-13-r1"}}, Human: "started al-13-r1 for task al-13\n"}
	}
	h.selectRow("t:al-13")
	h.key("s")
	l := h.m.dlg.form.Field("profile")
	if l.Value() != "sonnet" || l.Opts[0].Disabled == "" {
		t.Fatalf("picker %+v", l)
	}
	// The unavailable profile cannot be chosen, by key or click.
	h.key("up")
	x, y := h.dialogPart("profile", "", 0)
	h.click(x, y)
	if l.Value() != "sonnet" {
		t.Fatalf("chose %s", l.Value())
	}
	x, y = h.dialogPart("profile", "", 1)
	h.click(x, y)
	h.click(x, y) // a double click submits
	h.wantCall("--project", "alpha", "run", "start", "al-13", "--profile", "sonnet", "--no-attach")
	if h.m.dlg == nil || h.m.dlg.form.Title != "Run started" || h.m.want != "r:al-13-r1" {
		t.Fatalf("result %+v want %q", h.m.dlg, h.m.want)
	}
	h.key("enter") // [Close]
	if h.m.dlg != nil {
		t.Fatal("result dialog stays")
	}
}

func TestStopAndPark(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.key("S")
	if len(h.m.dlg.form.Buttons) != 2 {
		t.Fatalf("buttons %+v (no open ask: no Park)", h.m.dlg.form.Buttons)
	}
	h.typeText("enough")
	h.key("ctrl+s")
	h.wantCall("--project", "alpha", "run", "stop", "al-12-r2", "--reason", "enough")
	// al-14-r1 asked #42: Park is offered.
	h.selectRow("r:al-14-r1")
	h.key("S")
	x, y := h.dialogPart("", "park", -1)
	h.click(x, y)
	h.wantCall("--project", "alpha", "run", "stop", "al-14-r1", "--park")
	h.key("p", "enter")
	h.wantCall("--project", "alpha", "run", "stop", "al-14-r1", "--park")
	// An ended run has nothing to stop.
	n := len(h.calls)
	h.selectRow("t:al-9")
	h.key("S")
	if h.m.dlg != nil || len(h.calls) != n {
		t.Fatal("stop of an ended run")
	}
}

func TestCoordinator(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("p:beta")
	h.key("c", "enter")
	h.wantCall("--project", "beta", "coord", "beta", "--profile", "sonnet")
	h.key("esc") // the result
	// alpha's coordinator runs: c selects it.
	n := len(h.calls)
	h.selectRow("t:al-13")
	h.key("c")
	if h.selected() != "c:alpha" || h.m.dlg != nil || len(h.calls) != n {
		t.Fatalf("selected %s", h.selected())
	}
}

func TestReviewDialog(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.snap = func() *data.Snapshot {
		s := fixture.Snapshot(false)
		s.Open[0].Status = task.Review
		return s
	}
	h.send(loadMsg{snap: h.snap()})
	h.selectRow("t:al-17") // the review task: v reviews the task it reviews
	h.key("v")
	if h.m.dlg == nil || h.m.dlg.form.Title != "Review al-12" {
		t.Fatalf("dialog %+v", h.m.dlg)
	}
	x, y := h.dialogPart("", "reject", -1)
	h.click(x, y)
	if len(h.calls) != 0 || h.m.dlg.form.Err != "a reject needs a reason" {
		t.Fatalf("reject without reason: %v %q", h.calls, h.m.dlg.form.Err)
	}
	h.typeText("tests fail")
	x, y = h.dialogPart("", "reject", -1) // the error line moved the buttons
	h.click(x, y)
	h.wantCall("--project", "alpha", "review", "al-12", "--reject", "--", "tests fail")
	h.selectRow("t:al-12")
	h.key("v")
	h.key("ctrl+s") // accept, no note
	h.wantCall("--project", "alpha", "review", "al-12", "--accept")
	h.key("v")
	h.typeText("good")
	h.key("ctrl+s")
	h.wantCall("--project", "alpha", "review", "al-12", "--accept", "--", "good")
	h.selectRow("t:al-13")
	n := len(h.calls)
	h.key("v")
	if h.m.dlg != nil || len(h.calls) != n {
		t.Fatal("review of a ready task")
	}
}

func TestDeliverDialog(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-9")
	h.key("D")
	f := h.m.dlg.form
	if f.Field("delete").On || f.Field("force").On || !strings.Contains(f.Fields[0].Text, "branch otag/al-9") {
		t.Fatalf("defaults: %+v %q", f.Fields, f.Fields[0].Text)
	}
	h.key("ctrl+s")
	h.wantCall("--project", "alpha", "task", "deliver", "al-9")
	if h.m.dlg == nil || h.m.dlg.form.Title != "Delivered al-9" {
		t.Fatalf("result %+v", h.m.dlg)
	}
	h.key("esc")
	// From the group row; delete the branch; force asks again.
	h.selectRow("g:deliver:alpha")
	h.key("D")
	f = h.m.dlg.form
	h.typeText("merged by hand")
	x, y := h.dialogPart("delete", "", -1)
	h.click(x, y)
	h.key("tab", "space") // force
	h.key("ctrl+s")
	if len(h.calls) != 1 || f.Buttons[1].ID != "force" {
		t.Fatalf("no second confirm: %v %+v", h.calls, f.Buttons)
	}
	h.key("enter") // the focus is on Back
	if f.Buttons[0].ID != "deliver" || len(h.calls) != 1 {
		t.Fatalf("back: %+v", f.Buttons)
	}
	h.key("ctrl+s")
	x, y = h.dialogPart("", "force", -1)
	h.click(x, y)
	h.wantCall("--project", "alpha", "task", "deliver", "al-9", "--note", "merged by hand", "--delete-branch", "--force")
}

// Force is never one gesture away: a double click on [Deliver] or ctrl+s twice lands
// on Back (the default of the second step), not on force.
func TestDeliverForceNeedsItsOwnClick(t *testing.T) {
	for _, how := range []string{"double click", "ctrl+s twice"} {
		h := newHarness(t, 120, 40)
		h.selectRow("t:al-9")
		h.key("D", "tab", "tab", "space") // note → delete → force
		if !h.m.dlg.form.Field("force").On {
			t.Fatal("force not checked")
		}
		if how == "ctrl+s twice" {
			h.key("ctrl+s", "ctrl+s")
		} else {
			x, y := h.dialogPart("", "deliver", -1)
			h.click(x, y)
			h.click(x, y)
		}
		if len(h.calls) != 0 {
			t.Fatalf("%s: %q", how, h.calls)
		}
		// Explicitly: Tab to [Deliver with force], Enter.
		h.key("ctrl+s", "tab", "enter")
		h.wantCall("--project", "alpha", "task", "deliver", "al-9", "--force")
	}
}

// A late result never replaces the dialog in use nor moves the selection: the result
// of a dialog closed with Esc is a toast; another action's result dialog waits.
func TestLateResults(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.result = func([]string) Outcome {
		return Outcome{Data: &run.StartResult{Run: &run.Run{ID: "al-13-r1"}}, Human: "started al-13-r1 for task al-13\n"}
	}
	h.selectRow("t:al-13")
	h.key("s")
	h.hold = true
	h.key("enter", "esc", "n")
	h.typeText("my new task")
	h.release()
	h.fire("load")
	if h.m.dlg == nil || h.m.dlg.form.Title != "New task in alpha" || h.m.dlg.form.Get("title") != "my new task" ||
		h.m.want != "" || h.selected() != "t:al-13" || !strings.Contains(h.m.toast.text, "started al-13-r1") {
		t.Fatalf("dialog %+v want %q selected %s toast %q", h.m.dlg, h.m.want, h.selected(), h.m.toast.text)
	}
	// [Mark read] finishing under an open dialog: its result waits for it.
	h.key("esc", "4")
	h.hold = true
	x, y := h.at(regItemAct, "1:0")
	h.click(x, y)
	h.key("n") // n is not about a row: it works from the Inbox too
	h.release()
	if h.m.dlg.form.Title != "New task in alpha" || len(h.m.queued) != 1 {
		t.Fatalf("dialog %q queued %d", h.m.dlg.form.Title, len(h.m.queued))
	}
	h.key("esc")
	if h.m.dlg == nil || h.m.dlg.form.Title != "Messages (now read)" {
		t.Fatalf("queued result: %+v", h.m.dlg)
	}
}

// A send finishing after the user entered reply mode leaves reply mode alone.
func TestSendKeepsNewReplyMode(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	h.key("m")
	h.typeText("hello")
	h.hold = true
	h.key("enter", "esc")
	h.selectRow("t:al-14")
	h.key("r")
	h.typeText("blue")
	h.release()
	if h.m.box.reply != 42 {
		t.Fatalf("reply mode lost: %d", h.m.box.reply)
	}
	h.key("enter")
	h.wantCall("--project", "alpha", "reply", "42", "--", "helloblue")
}

// The Needs selection is the item, not its position: a reload that drops an item
// above it keeps it.
func TestItemSelectionByKey(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("3", "tab", "down") // the blocked task al-16
	s := fixture.Snapshot(false)
	s.Needs = &msg.Needs{Blocked: []msg.NeedBlocked{
		{Task: "al-99", Title: "Other", Reason: "y", Action: "otag task ready al-99"},
		{Task: "al-16", Title: "Blocked thing", Reason: "x", Action: "otag task ready al-16"}}}
	h.snap = func() *data.Snapshot { return s }
	h.send(loadMsg{snap: s})
	h.key("enter")
	h.wantCall("--project", "alpha", "task", "ready", "al-16")
}

// With Needs or Inbox focused, action keys act on the selected item (or refuse),
// never on the tree row out of sight.
func TestActionKeysOnItems(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.key("3", "tab", "down") // al-16, blocked
	h.key("R")
	h.wantCall("--project", "alpha", "task", "ready", "al-16")
	n := len(h.calls)
	h.key("u", "D", "x")
	if len(h.calls) != n || h.m.dlg != nil || !h.m.toast.bad {
		t.Fatalf("calls %q dialog %v toast %q", h.calls[n:], h.m.dlg, h.m.toast.text)
	}
	h.key("up", "r") // the question: its Reply dialog
	if h.m.dlg == nil || h.m.dlg.form.Title != "Reply to #42" {
		t.Fatalf("r on a question: %+v", h.m.dlg)
	}
	h.key("esc", "n") // n is not about a row: the new task dialog
	if h.m.dlg == nil || h.m.dlg.form.Title != "New task in alpha" {
		t.Fatalf("n: %+v", h.m.dlg)
	}
	// From the tree, the same keys act on the tree row.
	h.key("esc", "shift+tab", "R")
	h.wantCall("--project", "alpha", "task", "ready", "al-13")
}

// The busy key is the whole command line: another target or text is not "already
// running"; the same one is.
func TestBusyKeyIsTheCommand(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.hold = true
	h.selectRow("t:al-13")
	h.key("R")
	h.selectRow("t:al-16")
	h.key("R")
	h.selectRow("t:al-13")
	h.key("R")
	if len(h.calls) != 2 || !strings.Contains(h.m.toast.text, "already running") {
		t.Fatalf("calls %q toast %q", h.calls, h.m.toast.text)
	}
}

// The wanted profile stays selected when it is unavailable: Start says why instead of
// starting another model.
func TestStartKeepsUnavailableProfile(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.snap = func() *data.Snapshot {
		s := fixture.Snapshot(false)
		s.Open[1].Profile = "opus"
		return s
	}
	h.send(loadMsg{snap: h.snap()})
	h.selectRow("t:al-13")
	h.key("s", "enter")
	if len(h.calls) != 0 || !strings.Contains(h.m.dlg.form.Err, "opus is not available") {
		t.Fatalf("calls %q err %q", h.calls, h.m.dlg.form.Err)
	}
	h.key("down", "enter")
	h.wantCall("--project", "alpha", "run", "start", "al-13", "--profile", "sonnet", "--no-attach")
}

// When the selected item goes away, the item that takes its place becomes the
// selection: a later reload with a new item above it does not move it again.
func TestItemFallbackIsAdopted(t *testing.T) {
	h := newHarness(t, 120, 40)
	mk := func(notes []int64, extraMsg bool) *data.Snapshot {
		s := fixture.Snapshot(false)
		s.Notes = nil
		for _, n := range notes {
			s.Notes = append(s.Notes, notify.Notification{ID: n, Project: "alpha", Kind: "run.crashed",
				Severity: notify.Warn, Title: "x"})
		}
		s.Unread = len(notes)
		if extraMsg {
			s.Inbox = append(s.Inbox, msg.Item{Message: msg.Message{ID: 50, Project: "alpha", Kind: msg.KindInfo, Body: "new"}})
		}
		return s
	}
	h.send(loadMsg{snap: mk([]int64{7, 8, 9}, false)})
	h.key("4", "tab")
	for range 10 {
		if items := h.m.items(h.m.tab); items[h.m.itemIndex(h.m.tab, items)].Key == "n:8" {
			break
		}
		h.key("down")
	}
	h.key("enter")
	h.wantCall("notify", "ack", "8")
	h.send(loadMsg{snap: mk([]int64{7, 9}, false)}) // 8 acked: 9 takes its place...
	h.send(loadMsg{snap: mk([]int64{7, 9}, true)})  // ...and stays selected when a message arrives
	h.key("enter")
	h.wantCall("notify", "ack", "9")
}

// Footer buttons act like their keys: on the selected item while Needs has the focus.
func TestFooterOnItems(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.key("3", "tab") // the question #42
	for i, b := range hints() {
		if b.act == actReply {
			x, y := h.at(regHint, strconv.Itoa(i))
			h.click(x, y)
		}
	}
	if h.m.dlg == nil || h.m.dlg.form.Title != "Reply to #42" {
		t.Fatalf("footer reply on the question: %+v", h.m.dlg)
	}
	h.key("esc")
	for i, b := range hints() {
		if b.act == actStart {
			x, y := h.at(regHint, strconv.Itoa(i))
			h.click(x, y)
		}
	}
	if h.m.dlg != nil || !h.m.toast.bad || len(h.calls) != 0 {
		t.Fatalf("footer start on an item: %+v %q", h.m.dlg, h.m.toast.text)
	}
}

func TestHelpCtrlCWaitsToo(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.hold = true
	h.key("R", "?", "ctrl+c")
	if h.quit || !h.m.quitting {
		t.Fatal("ctrl+c in the help quit at once")
	}
}

func TestReplyMode(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-14")
	h.key("r")
	if h.m.focus != focusBox || h.m.box.reply != 42 {
		t.Fatalf("focus %v reply %d", h.m.focus, h.m.box.reply)
	}
	h.typeText("blue")
	h.key("alt+enter")
	h.typeText("please")
	h.key("enter")
	h.wantCall("--project", "alpha", "reply", "42", "--", "blue\nplease")
	if h.m.box.reply != 0 || h.m.box.ed.String() != "" {
		t.Fatalf("box after the reply: %+v", h.m.box)
	}
	// No open question: r says so.
	h.key("esc")
	h.selectRow("t:al-13")
	h.key("r")
	if h.m.focus == focusBox || !h.m.toast.bad {
		t.Fatal("reply without a question")
	}
}

func TestMessageBox(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("r:al-12-r2")
	if h.m.box.to.to != "al-12-r2" {
		t.Fatalf("recipient %+v", h.m.box.to)
	}
	h.key("m")
	h.key("enter")
	if len(h.calls) != 0 || !h.m.toast.bad {
		t.Fatal("empty message sent")
	}
	h.typeText("q is a letter")
	h.key("ctrl+k")
	h.key("enter")
	h.wantCall("send", "--project", "alpha", "--kind", "command", "--", "al-12-r2", "q is a letter")
	if h.m.box.ed.String() != "" || h.quit {
		t.Fatal("not cleared")
	}
	// The recipient picker, by mouse; [Send] by mouse.
	x, y := h.at(regBoxTo, "")
	h.click(x, y)
	if h.m.dlg == nil {
		t.Fatal("no picker")
	}
	x, y = h.dialogPart("to", "", 2) // this run, this task, coord, human
	h.click(x, y)
	h.key("enter")
	if h.m.box.to.to != "coord" || !h.m.box.pinned || h.m.focus != focusBox {
		t.Fatalf("picked %+v", h.m.box)
	}
	h.typeText("hi")
	x, y = h.at(regBoxSend, "")
	h.click(x, y)
	h.wantCall("send", "--project", "alpha", "--kind", "command", "--", "coord", "hi")
	// Pinned: the recipient no longer follows the selection.
	h.key("esc")
	h.selectRow("t:al-13")
	if h.m.box.to.to != "coord" {
		t.Fatalf("pinned recipient moved: %+v", h.m.box.to)
	}
	// m takes the selection again; a paste goes into the box.
	h.key("m")
	h.send(tea.PasteMsg{Content: "pasted\ntext"})
	if h.m.box.to.to != "al-13" || h.m.box.ed.String() != "pasted\ntext" {
		t.Fatalf("box %+v %q", h.m.box.to, h.m.box.ed.String())
	}
	// Tab leaves the box along the focus ring; a failed send keeps the text.
	h.result = func([]string) Outcome { return Outcome{Err: apperr.New(apperr.NotFound, "task al-13 not found")} }
	h.key("enter")
	if h.m.box.ed.String() != "pasted\ntext" || !strings.Contains(h.m.toast.text, "NOT_FOUND") {
		t.Fatalf("after a failure: %q %q", h.m.box.ed.String(), h.m.toast.text)
	}
	h.key("tab")
	if h.m.focus != focusTree {
		t.Fatalf("focus %v", h.m.focus)
	}
}

func TestNeedsAndInboxActions(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("3", "tab")
	h.key("enter") // the question: the reply dialog
	if h.m.dlg == nil || !strings.Contains(h.m.dlg.form.Fields[0].Text, "Which color?") {
		t.Fatalf("reply dialog %+v", h.m.dlg)
	}
	h.typeText("green")
	h.key("ctrl+s")
	h.wantCall("--project", "alpha", "reply", "42", "--", "green")
	h.key("down", "enter") // the blocked task
	h.wantCall("--project", "alpha", "task", "ready", "al-16")
	// Inbox: a click on [Ack]; A acks all after a confirm.
	h.key("4")
	x, y := h.at(regItemAct, "5:0")
	h.click(x, y)
	h.wantCall("notify", "ack", "7")
	h.key("A")
	if h.m.dlg == nil {
		t.Fatal("no confirm")
	}
	h.key("enter")
	h.wantCall("notify", "ack", "--all")
	x, y = h.at(regItemAct, "1:0")
	h.click(x, y)
	h.wantCall("inbox")
}

func TestReadOnly(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.snap = func() *data.Snapshot {
		s := fixture.Snapshot(false)
		s.ReadOnly = true
		return s
	}
	h.send(loadMsg{snap: h.snap()})
	h.selectRow("t:al-13")
	for _, k := range []string{"n", "s", "e", "x", "R", "D"} {
		h.key(k)
		if h.m.dlg != nil || !strings.HasPrefix(h.m.toast.text, "read-only") {
			t.Fatalf("%s: dialog %v toast %q", k, h.m.dlg, h.m.toast.text)
		}
	}
	h.key("m")
	h.typeText("x")
	h.key("enter")
	if len(h.calls) != 0 {
		t.Fatalf("calls %v", h.calls)
	}
}

func TestQuitWaitsForActions(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.hold = true
	h.key("R", "q")
	if h.quit || len(h.pending("quit")) != 1 || !strings.Contains(h.m.toast.text, "finishing 1") {
		t.Fatalf("quit %v pending %v toast %q", h.quit, h.pending("quit"), h.m.toast.text)
	}
	h.release()
	if !h.quit {
		t.Fatal("no quit after the action")
	}
	// With an action stuck, the timer quits anyway.
	h = newHarness(t, 120, 40)
	h.selectRow("t:al-13")
	h.hold = true
	h.key("R", "q")
	h.fire("quit")
	if !h.quit {
		t.Fatal("no quit after the wait")
	}
}

// A dialog owns the keyboard and ignores clicks outside it; its checkbox, select and
// buttons work with the mouse.
func TestDialogMouse(t *testing.T) {
	h := newHarness(t, 120, 40)
	h.key("n")
	sel := h.selected()
	x, y := h.at(regRow, "t:al-13")
	h.click(x, y)
	if h.selected() != sel || h.m.dlg == nil {
		t.Fatal("click outside the dialog went through")
	}
	h.key("q") // typed into the title
	if h.quit || h.m.dlg.form.Get("title") != "q" {
		t.Fatalf("q in a dialog: quit %v title %q", h.quit, h.m.dlg.form.Get("title"))
	}
	x, y = h.dialogPart("draft", "", -1)
	h.click(x, y)
	x, y = h.dialogPart("profile", "", -1)
	h.click(x, y)
	x, y = h.dialogPart("body", "", -1)
	h.click(x, y)
	h.typeText("b")
	x, y = h.dialogPart("", "add", -1)
	h.click(x, y)
	h.wantCall("task", "add", "--project", "alpha", "--body", "b", "--profile", "opus", "--draft", "--", "q")
	x, y = h.at(regToast, "")
	h.click(x, y)
	if h.m.tab != tabs.Log {
		t.Fatal("toast click")
	}
}

// The TUI never writes the database: no Exec or Tx under internal/tui.
func TestNoWritesInTUI(t *testing.T) {
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, bad := range []string{"ExecContext(", ".Exec(", ".Tx(", "BeginTx(ctx, nil)"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s: %s", path, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
