package daemon

import (
	"testing"

	"github.com/nesiler/otag/internal/run"
)

// A stray tool end (its start was never reported) does not hide a later tool call.
func TestToolCountIgnoresStrayEnds(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.slow]
account = "f"
args = ["start", "busy", "wait"]
`)
	r := e.start(e.addTask("slow", "slow"))
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	e.hook(r.ID, "activity", `{"tool":"AskUserQuestion","phase":"end"}`)
	e.hook(r.ID, "activity", `{"tool":"Bash","phase":"start"}`)
	if in, err := e.d.toolRunning(e.ctx, r.ID); err != nil || !in {
		t.Fatalf("tool running %v %v", in, err)
	}
	e.hook(r.ID, "activity", `{"tool":"Bash","phase":"end"}`)
	if in, _ := e.d.toolRunning(e.ctx, r.ID); in {
		t.Fatal("tool still running after its end")
	}
}
