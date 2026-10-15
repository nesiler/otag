package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFreshUserE2E is a new user's first minutes, with the real binary: a temporary
// HOME and otag directories, stub agent CLIs that print a version, a git repository
// as the working directory. `otag` without a terminal points to `otag init`; `otag
// init --yes --no-daemon` sets everything up; `otag doctor` then finds no failure and
// the project is registered. Nothing outside the temporary directory is touched: the
// daemon is not installed (and launchctl points nowhere), the tmux socket is private.
// CI runs it (.github/workflows/ci.yml); there it must not be skipped.
func TestFreshUserE2E(t *testing.T) {
	need := func(tool string) string {
		p, err := exec.LookPath(tool)
		if err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is required in CI: %v", tool, err)
			}
			t.Skipf("%s not installed", tool)
		}
		return p
	}
	tmuxBin, gitBin, goBin := need("tmux"), need("git"), need("go")

	root := t.TempDir()
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	stubs := filepath.Join(root, "stubs")
	repo := filepath.Join(home, "dev", "shop")
	for _, d := range []string{home, bin, stubs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	otag := filepath.Join(bin, "otag")
	if out, err := exec.Command(goBin, "build", "-o", otag, "github.com/nesiler/otag/cmd/otag").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Only the stubs, otag, tmux and git: no real agent CLI of this machine.
	for _, tool := range []string{tmuxBin, gitBin} {
		if err := os.Symlink(tool, filepath.Join(bin, filepath.Base(tool))); err != nil {
			t.Fatal(err)
		}
	}
	for name, version := range map[string]string{"claude": "2.1.0 (Claude Code)", "codex": "codex-cli 0.50.0", "opencode": "1.0.0"} {
		script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '" + version + "'; exit 0; fi\nexit 1\n"
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{
		"HOME=" + home,
		"USER=tester",
		"PATH=" + stubs + ":" + bin + ":/usr/bin:/bin",
		"OTAG_CONFIG_DIR=" + filepath.Join(root, "config"),
		"OTAG_DATA_DIR=" + filepath.Join(root, "data"),
		"OTAG_DB=" + filepath.Join(root, "data", "otag.db"),
		"OTAG_TMUX_SOCKET=otag-e2e-fresh-" + filepath.Base(root),
		"OTAG_LAUNCHCTL=" + filepath.Join(root, "no-launchctl"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"),
	}
	run := func(dir string, args ...string) (int, string, string) {
		t.Helper()
		cmd := exec.Command(otag, args...)
		cmd.Dir, cmd.Env = dir, env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, out.String(), errb.String()
		case errors.As(err, &exit):
			return exit.ExitCode(), out.String(), errb.String()
		}
		t.Fatalf("otag %v: %v", args, err)
		return 0, "", ""
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command(gitBin, append([]string{"-C", repo}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Bare otag without a terminal: the usage and how to start.
	code, out, errs := run(repo)
	if code != ExitUsage || !strings.Contains(errs, "run `otag init` to set up") {
		t.Fatalf("bare otag: %d %q %q", code, out, errs)
	}
	// doctor on the fresh machine: a clear failure with its fix.
	code, out, _ = run(repo, "--json", "doctor")
	if code == 0 || !strings.Contains(out, `"fix":"otag init"`) {
		t.Fatalf("doctor before init: %d %s", code, out)
	}

	code, out, errs = run(repo, "init", "--yes", "--no-daemon")
	if code != 0 {
		t.Fatalf("init: %d\n%s%s", code, out, errs)
	}
	if !strings.Contains(out, "otag coord shop --attach") {
		t.Fatalf("init output:\n%s", out)
	}

	code, out, errs = run(repo, "--json", "doctor")
	var doc struct {
		OK   bool    `json:"ok"`
		Data []check `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("doctor: %v\n%s%s", err, out, errs)
	}
	for _, c := range doc.Data {
		if c.Status == "fail" {
			t.Errorf("doctor FAIL %s: %s (fix: %s)", c.Name, c.Detail, c.Fix)
		}
		if c.Status != "ok" && c.Fix == "" {
			t.Errorf("doctor %s %s without a fix", c.Status, c.Name)
		}
	}
	if code != 0 || !doc.OK {
		t.Fatalf("doctor exit %d:\n%s", code, out)
	}

	code, out, _ = run(home, "--json", "project", "list")
	var projects struct {
		Data []projectInfo `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &projects); err != nil || code != 0 {
		t.Fatalf("project list: %d %v\n%s", code, err, out)
	}
	if len(projects.Data) != 1 || projects.Data[0].Name != "shop" || projects.Data[0].CoordinatorProfile != "claude-opus" {
		t.Fatalf("projects %+v", projects.Data)
	}
	// The project is in the database too: the repository resolves to it.
	code, out, _ = run(filepath.Join(repo), "--json", "context")
	if code != 0 || !strings.Contains(out, `"name":"shop"`) {
		t.Fatalf("context in the repository: %d %s", code, out)
	}
	// Nothing was written outside the temporary directory's otag paths and HOME.
	for _, p := range []string{filepath.Join(home, "Library", "LaunchAgents"), filepath.Join(home, ".agents"), filepath.Join(home, ".claude")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was created", p)
		}
	}
}
