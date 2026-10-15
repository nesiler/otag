package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillStates runs `otag skill <args> --dir home` and returns path -> state; a failing
// command returns its error code.
func skillStates(t *testing.T, h *msgHarness, home string, args ...string) (map[string]string, string) {
	t.Helper()
	code, e := h.json(append(append([]string{"skill"}, args...), "--dir", home)...)
	if code != 0 {
		return nil, e.Error.Code
	}
	var ss []skillState
	_ = json.Unmarshal(e.Data, &ss)
	m := map[string]string{}
	for _, s := range ss {
		m[s.Path] = s.State
	}
	return m, ""
}

func TestSkillMarkerRoundTrip(t *testing.T) {
	h := newMsgHarness(t)
	home := t.TempDir()
	p := skillPathsIn(home)
	if s, _ := skillStates(t, h, home, "install"); s[p.file] != "ours" || s[p.link] != "ours" {
		t.Fatalf("install %v", s)
	}
	mb, _ := os.ReadFile(p.marker)
	if parseSkillMarker(mb) != skillHash(skillText) || !strings.HasPrefix(string(mb), "otag-skill v1 sha256:") {
		t.Fatalf("marker %q", mb)
	}
	// Upgrade: owned, older text -> both files rewritten.
	old := []byte("---\nname: otag\n---\nold text\n")
	_ = os.WriteFile(p.file, old, 0o644)
	_ = os.WriteFile(p.marker, []byte(skillMarkerTag+skillHash(old)+"\n"), 0o644)
	if s, _ := skillStates(t, h, home, "status"); s[p.file] != "outdated" {
		t.Fatalf("older owned text: %v", s)
	}
	if s, _ := skillStates(t, h, home, "install"); s[p.file] != "ours" {
		t.Fatalf("upgrade %v", s)
	}
	if b, _ := os.ReadFile(p.file); string(b) != string(skillText) {
		t.Fatal("SKILL.md not upgraded")
	}
	mb, _ = os.ReadFile(p.marker)
	if parseSkillMarker(mb) != skillHash(skillText) {
		t.Fatal("marker not upgraded")
	}
}

func TestSkillLookalikesAreForeign(t *testing.T) {
	// Every frontmatter trick, without a marker: foreign, never replaced, not even by --force.
	tricks := map[string]string{
		"otag's own text, no marker": string(skillText),
		"block scalar":               "---\nname: otag\nmetadata:\n  description: |\n    managed-by: otag\n---\n",
		"multiline quoted value":     "---\nname: otag\ndescription: \"x\nmetadata:\n  managed-by: otag\"\n---\n",
		"duplicate keys":             "---\nname: otag\nmetadata:\n  managed-by: other\nmetadata:\n  managed-by: otag\n---\n",
		"comment delimiter":          "---\nname: otag\nmetadata:\n  managed-by: otag\n--- # end\n",
		"unclosed":                   "---\nname: otag\nmetadata:\n  managed-by: otag\n",
	}
	for name, text := range tricks {
		h := newMsgHarness(t)
		home := t.TempDir()
		p := skillPathsIn(home)
		_ = os.MkdirAll(p.dir, 0o755)
		_ = os.WriteFile(p.file, []byte(text), 0o644)
		for _, args := range [][]string{{"install"}, {"install", "--force"}, {"uninstall"}, {"uninstall", "--force"}} {
			if _, code := skillStates(t, h, home, args...); code != "CONFLICT" {
				t.Errorf("%s: skill %v = %q, want CONFLICT", name, args, code)
			}
		}
		if s, _ := skillStates(t, h, home, "status"); s[p.file] != "foreign" {
			t.Errorf("%s: status %v", name, s)
		}
		if b, _ := os.ReadFile(p.file); string(b) != text {
			t.Errorf("%s: file changed", name)
		}
		if _, err := os.Lstat(p.link); !os.IsNotExist(err) {
			t.Errorf("%s: symlink written", name)
		}
	}
}

