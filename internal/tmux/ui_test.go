package tmux

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPaneView(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	// 60 lines: the first 10 scroll into the history of a 50-row pane.
	ref, err := c.NewWindow(ctx, Window{Session: "otag-v", Name: "v", Argv: []string{"/bin/sh", "-c",
		`i=1; while [ $i -le 60 ]; do printf '\033[31mline %d\033[0m\n' $i; i=$((i+1)); done; printf 'end;'; sleep 30`}})
	if err != nil {
		t.Fatal(err)
	}
	var v PaneView
	for i := 0; i < 100; i++ {
		if v, err = c.PaneView(ctx, ref.PaneID, 0); err == nil && strings.Contains(strings.Join(v.Lines, "\n"), "end;") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if v.Width != 200 || v.Height != 50 || len(v.Lines) != 50 || v.Dead || v.AltScreen || v.InMode {
		t.Fatalf("view %+v (%d lines)", v, len(v.Lines))
	}
	// The screen keeps its rows (the blank ones below the text come back) and the SGR.
	if !strings.Contains(v.Lines[0], "\x1b[31mline 12") || !strings.HasPrefix(v.Lines[49], "end;") {
		t.Fatalf("screen %q ... %q", v.Lines[0], v.Lines[49])
	}
	if v.CursorY != 49 || v.CursorX != 4 {
		t.Fatalf("cursor %d,%d", v.CursorX, v.CursorY)
	}

	h, err := c.PaneView(ctx, ref.PaneID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Lines) != 5 || !strings.Contains(h.Lines[4], "line 11") || !strings.Contains(h.Lines[0], "line 7") {
		t.Fatalf("history %q", h.Lines)
	}

	if _, err := c.PaneView(ctx, "%999", 0); err == nil {
		t.Fatal("no error for a missing pane")
	}
}

func TestEnsureUI(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	if err := c.EnsureUI(ctx); err != nil {
		t.Fatalf("no server: %v", err)
	}
	if _, err := c.NewWindow(ctx, Window{Session: "otag-u", Name: "u", Argv: []string{"/bin/sh", "-c", "sleep 30"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureUI(ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := c.run(ctx, "list-keys", "-T", "root")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"M-q", "C-q", "MouseDown1Control0"} {
		if !strings.Contains(keys, k+" ") || !strings.Contains(keys, "detach-client") {
			t.Errorf("no root binding %s:\n%s", k, keys)
		}
	}
	for opt, want := range map[string]string{"mouse": "on", "status-left": "#[range=control|0,reverse] ← otag #[norange default] ",
		"status-right": "#{?@otag_run,run #{@otag_run} · ,}M-q / C-q: back to otag "} {
		if got, _ := c.run(ctx, "show-options", "-gv", opt); got != want {
			t.Errorf("%s = %q, want %q", opt, got, want)
		}
	}
	if got, _ := c.run(ctx, "show-options", "-sv", "extended-keys"); got != "on" {
		t.Errorf("extended-keys %q", got)
	}

	// @otag_ui off: EnsureUI changes nothing.
	for _, args := range [][]string{{"set", "-g", OptUI, "off"}, {"set", "-g", "mouse", "off"}, {"unbind", "-n", "C-q"}} {
		if _, err := c.run(ctx, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EnsureUI(ctx); err != nil {
		t.Fatal(err)
	}
	keys, _ = c.run(ctx, "list-keys", "-T", "root")
	if got, _ := c.run(ctx, "show-options", "-gv", "mouse"); got != "off" || strings.Contains(keys, "C-q ") {
		t.Errorf("@otag_ui off ignored: mouse %s\n%s", got, keys)
	}
}

func TestGuardedSend(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	out := t.TempDir() + "/out"
	// cat in raw mode: every byte reaches the file as written (CR stays CR).
	ref, err := c.NewWindow(ctx, Window{Session: "otag-k", Name: "k", Run: "al-1-r1",
		Argv: []string{"/bin/sh", "-c", "stty raw -echo; cat > " + out}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // stty ran
	g := Guard{Pane: ref.PaneID, Run: "al-1-r1", PID: ref.PID}
	// Text that would be a separator, an option, a format, a quote or a command in a tmux
	// command string goes through as typed.
	tricky := []string{`-R x;`, `a ; kill-server`, `#{pane_id} #(echo hi) ##`, `it's "quoted" \ $HOME`, `{ }`, `é中`, `;`}
	want := ""
	for _, s := range tricky {
		if err := c.SendText(ctx, g, "otag-test", s, false); err != nil {
			t.Fatalf("text %q: %v", s, err)
		}
		want += s
	}
	for _, k := range []string{"Enter", "M-;", "M-'", `M-\`, "M-#", "M-$", `M-"`} {
		if err := c.SendKey(ctx, g, k); err != nil {
			t.Fatalf("key %q: %v", k, err)
		}
	}
	want += "\r\x1b;\x1b'\x1b\\\x1b#\x1b$\x1b\""
	// A paste: LF becomes CR (cat asked for no bracketed paste: no codes).
	if err := c.SendText(ctx, g, "otag-test", "p1\np2", true); err != nil {
		t.Fatal(err)
	}
	want += "p1\rp2"
	var got []byte
	for i := 0; i < 100; i++ {
		if got, _ = os.ReadFile(out); string(got) == want {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if string(got) != want {
		t.Fatalf("pane got %q\nwant      %q", got, want)
	}
	if bufs, _ := c.run(ctx, "list-buffers"); strings.Contains(bufs, "otag-test") {
		t.Errorf("buffer kept: %s", bufs)
	}
	if !c.HasSession(ctx, "otag-k") {
		t.Fatal("the server ran typed text as a command")
	}

	// The guard: another run, another pid, a missing pane get nothing, and the
	// buffer is gone.
	for _, bad := range []struct {
		g    Guard
		want string
	}{
		{Guard{Pane: ref.PaneID, Run: "al-2-r1", PID: ref.PID}, "now belongs to run al-1-r1"},
		{Guard{Pane: ref.PaneID, Run: "al-1-r1", PID: ref.PID + 1}, "now runs pid"},
		{Guard{Pane: ref.PaneID, Run: "al-2-r1"}, "now belongs to run al-1-r1"},
		{Guard{Pane: "%999", Run: "al-1-r1", PID: 1}, "%999"},
		{Guard{Pane: ref.PaneID}, "nothing to check"},
		{Guard{Pane: "%1;x", Run: "al-1-r1"}, "bad pane"},
	} {
		if err := c.SendText(ctx, bad.g, "otag-test", "LEAK", false); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("text %+v: %v", bad.g, err)
		}
		if err := c.SendKey(ctx, bad.g, "Enter"); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("key %+v: %v", bad.g, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got, _ = os.ReadFile(out); string(got) != want {
		t.Fatalf("a failed guard sent %q", strings.TrimPrefix(string(got), want))
	}
	if bufs, _ := c.run(ctx, "list-buffers"); strings.Contains(bufs, "otag-test") {
		t.Errorf("buffer kept after a failed guard: %s", bufs)
	}
	// An untagged window passes on its pid alone.
	u, err := c.NewWindow(ctx, Window{Session: "otag-k", Name: "u", Argv: []string{"/bin/sh", "-c", "sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendKey(ctx, Guard{Pane: u.PaneID, Run: "al-1-r1", PID: u.PID}, "Enter"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendKey(ctx, Guard{Pane: u.PaneID, Run: "al-1-r1"}, "Enter"); err == nil {
		t.Fatal("an untagged pane passed without a pid")
	}
}

func TestQuoteCommandArg(t *testing.T) {
	for in, want := range map[string]string{"Enter": "'Enter'", "M-;": "'M-;'", "M-#": "'M-#'",
		"M-'": `"M-'"`, `M-'\"$`: `"M-'\\\"\$"`} {
		if got := QuoteCommandArg(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

func TestPopupArgs(t *testing.T) {
	c := &Client{Socket: "otag", Bin: "/opt/tmux"}
	got := c.PopupArgs("tmux", "%3", "otag-alpha", "%7")
	want := []string{"tmux", "display-popup", "-E", "-B", "-w", "100%", "-h", "100%", "-t", "%3",
		"env -u TMUX -u TMUX_PANE /opt/tmux -u -S " + SocketPath("otag") + " select-window -t '%7' \\; attach-session -t '=otag-alpha'"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q\nwant %q", got, want)
	}
	for v, ok := range map[string]bool{"tmux 3.8": true, "tmux 3.3a": true, "tmux 3.2a": false, "tmux 2.9": false,
		"tmux next-3.6": true, "tmux master": true, "tmux 4.0": true, "garbage": false, "": false} {
		if PopupSupported(v) != ok {
			t.Errorf("PopupSupported(%q) = %v", v, !ok)
		}
	}
}

func TestParseView(t *testing.T) {
	v, err := parseView("80\t24\t3\t5\t1\t1\t0\t1")
	if err != nil || !reflect.DeepEqual(v, PaneView{Width: 80, Height: 24, CursorX: 3, CursorY: 5, CursorOn: true, AltScreen: true, Dead: true}) {
		t.Fatalf("%+v %v", v, err)
	}
	for _, bad := range []string{"", "80\t24", "x\t24\t3\t5\t1\t1\t0\t1"} {
		if _, err := parseView(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
