package tui_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/tmux"
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

// outer is a throwaway tmux server (-L otag-tui-test-<pid>-<rand>) whose one pane runs
// `otag tui`, driven like a terminal: send-keys in, capture-pane out.
type outer struct {
	t      *testing.T
	socket string
	target string // the pane keys go to and the screen comes from (default "t")
	conf   string // the server's config (default none)
}

func (o *outer) tmux(args ...string) (string, error) {
	conf := o.conf
	if conf == "" {
		conf = os.DevNull
	}
	cmd := exec.Command("tmux", append([]string{"-L", o.socket, "-f", conf}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_PANE=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func startTUI(t *testing.T, e *tuitest.Env, w, h int) *outer {
	t.Helper()
	o := &outer{t: t, socket: fmt.Sprintf("otag-tui-test-%d-%d", os.Getpid(), rand.IntN(1e9))}
	t.Cleanup(func() {
		_, _ = o.tmux("kill-server")
		_ = os.Remove(tmux.SocketPath(o.socket)) // tmux leaves the socket file behind
	})
	// Outside tmux: the TUI's attach is the exec path (TestE2EAttachPopup runs it inside).
	argv := []string{"new-session", "-d", "-s", "t", "-x", fmt.Sprint(w), "-y", fmt.Sprint(h), "--",
		"env", "-u", "TMUX", "-u", "TMUX_PANE"}
	argv = append(append(argv, e.Environ()...), "TERM=xterm-256color", otagBin, "tui")
	if out, err := o.tmux(argv...); err != nil {
		t.Fatalf("start tui: %v\n%s", err, out)
	}
	return o
}

func (o *outer) pane() string {
	if o.target == "" {
		return "t"
	}
	return o.target
}

func (o *outer) screen() string {
	out, _ := o.tmux("capture-pane", "-p", "-t", o.pane())
	return out
}

// waitFor polls the screen until ok accepts it, for up to timeout.
func (o *outer) waitFor(what string, timeout time.Duration, ok func(string) bool) (string, time.Duration) {
	o.t.Helper()
	start := time.Now()
	var s string
	for time.Since(start) < timeout {
		if s = o.screen(); ok(s) {
			return s, time.Since(start)
		}
		time.Sleep(20 * time.Millisecond)
	}
	o.t.Fatalf("timeout waiting for %s; screen:\n%s", what, s)
	return "", 0
}

func contains(subs ...string) func(string) bool {
	return func(s string) bool {
		for _, sub := range subs {
			if !strings.Contains(s, sub) {
				return false
			}
		}
		return true
	}
}

func (o *outer) keys(keys ...string) {
	o.t.Helper()
	if out, err := o.tmux(append([]string{"send-keys", "-t", o.pane()}, keys...)...); err != nil {
		o.t.Fatalf("send-keys %v: %v\n%s", keys, err, out)
	}
}

// click sends a left click at the 0-based cell x, y as the SGR mouse sequences a
// terminal would (the TUI turned SGR mouse reporting on).
func (o *outer) click(x, y int) {
	o.t.Helper()
	o.keys("-l", fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", x+1, y+1, x+1, y+1))
}

// find returns the cell where text first appears on screen.
func find(screen, text string) (int, int, bool) {
	for y, line := range strings.Split(screen, "\n") {
		if i := strings.Index(line, text); i >= 0 {
			return len([]rune(line[:i])), y, true
		}
	}
	return 0, 0, false
}

// E2E 1: start-up shows the projects, tasks and runs of the database with the header
// counts; a click selects and previews a run; a change made by the CLI shows quickly
// and the selection stays.
func TestE2EStartup(t *testing.T) {
	e := tuitest.New(t, otagBin)
	fix := e.AddTask("alpha", "Fix login", "--priority", "2")
	e.AddTask("alpha", "Add CSV export")
	e.AddTask("alpha", "Docs", "--draft")
	blocked := e.AddTask("beta", "Beta thing")
	e.OK("task", "block", blocked, "--reason", "waits")
	runID := e.Start(fix)
	e.WaitRun(runID, func(status, _, _ string) bool { return status == "idle" })

	o := startTUI(t, e, 160, 45)
	s, _ := o.waitFor("the tree", 10*time.Second, contains("▾ alpha", "▾ beta", "Fix login", "Add CSV export",
		"Docs", "Beta thing", "r1 manual"))
	// The idle run and the blocked task need the human; the daemon is not running.
	for _, want := range []string{"⚑ 2 needs you", "✉ 0", "daemon ○", "3 Needs 2"} {
		if !strings.Contains(s, want) {
			t.Errorf("screen lacks %q:\n%s", want, s)
		}
	}

	x, y, ok := find(s, "r1 manual")
	if !ok {
		t.Fatal("no run row")
	}
	o.click(x, y)
	o.waitFor("the preview", 5*time.Second, contains(runID+" · manual · idle", "FOLLOW", "fake: start", "fake: wait"))

	e.AddTask("alpha", "Latency probe")
	s, took := o.waitFor("the new task", 5*time.Second, contains("Latency probe"))
	t.Logf("a task added by the CLI showed after %v", took)
	if took > 500*time.Millisecond {
		t.Errorf("the change took %v to show", took)
	}
	if !strings.Contains(s, runID+" · manual · idle") {
		t.Errorf("the selection moved:\n%s", s)
	}

	// Keys: the Needs tab, then quit.
	o.keys("3")
	o.waitFor("the Needs tab", 5*time.Second, contains("Blocked tasks (1)", "Idle runs (1)"))
	o.keys("q")
	for i := 0; i < 100; i++ {
		if _, err := o.tmux("has-session", "-t", "t"); err != nil {
			return // the TUI exited and its session with it
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("q did not quit:\n%s", o.screen())
}