func TestSkillEditedIsModified(t *testing.T) {
	h := newMsgHarness(t)
	home := t.TempDir()
	p := skillPathsIn(home)
	skillStates(t, h, home, "install")
	_ = os.WriteFile(p.file, append([]byte("# mine\n"), skillText...), 0o644)
	if s, _ := skillStates(t, h, home, "status"); s[p.file] != "modified" {
		t.Fatalf("status %v", s)
	}
	for _, cmd := range []string{"install", "uninstall"} {
		if _, code := skillStates(t, h, home, cmd); code != "CONFLICT" {
			t.Errorf("%s of an edited skill = %q", cmd, code)
		}
	}
	if b, _ := os.ReadFile(p.file); !strings.HasPrefix(string(b), "# mine") {
		t.Fatal("edit lost without --force")
	}
	if s, _ := skillStates(t, h, home, "install", "--force"); s[p.file] != "ours" {
		t.Fatalf("forced install %v", s)
	}
	_ = os.WriteFile(p.file, []byte("edited again"), 0o644)
	s, code := skillStates(t, h, home, "uninstall", "--force")
	if code != "" || s[p.file] != "missing" || s[p.link] != "missing" {
		t.Fatalf("forced uninstall %v %q", s, code)
	}
	for _, path := range []string{p.dir, p.link} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s left behind", path)
		}
	}
}

func TestSkillMarkerProblemsAreForeign(t *testing.T) {
	h := newMsgHarness(t)
	// A marker that is a symlink.
	home := t.TempDir()
	p := skillPathsIn(home)
	_ = os.MkdirAll(p.dir, 0o755)
	_ = os.WriteFile(p.file, skillText, 0o644)
	real := filepath.Join(home, "real-marker")
	_ = os.WriteFile(real, []byte(skillMarkerTag+skillHash(skillText)+"\n"), 0o644)
	_ = os.Symlink(real, p.marker)
	if s, _ := skillStates(t, h, home, "status"); s[p.file] != "foreign" {
		t.Errorf("symlinked marker: %v", s)
	}
	if _, code := skillStates(t, h, home, "install", "--force"); code != "CONFLICT" {
		t.Errorf("install over a symlinked marker = %q", code)
	}
	// An unparsable marker.
	home2 := t.TempDir()
	p2 := skillPathsIn(home2)
	_ = os.MkdirAll(p2.dir, 0o755)
	_ = os.WriteFile(p2.file, skillText, 0o644)
	_ = os.WriteFile(p2.marker, []byte("otag-skill v1 sha256:zz\n"), 0o644)
	if s, _ := skillStates(t, h, home2, "status"); s[p2.file] != "foreign" {
		t.Errorf("bad marker: %v", s)
	}
}

func TestSkillUpdateIgnoresPredictableTemp(t *testing.T) {
	h := newMsgHarness(t)
	home := t.TempDir()
	p := skillPathsIn(home)
	skillStates(t, h, home, "install")
	old := []byte("---\nname: otag\n---\nold\n")
	_ = os.WriteFile(p.file, old, 0o644)
	_ = os.WriteFile(p.marker, []byte(skillMarkerTag+skillHash(old)+"\n"), 0o644)
	victim := filepath.Join(home, "victim")
	_ = os.WriteFile(victim, []byte("precious"), 0o644)
	_ = os.Symlink(victim, filepath.Join(p.dir, "SKILL.md.tmp"))
	_ = os.Symlink(victim, filepath.Join(p.dir, ".otag-managed.tmp"))
	foreign := filepath.Join(p.dir, ".SKILL.md.tmp")
	_ = os.WriteFile(foreign, []byte("keep"), 0o644)

	if _, code := skillStates(t, h, home, "install"); code != "" {
		t.Fatalf("update: %s", code)
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("symlinked temp name was followed: %s", b)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "keep" {
		t.Fatalf("foreign temp file touched: %s", b)
	}
	if b, _ := os.ReadFile(p.file); string(b) != string(skillText) {
		t.Fatalf("not updated:\n%s", b)
	}
}

func TestSkillRefusesSymlinkedPaths(t *testing.T) {
	h := newMsgHarness(t)
	// The skill dir itself is a symlink.
	home := t.TempDir()
	elsewhere := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".agents", "skills"), 0o755)
	_ = os.Symlink(elsewhere, filepath.Join(home, ".agents", "skills", "otag"))
	if _, code := skillStates(t, h, home, "install", "--force"); code != "CONFLICT" {
		t.Fatalf("symlinked dir = %q", code)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatalf("wrote through the symlinked dir: %v", ents)
	}
	// SKILL.md is a symlink and there is no marker.
	home2 := t.TempDir()
	p := skillPathsIn(home2)
	_ = os.MkdirAll(p.dir, 0o755)
	target := filepath.Join(home2, "target")
	_ = os.WriteFile(target, skillText, 0o644)
	_ = os.Symlink(target, p.file)
	if _, code := skillStates(t, h, home2, "install", "--force"); code != "CONFLICT" {
		t.Fatalf("symlinked file = %q", code)
	}
}
