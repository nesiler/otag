package cli

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/run"
)

// A project without a guard key runs light: the run records it, status/show/doctor
// say so, and the hook applies the light rules (push refused, a sed -i outside the
// workspace allowed); a strict project's run keeps strict.
func TestLightGuardRun(t *testing.T) {
	h := newRunHarness(t)
	repo := filepath.Join(h.root, "gamma")
	initRepo(t, repo)
	h.write("projects/gamma.toml", "path = \""+repo+"\"\nprefix = \"ga\"\ndefault_profile = \"manual\"\n")
	h.ok("sync")
	id := h.addTask("gamma", "Light")
	res := h.start(id)
	r := h.waitRun(res.Run.ID, "idle", func(r run.Run) bool { return r.Status == run.Idle })
	if r.Guard != "light" {
		t.Fatalf("run guard %q", r.Guard)
	}
	if out := h.ok("run", "show", r.ID); !regexp.MustCompile(`guard:\s+light`).MatchString(out) {
		t.Fatalf("run show: %s", out)
	}
	if out := h.ok("status", "--all"); !strings.Contains(out, "GUARD") || !regexp.MustCompile(`gamma\s+light`).MatchString(out) ||
		!regexp.MustCompile(`alpha\s+strict`).MatchString(out) {
		t.Fatalf("status: %s", out)
	}
	if _, out, _ := h.run("doctor"); !strings.Contains(out, "gamma=light") || !strings.Contains(out, "alpha=strict") {
		t.Fatalf("doctor: %s", out)
	}
	env := map[string]string{"OTAG_RUN_ID": r.ID, run.TokenEnv: h.token(r.ID)}
	pre := func(cmd string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"command": cmd})
		_, out, _ := h.hook(env, string(b), "fake", "pre_tool")
		return out
	}
	outside := filepath.Join(h.root, "elsewhere.txt")
	for _, cmd := range []string{"git push origin HEAD", "git push --force", "git -C " + h.repo + " reset --hard", "cat .env"} {
		if out := pre(cmd); !strings.Contains(out, `"decision":"deny"`) {
			t.Errorf("light allowed %q: %s", cmd, out)
		}
	}
	for _, cmd := range []string{"sed -i '' s/a/b/ " + outside, "go test ./...", "echo x > " + outside} {
		if out := pre(cmd); out != `{"decision":"allow"}` {
			t.Errorf("light refused %q: %s", cmd, out)
		}
	}
	h.ok("run", "stop", r.ID)

	// The level is the run's: a strict project's run stays strict in its hooks.
	s := h.addTask("beta", "Strict")
	rs := h.start(s).Run
	h.waitRun(rs.ID, "idle", func(r run.Run) bool { return r.Status == run.Idle })
	env = map[string]string{"OTAG_RUN_ID": rs.ID, run.TokenEnv: h.token(rs.ID)}
	if out := pre("sed -i '' s/a/b/ " + outside); !strings.Contains(out, `"decision":"deny"`) || h.show(rs.ID).Guard != "strict" {
		t.Errorf("strict run allowed a sed -i outside: %s", out)
	}
	h.ok("run", "stop", rs.ID)
}
