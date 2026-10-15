package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Under the run env a real git push to a local bare remote fails, a fetch still works,
// and GIT_CONFIG_* slots the env already had keep applying.
func TestPushBlock(t *testing.T) {
	if gitVersion(context.Background()) < gitConfigEnvVersion {
		t.Skip("git older than 2.31")
	}
	root := t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	run := func(env []string, dir string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(env []string, dir string, args ...string) string {
		t.Helper()
		out, err := run(env, dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return out
	}
	base := []string{"GIT_CONFIG_NOSYSTEM=1", "HOME=" + root, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e"}
	must(base, root, "init", "-q", "--bare", remote)
	must(base, root, "init", "-q", "-b", "main", repo)
	must(base, repo, "commit", "-q", "--allow-empty", "-m", "x")
	must(base, repo, "remote", "add", "origin", remote)

	env := map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "from-env"}
	addPushBlock(context.Background(), env)
	if env["GIT_CONFIG_COUNT"] != "3" || env["GIT_CONFIG_KEY_0"] != "user.name" || env["GIT_CONFIG_KEY_1"] != pushBlock[0][0] {
		t.Fatalf("slots: %v", env)
	}
	blocked := append([]string(nil), base...)
	for k, v := range env {
		blocked = append(blocked, k+"="+v)
	}
	if out, err := run(blocked, repo, "push", "origin", "main"); err == nil {
		t.Fatalf("push went through: %s", out)
	}
	if out, _ := run(base, remote, "for-each-ref"); out != "" {
		t.Fatalf("remote got refs: %s", out)
	}
	must(blocked, repo, "fetch", "-q", "origin")
	if got := must(blocked, repo, "config", "user.name"); got != "from-env" {
		t.Fatalf("existing slot lost: %q", got)
	}
	// Without the block the same push works (the setup is sound).
	must(base, repo, "push", "-q", "origin", "main")

	bogus := map[string]string{"GIT_CONFIG_COUNT": "x"}
	addPushBlock(context.Background(), bogus)
	if len(bogus) != 1 {
		t.Fatalf("bogus count extended: %v", bogus)
	}
	saved := gitVersion
	gitVersion = func(context.Context) int { return 2030 }
	defer func() { gitVersion = saved }()
	old := map[string]string{}
	addPushBlock(context.Background(), old)
	if len(old) != 0 {
		t.Fatalf("old git got the block: %v", old)
	}
}
