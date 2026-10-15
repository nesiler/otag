package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/nesiler/otag/internal/adapter"
)

func probeSpec(t *testing.T) adapter.RunSpec {
	t.Helper()
	root, err := os.MkdirTemp("/private/tmp", "otag-cx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	probe := filepath.Join(root, "codex")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexec "+shellQuote(os.Args[0])+" -test.run=TestCodexProbeHelper -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return adapter.RunSpec{Harness: "codex", Workdir: root, RunDir: filepath.Join(root, "runs", "r1"), CacheDir: filepath.Join(root, "cache"), OtagBin: "/otag", Account: adapter.Account{Command: probe, Env: map[string]string{"OTAG_CODEX_PROBE_TEST": "1"}}, Env: map[string]string{"OTAG_DB": filepath.Join(root, "db", "otag.db"), "OTAG_DATA_DIR": filepath.Join(root, "data")}}
}
func TestReviewCwd(t *testing.T) {
	root := t.TempDir()
	hc := adapter.HookContext{Workdir: root, Deny: []string{".env*", "infra/prod/**"}}
	for _, tt := range []struct {
		cwd, command string
		allow        bool
	}{
		{root + "/", "touch ok", true}, {"sub", "touch ok", true}, {"sub", "touch .env.local", false},
		{"infra/prod", "touch config", false}, {"sub", "touch ../../outside", false}, {"/tmp", "git status", true}, {"/tmp", "touch bad", false},
	} {
		raw, _ := json.Marshal(map[string]string{"cmd": tt.command, "workdir": tt.cwd})
		d := checkTool(hc, "exec_command", raw)
		if d.Allow != tt.allow {
			t.Errorf("%+v: %+v", tt, d)
		}
		if !tt.allow && strings.Contains(d.Reason, "cwd override") {
			t.Fatal("denied for wrong reason", d)
		}
	}
	raw := []byte(`{"tool_name":"Bash","cwd":"infra/prod","tool_input":{"command":"touch config"}}`)
	r, err := New().ParseEvent(hc, "PreToolUse", raw)
	if err != nil || len(r.Output) == 0 {
		t.Fatal(r, err)
	}
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]string{"cmd": "touch ok", "workdir": link})
	if d := checkTool(hc, "exec_command", raw); !d.Allow {
		t.Fatal(d)
	}
	if d := checkTool(adapter.HookContext{}, "Write", []byte(`{"path":"/elsewhere/file"}`)); !d.Allow {
		t.Fatal(d)
	}
}
func TestReviewEnvironment(t *testing.T) {
	for _, key := range scrubEnv {
		t.Setenv(key, "coordinator")
	}
	env := map[string]string{"CODEX_API_KEY": "explicit"}
	actual := exec.Command(scrubArgv(env, []string{"/usr/bin/env"})[0], scrubArgv(env, []string{"/usr/bin/env"})[1:]...)
	actual.Env = append(processEnv(env), "CODEX_SANDBOX_FUTURE=coordinator")
	b, err := actual.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "coordinator") || !strings.Contains(string(b), "CODEX_API_KEY=explicit") {
		t.Fatal("scrub failed")
	}
	// Inspect the launcher argv too: tmux may retain variables absent from otag.
	args := scrubArgv(env, []string{"codex"})
	for _, key := range scrubEnv {
		i := slices.Index(args, key)
		if key == "CODEX_API_KEY" {
			if i >= 0 {
				t.Fatal("explicit variable scrubbed")
			}
		} else if i < 1 || args[i-1] != "-u" {
			t.Fatal("missing env -u", key)
		}
	}
}
func TestReviewNativeRootsAndStableTrust(t *testing.T) {
	spec := probeSpec(t)
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = spec.Workdir
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatal(err, string(b))
		}
	}
	run("init", "-q")
	run("-c", "user.name=smoke", "-c", "user.email=smoke@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "base")
	worktree := filepath.Join(t.TempDir(), "worktree")
	run("worktree", "add", "-qb", "test", worktree)
	spec.Workdir = worktree
	spec.Deny = []string{"/.env*", "/infra/prod/**"}
	spec.Profile.Env = map[string]string{"OTAG_CODEX_MODE": "hooks"}
	ls, err := New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for i, a := range ls.Argv {
		if a == "-c" && !strings.ContainsAny(strings.SplitN(ls.Argv[i+1], "=", 2)[0], " \n") {
			text.WriteString(ls.Argv[i+1] + "\n")
		}
	}
	var cfg struct {
		Permissions map[string]struct{ Filesystem map[string]any }
	}
	if _, err := toml.Decode(text.String(), &cfg); err != nil {
		t.Fatal(err)
	}
	fs := cfg.Permissions["otag"].Filesystem
	common := filepath.Join(filepath.Dir(spec.Account.Command), ".git")
	own := filepath.Join(common, "worktrees", "worktree")
	db := spec.Env["OTAG_DB"]
	for path, want := range map[string]any{
		filepath.Join(common, "objects"): "write", filepath.Join(common, "refs"): "write", filepath.Join(common, "logs"): "write",
		filepath.Join(common, "packed-refs"): "write", own: "write", db: "write", db + "-wal": "write", db + "-shm": "write",
		// The coordinator's unsandboxed git must not run agent-written config or hooks.
		common: nil, filepath.Join(common, "config"): nil, filepath.Join(common, "hooks"): nil,
		filepath.Join(own, "commondir"): "read", filepath.Join(own, "gitdir"): "read", filepath.Join(own, "config.worktree"): "read",
		// Other runs, worktrees and the TUI home live in the data directory.
		spec.Env["OTAG_DATA_DIR"]: nil, filepath.Dir(db): nil,
	} {
		if fs[path] != want {
			t.Errorf("rule for %s = %v, want %v", path, fs[path], want)
		}
	}
	// On its own branch the worktree may move only that ref (and write its reflog).
	ownRef := map[string]string{}
	meta, err := metadataRules(context.Background(), worktree, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range meta {
		ownRef[r[0]] = r[1]
	}
	for path, want := range map[string]string{
		filepath.Join(common, "objects"): "write", filepath.Join(common, "refs", "heads", "test"): "write",
		filepath.Join(common, "refs", "heads", "test.lock"): "write", filepath.Join(common, "logs", "refs", "heads"): "write",
		filepath.Join(common, "refs"): "", filepath.Join(common, "packed-refs"): "", own: "write",
	} {
		if ownRef[path] != want {
			t.Errorf("own-branch rule for %s = %q, want %q", path, ownRef[path], want)
		}
	}
	// A reftable repository cannot grant one branch: a worktree run there is refused.
	_ = os.MkdirAll(filepath.Join(common, "reftable"), 0o755)
	if _, err := metadataRules(context.Background(), worktree, "test", ""); err == nil || !strings.Contains(err.Error(), "reftable") {
		t.Errorf("reftable worktree: %v", err)
	}
	_ = os.RemoveAll(filepath.Join(common, "reftable"))
	// The project checkout itself (workspace mode "same") keeps config and hooks read-only.
	same, _ := metadataRules(context.Background(), filepath.Dir(spec.Account.Command), "", "")
	got := map[string]string{}
	for _, r := range same {
		got[r[0]] = r[1]
	}
	for _, name := range []string{"config", "hooks", "worktrees", "modules", "info"} {
		if got[filepath.Join(common, name)] != "read" || got[common] != "write" {
			t.Errorf("same-mode rule for %s: %v", name, got)
		}
	}
	roots := fs[":workspace_roots"].(map[string]any)
	if roots["**/.env*"] != "deny" || roots["infra/prod/**"] != "read" {
		t.Fatal(roots)
	}
	before, _ := filepath.Glob(filepath.Join(spec.CacheDir, "hooks-*.json"))
	spec.RunDir = filepath.Join(filepath.Dir(spec.RunDir), "r2")
	if _, err := New().Prepare(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	after, _ := filepath.Glob(filepath.Join(spec.CacheDir, "hooks-*.json"))
	if len(before) != 1 || len(after) != 1 {
		t.Fatal("per-run trust cache miss", before, after)
	}
	for _, arg := range []string{"--disable", "hooks", "--enable=hooks", "-a", "--ask-for-approval=never", "--full-auto", "-p", "--profile=x", "-c", "sandbox_mode=\"danger-full-access\"", "--yolo", "-lc"} {
		spec.Profile.Args = []string{arg}
		if _, err := New().Prepare(context.Background(), spec); err == nil {
			t.Errorf("accepted %q", arg)
		}
	}
	spec.Profile.Args = nil
	spec.Profile.Env = map[string]string{"OTAG_CODEX_MODE": "server"}
	spec.RunDir = "/" + strings.Repeat("x", 110)
	if _, err := New().Prepare(context.Background(), spec); err == nil {
		t.Fatal("overlong socket accepted")
	}
}
func TestSystemErrorAndUsage(t *testing.T) {
	r, err := New().ParseEvent(adapter.HookContext{}, "thread/status/changed", []byte(`{"threadId":"s","status":{"type":"systemError"}}`))
	if err != nil || len(r.Events) != 2 || r.Events[1].Type != adapter.TurnEnd || r.Events[1].Reason != "error" {
		t.Fatal(r, err)
	}
	r, err = New().ParseEvent(adapter.HookContext{}, "thread/tokenUsage/updated", []byte(`{"threadId":"s","turnId":"t","model":"actual","tokenUsage":{"last":{"inputTokens":10,"cachedInputTokens":4,"outputTokens":7,"reasoningOutputTokens":3,"totalTokens":17}}}`))
	if err != nil {
		t.Fatal(err)
	}
	u := r.Events[0].Usage
	if u.InputTokens != 6 || u.CacheReadTokens != 4 || u.OutputTokens != 7 || u.ReasoningTokens != 3 || u.Model != "actual" {
		t.Fatal(u)
	}
}

// The generated profile under the real Codex sandbox (no model call): a commit and a
// DB write succeed; git config/hooks/pointers and the rest of the data dir stay read-only.
func TestNativeProfileSandbox(t *testing.T) {
	if os.Getenv("OTAG_CODEX_SANDBOX") == "" {
		t.Skip("set OTAG_CODEX_SANDBOX=1 to run `codex sandbox` with the generated profile")
	}
	spec := probeSpec(t)
	repo := filepath.Dir(spec.Account.Command)
	git := func(dir string, args ...string) {
		t.Helper()
		if b, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(err, string(b))
		}
	}
	git(repo, "init", "-q")
	git(repo, "-c", "user.name=a", "-c", "user.email=a@b", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "base")
	wt := filepath.Join(repo, "data", "worktrees", "p", "t")
	git(repo, "worktree", "add", "-qb", "t", wt)
	git(repo, "worktree", "add", "-qb", "other", filepath.Join(repo, "data", "worktrees", "p", "other"))
	db := filepath.Join(repo, "data", "otag.db")
	spec.Workdir, spec.Env["OTAG_DATA_DIR"], spec.Env["OTAG_DB"] = wt, filepath.Join(repo, "data"), db
	spec.Profile.Env = map[string]string{"OTAG_CODEX_MODE": "hooks"}
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ls, err := New().Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	profile := ""
	for i, a := range ls.Argv {
		if a == "-c" && strings.HasPrefix(ls.Argv[i+1], "permissions.otag=") {
			profile = ls.Argv[i+1]
		}
	}
	common := filepath.Join(repo, ".git")
	script := `set -u
echo x > f && git add f && git -c user.name=a -c user.email=a@b -c commit.gpgsign=false commit -qm w && echo OK-commit
echo x >> "$1" && echo x >> "$1-wal" && echo OK-db
shift
for p in "$@"; do (echo x >> "$p") 2>/dev/null && echo "WRITABLE $p"; done; true`
	args := []string{"sandbox", "-C", wt, "-P", "otag", "-c", profile, "-c", `default_permissions="otag"`, "--", "/bin/sh", "-c", script, "sh", db,
		filepath.Join(common, "config"), filepath.Join(common, "hooks", "pre-commit"), filepath.Join(common, "worktrees", "t", "commondir"),
		filepath.Join(common, "worktrees", "other", "HEAD"), filepath.Join(repo, "data", "worktrees", "p", "other", "f"),
		filepath.Join(repo, "data", "runs", "evil"), filepath.Join(repo, "data", "evil"), filepath.Join(wt, ".git")}
	cmd := exec.Command("codex", args...)
	cmd.Dir, cmd.Env = wt, append(os.Environ(), "CODEX_HOME="+t.TempDir())
	out, _ := cmd.CombinedOutput()
	t.Logf("%s", out)
	if !strings.Contains(string(out), "OK-commit") || !strings.Contains(string(out), "OK-db") || strings.Contains(string(out), "WRITABLE") {
		t.Fatalf("%s", out)
	}
}
