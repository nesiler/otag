package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checksOf indexes doctor's checks by name (the first non-ok one wins) and fails a
// warn or fail without a fix.
func checksOf(t *testing.T, data json.RawMessage) map[string]check {
	t.Helper()
	var cs []check
	if err := json.Unmarshal(data, &cs); err != nil {
		t.Fatal(err)
	}
	out := map[string]check{}
	for _, c := range cs {
		if c.Status != "ok" && c.Fix == "" {
			t.Errorf("%s %s has no fix: %s", c.Status, c.Name, c.Detail)
		}
		if _, dup := out[c.Name]; !dup || c.Status != "ok" {
			out[c.Name] = c
		}
	}
	return out
}

func TestDoctorFixes(t *testing.T) {
	f := newFresh(t, "claude")
	code, e := f.json("doctor")
	if code != ExitError {
		t.Fatalf("doctor on a fresh machine: exit %d", code)
	}
	cs := checksOf(t, e.Data)
	if c := cs["registry"]; c.Status != "fail" || c.Fix != "otag init" || !strings.Contains(c.Detail, "not set up yet") {
		t.Fatalf("registry %+v", c)
	}
	if c := cs["harness codex"]; c.Status != "warn" || !strings.Contains(c.Fix, "brew install --cask codex") {
		t.Fatalf("codex %+v", c)
	}
	if c := cs["daemon"]; c.Status != "warn" || c.Fix != "otag daemon install" {
		t.Fatalf("daemon %+v", c)
	}
	_, out, _ := f.run("doctor")
	if !strings.Contains(out, "fix: otag init") {
		t.Fatalf("human doctor:\n%s", out)
	}

	f.cwd = f.root
	f.ok("init", "--yes", "--no-daemon")
	code, e = f.json("doctor")
	cs = checksOf(t, e.Data)
	if code != 0 || cs["registry"].Status != "ok" || cs["tmux"].Status != "ok" || cs["git"].Status != "ok" {
		t.Fatalf("after init: %d %s", code, e.Data)
	}
	if c := cs["projects"]; c.Status != "warn" || !strings.HasPrefix(c.Fix, "otag project add") {
		t.Fatalf("projects %+v", c)
	}
	// An edit not synced yet.
	if err := os.WriteFile(filepath.Join(f.cfg, "registry.toml"), []byte(f.read("registry.toml")+
		"\n[profiles.extra]\naccount = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, e = f.json("doctor")
	if c := checksOf(t, e.Data)["sync"]; c.Status != "warn" || c.Fix != "otag sync" {
		t.Fatalf("sync %+v", c)
	}
	// A harness an account uses goes missing.
	_ = os.Remove(filepath.Join(f.stubs, "claude"))
	code, e = f.json("doctor")
	if c := checksOf(t, e.Data)["harness claude"]; code == 0 || c.Status != "fail" || !strings.Contains(c.Fix, "claude.ai/install.sh") {
		t.Fatalf("claude %d %+v", code, c)
	}
}
