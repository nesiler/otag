package claude_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLive runs the real Claude Code CLI through otag: start → one tiny task (with a
// guarded `git push`) → turn_end → a follow-up prompt → `otag done`. It costs two short
// turns of the cheapest model and is skipped unless OTAG_LIVE=claude.
//
//	OTAG_LIVE=claude go test ./internal/adapter/claude -run TestLive -v
//
// OTAG_LIVE_CLAUDE_CONFIG_DIR (default ~/.claude-work) is the account; OTAG_LIVE_CLAUDE_MODEL
// (default claude-haiku-5-5) the model. otag writes nothing to the account dir; Claude
// Code itself records the transcript and the accepted folder trust there as usual.
// Everything else lives in a temp dir with its own database and tmux socket.
func TestLive(t *testing.T) {
	if os.Getenv("OTAG_LIVE") != "claude" {
		t.Skip("set OTAG_LIVE=claude to run the live Claude Code smoke test")
	}
	for _, bin := range []string{"tmux", "git", "claude"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	home, _ := os.UserHomeDir()
	configDir := envOr("OTAG_LIVE_CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude-work"))
	model := envOr("OTAG_LIVE_CLAUDE_MODEL", "claude-haiku-5-5")

	root, err := os.MkdirTemp("", "otag-live-claude-")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() {
		if !t.Failed() && os.Getenv("OTAG_LIVE_KEEP") == "" {
			_ = os.RemoveAll(root)
		} else {
			t.Logf("kept %s", root)
		}
	})
	otagBin := filepath.Join(root, "bin", "otag")
	if out, err := exec.Command("go", "build", "-o", otagBin, "github.com/nesiler/otag/cmd/otag").CombinedOutput(); err != nil {
		t.Fatalf("build otag: %v\n%s", err, out)
	}
	socket := fmt.Sprintf("otag-live-%d", os.Getpid())
	cfg := filepath.Join(root, "cfg")
	env := map[string]string{
		"OTAG_DB": filepath.Join(root, "otag.db"), "OTAG_CONFIG_DIR": cfg, "OTAG_DATA_DIR": filepath.Join(root, "data"),
		"OTAG_TMUX_SOCKET": socket, "OTAG_TMUX_CONF": os.DevNull, "OTAG_BIN": otagBin,
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	otag := func(args ...string) (json.RawMessage, error) {
		t.Helper()
		cmd := exec.Command(otagBin, append([]string{"--json"}, args...)...)
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		runErr := cmd.Run()
		var e struct {
			OK    bool            `json:"ok"`
			Data  json.RawMessage `json:"data"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("otag %v: %v %v\n%s%s", args, runErr, err, out.String(), errb.String())
		}
		if !e.OK {
			return nil, fmt.Errorf("otag %v: %s", args, e.Error)
		}
		return e.Data, nil
	}
	must := func(args ...string) json.RawMessage {
		t.Helper()
		d, err := otag(args...)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// A repository with a bare remote: a push that got through would show up there.
	repo, remote := filepath.Join(root, "repo"), filepath.Join(root, "remote.git")
	_ = os.MkdirAll(repo, 0o755)
	git(root, "init", "-q", "--bare", remote)
	git(repo, "init", "-q", "-b", "main")
	git(repo, "config", "user.name", "otag-live")
	git(repo, "config", "user.email", "otag-live@example.com")
	git(repo, "config", "commit.gpgsign", "false")
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("live test\n"), 0o644)
	git(repo, "add", "README.md")
	git(repo, "commit", "-q", "-m", "init")
	git(repo, "remote", "add", "origin", remote)

	_ = os.MkdirAll(filepath.Join(cfg, "projects"), 0o755)
	_ = os.WriteFile(filepath.Join(cfg, "registry.toml"), []byte(fmt.Sprintf(`
[accounts.live]
harness = "claude"
config_dir = %q
[profiles.live]
account = "live"
model = %q
`, configDir, model)), 0o644)
	_ = os.WriteFile(filepath.Join(cfg, "projects", "live.toml"), []byte(fmt.Sprintf(
		"path = %q\nprefix = \"lv\"\ndefault_profile = \"live\"\n", repo)), 0o644)
	must("sync")

	body := `This is an automated smoke test. Be brief and use as few tool calls as possible.
1. Run exactly this shell command once: git push origin HEAD
   It is expected to be refused; do not retry it or work around it.
2. Create the file hello.txt containing the single line: hi
3. Commit it with the message "add hello".
4. Reply with just the word READY and stop. Do NOT run otag done yet; you will be told when.`
	var task struct{ ID string }
	_ = json.Unmarshal(must("--project", "live", "task", "add", "Add hello.txt", "--body", body), &task)

	start := time.Now()
	var started struct {
		Trust     string `json:"trust"`
		FirstSeen bool   `json:"first_event_seen"`
		Run       struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	_ = json.Unmarshal(must("run", "start", task.ID, "--no-attach"), &started)
	t.Logf("run %s started in %s: trust=%s first_event_seen=%v", started.Run.ID,
		time.Since(start).Round(time.Millisecond), started.Trust, started.FirstSeen)
	if started.Trust != "accepted" || !started.FirstSeen {
		dumpPane(t, socket)
		t.Fatalf("trust dialog not handled: %+v", started)
	}

	type runView struct {
		Status      string `json:"status"`
		EndedReason string `json:"ended_reason"`
		Error       string `json:"error"`
		TaskStatus  string `json:"task_status"`
		Session     struct {
			HarnessSessionID string `json:"harness_session_id"`
			TmuxPane         string `json:"tmux_pane"`
			TranscriptPath   string `json:"transcript_path"`
		} `json:"session"`
		Usage *struct {
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
			CacheReadTokens  int64 `json:"cache_read_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
			Rows             int   `json:"rows"`
		} `json:"usage"`
		Events []struct {
			Kind     string          `json:"kind"`
			ToStatus string          `json:"to_status"`
			Data     json.RawMessage `json:"data"`
		} `json:"events"`
	}
	show := func() runView {
		var r runView
		if d, err := otag("run", "show", started.Run.ID, "--events", "500"); err == nil {
			_ = json.Unmarshal(d, &r)
		}
		return r
	}
	has := func(r runView, kind string) bool {
		for _, e := range r.Events {
			if e.Kind == kind {
				return true
			}
		}
		return false
	}
	wait := func(what string, timeout time.Duration, ok func(runView) bool) runView {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			r := show()
			if ok(r) {
				t.Logf("%s after %s", what, time.Since(start).Round(time.Second))
				return r
			}
			if time.Now().After(deadline) || r.Status == "ended" {
				dumpPane(t, socket)
				b, _ := json.MarshalIndent(r, "", "  ")
				t.Fatalf("waiting for %s:\n%s\nhook errors:\n%s", what, b, readFile(filepath.Join(root, "data", "hook-errors.log")))
			}
			time.Sleep(time.Second)
		}
	}

	// Turn 1: guarded push, a commit, then idle.
	r := wait("first turn_end", 3*time.Minute, func(r runView) bool {
		return r.Status == "idle" && has(r, "hook.turn_end") && has(r, "hook.turn_start")
	})
	for _, k := range []string{"hook.session_start", "hook.turn_start", "hook.activity", "hook.turn_end"} {
		if !has(r, k) {
			t.Errorf("no %s event", k)
		}
	}
	if r.Session.HarnessSessionID == "" || r.Session.TranscriptPath == "" {
		t.Errorf("session not bound: %+v", r.Session)
	}
	if refs := git(remote, "for-each-ref"); refs != "" {
		t.Errorf("git push was not blocked; remote has %s", refs)
	}

	// Turn 2: tell it to finish (typed into the pane like a person would).
	pane := r.Session.TmuxPane
	if out, err := exec.Command("tmux", "-L", socket, "send-keys", "-t", pane, "-l",
		`Now run: otag done "added hello"`).CombinedOutput(); err != nil {
		t.Fatalf("send-keys: %v %s", err, out)
	}
	time.Sleep(300 * time.Millisecond)
	_ = exec.Command("tmux", "-L", socket, "send-keys", "-t", pane, "Enter").Run()
	r = wait("otag done", 3*time.Minute, func(r runView) bool { return r.Status == "ended" && r.EndedReason == "done" })
	if r.TaskStatus != "done" {
		t.Errorf("task status %q", r.TaskStatus)
	}
	branch := "feature/" + task.ID
	if log := git(repo, "log", "--format=%s", "main.."+branch); !strings.Contains(log, "hello") {
		t.Errorf("no commit on %s: %q", branch, log)
	}
	if r.Usage == nil || r.Usage.Rows == 0 || r.Usage.OutputTokens == 0 {
		t.Errorf("no usage recorded: %+v", r.Usage)
	} else {
		t.Logf("usage: %+v", *r.Usage)
	}
	if errs := readFile(filepath.Join(root, "data", "hook-errors.log")); errs != "" {
		t.Errorf("hook errors:\n%s", errs)
	}
	for _, e := range r.Events {
		if strings.HasPrefix(e.Kind, "hook.") && e.Kind != "hook.usage" {
			t.Logf("%-20s %s", e.Kind, e.Data)
		}
	}
	// The guard's deny reaches the model through the hook output: the transcript has
	// the reason as the tool result.
	if tr := readFile(r.Session.TranscriptPath); !strings.Contains(tr, "otag guard") {
		t.Errorf("no guard deny in the transcript %s", r.Session.TranscriptPath)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func dumpPane(t *testing.T, socket string) {
	t.Helper()
	out, _ := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", "otag-live:").CombinedOutput()
	t.Logf("pane:\n%s", out)
}
