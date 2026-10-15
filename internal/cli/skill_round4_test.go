package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSkillNonRegularFileIsForeignDespiteMarker(t *testing.T) {
	h := newMsgHarness(t)
	for _, kind := range []string{"symlink", "dir"} {
		home := t.TempDir()
		p := skillPathsIn(home)
		_ = os.MkdirAll(p.dir, 0o755)
		_ = os.WriteFile(p.marker, []byte(skillMarkerTag+skillHash(skillText)+"\n"), 0o644)
		keep := filepath.Join(home, "keep")
		if kind == "symlink" {
			_ = os.WriteFile(keep, []byte("mine"), 0o644)
			_ = os.Symlink(keep, p.file)
		} else {
			_ = os.MkdirAll(filepath.Join(p.file, "sub"), 0o755)
		}
		if s, _ := skillStates(t, h, home, "status"); s[p.file] != "foreign" {
			t.Errorf("%s: status %v", kind, s)
		}
		for _, args := range [][]string{{"install", "--force"}, {"uninstall", "--force"}} {
			if _, code := skillStates(t, h, home, args...); code != "CONFLICT" {
				t.Errorf("%s: %v = %q", kind, args, code)
			}
		}
		if _, err := os.Lstat(p.file); err != nil {
			t.Errorf("%s: object removed: %v", kind, err)
		}
	}
}

// A marker written but SKILL.md not (the install failed in between) is modified, and
// --force finishes the job.
func TestSkillInterruptedInstallIsRecoverable(t *testing.T) {
	h := newMsgHarness(t)
	home := t.TempDir()
	p := skillPathsIn(home)
	_ = os.MkdirAll(p.dir, 0o755)
	_ = os.WriteFile(p.marker, []byte(skillMarkerTag+skillHash(skillText)+"\n"), 0o644)
	if s, _ := skillStates(t, h, home, "status"); s[p.file] != "modified" {
		t.Fatalf("marker without file: %v", s)
	}
	if _, code := skillStates(t, h, home, "install"); code != "CONFLICT" {
		t.Fatalf("plain install = %q", code)
	}
	if s, code := skillStates(t, h, home, "install", "--force"); code != "" || s[p.file] != "ours" {
		t.Fatalf("forced install %v %q", s, code)
	}
	if b, _ := os.ReadFile(p.file); string(b) != string(skillText) {
		t.Fatal("SKILL.md not written")
	}
	// Same for an uninstall of the half state.
	_ = os.Remove(p.file)
	if s, code := skillStates(t, h, home, "uninstall", "--force"); code != "" || s[p.file] != "missing" {
		t.Fatalf("forced uninstall %v %q", s, code)
	}
}
