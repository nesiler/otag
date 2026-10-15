package codex_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/codex"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/run"
)

// Opt-in only (OTAG_LIVE=codex-deliver): one strict mode-B run in an isolated data
// dir, tmux server and repo; a delivery while idle (turn/start) and one while busy
// (turn/steer), each with a marker. Reports the turn ids and whether Codex fired
// UserPromptSubmit with the marker. Authentication is copied; no user config is written.
func TestLiveDeliver(t *testing.T) {
	if os.Getenv("OTAG_LIVE") != "codex-deliver" {
		t.Skip("set OTAG_LIVE=codex-deliver to run the real CLI")
	}
	root, err := os.MkdirTemp("/private/tmp", "otag-cdl-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
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
	auth, err := os.ReadFile(filepath.Join(account, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if after, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(after) != string(auth) {
			t.Error("Codex rewrote the copied auth.json (token refresh)")
		}
	}()
	otag := filepath.Join(root, "otag")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", otag, "github.com/nesiler/otag/cmd/otag").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	socket := fmt.Sprintf("otag-cdl-%d", time.Now().UnixNano())
	defer func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() }()
	env := append(os.Environ(), "OTAG_DB="+filepath.Join(root, "otag.db"), "OTAG_CONFIG_DIR="+cfg, "OTAG_DATA_DIR="+filepath.Join(root, "data"), "OTAG_TMUX_SOCKET="+socket, "OTAG_TMUX_CONF=/dev/null", "OTAG_BIN="+otag, "CODEX_HOME="+home)
	command := func(bin string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
		}
		return out
	}
	command("git", "init", "-q", "-b", "main")
	command("git", "-c", "user.name=n", "-c", "user.email=n@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "initial")
	registry := fmt.Sprintf("[accounts.c]\nharness = \"codex\"\nconfig_dir = %q\n[profiles.p]\naccount = \"c\"\nmodel = \"gpt-6.1-sol\"\neffort = \"low\"\nargs = [\"--no-alt-screen\"]\n", home)
	_ = os.WriteFile(filepath.Join(cfg, "registry.toml"), []byte(registry), 0o600)
	// Full (redacted) hook payloads: the UserPromptSubmit prompt is the marker proof.
	_ = os.WriteFile(filepath.Join(cfg, "otag.toml"), []byte("[daemon]\nraw_payloads = true\n"), 0o600)
	_ = os.WriteFile(filepath.Join(cfg, "projects", "dl.toml"), []byte(fmt.Sprintf("path = %q\nprefix = \"dl\"\nworkspace = \"worktree\"\ndefault_profile = \"p\"\nguard = \"strict\"\n", repo)), 0o600)
	decode := func(out []byte, result any) {
		t.Helper()
		var e struct {
			OK   bool
			Data json.RawMessage
		}
		if err := json.Unmarshal(out, &e); err != nil || !e.OK {
			t.Fatalf("otag envelope: %s (%v)", out, err)
		}
		if result != nil {
			_ = json.Unmarshal(e.Data, result)
		}
	}
	decode(command(otag, "--json", "sync"), nil)
	var task struct{ ID string }
	decode(command(otag, "--json", "--project", "dl", "task", "add", "Delivery test", "--body", "Delivery test. Reply exactly READY. No tools. Do not commit or call otag done/fail; later messages will tell you what to do."), &task)
	var started run.StartResult
	decode(command(otag, "--json", "run", "start", task.ID, "--no-attach"), &started)
	d, err := db.Open(ctx, filepath.Join(root, "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	runID := started.Run.ID
	runDir := filepath.Join(root, "data", "runs", runID)
	turnEnds := func() int {
		var n int
		_ = d.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE run_id=? AND kind='hook.turn_end'", runID).Scan(&n)
		return n
	}
	waitFor := func(what string, limit time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !ok() {
			if time.Now().After(deadline) || ctx.Err() != nil {
				screen, _ := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", started.Run.Session.TmuxPane).CombinedOutput()
				logs, _ := filepath.Glob(filepath.Join(runDir, "*.log"))
				for _, log := range logs {
					b, _ := os.ReadFile(log)
					t.Logf("%s: %s", filepath.Base(log), b)
				}
				t.Fatalf("timed out waiting for %s\n%s", what, screen)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	lastAnswer := func() string {
		var text string
		_ = d.QueryRowContext(ctx, "SELECT data FROM events WHERE run_id=? AND kind='hook.turn_end' ORDER BY id DESC LIMIT 1", runID).Scan(&text)
		return text
	}
	waitFor("first turn_end", 90*time.Second, func() bool { return turnEnds() >= 1 })
	waitFor("TUI attach", 30*time.Second, func() bool { _, err := os.Stat(filepath.Join(runDir, "tui-attached")); return err == nil })
	var current run.Run
	decode(command(otag, "--json", "run", "show", runID), &current)
	target := adapter.Target{RunID: runID, HarnessSessionID: current.Session.HarnessSessionID, ServerURL: current.Session.ServerURL}
	deliver := func(id, text string, steer bool) (codex.Receipt, error) {
		var r codex.Receipt
		var err error
		for range 50 { // not_ready while the run settles (status/TUI)
			start := time.Now()
			r, err = codex.DeliverReceipt(ctx, target, adapter.Message{ID: id, Text: text, Steer: steer})
			var de *codex.DeliveryError
			if !errors.As(err, &de) || de.Reason != codex.ReasonNotReady {
				t.Logf("deliver %s steer=%v -> %+v %v (%s)", id, steer, r, err, time.Since(start).Round(time.Millisecond))
				return r, err
			}
			time.Sleep(200 * time.Millisecond)
		}
		return r, err
	}
	// 1. Idle: turn/start.
	r1, err := deliver("d1", "[otag#d1] reply OK-1", false)
	if err != nil || r1.Kind != "start" || r1.TurnID == "" {
		t.Fatal(r1, err)
	}
	waitFor("OK-1 turn_end", 90*time.Second, func() bool { return turnEnds() >= 2 })
	if a := lastAnswer(); !strings.Contains(a, "OK-1") {
		t.Fatalf("answer to d1: %s", a)
	}
	// 2. Busy: a turn that sleeps, then a plain delivery (busy) and a steer.
	r2, err := deliver("d2", "[otag#d2] Run exactly one shell command: sleep 20. Then reply exactly DONE-2.", false)
	if err != nil || r2.Kind != "start" {
		t.Fatal(r2, err)
	}
	waitFor("the sleep tool call", 60*time.Second, func() bool {
		var n int
		_ = d.QueryRowContext(ctx, "SELECT count(*) FROM raw_hook_payloads WHERE run_id=? AND hook='PreToolUse' AND payload LIKE '%sleep 20%'", runID).Scan(&n)
		return n > 0 || turnEnds() >= 3
	})
	if turnEnds() >= 3 {
		t.Fatal("the sleeping turn ended before the steer:", lastAnswer())
	}
	var de *codex.DeliveryError
	if _, err := deliver("d3", "[otag#d3] not steered", false); !errors.As(err, &de) || de.Reason != codex.ReasonBusy {
		t.Fatal("busy delivery without steer:", err)
	}
	r4, err := deliver("d4", "[otag#d4] Also append OK-4 at the end of your final reply.", true)
	if err != nil || r4.Kind != "steer" || r4.TurnID != r2.TurnID {
		t.Fatal(r4, err)
	}
	// What Codex answers to a stale expectedTurnId (busy, and later idle).
	t.Logf("RESULT stale steer while busy: %s", rawSteer(ctx, t, current.Session.ServerURL, target.HarnessSessionID, r1.TurnID))
	waitFor("DONE-2 turn_end", 120*time.Second, func() bool { return turnEnds() >= 3 })
	t.Logf("RESULT stale steer while idle: %s", rawSteer(ctx, t, current.Session.ServerURL, target.HarnessSessionID, r2.TurnID))
	answer := lastAnswer()
	t.Logf("busy turn answer: %s", answer)
	// UserPromptSubmit (mode B hook) for app-server turn/start and turn/steer input.
	rows, err := d.QueryContext(ctx, "SELECT payload FROM raw_hook_payloads WHERE run_id=? AND hook='UserPromptSubmit' ORDER BY id", runID)
	if err != nil {
		t.Fatal(err)
	}
	ups := map[string]string{}
	for rows.Next() {
		var raw []byte
		_ = rows.Scan(&raw)
		var p struct {
			Prompt string `json:"prompt"`
			TurnID string `json:"turn_id"`
		}
		_ = json.Unmarshal(raw, &p)
		for _, marker := range []string{"[otag#d1]", "[otag#d2]", "[otag#d4]"} {
			if strings.Contains(p.Prompt, marker) {
				ups[marker] = p.TurnID
			}
		}
	}
	_ = rows.Close()
	t.Logf("RESULT start d1 turn=%s; start d2 turn=%s; steer d4 turn=%s; busy-without-steer refused; answer has OK-4=%v", r1.TurnID, r2.TurnID, r4.TurnID, strings.Contains(answer, "OK-4"))
	t.Logf("RESULT UserPromptSubmit with marker (hook turn_id): %v", ups)
	b, _ := os.ReadFile(current.Session.TranscriptPath)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, `"turn_context"`) {
			var f struct {
				Payload struct {
					TurnID   string `json:"turn_id"`
					Approval string `json:"approval_policy"`
					Reviewer string `json:"approvals_reviewer"`
					Profile  struct {
						ID string `json:"id"`
					} `json:"active_permission_profile"`
					Cwd string `json:"cwd"`
				} `json:"payload"`
			}
			_ = json.Unmarshal([]byte(line), &f)
			t.Logf("RESULT turn_context %+v", f.Payload)
		}
	}
	command(otag, "--json", "run", "stop", runID)
}

// rawSteer sends one turn/steer with expectedTurnId from a separate client and
// returns Codex's raw answer (it must be refused).
func rawSteer(ctx context.Context, t *testing.T, serverURL, thread, expected string) string {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(serverURL, "unix://"))
	}}
	ws, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(128 << 20)
	send := func(v any) { b, _ := json.Marshal(v); _ = ws.Write(ctx, websocket.MessageText, b) }
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "otag-test", "version": "0"}, "capabilities": map[string]any{"experimentalApi": true}}})
	send(map[string]any{"method": "initialized", "params": map[string]any{}})
	send(map[string]any{"id": 2, "method": "turn/steer", "params": map[string]any{"threadId": thread, "expectedTurnId": expected, "input": []any{map[string]any{"type": "text", "text": "stale steer must be refused", "text_elements": []any{}}}}})
	for {
		_, b, err := ws.Read(ctx)
		if err != nil {
			return err.Error()
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(b, &msg) == nil && string(msg.ID) == "2" && msg.Method == "" {
			return string(b)
		}
	}
}
