package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
)

// The MR flow keys: absent, branch_prefix is feature/ and push coordinator (and
// base/remote empty); "" is a valid prefix (the task id alone); an explicit default
// does not change the project. Bad values are CONFIG_INVALID with the key named.
func TestProjectMRFlowKeys(t *testing.T) {
	d := openDB(t)
	dir := t.TempDir()
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir)})
	sync(t, d, dir)
	read := func() ProjectRow {
		t.Helper()
		ps, err := ListProjects(context.Background(), d)
		if err != nil || len(ps) != 1 {
			t.Fatalf("projects %v %v", ps, err)
		}
		return ps[0]
	}
	if p := read(); p.BranchPrefix != "feature/" || p.Push != "coordinator" || p.Base != "" || p.Remote != "" {
		t.Fatalf("defaults %+v", p)
	}
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir) + "\nbranch_prefix = \"feature/\"\npush = \"coordinator\"\n"})
	if r := sync(t, d, dir); r.Projects.Unchanged != 1 {
		t.Fatalf("explicit defaults changed the project: %+v", r.Projects)
	}
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir) +
		"\nbranch_prefix = \"\"\nbase = \"origin/dev\"\nremote = \"upstream\"\npush = \"child\"\n"})
	if r := sync(t, d, dir); r.Projects.Updated != 1 {
		t.Fatalf("settings: %+v", r.Projects)
	}
	if p := read(); p.BranchPrefix != "" || p.Push != "child" || p.Base != "origin/dev" || p.Remote != "upstream" {
		t.Fatalf("settings %+v", p)
	}

	for key, bad := range map[string]string{
		"branch_prefix": "branch_prefix = \"feature//\"",
		"push":          "push = \"everyone\"",
		"base":          "base = \"--upload-pack=x\"",
		"remote":        "remote = \"origin/main\"",
	} {
		writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir) + "\n" + bad + "\n"})
		_, err := Load(dir, getenv("/h"))
		if apperr.Code(err) != apperr.ConfigInvalid || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	for _, bad := range []string{"branch_prefix = \"-x/\"", "branch_prefix = \"a..b/\"", "branch_prefix = \"x.lock/\"",
		"branch_prefix = \"a b/\"", "base = \"a..b\""} {
		writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir) + "\n" + bad + "\n"})
		if _, err := Load(dir, getenv("/h")); apperr.Code(err) != apperr.ConfigInvalid {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
}
