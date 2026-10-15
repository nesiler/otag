package opencode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/opencode"
	"github.com/nesiler/otag/internal/cli"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tmux"
)

// TestLive runs the real OpenCode TUI against a otag-owned server in a temp git repo on
// a test tmux socket: start -> one tiny task -> turn_end -> `otag done`. Opt in with
// OTAG_LIVE=opencode. It uses the account dir (~/.opencode-work, read only) and
// OTAG_LIVE_MODEL (default litellm/deepseek-flash-deepseek; the free fallback is
// opencode/mimo-v2.6-flash-free).
func TestLive(t *testing.T) {
	if os.Getenv("OTAG_LIVE") != "opencode" {
		t.Skip("set OTAG_LIVE=opencode to run the live OpenCode smoke test")
	}
	for _, bin := range []string{"tmux", "opencode", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	home, _ := os.UserHomeDir()
	account := filepath.Join(home, ".opencode-work")
	if _, err := os.Stat(account); err != nil {
		t.Skipf("no OpenCode account dir %s", account)
	}
	model := os.Getenv("OTAG_LIVE_MODEL")
	if model == "" {
		model = "litellm/deepseek-flash-deepseek"
	}
	command := ""
	if oc := filepath.Join(account, "bin", "oc"); fileExists(oc) {
		command = "oc" // not on PATH (a shell alias): the adapter finds it in <account>/bin
	}

	root := t.TempDir()
	otagBin := filepath.Join(root, "bin", "otag")
	if out, err := exec.Command("go", "build", "-o", otagBin, "github.com/nesiler/otag/cmd/otag").CombinedOutput(); err != nil {
		t.Fatalf("build otag: %v\n%s", err, out)
	}
	repo := filepath.Join(root, "repo")
	git(t, "", "init", "-q", "-b", "main", repo)
	git(t, repo, "config", "user.name", "tester")
	git(t, repo, "config", "user.email", "tester@example.com")
	git(t, repo, "config", "commit.gpgsign", "false")
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644)
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-q", "-m", "init")

	cfg := filepath.Join(root, "cfg")
	_ = os.MkdirAll(filepath.Join(cfg, "projects"), 0o755)
	write(t, filepath.Join(cfg, "registry.toml"), fmt.Sprintf(`
[accounts.oc]
harness = "opencode"
config_dir = %q
command = %q
[profiles.live]
account = "oc"
model = %q
`, account, command, model))
	write(t, filepath.Join(cfg, "projects", "live.toml"), fmt.Sprintf("path = %q\nprefix = \"lv\"\ndefault_profile = \"live\"\ndeny = [\".env*\"]\n", repo))

	socket := fmt.Sprintf("otag-live-oc-%d", os.Getpid())
	tm := tmux.New(socket, os.DevNull)
	env := map[string]string{
		"HOME": home, "USER": os.Getenv("USER"), "PATH": os.Getenv("PATH"),
		"OTAG_DB": filepath.Join(root, "otag.db"), "OTAG_CONFIG_DIR": cfg, "OTAG_DATA_DIR": filepath.Join(root, "data"),
		"OTAG_TMUX_SOCKET": socket, "OTAG_TMUX_CONF": os.DevNull, "OTAG_BIN": otagBin,
	}
	otag := func(args ...string) (int, json.RawMessage, string) {
		var out, errb bytes.Buffer
		code := cli.Main(context.Background(), append([]string{"--json"}, args...), cli.Env{
			Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }, Cwd: repo})
		var e struct {
			Data  json.RawMessage `json:"data"`
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(out.Bytes(), &e)
		if code != 0 {
			return code, e.Error, errb.String()
		}
		return code, e.Data, errb.String()
	}
	show := func(ref string) run.Run {
		var r run.Run
		if code, data, _ := otag("run", "show", ref, "--events", "200"); code == 0 {
			_ = json.Unmarshal(data, &r)
		}
		return r
	}

	if code, data, errs := otag("sync"); code != 0 {
		t.Fatalf("sync: %s %s", data, errs)
	}
	_, data, _ := otag("--project", "live", "task", "add", "Two-step smoke test",
		"--body", "This task comes in two steps. Step 1 (now): run the shell command `touch ../otag-outside.txt` "+
			"exactly once (it may be refused; do not retry or work around it), do not run otag done yet, then reply "+
			"with one line: READY followed by the otag run id from your system instructions, and end your turn. "+
			"Step 2 will arrive as a new message.")
	var tk struct{ ID string }
	_ = json.Unmarshal(data, &tk)
	if tk.ID == "" {
		t.Fatalf("task add: %s", data)
	}

	var runID string
	t.Cleanup(func() {
		if t.Failed() && runID != "" {
			r := show(runID)
			b, _ := json.MarshalIndent(r, "", "  ")
			t.Logf("run:\n%s", b)
			if r.Session != nil {
				for _, p := range append([]run.AuxPane{{Name: "tui", Pane: r.Session.TmuxPane}}, r.Session.AuxPanes...) {
					if out, err := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", p.Pane).Output(); err == nil {
						t.Logf("pane %s:\n%s", p.Name, out)
					}
				}
			}
		}
		if runID != "" {
			otag("run", "stop", runID)
		}
		_ = tm.KillServer(context.Background())
	})

	start := time.Now()
	code, data, errs := otag("run", "start", tk.ID, "--no-attach")
	if code != 0 {
		t.Fatalf("run start: %s %s", data, errs)
	}
	var sr run.StartResult
	_ = json.Unmarshal(data, &sr)
	runID = sr.Run.ID
	t.Logf("started %s in %s (session %s, attach: %s)", runID, time.Since(start).Round(time.Millisecond),
		sr.Run.Session.HarnessSessionID, sr.Attach)
	if sr.Run.Session == nil || !strings.HasPrefix(sr.Run.Session.HarnessSessionID, "ses_") {
		t.Fatalf("no native session id bound at start: %+v", sr.Run.Session)
	}

	wait := func(what string, ok func(run.Run) bool) run.Run {
		deadline := time.Now().Add(4 * time.Minute)
		var r run.Run
		for time.Now().Before(deadline) {
			if r = show(runID); ok(r) || r.Status == "ended" {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Logf("%s: status %s/%s after %s, error %q", what, r.Status, r.EndedReason,
			time.Since(start).Round(time.Second), r.Error)
		return r
	}
	turnEnd := func(r run.Run) *run.EventRow {
		for i := len(r.Events) - 1; i >= 0; i-- {
			if r.Events[i].Kind == "hook.turn_end" {
				return &r.Events[i]
			}
		}
		return nil
	}

	// Step 1: one turn without tools; turn_end carries the answer (the system note
	// injected by the plugin names the run).
	r := wait("step 1", func(r run.Run) bool { return r.Status == "idle" && turnEnd(r) != nil })
	te := turnEnd(r)
	if r.Status != "idle" || te == nil {
		t.Fatalf("no turn_end after step 1")
	}
	t.Logf("turn_end: %s", te.Data)
	if !strings.Contains(string(te.Data), runID) {
		t.Errorf("turn_end text does not name the run (system note missing?): %s", te.Data)
	}

	guarded := false
	for _, e := range r.Events {
		if e.Kind == "hook.activity" && strings.Contains(string(e.Data), `"guard":"deny"`) {
			guarded = true
			t.Logf("guard: %s", e.Data)
		}
	}
	if !guarded {
		t.Errorf("no guard denial recorded for the write outside the worktree")
	}
	if fileExists(filepath.Join(root, "data", "worktrees", "live", "otag-outside.txt")) {
		t.Errorf("the guard did not stop the write outside the worktree")
	}

	// Step 2: delivered through the server API (Deliverer), ends with `otag done`.
	ad := opencode.New()
	ad.Getenv = func(k string) string { return env[k] }
	err := ad.Deliver(context.Background(), adapter.Target{RunID: runID, HarnessSessionID: r.Session.HarnessSessionID,
		ServerURL: r.Session.ServerURL}, adapter.Message{Text: "Step 2: create a file hello.txt containing exactly " +
		"the word hi, commit it with message 'add hello', then run: otag done \"added hello.txt\""})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	r = wait("step 2", func(r run.Run) bool { return r.Status == "ended" })
	if r.Status != "ended" || r.EndedReason != "done" {
		t.Fatalf("run did not finish with otag done")
	}
	kinds := map[string]int{}
	for _, e := range r.Events {
		kinds[e.Kind]++
	}
	t.Logf("event kinds: %v", kinds)
	for _, k := range []string{"hook.session_start", "hook.turn_start", "hook.turn_end", "hook.activity", "hook.usage"} {
		if kinds[k] == 0 {
			t.Errorf("no %s event", k)
		}
	}
	if r.Usage == nil || r.Usage.Rows == 0 {
		t.Errorf("no usage rows")
	} else {
		t.Logf("usage: %+v", *r.Usage)
	}
	if got := git(t, repo, "show", "otag/"+tk.ID+":hello.txt"); strings.TrimSpace(got) != "hi" {
		t.Errorf("hello.txt on the task branch = %q", got)
	}

	// Resume: a new server for the same account finds the session by id
	// (ServerSetup), keeps it and points the TUI at it. No model call.
	sid := r.Session.HarnessSessionID
	spec := adapter.RunSpec{RunID: runID + "-resume", TaskID: tk.ID, Project: "live", Harness: "opencode",
		Account: adapter.Account{ConfigDir: account, Command: command}, Profile: adapter.Profile{Model: model},
		Workdir: r.Workspace.Path, RunDir: t.TempDir(), OtagBin: otagBin, ResumeSessionID: sid,
		Env: map[string]string{"OTAG_RUN_ID": runID + "-resume", "OTAG_BIN": otagBin, "OTAG_DB": env["OTAG_DB"]}}
	ls, err := ad.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ls.Files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(spec.RunDir, f.Path)), 0o700)
		mode := f.Mode
		if mode == 0 {
			mode = 0o600
		}
		_ = os.WriteFile(filepath.Join(spec.RunDir, f.Path), f.Data, mode)
	}
	srv := exec.Command(ls.Server.Argv[0], ls.Server.Argv[1:]...)
	srv.Dir = ls.Server.Cwd
	srv.Env = os.Environ()
	for _, m := range []map[string]string{spec.Env, ls.Env, ls.Server.Env} {
		for k, v := range m {
			srv.Env = append(srv.Env, k+"="+v)
		}
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Process.Kill(); _ = srv.Wait() }()
	if err := ad.SetupServer(context.Background(), spec, ls); err != nil {
		t.Fatalf("resume setup: %v", err)
	}
	if ls.HarnessSessionID != sid || !strings.Contains(strings.Join(ls.Argv, " "), "-s "+sid) {
		t.Errorf("resume: session %s, argv %q", ls.HarnessSessionID, ls.Argv)
	}
	t.Logf("resume: server %s reopened %s", ls.Server.URL, sid)

	// Stop: a second run that answers once, then `otag run stop` (Stopper: interrupt +
	// quit the TUI, whose wrapper reports otag.tui.exit).
	_, data, _ = otag("--project", "live", "task", "add", "Say ready",
		"--body", "Do not use any tool. Reply with the single word READY and end your turn.")
	tk = struct{ ID string }{}
	_ = json.Unmarshal(data, &tk)
	code, data, errs = otag("run", "start", tk.ID, "--no-attach")
	if code != 0 {
		t.Fatalf("run start: %s %s", data, errs)
	}
	_ = json.Unmarshal(data, &sr)
	runID = sr.Run.ID
	r = wait("stop run", func(r run.Run) bool { return r.Status == "idle" && turnEnd(r) != nil })
	if r.Status != "idle" {
		t.Fatalf("second run not idle")
	}
	stopAt := time.Now()
	if code, data, errs := otag("run", "stop", runID); code != 0 {
		t.Fatalf("run stop: %s %s", data, errs)
	}
	t.Logf("stop took %s", time.Since(stopAt).Round(time.Millisecond))
	r = show(runID)
	if r.Status != "ended" || r.EndedReason != "stopped" {
		t.Errorf("after stop: %s/%s", r.Status, r.EndedReason)
	}
	d, err := db.Open(context.Background(), env["OTAG_DB"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	var exits int
	_ = d.QueryRow(`SELECT count(*) FROM raw_hook_payloads WHERE run_id = ? AND hook = 'otag.tui.exit'`, runID).Scan(&exits)
	t.Logf("otag.tui.exit payloads after stop: %d", exits)

	// The user quits the TUI: tui.sh reports the exit, the run ends as exited, the task
	// is requeued and the server and sidecar are gone with it.
	_, data, _ = otag("--project", "live", "task", "add", "Say ready again",
		"--body", "Do not use any tool. Reply with the single word READY and end your turn.")
	tk = struct{ ID string }{}
	_ = json.Unmarshal(data, &tk)
	if code, data, errs = otag("run", "start", tk.ID, "--no-attach"); code != 0 {
		t.Fatalf("run start: %s %s", data, errs)
	}
	_ = json.Unmarshal(data, &sr)
	runID = sr.Run.ID
	r = wait("quit run", func(r run.Run) bool { return r.Status == "idle" && turnEnd(r) != nil })
	_ = exec.Command("tmux", "-L", socket, "send-keys", "-t", r.Session.TmuxPane, "C-c").Run()
	r = wait("after quit", func(r run.Run) bool { return r.Status == "ended" })
	if r.EndedReason != "exited" {
		t.Errorf("after the TUI quit: %s/%s", r.Status, r.EndedReason)
	}
	var taskStatus string
	_ = d.QueryRow(`SELECT status FROM tasks WHERE id = ?`, tk.ID).Scan(&taskStatus)
	if taskStatus != "ready" {
		t.Errorf("task after exit: %s", taskStatus)
	}
	for _, p := range r.Session.AuxPanes {
		if l := tm.Check(context.Background(), p.Pane, p.PID); l.State != tmux.Gone {
			t.Errorf("%s still %s after the TUI quit", p.Name, l.State)
		}
	}
	if l := tm.Check(context.Background(), r.Session.TmuxPane, r.Session.PID); l.State != tmux.Gone {
		t.Errorf("pane still %s after the TUI quit", l.State)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
