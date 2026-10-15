package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/run"
)

func TestPrimeCLI(t *testing.T) {
	h := newMsgHarness(t)
	prime := func(code int, e env) run.PrimeInfo {
		t.Helper()
		var p run.PrimeInfo
		if code != 0 || json.Unmarshal(e.Data, &p) != nil {
			t.Fatalf("prime: exit %d %+v %s", code, e.Error, e.Data)
		}
		return p
	}

	// A message for the child is listed, not consumed.
	if code, e := h.as(h.coordEnv, "send", h.child, "use sqlite"); code != 0 {
		t.Fatalf("send: %+v", e.Error)
	}
	p := prime(h.as(h.childEnv, "prime"))
	if p.Kind != run.PrimeChild || p.Task.ID != h.task || len(p.Messages) != 1 || len(p.Commands) < 8 {
		t.Fatalf("child prime %+v", p)
	}
	code, e := h.as(h.childEnv, "inbox")
	if code != 0 || !strings.Contains(string(e.Data), "use sqlite") {
		t.Fatalf("inbox after prime: %d %s", code, e.Data)
	}
	if p = prime(h.as(h.childEnv, "prime")); len(p.Messages) != 0 {
		t.Fatalf("read message still listed: %+v", p.Messages)
	}

	// Text form stays short.
	h.env["OTAG_RUN_ID"], h.env[run.TokenEnv] = h.child, "child-token"
	_, out, _ := h.run("prime")
	delete(h.env, "OTAG_RUN_ID")
	delete(h.env, run.TokenEnv)
	if !strings.Contains(out, "otag done") || strings.Count(out, "\n") > 60 {
		t.Fatalf("child text (%d lines):\n%s", strings.Count(out, "\n"), out)
	}

	// A forged token is refused.
	code, e = h.as(map[string]string{"OTAG_RUN_ID": h.child, run.TokenEnv: "nope"}, "prime")
	wantCode(t, "forged token", code, e, "FORBIDDEN")

	// Coordinator, and the human inside and outside a project.
	p = prime(h.as(h.coordEnv, "prime"))
	if p.Kind != run.PrimeCoordinator || p.Counts["running"] != 1 || len(p.Live) == 0 {
		t.Fatalf("coordinator prime %+v", p)
	}
	p = prime(h.json("prime"))
	if p.Kind != run.PrimeProject || p.Project != "alpha" {
		t.Fatalf("human prime %+v", p)
	}
	h.cwd = t.TempDir()
	p = prime(h.json("prime"))
	if p.Kind != run.PrimeNone || len(p.Projects) != 2 {
		t.Fatalf("no-project prime %+v", p)
	}
}

func TestSkillCLI(t *testing.T) {
	h := newMsgHarness(t)
	home := t.TempDir()
	file := filepath.Join(home, ".agents", "skills", "otag", "SKILL.md")
	link := filepath.Join(home, ".claude", "skills", "otag")
	states := func(args ...string) map[string]string {
		t.Helper()
		code, e := h.json(append([]string{"skill"}, append(args, "--dir", home)...)...)
		if code != 0 {
			t.Fatalf("skill %v: %+v", args, e.Error)
		}
		var ss []skillState
		_ = json.Unmarshal(e.Data, &ss)
		m := map[string]string{}
		for _, s := range ss {
			m[s.Path] = s.State
		}
		return m
	}
	if s := states("status"); s[file] != "missing" || s[link] != "missing" {
		t.Fatalf("fresh status %v", s)
	}
	for i := 0; i < 2; i++ { // idempotent
		if s := states("install"); s[file] != "ours" || s[link] != "ours" {
			t.Fatalf("install %v", s)
		}
	}
	b, _ := os.ReadFile(file)
	if string(b) != string(skillText) || !strings.HasPrefix(string(b), "---\nname: otag\n") {
		t.Fatalf("skill file:\n%s", b)
	}
	if to, _ := os.Readlink(link); to != filepath.Dir(file) {
		t.Fatalf("link -> %q", to)
	}
	if lines := strings.Count(string(b), "\n"); lines > 40 {
		t.Fatalf("skill has %d lines", lines)
	}
	// A child may not install it.
	code, e := h.as(h.childEnv, "skill", "install", "--dir", home)
	wantCode(t, "child skill", code, e, "FORBIDDEN")

	if s := states("uninstall"); s[file] != "missing" || s[link] != "missing" {
		t.Fatalf("uninstall %v", s)
	}
	if _, err := os.Stat(filepath.Dir(file)); !os.IsNotExist(err) {
		t.Fatalf("skill dir left behind: %v", err)
	}
}

func TestSkillRefusesForeign(t *testing.T) {
	h := newMsgHarness(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".agents", "skills", "otag")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("mine"), 0o644)
	code, e := h.json("skill", "install", "--dir", home)
	wantCode(t, "foreign skill", code, e, "CONFLICT")
	if b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); string(b) != "mine" {
		t.Fatalf("foreign file overwritten: %s", b)
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatal("symlink written despite the refusal")
	}
	h.json("skill", "uninstall", "--dir", home)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal("uninstall removed a foreign file")
	}

	// A foreign link path is kept too.
	home2 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home2, ".claude", "skills", "otag"), 0o755)
	code, e = h.json("skill", "install", "--dir", home2)
	wantCode(t, "foreign link", code, e, "CONFLICT")
	if _, err := os.Stat(filepath.Join(home2, ".agents")); !os.IsNotExist(err) {
		t.Fatal("skill written despite the refusal")
	}
}
