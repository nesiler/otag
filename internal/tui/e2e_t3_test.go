package tui_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/tuitest"
)

// idle accepts a run that is idle.
func idle(status, _, _ string) bool { return status == "idle" }

// pasteLog returns the texts pastecheck submitted so far.
func pasteLog(e *tuitest.Env) []string {
	b, _ := os.ReadFile(e.PasteLog)
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// pasteLogWait waits up to 30 s for n texts and returns what there is.
func pasteLogWait(e *tuitest.Env, n int) []string {
	var got []string
	for i := 0; i < 1500; i++ {
		if got = pasteLog(e); len(got) >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return got
}

// waitLog waits until pastecheck submitted n texts.
func waitLog(t *testing.T, e *tuitest.Env, n int) []string {
	t.Helper()
	var got []string
	for i := 0; i < 1500; i++ {
		if got = pasteLog(e); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pastecheck submitted %d of %d: %q", len(got), n, got)
	return nil
}

// tuiBack waits for the TUI's own screen after an attach: its footer and header, and
// no status bar of otag's tmux server.
func (o *outer) tuiBack(what string) string {
	o.t.Helper()
	s, _ := o.waitFor(what, 10*time.Second, func(s string) bool {
		return strings.Contains(s, "[q] quit") && strings.Contains(s, " otag │") && !strings.Contains(s, "← otag")
	})
	return s
}

// attached waits for otag's tmux server on screen: the agent's window and the status
// bar EnsureUI put there.
func (o *outer) attached(runID, text string) string {
	o.t.Helper()
	s, _ := o.waitFor("the attached window of "+runID, 10*time.Second, contains("← otag", "M-q / C-q: back to otag",
		"run "+runID, text))
	return s
}

// clickBackButton clicks "← otag" in otag's status bar.
func (o *outer) clickBackButton() {
	o.t.Helper()
	s := o.screen()
	x, y, ok := find(s, "← otag")
	if !ok {
		o.t.Fatalf("no ← otag:\n%s", s)
	}
	o.click(x+2, y)
}

// mouseWorks checks that clicks reach the TUI after a return: a click on a row selects
// it (the message box follows).
func (o *outer) mouseWorks(title, id string) {
	o.t.Helper()
	o.selectTask(title, id)
}

// E2E 5: typing mode. The run's pane is an input box with paste detection
// (pastecheck, like Claude's): text typed fast and an Enter right after it would be a
// newline inside a paste. Through the TUI each key is its own send-keys and Enter is
// held 150 ms after the text, so all ten messages submit, each exactly once and whole.
func TestE2ETyping(t *testing.T) {
	e := tuitest.New(t, otagBin)
	id := e.AddTask("alpha", "Type into me")
	runID := e.Start(id, "--profile", "typist")
	e.WaitRun(runID, idle)
	o := startTUI(t, e, 160, 45)
	o.clickText("r1 typist")
	o.waitFor("the preview", 10*time.Second, contains(runID+" · typist", "pastecheck ready"))
	o.keys("i")
	o.waitFor("typing mode", 5*time.Second, contains("TYPING → "+runID, "C-q / M-q / click outside to leave"))

	var want []string
	for i := 0; i < 10; i++ {
		text := fmt.Sprintf("message %d: fix the login bug, then run the tests", i)
		want = append(want, text)
		o.typeText(text) // one write: the TUI reads the keys in a burst
		o.keys("Enter")  // at once, as a fast typist would
	}
	if got := pasteLogWait(e, 10); len(got) < 10 {
		agent, _ := e.Tmux.Control(paneOf(t, e, runID)).Capture(t.Context())
		t.Fatalf("submitted %d of 10: %q\nTUI:\n%s\nagent:\n%s", len(got), got, o.screen(), agent)
	}
	if got := waitLog(t, e, 10); !slices.Equal(got, want) {
		t.Fatalf("submitted %q\nwant      %q", got, want)
	}
	o.waitFor("the submits in the preview", 5*time.Second, contains("SUBMIT[message 9: fix the login bug"))

	// A paste (bracketed, from the terminal) goes as a paste; its Enter waits 500 ms.
	o.keys("-l", "\x1b[200~pasted one\npasted two\x1b[201~")
	o.keys("Enter")
	if got := waitLog(t, e, 11); got[10] != "pasted one⏎pasted two" {
		t.Fatalf("paste submitted as %q", got[10])
	}

	// Esc goes to the agent: typing mode stays. C-q leaves; the TUI has its keys back.
	o.escape()
	o.escape()
	if s := o.screen(); !strings.Contains(s, "TYPING → ") {
		t.Fatalf("Esc left typing mode:\n%s", s)
	}
	o.keys("C-q")
	o.waitFor("typing mode left", 5*time.Second, func(s string) bool {
		return !strings.Contains(s, "TYPING → ") && strings.Contains(s, runID+" · typist")
	})
	o.keys("2")
	o.waitFor("the Details tab", 5*time.Second, contains("details of "+runID))

	// The control: the same text and Enter as two quick tmux commands, without the
	// gate, is taken for a paste and does not submit.
	c := e.Tmux.Control(paneOf(t, e, runID))
	if err := c.SendText(t.Context(), "no gate"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendKeys(t.Context(), "Enter"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := pasteLog(e); len(got) != 11 {
		t.Fatalf("without the gate the Enter submitted: %q", got)
	}
}

// paneOf returns the tmux pane of a run.
func paneOf(t *testing.T, e *tuitest.Env, runID string) string {
	t.Helper()
	var r struct {
		Session struct {
			Pane string `json:"tmux_pane"`
		} `json:"session"`
	}
	e.JSON(&r, "run", "show", runID)
	if r.Session.Pane == "" {
		t.Fatalf("run %s has no pane", runID)
	}
	return r.Session.Pane
}

// E2E 6: attach, exec path (the TUI outside tmux): Enter attaches; M-q, C-q and a
// click on "← otag" each return; the mouse works afterwards. `otag run attach` gets the
// same return keys.
func TestE2EAttachExec(t *testing.T) {
	e := tuitest.New(t, otagBin)
	id := e.AddTask("alpha", "Attach to me")
	other := e.AddTask("alpha", "Another task")
	runID := e.Start(id)
	e.WaitRun(runID, idle)

	// otag run attach applies the bindings itself: C-q returns to the shell.
	o := startTUI(t, e, 160, 45)
	argv := append([]string{"new-window", "-d", "-t", "t:", "-n", "cli", "--", "env", "-u", "TMUX", "-u", "TMUX_PANE"},
		e.Environ()...)
	argv = append(argv, "TERM=xterm-256color", "sh", "-c", otagBin+" run attach "+runID+"; echo DETACHED; sleep 60")
	if out, err := o.tmux(argv...); err != nil {
		t.Fatalf("new-window: %v\n%s", err, out)
	}
	cli := &outer{t: t, socket: o.socket, target: "t:cli"}
	cli.attached(runID, "fake: wait")
	cli.keys("C-q")
	cli.waitFor("otag run attach to end", 10*time.Second, contains("DETACHED"))
	if out, err := o.tmux("kill-window", "-t", "t:cli"); err != nil {
		t.Fatalf("kill-window: %v\n%s", err, out)
	}

	o.clickText("r1 manual")
	o.waitFor("the preview", 10*time.Second, contains(runID+" · manual", "fake: wait"))
	o.keys("Enter")
	o.attached(runID, "fake: wait")
	o.keys("M-q")
	o.tuiBack("the TUI after M-q")
	o.mouseWorks("Another task", other)

	// A double click on the run row, then C-q.
	s := o.screen()
	x, y, ok := find(s, "r1 manual")
	if !ok {
		t.Fatalf("no run row:\n%s", s)
	}
	o.click(x, y)
	o.click(x, y)
	o.attached(runID, "fake: wait")
	o.keys("C-q")
	o.tuiBack("the TUI after C-q")
	o.mouseWorks("Attach to me", id)

	// Enter on the task row (its run is live), then a click on "← otag".
	o.keys("Enter")
	o.attached(runID, "fake: wait")
	o.clickBackButton()
	o.tuiBack("the TUI after a click on ← otag")
	o.mouseWorks("Another task", other)
	if s := o.screen(); strings.Contains(s, "[<0;") || strings.Contains(s, "attach "+runID+" (exec)") {
		t.Fatalf("stray input or an attach error:\n%s", s)
	}
}

// startTUIInTmux runs `otag tui` inside a tmux of its own (the user's tmux, a throwaway
// server -L otag-tui-user-…, mouse on) whose client is the outer server's pane: the
// TUI's attach is the popup path.
func startTUIInTmux(t *testing.T, e *tuitest.Env, w, h int) (*outer, *outer) {
	t.Helper()
	o := &outer{t: t, socket: fmt.Sprintf("otag-tui-test-%d-%d", os.Getpid(), rand.IntN(1e9))}
	u := &outer{t: t, socket: fmt.Sprintf("otag-tui-user-%d-%d", os.Getpid(), rand.IntN(1e9)), target: "u"}
	conf := filepath.Join(t.TempDir(), "user.conf")
	if err := os.WriteFile(conf, []byte("set -g mouse on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	u.conf = conf
	t.Cleanup(func() {
		_, _ = u.tmux("kill-server")
		_ = os.Remove(tmux.SocketPath(u.socket))
		_, _ = o.tmux("kill-server")
		_ = os.Remove(tmux.SocketPath(o.socket))
	})
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	inner := []string{tmuxBin, "-L", u.socket, "-f", conf, "new-session", "-s", "u", "--", "env"}
	inner = append(append(inner, e.Environ()...), "TERM=xterm-256color", otagBin, "tui")
	argv := []string{"new-session", "-d", "-s", "t", "-x", fmt.Sprint(w), "-y", fmt.Sprint(h), "--",
		"env", "-u", "TMUX", "-u", "TMUX_PANE", "TERM=xterm-256color"}
	argv = append(argv, inner...)
	if out, err := o.tmux(argv...); err != nil {
		t.Fatalf("start the user's tmux: %v\n%s", err, out)
	}
	return o, u
}

// E2E 7: attach, popup path (the TUI inside the user's tmux): the window shows in a
// full-screen display-popup, not in the TUI's pane; M-q, C-q and a click on "← otag"
// each close it.
func TestE2EAttachPopup(t *testing.T) {
	e := tuitest.New(t, otagBin)
	id := e.AddTask("alpha", "Attach to me")
	other := e.AddTask("alpha", "Another task")
	runID := e.Start(id)
	e.WaitRun(runID, idle)
	o, u := startTUIInTmux(t, e, 160, 45)
	o.waitFor("the tree", 15*time.Second, contains("Attach to me", "r1 manual"))
	o.clickText("r1 manual")
	o.waitFor("the preview", 10*time.Second, contains(runID+" · manual", "fake: wait"))

	for i, back := range []func(){
		func() { o.keys("M-q") },
		func() { o.keys("C-q") },
		o.clickBackButton,
	} {
		o.keys("Enter")
		o.attached(runID, "fake: wait")
		// A popup: the TUI's own pane in the user's tmux does not show otag's server.
		if pane := u.screen(); strings.Contains(pane, "← otag") {
			t.Fatalf("attach %d is not a popup; the TUI's pane shows:\n%s", i, pane)
		}
		back()
		o.tuiBack(fmt.Sprintf("the TUI after return %d", i))
		o.mouseWorks("Another task", other)
		o.clickText("r1 manual")
		o.waitFor("the preview", 10*time.Second, contains(runID+" · manual"))
	}
}
