package attach

import (
	"slices"
	"testing"

	"github.com/nesiler/otag/internal/tmux"
)

func TestDecide(t *testing.T) {
	tc := &tmux.Client{Socket: "otag", Bin: "/bin/tmux"}
	tg := Target{Run: "al-1-r1", Session: "otag-alpha", Pane: "%7"}
	env := []string{"HOME=/h", "TMUX=/tmp/tmux-1/default,1,0", "TMUX_PANE=%1", "TERM=xterm"}

	// Outside tmux: exec, without TMUX.
	p := Decide(tc, tg, Env{Outer: "/bin/tmux", Version: "tmux 3.8", Environ: []string{"HOME=/h", "TERM=xterm"}})
	if p.Mode != Exec || !slices.Equal(p.Argv, tc.AttachArgs("otag-alpha", "%7")) {
		t.Fatalf("outside tmux: %+v", p)
	}

	// Inside a recent tmux: the popup, $TMUX kept for the popup command.
	in := Env{TMUX: "/tmp/tmux-1/default,1,0", TMUXPane: "%1", Outer: "/usr/bin/tmux", Version: "tmux 3.4", Environ: env}
	p = Decide(tc, tg, in)
	if p.Mode != Popup || p.Argv[0] != "/usr/bin/tmux" || p.Argv[1] != "display-popup" || !slices.Equal(p.Env, env) ||
		!slices.Equal(p.Argv, tc.PopupArgs("/usr/bin/tmux", "%1", "otag-alpha", "%7")) {
		t.Fatalf("inside tmux: %+v", p)
	}
	if c, _ := p.Command(); c.Path == "" || !slices.Equal(c.Args, p.Argv) || !slices.Equal(c.Env, env) {
		t.Fatalf("command %+v", c)
	}

	// Inside an old tmux (or an unknown version): a nested client, TMUX unset.
	for _, v := range []string{"tmux 3.2a", ""} {
		in.Version = v
		p = Decide(tc, tg, in)
		if p.Mode != Exec || !slices.Equal(p.Env, []string{"HOME=/h", "TERM=xterm"}) {
			t.Fatalf("old tmux %q: %+v", v, p)
		}
	}
}
