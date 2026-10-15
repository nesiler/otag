package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/run"
)

// msgHarness is a harness with a child run and a coordinator run recorded directly in
// the database (no tmux): enough for the messaging commands.
type msgHarness struct {
	*harness
	task, child, coord string
	childEnv, coordEnv map[string]string
}

func newMsgHarness(t *testing.T) *msgHarness {
	t.Helper()
	h := &msgHarness{harness: newHarness(t)}
	h.cwd = h.alpha
	_, e := h.json("task", "add", "Pick a database")
	var tk struct{ ID string }
	_ = json.Unmarshal(e.Data, &tk)
	h.task, h.child, h.coord = tk.ID, tk.ID+"-r1", "al-coord-r1"
	d, err := db.Open(context.Background(), h.env["OTAG_DB"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	hash := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	insert := `INSERT INTO runs (id, project_id, task_id, role, profile_id, profile_revision_id, account_id, harness, status, token_hash)
SELECT ?, pr.id, ?, ?, p.id, pv.id, pv.account_id, pv.harness, 'idle', ?
FROM projects pr, profiles p JOIN profile_revisions pv ON pv.profile_id = p.id AND pv.config_hash = p.config_hash
WHERE pr.name = 'alpha' AND p.name = 'sonnet'`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{insert, []any{h.child, h.task, "child", hash("child-token")}},
		{insert, []any{h.coord, nil, "coordinator", hash("coord-token")}},
		{`UPDATE tasks SET status = 'running', current_run_id = ? WHERE id = ?`, []any{h.child, h.task}},
	} {
		if _, err := d.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	h.childEnv = map[string]string{"OTAG_RUN_ID": h.child, run.TokenEnv: "child-token"}
	h.coordEnv = map[string]string{"OTAG_RUN_ID": h.coord, run.TokenEnv: "coord-token"}
	return h
}

// as runs `otag --json args` with extra environment and returns the exit code and envelope.
func (h *msgHarness) as(extra map[string]string, args ...string) (int, env) {
	h.t.Helper()
	saved := map[string]string{}
	for k, v := range extra {
		saved[k] = h.env[k]
		h.env[k] = v
	}
	defer func() {
		for k, v := range saved {
			if v == "" {
				delete(h.env, k)
			} else {
				h.env[k] = v
			}
		}
	}()
	return h.json(args...)
}

func (h *msgHarness) taskStatus() string {
	h.t.Helper()
	_, e := h.json("task", "show", h.task)
	var tk struct{ Status string }
	_ = json.Unmarshal(e.Data, &tk)
	return tk.Status
}

func wantCode(t *testing.T, what string, code int, e env, want string) {
	t.Helper()
	if code == 0 || e.Error == nil || e.Error.Code != want {
		t.Fatalf("%s: exit %d %+v, want %s", what, code, e.Error, want)
	}
}

func TestMessagingCLI(t *testing.T) {
	h := newMsgHarness(t)
	type sent struct {
		Message  msg.Message         `json:"message"`
		Replayed bool                `json:"replayed"`
		Attempts []msg.AttemptResult `json:"attempts"`
		Resume   any                 `json:"resume"`
		Park     *bool               `json:"park"`
		Instr    string              `json:"instruction"`
	}
	decode := func(e env) sent {
		t.Helper()
		var s sent
		if err := json.Unmarshal(e.Data, &s); err != nil {
			t.Fatalf("%s: %v", e.Data, err)
		}
		return s
	}

	// The child asks; the task waits; ask returns at once.
	code, e := h.as(h.childEnv, "ask", "Which database?")
	if code != 0 {
		t.Fatalf("ask: %+v", e.Error)
	}
	q := decode(e)
	if !q.Message.Ask || q.Park == nil || *q.Park || !strings.Contains(q.Instr, "End your turn") ||
		len(q.Attempts) != 1 || q.Attempts[0].Result != "queued" || str(q.Message.Deliveries[0].To.Run) != h.coord {
		t.Fatalf("ask result %s", e.Data)
	}
	if h.taskStatus() != "waiting" {
		t.Fatalf("task %s after ask", h.taskStatus())
	}
	code, e = h.as(h.childEnv, "ask", "Something else?")
	wantCode(t, "second ask", code, e, "CONFLICT")
	code, e = h.json("ask", "from a human?")
	wantCode(t, "ask outside a run", code, e, "USAGE")
	code, e = h.as(h.coordEnv, "ask", "coordinator?")
	wantCode(t, "ask from the coordinator", code, e, "USAGE")

	// The child's scope: send only to coord, no needs, a forged token is refused.
	code, e = h.as(h.childEnv, "send", "human", "hi")
	wantCode(t, "child send to human", code, e, "FORBIDDEN")
	code, e = h.as(h.childEnv, "send", h.coord, "hi")
	wantCode(t, "child send to a run", code, e, "FORBIDDEN")
	code, e = h.as(h.childEnv, "needs")
	wantCode(t, "child needs", code, e, "FORBIDDEN")
	code, e = h.as(map[string]string{"OTAG_RUN_ID": h.child, run.TokenEnv: "forged"}, "send", "coord", "hi")
	wantCode(t, "forged token", code, e, "FORBIDDEN")
	code, e = h.json("--run", h.child, "inbox")
	wantCode(t, "human --run", code, e, "USAGE")
	// A child that dropped OTAG_RUN_ID does not pass for the human.
	code, e = h.as(map[string]string{run.TokenEnv: "child-token"}, "send", "human", "hi")
	wantCode(t, "token without run id", code, e, "FORBIDDEN")
	code, e = h.as(map[string]string{run.TokenEnv: "child-token"}, "task", "cancel", h.task)
	wantCode(t, "coordinator command with a token", code, e, "FORBIDDEN")
	wt := filepath.Join(paths.DataDir(func(k string) string { return h.env[k] }), "worktrees", "alpha", h.task)
	if err := os.MkdirAll(filepath.Join(wt, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.cwd = filepath.Join(wt, "src")
	code, e = h.json("--project", "alpha", "reply", "1", "x")
	wantCode(t, "human in a run workspace", code, e, "FORBIDDEN")
	h.cwd = h.alpha
	code, e = h.as(h.childEnv, "send", "coord", "FYI: tests are slow")
	if code != 0 {
		t.Fatalf("child send coord: %+v", e.Error)
	}
	fyi := decode(e).Message

	// The human sees the question in needs; the coordinator reads its inbox.
	_, e = h.json("needs")
	var needs msg.Needs
	_ = json.Unmarshal(e.Data, &needs)
	if len(needs.Questions) != 1 || needs.Questions[0].Message.ID != q.Message.ID || needs.Questions[0].To != "coordinator "+h.coord {
		t.Fatalf("needs %s", e.Data)
	}
	code, e = h.as(h.coordEnv, "inbox")
	var inbox inboxResult
	_ = json.Unmarshal(e.Data, &inbox)
	if code != 0 || len(inbox.Messages) != 2 || inbox.Messages[0].ID != q.Message.ID || inbox.Messages[1].Delivery.State != msg.Read {
		t.Fatalf("coordinator inbox: %d %s", code, e.Data)
	}
	if _, e = h.as(h.coordEnv, "inbox"); !strings.Contains(string(e.Data), `"messages":[]`) {
		t.Fatalf("inbox after read: %s", e.Data)
	}

	// The coordinator answers; the task waits until the child has the answer.
	code, e = h.as(h.coordEnv, "reply", strconv.FormatInt(q.Message.ID, 10), "SQLite.")
	ans := decode(e)
	if code != 0 || ans.Message.Kind != msg.KindAnswer || ans.Resume != nil || str(ans.Message.Deliveries[0].To.Run) != h.child {
		t.Fatalf("reply: %d %s", code, e.Data)
	}
	if h.taskStatus() != "waiting" {
		t.Fatal("task running before the answer arrived")
	}
	code, e = h.as(h.coordEnv, "reply", strconv.FormatInt(q.Message.ID, 10), "Postgres.")
	wantCode(t, "second answer", code, e, "CONFLICT")
	code, e = h.as(h.childEnv, "inbox", "--wait", "--timeout", "2s")
	_ = json.Unmarshal(e.Data, &inbox)
	if code != 0 || len(inbox.Messages) != 1 || inbox.Messages[0].ID != ans.Message.ID || inbox.TimedOut {
		t.Fatalf("child inbox: %d %s", code, e.Data)
	}
	if h.taskStatus() != "running" {
		t.Fatalf("task %s after the answer was read", h.taskStatus())
	}
	if code, e = h.as(h.childEnv, "inbox", "--wait", "--timeout", "100ms"); !strings.Contains(string(e.Data), `"timed_out":true`) {
		t.Fatalf("empty wait: %d %s", code, e.Data)
	}

	// The child replies only to messages for its run or task.
	code, e = h.as(h.childEnv, "reply", strconv.FormatInt(fyi.ID, 10), "self?")
	wantCode(t, "child reply to its own message", code, e, "FORBIDDEN")
	code, e = h.as(h.childEnv, "reply", strconv.FormatInt(ans.Message.ID, 10), "Thanks, done soon.")
	if code != 0 || decode(e).Message.Kind != msg.KindInfo || decode(e).Message.Deliveries[0].To.Recipient != h.coord {
		t.Fatalf("child reply: %d %s", code, e.Data)
	}

	// --request-id: the same send is the same message.
	_, e = h.json("send", h.child+",coord", "Rebase on main.", "--kind", "command", "--request-id", "r-1")
	first := decode(e)
	_, e = h.json("send", h.child+",coord", "Rebase on main.", "--kind", "command", "--request-id", "r-1")
	again := decode(e)
	if first.Message.ID != again.Message.ID || !again.Replayed || len(first.Message.Deliveries) != 2 {
		t.Fatalf("replay: %+v %+v", first.Message.ID, again)
	}
	code, e = h.json("send", "nobody", "x")
	wantCode(t, "unknown recipient", code, e, "NOT_FOUND")
	code, e = h.json("send", "human", "x", "--kind", "answer")
	wantCode(t, "send answer", code, e, "USAGE")

	// Human output.
	out := h.ok("send", "human", "note to self", "--ttl", "1h")
	if !strings.Contains(out, "info #") || !strings.Contains(out, "→ human: skipped") {
		t.Fatalf("send output %q", out)
	}
	if out := h.ok("inbox"); !strings.Contains(out, "note to self") || !strings.Contains(out, "from the human") {
		t.Fatalf("inbox output %q", out)
	}
	if out := h.ok("needs"); !strings.Contains(out, "nothing needs you") && !strings.Contains(out, "idle runs") {
		t.Fatalf("needs output %q", out)
	}
	if out, _ := h.runAs(h.childEnv, "ask", "Second question?"); !strings.HasPrefix(out, "Question #") {
		t.Fatalf("ask output %q", out)
	}
}

func (h *msgHarness) runAs(extra map[string]string, args ...string) (string, int) {
	for k, v := range extra {
		h.env[k] = v
	}
	defer func() {
		for k := range extra {
			delete(h.env, k)
		}
	}()
	code, out, _ := h.run(args...)
	return out, code
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// The phase 4 lifecycle commands are registered and implemented (P4): on a task with
// no runs they refuse with their own errors.
func TestPhase4Commands(t *testing.T) {
	h := newHarness(t)
	h.cwd = h.alpha
	for _, c := range []struct {
		args []string
		code string
	}{
		{[]string{"review", "al-1", "--accept"}, "NOT_FOUND"},
		{[]string{"review", "al-1"}, "USAGE"},
		{[]string{"run", "resume", "al-1"}, "NOT_FOUND"},
		{[]string{"run", "stop", "al-1", "--park"}, "NOT_FOUND"},
	} {
		code, e := h.json(c.args...)
		wantCode(t, strings.Join(c.args, " "), code, e, c.code)
	}
	_, errs, _ := h.run("help") // explicit help goes to stdout
	for _, c := range []string{"send", "inbox", "reply", "ask", "needs", "coord", "review", "stats"} {
		if !strings.Contains(errs, "\n  "+c+" ") {
			t.Errorf("usage lacks %s", c)
		}
	}
}
