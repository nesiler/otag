package registry

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
)

const baseRegistry = `
[accounts.claude-c1]
harness = "claude"
config_dir = "~/.claude"
command = "~/bin/claude-wrap"

[accounts.codex]
harness = "codex"

[profiles.sonnet]
account = "claude-c1"
model = "sonnet"
effort = "high"
tags = ["fast", "review"]
fallback = ["gpt"]

[profiles.gpt]
account = "codex"
model = "gpt-5"
tags = ["smart"]
`

const baseProject = `
path = "%s"
prefix = "tag"
default_profile = "sonnet"
review_profile = "gpt"
deny = [".env"]
team = "core"   # extra field
`

func writeConfig(t *testing.T, dir, registry string, projects map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(dir, "projects"))
	_ = os.MkdirAll(filepath.Join(dir, "projects"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "registry.toml"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range projects {
		if err := os.WriteFile(filepath.Join(dir, "projects", name+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func getenv(home string) func(string) string {
	return func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
}

func project(path string) string { return strings.Replace(baseProject, "%s", path, 1) }

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir)})
	cfg, err := Load(dir, getenv("/home/x"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Accounts["claude-c1"].ConfigDir; got != "/home/x/.claude" {
		t.Errorf("config_dir = %q", got)
	}
	if got := cfg.Accounts["claude-c1"].Command; got != "/home/x/bin/claude-wrap" {
		t.Errorf("command = %q", got)
	}
	p := cfg.Projects[0]
	if p.Name != "myapp" || p.Workspace != "worktree" || p.Raw["team"] != "core" {
		t.Errorf("project = %+v", p)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]struct {
		registry string
		projects map[string]string
		want     string
	}{
		"bad harness": {
			registry: baseRegistry + "\n[accounts.x]\nharness = \"cursor\"\n",
			want:     `harness "cursor"`,
		},
		"unknown account": {
			registry: baseRegistry + "\n[profiles.p]\naccount = \"nope\"\n",
			want:     `unknown account "nope"`,
		},
		"unknown fallback": {
			registry: baseRegistry + "\n[profiles.p]\naccount = \"codex\"\nfallback = [\"ghost\"]\n",
			want:     `unknown fallback profile "ghost"`,
		},
		"bad env_passthrough": {
			registry: baseRegistry + "\n[profiles.p]\naccount = \"codex\"\nenv_passthrough = [\"LITELLM_*\", \"OTAG_DB\"]\n",
			want:     `env_passthrough "OTAG_DB"`,
		},
		"duplicate prefix": {
			projects: map[string]string{"a": project("/tmp"), "b": project("/tmp")},
			want:     `duplicate prefix "tag"`,
		},
		"unknown project profile": {
			projects: map[string]string{"a": project("/tmp") + "\ncoordinator_profile = \"ghost\"\n"},
			want:     `coordinator_profile references unknown profile "ghost"`,
		},
		"bad workspace": {
			projects: map[string]string{"a": project("/tmp") + "\nworkspace = \"copy\"\n"},
			want:     `workspace "copy"`,
		},
		"bad guard": {
			projects: map[string]string{"a": project("/tmp") + "\nguard = \"off\"\n"},
			want:     `guard "off" must be light|strict`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			reg := tc.registry
			if reg == "" {
				reg = baseRegistry
			}
			writeConfig(t, dir, reg, tc.projects)
			_, err := Load(dir, getenv("/h"))
			if apperr.Code(err) != apperr.ConfigInvalid || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// The guard level defaults to light, is part of the project's config hash and lands
// in projects.guard.
func TestSyncGuardLevel(t *testing.T) {
	d := openDB(t)
	dir := t.TempDir()
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir)})
	sync(t, d, dir)
	level := func() (string, string) {
		var g, hash string
		if err := d.QueryRow(`SELECT guard, config_hash FROM projects WHERE name = 'myapp'`).Scan(&g, &hash); err != nil {
			t.Fatal(err)
		}
		return g, hash
	}
	g, h1 := level()
	if g != "light" {
		t.Fatalf("default guard %q", g)
	}
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir) + "\nguard = \"light\"\n"})
	if r := sync(t, d, dir); r.Projects.Unchanged != 1 {
		t.Fatalf("explicit default changed the project: %+v", r.Projects)
	}
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir) + "\nguard = \"strict\"\n"})
	if r := sync(t, d, dir); r.Projects.Updated != 1 {
		t.Fatalf("strict: %+v", r.Projects)
	}
	if g, h2 := level(); g != "strict" || h2 == h1 {
		t.Fatalf("after strict: %s %s", g, h2)
	}
}

func TestUnreadableProjectsDirAborts(t *testing.T) {

	dir := t.TempDir()
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir)})
	pdir := filepath.Join(dir, "projects")
	if err := os.Chmod(pdir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(pdir, 0o755) }()
	if _, err := Load(dir, getenv("/h")); apperr.Code(err) != apperr.ConfigInvalid {
		t.Fatalf("got %v", err)
	}
	// A missing projects dir is fine: no projects.
	_ = os.Chmod(pdir, 0o755)
	_ = os.RemoveAll(pdir)
	cfg, err := Load(dir, getenv("/h"))
	if err != nil || len(cfg.Projects) != 0 {
		t.Fatalf("missing dir: %v %v", cfg, err)
	}
}

