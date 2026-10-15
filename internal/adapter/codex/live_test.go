package codex_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/codex"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tmux"
)

// Opt-in only: two tiny turns, isolated account home, git repo and tmux
// server. Authentication is copied; the user's Codex config is never written.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("OTAG_LIVE") != "codex" {
		t.Skip("set OTAG_LIVE=codex to run the real CLI")
	}
	for _, bin := range []string{"codex", "tmux", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.MkdirTemp("/private/tmp", "otag-codex-live-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repo, cfg, home := filepath.Join(root, "repo"), filepath.Join(root, "config"), filepath.Join(root, "codex-home")
	for _, dir := range []string{repo, filepath.Join(cfg, "projects"), home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	account := os.Getenv("CODEX_HOME")
	if account == "" {
		userHome, _ := os.UserHomeDir()
		account = filepath.Join(userHome, ".codex")
	}
	copiedAuth := false
	if auth, err := os.ReadFile(filepath.Join(account, "auth.json")); err == nil {
		copiedAuth = true
		if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0o600); err != nil {
			t.Fatal(err)
		}
	} else if os.Getenv("OPENAI_API_KEY") == "" {
		t.Fatal("no Codex auth.json or OPENAI_API_KEY")
	}
	// Deliberately hostile account defaults: a TUI-typed turn must still use otag.
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("approval_policy = \"never\"\nsandbox_mode = \"danger-full-access\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	otag := filepath.Join(root, "otag")
	build := exec.CommandContext(ctx, "go", "build", "-o", otag, "github.com/nesiler/otag/cmd/otag")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	socket := fmt.Sprintf("otag-codex-live-%d", time.Now().UnixNano())
	defer func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() }()
	env := append(os.Environ(), "OTAG_DB="+filepath.Join(root, "otag.db"), "OTAG_CONFIG_DIR="+cfg, "OTAG_DATA_DIR="+filepath.Join(root, "data"), "OTAG_TMUX_SOCKET="+socket, "OTAG_TMUX_CONF=/dev/null", "OTAG_BIN="+otag, "CODEX_HOME="+home)
	command := func(bin string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = repo
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			logs, _ := filepath.Glob(filepath.Join(root, "data", "runs", "*", "*.log"))
			for _, log := range logs {
				b, _ := os.ReadFile(log)
				t.Logf("%s diagnostics: %s", filepath.Base(log), b)
			}
			t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
		}
		return out
	}
	command("git", "init", "-q", "-b", "main")
	command("git", "config", "user.name", "otag smoke")
	command("git", "config", "user.email", "smoke@example.com")
	command("git", "config", "commit.gpgsign", "false")
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("smoke\n"), 0o600)
	command("git", "add", "README.md")
	command("git", "commit", "-qm", "initial")
	registry := fmt.Sprintf("[accounts.c]\nharness = \"codex\"\nconfig_dir = %q\n", home)
	if !copiedAuth {
		registry += fmt.Sprintf("env = { OPENAI_API_KEY = %q }\n", os.Getenv("OPENAI_API_KEY"))
	}
	registry += "[profiles.smoke]\naccount = \"c\"\nmodel = \"gpt-6.1-sol\"\neffort = \"low\"\n"
	registry += "args = [\"--no-alt-screen\"]\n"
	if os.Getenv("OTAG_CODEX_LIVE_MODE") == "hooks" {
		registry += "env = { OTAG_CODEX_MODE = \"hooks\" }\n"
	}
	if err := os.WriteFile(filepath.Join(cfg, "registry.toml"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	project := fmt.Sprintf("path = %q\nprefix = \"sm\"\nworkspace = \"worktree\"\ndefault_profile = \"smoke\"\ndeny = [\".env*\"]\n", repo)
	_ = os.WriteFile(filepath.Join(cfg, "projects", "smoke.toml"), []byte(project), 0o600)
	decode := func(out []byte, result any) {
		t.Helper()
		var e struct {
			OK    bool
			Data  json.RawMessage
			Error any
		}
		if err := json.Unmarshal(out, &e); err != nil || !e.OK {
			t.Fatalf("otag envelope: %s (%v)", out, err)
		}
		if result != nil {
			if err := json.Unmarshal(e.Data, result); err != nil {
				t.Fatal(err)
			}
		}
	}
	decode(command(otag, "--json", "sync"), nil)
	var task struct{ ID string }
	decode(command(otag, "--json", "--project", "smoke", "task", "add", "Live smoke: reply only", "--body", "This is an acknowledgement test. Reply with exactly OTAG_SMOKE_OK. Do not run tools, commit, or otag done; the test operator will invoke otag done after your response."), &task)
	d, err := db.Open(ctx, filepath.Join(root, "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	getenv := func(k string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if key, value, ok := strings.Cut(env[i], "="); ok && key == k {
				return value
			}
		}
		return ""
	}
	client := tmux.New(socket, "/dev/null")
	ackManager := &run.Manager{DB: d, Adapters: adapter.NewRegistry(&liveAckAdapter{codex.New()}), Tmux: client, Getenv: getenv, Actor: run.Actor{Name: "user:smoke"}}
	start, err := ackManager.Start(ctx, run.StartInput{TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	waitTurn := func(r *run.Run, minimum int) int {
		t.Helper()
		var count int
		deadline := time.Now().Add(45 * time.Second)
		for ctx.Err() == nil && time.Now().Before(deadline) {
			if err := d.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE run_id=? AND kind='hook.turn_end'", r.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count >= minimum {
				break
			}
			probe := exec.CommandContext(ctx, "tmux", "-L", socket, "display-message", "-p", "-t", r.Session.TmuxPane, "#{pane_dead}")
			if out, _ := probe.Output(); strings.TrimSpace(string(out)) == "1" {
				logs, _ := filepath.Glob(filepath.Join(root, "data", "runs", "*", "*"))
				for _, log := range logs {
					if strings.HasSuffix(log, ".log") || strings.HasSuffix(log, "tui.sh") || strings.HasSuffix(log, "thread-id") {
						b, _ := os.ReadFile(log)
						t.Logf("%s: %s", filepath.Base(log), b)
					}
				}
				screen, _ := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", r.Session.TmuxPane).CombinedOutput()
				t.Fatalf("Codex pane exited before turn_end: %s", screen)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if count < minimum {
			out, _ := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", r.Session.TmuxPane).CombinedOutput()
			t.Fatalf("no turn_end: %s", out)
		}
		return count
	}
	count := waitTurn(start.Run, 1)
	var current run.Run
	decode(command(otag, "--json", "run", "show", start.Run.ID, "--events", "100"), &current)
	if current.Status != "idle" || current.Error != "" {
		t.Fatalf("run: %+v", current)
	}
	var text string
	_ = d.QueryRowContext(ctx, "SELECT data FROM events WHERE run_id=? AND kind='hook.turn_end' ORDER BY id DESC LIMIT 1", start.Run.ID).Scan(&text)
	if !strings.Contains(text, "OTAG_SMOKE_OK") {
		t.Fatalf("unexpected answer: %s", text)
	}
	if os.Getenv("OTAG_CODEX_LIVE_MODE") != "hooks" {
		var early int
		if err := d.QueryRowContext(ctx, "SELECT count(*) FROM raw_hook_payloads WHERE run_id=? AND hook='otag/earlyRequestRejected'", start.Run.ID).Scan(&early); err != nil || early == 0 {
			t.Fatal("early approval was not exercised", early, err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(root, "data", "runs", start.Run.ID, "tui-attached")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("TUI did not subscribe")
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("PASS early_approval_resolved=%d delayed_TUI_subscribed=true", early)
	}
	// Stop hooks can arrive before the TUI has finished restoring its input.
	time.Sleep(time.Second)
	// This submission goes through the remote/local TUI, not listener turn/start.
	t.Logf("TUI second turn target: launch=%q persisted=%q", start.Run.Session.TmuxPane, current.Session.TmuxPane)
	command("tmux", "-L", socket, "send-keys", "-t", current.Session.TmuxPane, "Escape")
	time.Sleep(200 * time.Millisecond)
	typed := "Reply OTAG_SMOKE_OK. No tools. Do not finish."
	for attempt := 0; attempt < 3; attempt++ {
		command("tmux", "-L", socket, "send-keys", "-t", current.Session.TmuxPane, "-l", typed)
		visible := false
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			screen := command("tmux", "-L", socket, "capture-pane", "-p", "-t", current.Session.TmuxPane)
			if strings.Contains(string(screen), typed) {
				visible = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if visible {
			break
		}
		if attempt == 2 {
			screen := command("tmux", "-L", socket, "capture-pane", "-p", "-t", current.Session.TmuxPane)
			t.Fatalf("TUI did not accept typed prompt: %s", screen)
		}
	}
	time.Sleep(700 * time.Millisecond)
	command("tmux", "-L", socket, "send-keys", "-t", current.Session.TmuxPane, "Enter")
	if n := waitTurn(start.Run, 2); n != 2 {
		t.Fatalf("second TUI turn count %d", n)
	}
	decode(command(otag, "--json", "run", "show", start.Run.ID), &current)
	verifyPolicy(t, current.Session.TranscriptPath, 2)
	var done run.DoneResult
	decode(command(otag, "--json", "--run", start.Run.ID, "done", "Codex smoke passed"), &done)
	wantPanes := 3
	if os.Getenv("OTAG_CODEX_LIVE_MODE") == "hooks" {
		wantPanes = 2
	}
	if done.TaskStatus != "done" || len(done.Closed) != wantPanes {
		t.Fatalf("done: %+v", done)
	}
	var usage int
	_ = d.QueryRowContext(ctx, "SELECT count(*) FROM run_usage WHERE run_id=?", start.Run.ID).Scan(&usage)
	// Resume is an adapter-level contract; the phase-2 CLI does not expose
	// run resume yet. Inject ResumeSessionID through a test adapter while the
	// real Manager still performs every launch, hook, DB and stop operation.
	decode(command(otag, "--json", "--project", "smoke", "task", "add", "Live resume and guard"), &task)
	resume := &liveResumeAdapter{Adapter: codex.New(), session: current.Session.HarnessSessionID}
	manager := &run.Manager{DB: d, Adapters: adapter.NewRegistry(resume), Tmux: client, Getenv: getenv, Actor: run.Actor{Name: "user:smoke"}}
	resumed, err := manager.Start(ctx, run.StartInput{TaskID: task.ID})
	if err != nil {
		logs, _ := filepath.Glob(filepath.Join(root, "data", "runs", "*", "*.log"))
		for _, log := range logs {
			b, _ := os.ReadFile(log)
			t.Logf("%s: %s", log, b)
		}
		t.Fatal(err)
	}
	if n := waitTurn(resumed.Run, 1); n != 1 {
		t.Fatalf("replayed historical completions on resume: %d", n)
	}
	r, err := run.Get(ctx, d, resumed.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "idle" || r.Error != "" || r.Session.HarnessSessionID != resume.session {
		t.Fatalf("resume: %+v", r)
	}
	if err := d.QueryRowContext(ctx, "SELECT data FROM events WHERE run_id=? AND kind='hook.turn_end' ORDER BY id DESC LIMIT 1", r.ID).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "OTAG_SMOKE_OK-RESUMED") {
		b, _ := os.ReadFile(r.Session.TranscriptPath)
		anchor, _ := os.ReadFile(filepath.Join(root, "data", "runs", r.ID, "first-turn"))
		if len(b) > 16000 {
			b = b[len(b)-16000:]
		}
		t.Logf("anchor=%s rollout=%s", anchor, b)
		t.Fatalf("resume did not recall prior answer: %s", text)
	}
	var sessionReason string
	if err := d.QueryRowContext(ctx, "SELECT json_extract(data, '$.reason') FROM events WHERE run_id=? AND kind='hook.session_start'", r.ID).Scan(&sessionReason); err != nil || sessionReason != "resume" {
		t.Fatalf("session resume reason %q: %v", sessionReason, err)
	}
	rows, err := d.QueryContext(ctx, "SELECT payload FROM raw_hook_payloads WHERE run_id=? AND hook='PreToolUse'", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		parsed, err := codex.New().ParseEvent(adapter.HookContext{Workdir: r.Workspace.Path, Deny: []string{".env*"}}, "PreToolUse", raw)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(parsed.Output), `"permissionDecision":"deny"`) {
			denied++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	transcript, err := os.ReadFile(r.Session.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if denied == 0 || !strings.Contains(string(transcript), "otag guard:") {
		if len(transcript) > 12000 {
			transcript = transcript[len(transcript)-12000:]
		}
		t.Logf("resume rollout: %s", transcript)
		t.Fatalf("no live guard denial: denied_hooks=%d", denied)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace.Path, ".env-smoke")); !os.IsNotExist(err) {
		t.Fatalf("denied destination exists: %v", err)
	}
	var stopped run.StopResult
	decode(command(otag, "--json", "run", "stop", r.ID), &stopped)
	if stopped.Before.State != tmux.Alive || stopped.Run.Status != "ended" || stopped.Run.EndedReason != "stopped" {
		t.Fatalf("stop: %+v", stopped)
	}
	for _, pane := range append([]run.AuxPane{{Pane: r.Session.TmuxPane, PID: r.Session.PID}}, r.Session.AuxPanes...) {
		if live := client.Check(ctx, pane.Pane, pane.PID); live.State == tmux.Alive {
			t.Fatalf("pane survived stop: %+v", live)
		}
	}
	if r.Session.ServerURL != "" {
		endpoint := strings.TrimPrefix(r.Session.ServerURL, "unix://")
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := net.DialTimeout("unix", endpoint, 100*time.Millisecond)
			if err != nil {
				break
			}
			_ = conn.Close()
			if time.Now().After(deadline) {
				t.Fatal("app-server survived its closed pane")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	var taskStatus string
	if err := d.QueryRowContext(ctx, "SELECT status FROM tasks WHERE id=?", task.ID).Scan(&taskStatus); err != nil || taskStatus != "ready" {
		t.Fatalf("stopped task %q: %v", taskStatus, err)
	}
	decode(command(otag, "--json", "--project", "smoke", "task", "add", "Agent commit and done", "--body", "Run one shell command: printf 'agent smoke\\n' > agent.txt && git add agent.txt && git commit -qm 'agent smoke' && \"$OTAG_BIN\" done 'agent smoke complete'. Do not add attribution lines. Do not ask permission. Keep tokens minimal."), &task)
	var agentRun run.StartResult
	decode(command(otag, "--json", "run", "start", task.ID, "--no-attach"), &agentRun)
	for ctx.Err() == nil {
		var taskState string
		if err := d.QueryRowContext(ctx, "SELECT status FROM tasks WHERE id=?", task.ID).Scan(&taskState); err != nil {
			t.Fatal(err)
		}
		if taskState == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	var agentState run.Run
	decode(command(otag, "--json", "run", "show", agentRun.Run.ID), &agentState)
	if agentState.EndedReason != "done" {
		screen, _ := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", agentRun.Run.Session.TmuxPane).CombinedOutput()
		t.Fatalf("agent did not finish: %+v\n%s", agentState, screen)
	}
	if got := strings.TrimSpace(string(command("git", "-C", agentRun.Run.Workspace.Path, "log", "-1", "--format=%s"))); got != "agent smoke" {
		t.Fatalf("agent commit missing: %s", got)
	}
	for _, pane := range append([]run.AuxPane{{Pane: agentState.Session.TmuxPane, PID: agentState.Session.PID}}, agentState.Session.AuxPanes...) {
		deadline := time.Now().Add(5 * time.Second)
		for client.Check(ctx, pane.Pane, pane.PID).State == tmux.Alive && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if client.Check(ctx, pane.Pane, pane.PID).State == tmux.Alive {
			b, _ := os.ReadFile(agentState.Session.TranscriptPath)
			if len(b) > 8000 {
				b = b[len(b)-8000:]
			}
			t.Fatalf("agent done left pane alive %+v\n%s", pane, b)
		}
	}
	t.Log("PASS agent_git_commit=true agent_otag_done=true TUI_second_turn_policy=otag/on-request/restricted")
	t.Logf("PASS guard_denies=%d stop=%s all_panes_closed=true resume_verified=true session=%s", denied, stopped.Run.EndedReason, r.Session.HarnessSessionID)

	t.Logf("PASS codex=%s model=gpt-6.1-sol effort=low session=%s turn_end=%d usage_rows=%d closed_panes=%d", strings.TrimSpace(string(command("codex", "--version"))), current.Session.HarnessSessionID, count, usage, len(done.Closed))
}

// Override only the adapter spec, not the shared phase-2 lifecycle. The tiny
// prompt explicitly authorizes a blocked write so the model exercises the guard.
type liveResumeAdapter struct {
	*codex.Adapter
	session string
}

func (a *liveResumeAdapter) spec(s adapter.RunSpec) adapter.RunSpec {
	s.ResumeSessionID = a.session
	s.Instructions = "This is an isolated adapter smoke test. Do exactly the requested blocked write attempt once; after a denial do not retry or bypass. Do not commit, call otag done, or call otag fail. Keep your answer minimal."
	s.Prompt = "Attempt exactly once using the shell: printf blocked > .env-smoke. This deliberately tests a guard denial in an isolated temporary repo. After the tool is denied, recall the exact acknowledgement you gave in the previous turn and reply with that acknowledgement followed by -RESUMED. No other tools or text."
	return s
}

func (a *liveResumeAdapter) Prepare(ctx context.Context, s adapter.RunSpec) (*adapter.LaunchSpec, error) {
	return a.Adapter.Prepare(ctx, a.spec(s))
}

func (a *liveResumeAdapter) SetupServer(ctx context.Context, s adapter.RunSpec, l *adapter.LaunchSpec) error {
	return a.Adapter.SetupServer(ctx, a.spec(s), l)
}

func verifyPolicy(t *testing.T, path string, minimum int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(b), "\n") {
		var f struct {
			Type    string `json:"type"`
			Payload struct {
				Model    string `json:"model"`
				Effort   string `json:"effort"`
				Approval string `json:"approval_policy"`
				Profile  struct {
					ID string `json:"id"`
				} `json:"active_permission_profile"`
				Sandbox struct {
					Type    string `json:"type"`
					Network bool   `json:"network_access"`
				} `json:"sandbox_policy"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &f) != nil || f.Type != "turn_context" {
			continue
		}
		count++
		if f.Payload.Model != "gpt-6.1-sol" || f.Payload.Effort != "low" || f.Payload.Approval != "on-request" || f.Payload.Profile.ID != "otag" || f.Payload.Sandbox.Network || f.Payload.Sandbox.Type == "danger-full-access" {
			t.Fatalf("unsafe turn context: %s", line)
		}
	}
	if count < minimum {
		t.Fatalf("only %d turn contexts; want %d", count, minimum)
	}
}

type liveAckAdapter struct{ *codex.Adapter }

func (a *liveAckAdapter) spec(s adapter.RunSpec) adapter.RunSpec {
	s.Instructions = "This is an isolated adapter smoke test. Follow the user request literally. Do not commit or call otag done/fail unless the user explicitly asks for it. Keep replies minimal."
	s.Prompt = "Reply exactly OTAG_SMOKE_OK. No tools."
	if s.Profile.Env["OTAG_CODEX_MODE"] != "hooks" {
		s.Prompt = "Run exactly one exec_command with cmd=\"printf OTAG_EARLY_APPROVAL\", sandbox_permissions=\"require_escalated\", justification=\"early approval smoke\". This deliberately requests approval before the test TUI attaches. If rejected, do not retry. Then reply exactly OTAG_SMOKE_OK. No other tools."
	}
	return s
}
func (a *liveAckAdapter) Prepare(ctx context.Context, s adapter.RunSpec) (*adapter.LaunchSpec, error) {
	ls, err := a.Adapter.Prepare(ctx, a.spec(s))
	if err == nil && ls.Server != nil {
		for i, f := range ls.Files {
			if f.Path == "tui.sh" {
				ls.Files[i].Data = []byte(strings.Replace(string(f.Data), `exec "$@"`, `sleep 12
exec "$@"`, 1))
			}
		}
	}
	return ls, err
}
func (a *liveAckAdapter) SetupServer(ctx context.Context, s adapter.RunSpec, l *adapter.LaunchSpec) error {
	return a.Adapter.SetupServer(ctx, a.spec(s), l)
}
