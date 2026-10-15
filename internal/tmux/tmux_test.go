package tmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	c := New(fmt.Sprintf("otag-test-%d-%d", os.Getpid(), time.Now().UnixNano()%1e6), os.DevNull)
	if !c.Available() {
		t.Skip("tmux not installed")
	}
	t.Cleanup(func() { _ = c.KillServer(context.Background()) })
	return c
}

func waitState(t *testing.T, c *Client, pane string, pid int, want string) Liveness {
	t.Helper()
	var l Liveness
	for i := 0; i < 50; i++ {
		// Right after exit the process can be gone before tmux marks the pane dead.
		if l = c.Check(context.Background(), pane, pid); l.State == want && (want != Exited || l.ExitStatus != nil) {
			return l
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane %s: state %+v, want %s", pane, l, want)
	return l
}

func TestWindowLifecycle(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	dir := t.TempDir()
	ref, err := c.NewWindow(ctx, Window{Session: "otag-p", Name: "p-1", Cwd: dir,
		Env: map[string]string{"OTAG_X": "hello"}, Argv: []string{"/bin/sh", "-c", `echo "$OTAG_X:$(pwd -P)"; sleep 30`}})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Session != "otag-p" || !strings.HasPrefix(ref.PaneID, "%") || ref.PID == 0 {
		t.Fatalf("ref %+v", ref)
	}
	waitState(t, c, ref.PaneID, ref.PID, Alive)
	var screen string
	for i := 0; i < 50 && !strings.Contains(screen, "hello:"); i++ {
		time.Sleep(20 * time.Millisecond)
		screen, _ = c.Control(ref.PaneID).Capture(ctx)
	}
	if !strings.Contains(screen, "hello:") {
		t.Fatalf("env not passed: %q", screen)
	}

	// A second window in the same session; its process exits with status 3.
	ref2, err := c.NewWindow(ctx, Window{Session: "otag-p", Name: "p-2", Argv: []string{"/bin/sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	l := waitState(t, c, ref2.PaneID, ref2.PID, Exited)
	if l.ExitStatus == nil || *l.ExitStatus != 3 {
		t.Fatalf("exit status %+v", l)
	}
	// A stale pid means the pane id was reused.
	if l := c.Check(ctx, ref.PaneID, ref.PID+100000); l.State != Gone {
		t.Fatalf("pid mismatch: %+v", l)
	}
	// KillPane refuses a pane whose process is not the recorded one.
	if killed, err := c.KillPane(ctx, ref.PaneID, ref.PID+100000); killed || err != nil {
		t.Fatalf("killed a pane with another pid: %v %v", killed, err)
	}
	if killed, err := c.KillPane(ctx, ref.PaneID, ref.PID); !killed || err != nil {
		t.Fatalf("KillPane: %v %v", killed, err)
	}
	waitState(t, c, ref.PaneID, ref.PID, Gone)
	if err := c.KillWindow(ctx, ref.PaneID); err != nil {
		t.Fatalf("killing a missing window: %v", err)
	}
	if l := c.Check(ctx, "", 0); l.State != Unknown {
		t.Fatalf("no pane: %+v", l)
	}
	// Last window gone: server exits; checks report gone, not errors.
	_ = c.KillWindow(ctx, ref2.PaneID)
	if l := c.Check(ctx, ref2.PaneID, 0); l.State != Gone {
		t.Fatalf("after server exit: %+v", l)
	}
}

func TestArgvIsNotTmuxSyntax(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	out := filepath.Join(t.TempDir(), "args")
	argv := []string{"/bin/sh", "-c", `printf '%s|' "$@" > "$0.tmp" && mv "$0.tmp" "$0"; sleep 30`, out,
		"ok;", "set-option", "-g", "@pwned", "yes", `x\;`, ";", "{", "}"}
	if _, err := c.NewWindow(ctx, Window{Session: "otag-i", Name: "w;", Cwd: t.TempDir(),
		Env: map[string]string{"OTAG_X": "v;"}, Argv: argv}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for i := 0; i < 100 && len(got) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		got, _ = os.ReadFile(out)
	}
	if want := `ok;|set-option|-g|@pwned|yes|x\;|;|{|}|`; string(got) != want {
		t.Fatalf("args %q, want %q", got, want)
	}
	if v, _ := c.run(ctx, "show-options", "-gv", "@pwned"); v != "" {
		t.Fatalf("tmux command injected: @pwned=%q", v)
	}

	// A single argument is exec'd, not handed to a shell.
	marker := filepath.Join(t.TempDir(), "marker")
	ref, err := c.NewWindow(ctx, Window{Session: "otag-i", Name: "one", Argv: []string{"touch " + marker}})
	if err != nil {
		t.Fatal(err)
	}
	l := waitState(t, c, ref.PaneID, ref.PID, Exited)
	if _, err := os.Stat(marker); err == nil || *l.ExitStatus == 0 {
		t.Fatalf("single argv ran through a shell (exit %d)", *l.ExitStatus)
	}
}

func TestSendKeys(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	ref, err := c.NewWindow(ctx, Window{Session: "otag-k", Name: "k", Argv: []string{"/bin/sh", "-c", `printf 'ok? '; read -r a; echo "got:$a"; sleep 30`}})
	if err != nil {
		t.Fatal(err)
	}
	p := c.Control(ref.PaneID)
	if err := p.SendText(ctx, "yes"); err != nil {
		t.Fatal(err)
	}
	if err := p.SendKeys(ctx, "Enter"); err != nil {
		t.Fatal(err)
	}
	var screen string
	for i := 0; i < 50 && !strings.Contains(screen, "got:yes"); i++ {
		time.Sleep(20 * time.Millisecond)
		screen, _ = p.Capture(ctx)
	}
	if !strings.Contains(screen, "got:yes") {
		t.Fatalf("screen %q", screen)
	}
	args := c.AttachArgs("otag-k", ref.PaneID)
	if strings.Join(args[1:3], " ") != "-L "+c.Socket || args[len(args)-1] != "=otag-k" {
		t.Fatalf("attach args %v", args)
	}
}

// tmux gives a new pane its client's PATH even over -e PATH=...; Window.Env's PATH must
// still win, in a new session and in a new window of an existing one.
func TestWindowPath(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	dir := t.TempDir()
	want := dir + "/bin:" + os.Getenv("PATH")
	for _, name := range []string{"first", "second"} {
		out := filepath.Join(dir, name)
		_, err := c.NewWindow(ctx, Window{Session: "otag-path", Name: name, Env: map[string]string{"PATH": want},
			Argv: []string{"/bin/sh", "-c", `printf %s "$PATH" > ` + out}})
		if err != nil {
			t.Fatal(err)
		}
		var got []byte
		for i := 0; i < 50 && len(got) == 0; i++ {
			time.Sleep(50 * time.Millisecond)
			got, _ = os.ReadFile(out)
		}
		if string(got) != want {
			t.Fatalf("%s window PATH %q, want %q", name, got, want)
		}
	}
}

// Output of a pane moves its window_activity forward; a silent pane's stays put.
func TestPaneActivity(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	ref, err := c.NewWindow(ctx, Window{Session: "otag-a", Name: "a-1",
		Argv: []string{"/bin/sh", "-c", "sleep 1.2; echo tick; sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	first := waitState(t, c, ref.PaneID, ref.PID, Alive).Activity
	if first.IsZero() {
		t.Fatal("no activity time")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l := c.Check(ctx, ref.PaneID, ref.PID)
		if l.Activity.After(first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("activity did not move after output: %v -> %v", first, l.Activity)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Every window carries its tags from the start (set in the same command list), on the
// window just created, also when the session already has windows and when a window
// was killed in between; values that look like tmux syntax stay literal.
func TestWindowTags(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	refs := map[string]PaneRef{}
	for _, run := range []string{"r-1", "r-2;", "r-3"} {
		ref, err := c.NewWindow(ctx, Window{Session: "otag-t", Name: run, Run: run, Owner: "/db;x",
			Argv: []string{"/bin/sh", "-c", "sleep 30"}})
		if err != nil {
			t.Fatal(err)
		}
		refs[run] = ref
		switch run {
		case "r-1": // an untagged window between tagged ones
			if _, err := c.NewWindow(ctx, Window{Session: "otag-t", Name: "plain", Argv: []string{"/bin/sh", "-c", "sleep 30"}}); err != nil {
				t.Fatal(err)
			}
		case "r-2;": // a gap in the window indexes
			_ = c.KillWindow(ctx, refs["r-1"].PaneID)
		}
	}
	panes, err := c.Panes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tagged := 0
	for _, p := range panes {
		if p.Run == "" {
			if p.WindowName != "plain" || p.Owner != "" {
				t.Fatalf("untagged pane %+v", p)
			}
			continue
		}
		tagged++
		if ref := refs[p.Run]; ref.PaneID != p.PaneID || p.Owner != "/db;x" {
			t.Fatalf("pane %s tagged %q/%q, created for %+v", p.PaneID, p.Run, p.Owner, ref)
		}
	}
	if tagged != 2 {
		t.Fatalf("%d tagged panes, want 2: %+v", tagged, panes)
	}
}

// Only errors that say the server is absent mean "no panes"; a socket that cannot be
// opened says nothing about them.
func TestNoServerOnlyWhenAbsent(t *testing.T) {
	for msg, want := range map[string]bool{
		"no server running on /tmp/tmux-501/otag":                            true,
		"error connecting to /tmp/tmux-501/otag (No such file or directory)": true,
		"error connecting to /tmp/tmux-501/otag (Connection refused)":        true,
		"server exited unexpectedly":                                         true,
		"error connecting to /tmp/tmux-501/otag (Permission denied)":         false,
		"error connecting to /tmp/tmux-501/otag (Operation timed out)":       false,
	} {
		if got := noServer(&Error{Args: []string{"list-panes"}, Msg: msg}); got != want {
			t.Errorf("%q: %v, want %v", msg, got, want)
		}
	}
	// A server exiting after its last window: an empty listing for Panes only.
	for msg, want := range map[string]bool{"no current target": true, "no sessions": true, "can't find pane %3": false} {
		if got := noSessions(&Error{Args: []string{"list-panes"}, Msg: msg}); got != want {
			t.Errorf("noSessions(%q): %v, want %v", msg, got, want)
		}
	}
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "tmux")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\necho 'no current target' >&2\nexit 1\n"), 0o700)
	if m, err := (&Client{Socket: "otag-test-unused", Bin: bin}).Panes(ctx); err != nil || len(m) != 0 {
		t.Fatalf("exiting server: %v %v", m, err)
	}
}

// A list-panes answer that cannot be parsed, or a tmux that fails, is unknown: never
// "gone", which would make a live pane look dead.
func TestUnparsableListIsUnknown(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "tmux")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\necho '%1 garbage'\n"), 0o700)
	c := &Client{Socket: "otag-test-unused", Bin: bin}
	if _, err := c.Panes(ctx); err == nil {
		t.Fatal("garbage parsed")
	}
	if l := c.Check(ctx, "%1", 0); l.State != Unknown {
		t.Fatalf("check: %+v", l)
	}
	if killed, err := c.KillPane(ctx, "%1", 0); killed || err == nil {
		t.Fatalf("KillPane on unknown: %v %v", killed, err)
	}
	if c.WaitGone(ctx, "%1", 200*time.Millisecond) {
		t.Fatal("WaitGone took unknown for gone")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if c.WaitGone(cctx, "%1", time.Minute) {
		t.Fatal("WaitGone took a cancelled check for gone")
	}
}

// CleanEnv drops what the server's global environment holds; the window's own
// variables and the terminal's survive. SendText keeps text starting with "-" literal.
func TestCleanEnvAndLiteralText(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	out := filepath.Join(t.TempDir(), "env")
	// The first window starts the server; the second sees its global environment.
	if _, err := c.NewWindow(ctx, Window{Session: "otag-e", Name: "a", Argv: []string{"/bin/sh", "-c", "sleep 30"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.run(ctx, "set-environment", "-g", "LEAKED_TOKEN", "secret"); err != nil {
		t.Fatal(err)
	}
	envDir := t.TempDir()
	ref, err := c.NewWindow(ctx, Window{Session: "otag-e", Name: "b", CleanEnv: true, EnvDir: envDir,
		Env:  map[string]string{"KEEP_ME": "1", "SECRET_X": "a'b c", "PATH": os.Getenv("PATH")},
		Argv: []string{"/bin/sh", "-c", "/usr/bin/env > " + out + "; cat"}})
	if err != nil {
		t.Fatal(err)
	}
	var env string
	for i := 0; i < 100 && !strings.Contains(env, "KEEP_ME"); i++ {
		time.Sleep(20 * time.Millisecond)
		b, _ := os.ReadFile(out)
		env = string(b)
	}
	if !strings.Contains(env, "KEEP_ME=1") || !strings.Contains(env, "TERM=") || !strings.Contains(env, "TMUX_PANE=") ||
		strings.Contains(env, "LEAKED_TOKEN") || strings.Contains(env, "HOME=") {
		t.Fatalf("clean env:\n%s", env)
	}
	// The values came through a file the pane deleted, not through tmux arguments.
	if !strings.Contains(env, "SECRET_X=a'b c") {
		t.Fatalf("env file value:\n%s", env)
	}
	if left, _ := os.ReadDir(envDir); len(left) != 0 {
		t.Fatalf("env file left behind: %v", left)
	}
	ctl := c.Control(ref.PaneID)
	if err := ctl.SendText(ctx, "-R literal"); err != nil {
		t.Fatalf("send -R: %v", err)
	}
	var screen string
	for i := 0; i < 50 && !strings.Contains(screen, "-R literal"); i++ {
		time.Sleep(20 * time.Millisecond)
		screen, _ = ctl.Capture(ctx)
	}
	if !strings.Contains(screen, "-R literal") {
		t.Fatalf("screen %q", screen)
	}
}