func TestSyncPrefixSwap(t *testing.T) {
	d := openDB(t)
	dir := t.TempDir()
	mk := func(prefix string) string { return "path = \"" + dir + "\"\nprefix = \"" + prefix + "\"\n" }
	writeConfig(t, dir, baseRegistry, map[string]string{"a": mk("aa"), "b": mk("bb")})
	sync(t, d, dir)
	writeConfig(t, dir, baseRegistry, map[string]string{"a": mk("bb"), "b": mk("aa")})
	if r := sync(t, d, dir); r.Projects.Updated != 2 {
		t.Fatalf("swap: %+v", r)
	}
	var got string
	_ = d.QueryRow(`SELECT group_concat(name || '=' || prefix, ',' ORDER BY name) FROM projects`).Scan(&got)
	if got != "a=bb,b=aa" {
		t.Fatalf("prefixes %s", got)
	}
}

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func sync(t *testing.T, d *db.DB, dir string) SyncResult {
	t.Helper()
	cfg, err := Load(dir, getenv("/h"))
	if err != nil {
		t.Fatal(err)
	}
	var res SyncResult
	err = d.Tx(context.Background(), func(tx *sql.Tx) error {
		var err error
		res, err = Sync(context.Background(), tx, cfg, "test")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSyncIdempotentAndRetire(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	dir := t.TempDir()
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir)})

	r1 := sync(t, d, dir)
	if r1.Accounts.Added != 2 || r1.Profiles.Added != 2 || r1.Projects.Added != 1 || r1.Fallbacks != 1 {
		t.Fatalf("first sync: %+v", r1)
	}
	snapshot := func() string {
		var s string
		err := d.QueryRow(`SELECT group_concat(x, '|') FROM (
			SELECT name || config_hash || coalesce(retired_at,'') || updated_at AS x FROM accounts
			UNION ALL SELECT name || config_hash || coalesce(retired_at,'') || updated_at FROM profiles
			UNION ALL SELECT name || config_hash || coalesce(retired_at,'') || updated_at FROM projects
			UNION ALL SELECT profile_id || position || fallback_profile_id || updated_at FROM profile_fallbacks
			UNION ALL SELECT count(*) FROM events
			UNION ALL SELECT count(*) FROM profile_revisions)`).Scan(&s)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	r2 := sync(t, d, dir)
	if r2.Accounts.Unchanged != 2 || r2.Profiles.Unchanged != 2 || r2.Projects.Unchanged != 1 || r2.Fallbacks != 0 {
		t.Fatalf("second sync: %+v", r2)
	}
	if after := snapshot(); after != before {
		t.Fatalf("second sync changed state:\n%s\n%s", before, after)
	}

	// Reference the profile/project from history rows.
	var projID, profID, accID, revID int64
	_ = d.QueryRow(`SELECT id FROM projects WHERE name = 'myapp'`).Scan(&projID)
	_ = d.QueryRow(`SELECT id, account_id FROM profiles WHERE name = 'sonnet'`).Scan(&profID, &accID)
	_ = d.QueryRow(`SELECT id FROM profile_revisions WHERE profile_id = ?`, profID).Scan(&revID)
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO tasks (id, project_id, title) VALUES ('tag-abc', ?, 't')`, projID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness)
			VALUES ('r1', ?, 'tag-abc', 'child', ?, ?, ?, 'claude')`, projID, profID, revID, accID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Remove the sonnet profile, the claude account and the project.
	reg := `
[accounts.codex]
harness = "codex"

[profiles.gpt]
account = "codex"
model = "gpt-5"
tags = ["smart"]
`
	writeConfig(t, dir, reg, nil)
	r3 := sync(t, d, dir)
	if r3.Accounts.Retired != 1 || r3.Profiles.Retired != 1 || r3.Projects.Retired != 1 {
		t.Fatalf("retire sync: %+v", r3)
	}
	var n int
	_ = d.QueryRow(`SELECT count(*) FROM runs r JOIN profiles p ON p.id = r.profile_id JOIN tasks t ON t.id = r.task_id
		WHERE p.retired_at IS NOT NULL`).Scan(&n)
	if n != 1 {
		t.Fatal("history rows lost")
	}
	rows, err := d.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign key violations after retire")
	}
	_ = rows.Close()
	if r4 := sync(t, d, dir); r4.Accounts.Retired+r4.Profiles.Retired+r4.Projects.Retired != 0 {
		t.Fatalf("retire not idempotent: %+v", r4)
	}

	// Bring sonnet back: revived, not duplicated.
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(dir)})
	r5 := sync(t, d, dir)
	if r5.Profiles.Updated != 1 || r5.Profiles.Added != 0 || r5.Projects.Updated != 1 {
		t.Fatalf("revive: %+v", r5)
	}
	profs, err := ListProfiles(ctx, d, "review")
	if err != nil || len(profs) != 1 || profs[0].Name != "sonnet" || profs[0].Fallback[0] != "gpt" {
		t.Fatalf("profiles --tag review: %+v %v", profs, err)
	}
}

// A project inside otag's worktree root is refused: a run there could work in a task's
// worktree while delivery removes it.
func TestProjectInsideWorktreeRoot(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	inside := filepath.Join(data, "worktrees", "p", "p-1", "sub")
	_ = os.MkdirAll(inside, 0o755)
	env := func(k string) string {
		if k == "OTAG_DATA_DIR" {
			return data
		}
		return getenv("/home/x")(k)
	}
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(inside)})
	if _, err := Load(dir, env); apperr.Code(err) != apperr.ConfigInvalid || !strings.Contains(err.Error(), "worktree root") {
		t.Fatalf("project inside the worktree root: %v", err)
	}
	writeConfig(t, dir, baseRegistry, map[string]string{"myapp": project(data)})
	if _, err := Load(dir, env); err != nil {
		t.Fatalf("project at the data dir itself: %v", err)
	}
}
