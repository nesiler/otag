package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tmux"
)

// otagBin is a otag binary built once for tests whose panes call `otag event` / `otag done`.
var otagBin string

func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "otag-bin-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		otagBin = filepath.Join(dir, "otag")
		if out, err := exec.Command("go", "build", "-o", otagBin, "github.com/nesiler/otag/cmd/otag").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build otag: %v\n%s", err, out)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

type runHarness struct {
	*harness
	tmux *tmux.Client
	repo string // project "alpha": git repo, worktree mode, no review profile
	same string // project "beta": git repo, same mode, review profile
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "config", "user.name", "tester")
	gitIn(t, dir, "config", "user.email", "tester@example.com")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644)
	gitIn(t, dir, "add", "README.md")
	gitIn(t, dir, "commit", "-q", "-m", "init")
}

func newRunHarness(t *testing.T) *runHarness {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	h := &harness{t: t, root: root, cfg: filepath.Join(root, "cfg")}
	rh := &runHarness{harness: h, repo: filepath.Join(root, "alpha"), same: filepath.Join(root, "beta")}
	socket := fmt.Sprintf("otag-test-%d-%d", os.Getpid(), time.Now().UnixNano()%1e9)
	h.env = map[string]string{
		"HOME":             root,
		"USER":             "tester",
		"PATH":             os.Getenv("PATH"),
		"OTAG_DB":          filepath.Join(root, "otag.db"),
		"OTAG_CONFIG_DIR":  h.cfg,
		"OTAG_DATA_DIR":    filepath.Join(root, "data"),
		"OTAG_TMUX_SOCKET": socket,
		"OTAG_TMUX_CONF":   os.DevNull,
		"OTAG_BIN":         otagBin,
	}
	rh.tmux = tmux.New(socket, os.DevNull)
	t.Cleanup(func() { _ = rh.tmux.KillServer(context.Background()) })
	_ = os.MkdirAll(filepath.Join(h.cfg, "projects"), 0o755)
	initRepo(t, rh.repo)
	initRepo(t, rh.same)
	h.write("registry.toml", `
[accounts.f]
harness = "fake"
[profiles.auto]
account = "f"
args = ["start", "busy", "usage", "ask", "idle", "busy", "commit", "done:did the thing", "wait"]
[profiles.manual]
account = "f"
args = ["start", "wait"]
[profiles.trusty]
account = "f"
args = ["trust", "start", "wait"]
env = { FAKE_SERVER = "1", FAKE_SIDECAR = "1" }
[profiles.stuck]
account = "f"
args = ["start", "wait"]
env = { FAKE_SIDECAR = "never-ready", FAKE_READY_TIMEOUT = "20s" }
[profiles.served]
account = "f"
args = ["start", "wait"]
env = { FAKE_SERVER = "1", FAKE_SIDECAR = "1" }
[profiles.crashy]
account = "f"
args = ["start", "busy", "exit:3"]
[profiles.review]
account = "f"
`)
	// These tests were written for the strict guard; TestLightGuardRun covers light.
	h.write("projects/alpha.toml", "path = \""+rh.repo+"\"\nprefix = \"al\"\ndefault_profile = \"auto\"\nguard = \"strict\"\n")
	h.write("projects/beta.toml", "path = \""+rh.same+"\"\nprefix = \"be\"\nworkspace = \"same\"\n"+
		"default_profile = \"manual\"\nreview_profile = \"review\"\ndeny = [\".env*\"]\nguard = \"strict\"\n")

	h.ok("sync")
	return rh
}

func (h *runHarness) addTask(project, title string, extra ...string) string {
	h.t.Helper()
	_, e := h.json(append([]string{"--project", project, "task", "add", title}, extra...)...)
	var tk struct{ ID string }
	_ = json.Unmarshal(e.Data, &tk)
	if tk.ID == "" {
		h.t.Fatalf("task add: %s", e.Data)
	}
	return tk.ID
}

func (h *runHarness) start(taskID string, extra ...string) run.StartResult {
	h.t.Helper()
	code, e := h.json(append([]string{"run", "start", taskID, "--no-attach"}, extra...)...)
	if code != 0 {
		h.t.Fatalf("run start: %d %+v", code, e.Error)
	}
	var res run.StartResult
	if err := json.Unmarshal(e.Data, &res); err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *runHarness) show(ref string) run.Run {
	h.t.Helper()
	code, e := h.json("run", "show", ref, "--events", "100")
	if code != 0 {
		h.t.Fatalf("run show: %+v", e.Error)
	}
	var r run.Run
	if err := json.Unmarshal(e.Data, &r); err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *runHarness) waitRun(ref string, what string, ok func(run.Run) bool) run.Run {
	h.t.Helper()
	var r run.Run
	for i := 0; i < 200; i++ {
		// A task may have no run yet: keep polling.
		if code, e := h.json("run", "show", ref, "--events", "100"); code == 0 {
			r = run.Run{}
			if json.Unmarshal(e.Data, &r) == nil && ok(r) {
				return r
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	h.t.Fatalf("timeout waiting for %s:\n%s", what, b)
	return r
}

func (h *runHarness) db() *db.DB {
	h.t.Helper()
	d, err := db.Open(context.Background(), h.env["OTAG_DB"])
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = d.Close() })
	return d
}

func (h *runHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db().QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (h *runHarness) taskStatus(id string) string {
	h.t.Helper()
	_, e := h.json("task", "show", id)
	var tk struct{ Status string }
	_ = json.Unmarshal(e.Data, &tk)
	return tk.Status
}

// hook runs the event bridge in-process with payload on stdin.
func (h *runHarness) hook(env map[string]string, payload string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), append([]string{"event"}, args...), Env{
		Stdin: strings.NewReader(payload), Stdout: &out, Stderr: &errb, Cwd: h.root,
		Getenv: func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return h.env[k]
		},
	})
	return code, out.String(), errb.String()
}

// token is the OTAG_RUN_TOKEN of a fake run, to act as the run.
func (h *runHarness) token(runID string) string {
	return fake.Token(filepath.Join(h.env["OTAG_DATA_DIR"], "runs", runID))
}

func eventKinds(r run.Run) []string {
	var out []string
	for _, e := range r.Events {
		k := e.Kind
		if e.ToStatus != "" {
			k += ":" + e.ToStatus
		}
		out = append(out, k)
	}
	return out
}

// The child runs autonomously in its pane: events through hooks, a commit, `otag done`.
func TestRunFlowAutonomous(t *testing.T) {
	h := newRunHarness(t)
	id := h.addTask("alpha", "Add a line", "--body", "Append to fake.txt", "--extra-prompt", "Be brief.")
	res := h.start(id)
	if res.Run.ID != id+"-r1" || res.Run.Status == run.Queued || res.Run.Session == nil || res.Run.Session.TmuxPane == "" {
		t.Fatalf("start: %+v", res.Run)
	}
	if !strings.Contains(res.Attach, "-L "+h.env["OTAG_TMUX_SOCKET"]) || !res.FirstSeen {
		t.Fatalf("start result: %+v", res)
	}
	r := h.waitRun(res.Run.ID, "run done", func(r run.Run) bool { return r.Status == run.Ended })
	if r.EndedReason != run.ReasonDone || r.TaskStatus != "done" || r.FirstActivityAt == "" {
		t.Fatalf("ended: %s %s task %s", r.Status, r.EndedReason, r.TaskStatus)
	}

	wt := filepath.Join(h.env["OTAG_DATA_DIR"], "worktrees", "alpha", id)
	if r.Workspace == nil || r.Workspace.Path != wt || r.Workspace.Branch != "feature/"+id || r.Workspace.Status != "kept" {
		t.Fatalf("workspace: %+v", r.Workspace)
	}
	head := gitIn(t, wt, "rev-parse", "HEAD")
	var summary, commit string
	for _, o := range r.Outputs {
		switch o.Kind {
		case "summary":
			summary = o.Value
		case "commit":
			commit = o.Value
		}
	}
	if summary != "did the thing" || commit != head || r.Workspace.HeadSHA != head || head == r.Workspace.BaseSHA {
		t.Fatalf("outputs %+v head %s base %s", r.Outputs, head, r.Workspace.BaseSHA)
	}
	// The project working tree is untouched; the work is on the task branch.
	if st := gitIn(t, h.repo, "status", "--porcelain"); st != "" {
		t.Fatalf("project working tree changed: %q", st)
	}
	if _, err := os.Stat(filepath.Join(h.repo, "fake.txt")); err == nil {
		t.Fatal("fake.txt written into the project")
	}
	if gitIn(t, h.repo, "rev-parse", "feature/"+id) != head {
		t.Fatal("branch not at worktree HEAD")
	}

	if r.Session.HarnessSessionID != "fake-"+r.ID || r.Usage == nil || r.Usage.InputTokens != 100 || r.Usage.OutputTokens != 20 {
		t.Fatalf("session/usage: %+v %+v", r.Session, r.Usage)
	}
	kinds := strings.Join(eventKinds(r), " ")
	for _, want := range []string{"run.created:queued", "run.status:starting", "run.launched", "hook.session_start",
		"run.status:idle", "run.status:running", "hook.question", "run.status:waiting", "run.status:ended"} {
		if !strings.Contains(kinds, want) {
			t.Errorf("missing event %s in %s", want, kinds)
		}
	}
	if n := h.count(`SELECT count(*) FROM events WHERE task_id = ? AND kind = 'task.status' AND to_status = 'waiting'`, id); n != 1 {
		t.Errorf("task waiting transitions = %d", n)
	}
	files, _ := os.ReadDir(filepath.Join(h.env["OTAG_DATA_DIR"], "runs", r.ID))
	if len(files) != 4 { // the fake harness also keeps its token
		t.Errorf("run dir files: %v", files)
	}

	// done closed the pane (the harness was still in its "wait" step); nothing to stop.
	h.waitRun(r.ID, "pane closed", func(r run.Run) bool { return r.Live == nil || r.Live.State == tmux.Gone })
	if l := h.tmux.Check(context.Background(), r.Session.TmuxPane, r.Session.PID); l.State != tmux.Gone {
		t.Fatalf("pane after done: %+v", l)
	}
	if r.Session.Status != "ended" || r.Session.EndReason != "done" {
		t.Fatalf("session after done: %+v", r.Session)
	}
	_, e := h.json("run", "stop", id)
	var st run.StopResult
	_ = json.Unmarshal(e.Data, &st)
	if len(st.Killed) != 0 || st.Run.EndedReason != run.ReasonDone {
		t.Fatalf("stop after done: %s", e.Data)
	}
	// done/fail act as the run: without its token they are refused.
	if code, e := h.json("--run", r.ID, "done", "again"); code == 0 || e.Error.Code != "FORBIDDEN" {
		t.Fatalf("done without the token: %d %+v", code, e.Error)
	}
	h.env[run.TokenEnv] = "not-the-token"
	if code, e := h.json("--run", r.ID, "fail", "x"); code == 0 || e.Error.Code != "FORBIDDEN" {
		t.Fatalf("fail with another token: %d %+v", code, e.Error)
	}
	h.env[run.TokenEnv] = h.token(r.ID)
	if code, e := h.json("--run", r.ID, "done", "again"); code == 0 || e.Error.Code != "INVALID_TRANSITION" {
		t.Fatalf("second done: %d %+v", code, e.Error)
	}
	delete(h.env, run.TokenEnv)
}

// Events driven from the test: state machine, task coupling, dedupe, unknown runs,
// session-id fallback, task me and done into review (same-dir workspace).
func TestRunEventsDriven(t *testing.T) {
	h := newRunHarness(t)
	id := h.addTask("beta", "Investigate", "--extra-prompt", "Report numbers.")
	res := h.start(id)
	runID := res.Run.ID
	if res.Run.Workspace == nil || res.Run.Workspace.Mode != "same" || res.Run.Workspace.Path != h.same {
		t.Fatalf("same workspace: %+v", res.Run.Workspace)
	}
	h.waitRun(runID, "idle", func(r run.Run) bool { return r.Status == run.Idle })
	if h.taskStatus(id) != "running" {
		t.Fatalf("task not running: %s", h.taskStatus(id))
	}
	env := map[string]string{"OTAG_RUN_ID": runID, run.TokenEnv: h.token(runID)}

	send := func(kind, payload, eventID string) string {
		t.Helper()
		args := []string{"fake", kind}
		if eventID != "" {
			args = append(args, "--event-id", eventID)
		}
		code, out, errs := h.hook(env, payload, args...)
		if code != 0 || errs != "" {
			t.Fatalf("hook %s: %d %q %q", kind, code, out, errs)
		}
		return out
	}
	send("turn_start", "{}", "e1")
	send("turn_start", "{}", "e1") // duplicate delivery
	if r := h.show(runID); r.Status != run.Running {
		t.Fatalf("after turn_start: %s", r.Status)
	}
	if n := h.count(`SELECT count(*) FROM events WHERE run_id = ? AND kind = 'hook.turn_start'`, runID); n != 1 {
		t.Fatalf("dedupe: %d hook.turn_start events", n)
	}
	if n := h.count(`SELECT count(*) FROM raw_hook_payloads WHERE run_id = ? AND hook = 'turn_start' AND payload = '{}'`, runID); n != 2 {
		t.Fatalf("raw payloads: %d", n)
	}
	send("question", `{"text":"Which file?"}`, "e2")
	if r := h.show(runID); r.Status != run.Waiting || h.taskStatus(id) != "waiting" {
		t.Fatalf("question: run %s task %s", r.Status, h.taskStatus(id))
	}
	send("turn_end", `{"text":"ok"}`, "e3")
	if r := h.show(runID); r.Status != run.Idle || h.taskStatus(id) != "running" {
		t.Fatalf("turn_end: run %s task %s", r.Status, h.taskStatus(id))
	}
	if out := send("pre_tool", `{}`, ""); out != `{"decision":"allow"}` {
		t.Fatalf("pre_tool output %q", out)
	}
	send("usage", `{"input_tokens":5,"output_tokens":1,"id":"u-1"}`, "")
	send("usage", `{"input_tokens":5,"output_tokens":1,"id":"u-1"}`, "") // same usage id
	if r := h.show(runID); r.Usage.Rows != 1 || r.Status != run.Running {
		t.Fatalf("usage dedupe: %+v status %s", r.Usage, r.Status)
	}

	// The adapter's guard decision reaches the harness for a known run...
	if out := send("pre_tool", `{"command":"git push origin main"}`, ""); !strings.Contains(out, `"decision":"deny"`) {
		t.Fatalf("guard: %q", out)
	}
	// ...and a guarded hook fails closed: an unknown run, an unusable database or a
	// hook carrying another run's token is refused, whatever the command.
	code, out, errs := h.hook(map[string]string{"OTAG_RUN_ID": "ghost"}, `{"command":"ls"}`, "fake", "pre_tool")
	if code != 0 || !strings.Contains(out, `"decision":"deny"`) || !strings.Contains(out, "ghost") || !strings.Contains(errs, "ghost") {
		t.Fatalf("unknown run: %d %q %q", code, out, errs)
	}
	if _, out, _ := h.hook(map[string]string{"OTAG_DB": h.root}, `{"command":"ls"}`, "fake", "pre_tool"); !strings.Contains(out, `"decision":"deny"`) {
		t.Fatalf("db failure output %q", out)
	}
	if _, out, _ := h.hook(map[string]string{"OTAG_RUN_ID": runID, run.TokenEnv: "forged"}, `{"command":"ls"}`, "fake", "pre_tool"); !strings.Contains(out, `"decision":"deny"`) {
		t.Fatalf("forged token output %q", out)
	}
	// A non-guarded hook with a forged token is not recorded.
	before := h.count(`SELECT count(*) FROM events WHERE run_id = ?`, runID)
	if code, _, errs := h.hook(map[string]string{"OTAG_RUN_ID": runID, run.TokenEnv: "forged"}, `{}`, "fake", "turn_end"); code != 0 || !strings.Contains(errs, "FORBIDDEN") && !strings.Contains(errs, "does not belong") {
		t.Fatalf("forged turn_end: %d %q", code, errs)
	}
	if h.count(`SELECT count(*) FROM events WHERE run_id = ?`, runID) != before {
		t.Fatal("forged event recorded")
	}
	if _, out, _ := h.hook(env, `{}`, "nosuch", "PreToolUse"); out != "" {
		t.Fatalf("unknown harness answers %q", out)
	}
	if n := h.count(`SELECT count(*) FROM raw_hook_payloads WHERE run_id = 'ghost' AND event_id IS NULL`); n != 1 {
		t.Fatalf("ghost payload rows %d", n)
	}
	// Unknown harness, broken payload, unusable database, missing args: all exit 0.
	for name, c := range map[string]struct {
		env     map[string]string
		payload string
		args    []string
	}{
		"harness":  {env, `{}`, []string{"nope", "x"}},
		"payload":  {env, `{not json`, []string{"fake", "turn_start"}},
		"database": {map[string]string{"OTAG_DB": h.root, "OTAG_RUN_ID": runID}, `{}`, []string{"fake", "pre_tool"}}, "usage": {env, ``, []string{"fake"}},
	} {
		code, out, errs := h.hook(c.env, c.payload, c.args...)
		if code != 0 || errs == "" {
			t.Errorf("%s: exit %d stderr %q", name, code, errs)
		}
		if name == "database" && !strings.Contains(out, `"decision":"deny"`) {
			t.Errorf("database: output %q", out)
		}
	}
	if _, err := os.Stat(filepath.Join(h.env["OTAG_DATA_DIR"], "hook-errors.log")); err != nil {
		t.Errorf("hook log: %v", err)
	}
	// Without OTAG_RUN_ID the run is found through the harness session id.
	code, _, errs = h.hook(map[string]string{"OTAG_RUN_ID": "", run.TokenEnv: h.token(runID)}, `{"session_id":"fake-`+runID+`"}`, "fake", "question")
	if code != 0 || errs != "" || h.show(runID).Status != run.Waiting {
		t.Fatalf("session fallback: %d %q %s", code, errs, h.show(runID).Status)
	}

	// task me, as the child sees it.
	h.env["OTAG_RUN_ID"], h.env[run.TokenEnv] = runID, h.token(runID)
	// Inside a run the CLI itself refuses coordinator commands (a script, not a shell
	// line the guard saw); read-only views stay open.
	for _, args := range [][]string{{"task", "cancel", id}, {"run", "stop", runID}, {"sync"}, {"--project", "beta", "task", "add", "x"}} {
		if code, e := h.json(args...); code == 0 || e.Error.Code != "FORBIDDEN" {
			t.Fatalf("child %v: %d %+v", args, code, e.Error)
		}
	}
	if code, e := h.json("task", "list"); code != 0 {
		t.Fatalf("child task list: %+v", e.Error)
	}
	me := h.ok("task", "me")
	for _, want := range []string{id, "Report numbers.", "Never push", ".env*", `otag done "summary"`, h.same} {
		if !strings.Contains(me, want) {
			t.Errorf("task me lacks %q:\n%s", want, me)
		}
	}
	_, e := h.json("task", "me")
	var mej run.Me
	if _ = json.Unmarshal(e.Data, &mej); mej.Task == nil || mej.Run == nil || len(mej.Commands) != len(run.ChildCommands) || len(mej.Rules) == 0 {
		t.Fatalf("task me json: %s", e.Data)
	}
	// done while a question is open; the project has a review profile.
	_, e = h.json("done", "found it")
	var dr run.DoneResult
	_ = json.Unmarshal(e.Data, &dr)
	if dr.TaskStatus != "review" || dr.Commit != gitIn(t, h.same, "rev-parse", "HEAD") || dr.CommitsAhead != 0 ||
		dr.ReviewTask == "" || len(dr.Closed) != 1 {
		t.Fatalf("done: %s", e.Data)
	}
	// The linked review task: ready, review profile, review_of edge.
	_, e = h.json("task", "show", dr.ReviewTask)
	var rt struct {
		Title, Status, Profile, Body string
		Deps                         []struct {
			Kind   string
			TaskID string `json:"task_id"`
		} `json:"deps"`
	}
	_ = json.Unmarshal(e.Data, &rt)
	if rt.Title != "Review: Investigate" || rt.Status != "ready" || rt.Profile != "review" ||
		len(rt.Deps) != 1 || rt.Deps[0].Kind != "review_of" || rt.Deps[0].TaskID != id || !strings.Contains(rt.Body, "found it") {
		t.Fatalf("review task: %s", e.Data)
	}
	delete(h.env, "OTAG_RUN_ID")
	// Late hooks of an ended run are recorded without changing it; a late session_start
	// does not reopen a session.
	send("turn_start", "{}", "late")
	send("session_start", `{"session_id":"late-session"}`, "late2")
	if r := h.show(runID); r.Status != run.Ended || r.EndedReason != run.ReasonDone || r.Session.Status != "ended" {
		t.Fatalf("late event changed run: %s %+v", r.Status, r.Session)
	}
	if code, e := h.json("task", "me"); code == 0 || e.Error.Code != "NO_RUN" {
		t.Fatalf("task me without run: %d %+v", code, e.Error)
	}

	// Latency of the bridge as harness hooks call it: a fresh process per event.
	var times []time.Duration
	envList := os.Environ()
	for k, v := range h.env {
		envList = append(envList, k+"="+v)
	}
	envList = append(envList, "OTAG_RUN_ID="+runID)
	for i := 0; i < 40; i++ {
		cmd := exec.Command(otagBin, "event", "fake", "activity", "--event-id", fmt.Sprintf("lat-%d", i))
		cmd.Env, cmd.Stdin = envList, strings.NewReader(`{"tool":"Bash"}`)
		t0 := time.Now()
		if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("event process: %v %q", err, out)
		}
		times = append(times, time.Since(t0))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("otag event latency over %d calls: p50 %v p90 %v max %v", len(times), times[len(times)/2],
		times[len(times)*9/10], times[len(times)-1])
}

// Stop on a live pane, a pane that was killed behind otag's back, and a retry.
func TestRunStopDeadPaneRetry(t *testing.T) {
	h := newRunHarness(t)
	ctx := context.Background()
	a := h.addTask("alpha", "A")
	b := h.addTask("alpha", "B")
	ra := h.start(a, "--profile", "manual").Run
	rb := h.start(b, "--profile", "manual").Run

	// Killed behind otag's back: status reports it dead, stop ends it as crashed.
	if err := h.tmux.KillWindow(ctx, ra.Session.TmuxPane); err != nil {
		t.Fatal(err)
	}
	_, e := h.json("status")
	var st statusData
	_ = json.Unmarshal(e.Data, &st)
	if st.Dead != 1 || len(st.Runs) != 2 {
		t.Fatalf("status: %s", e.Data)
	}
	for _, r := range st.Runs {
		alive := r.Live != nil && r.Live.State == tmux.Alive
		if (r.ID == ra.ID) == alive {
			t.Fatalf("liveness of %s: %+v", r.ID, r.Live)
		}
	}
	if out := h.ok("status"); !strings.Contains(out, "DEAD (gone") {
		t.Fatalf("status text:\n%s", out)
	}
	if r := h.show(ra.ID); r.Live == nil || r.Live.State != tmux.Gone {
		t.Fatalf("show live: %+v", r.Live)
	}
	_, e = h.json("run", "stop", a)
	var stop run.StopResult
	_ = json.Unmarshal(e.Data, &stop)
	if stop.Run.EndedReason != run.ReasonCrashed || stop.Before.State != tmux.Gone || h.taskStatus(a) != "ready" {
		t.Fatalf("stop dead: %s", e.Data)
	}

	// Live pane: stopped, window killed, task back to ready.
	_, e = h.json("run", "stop", rb.ID, "--reason", "wrong approach")
	_ = json.Unmarshal(e.Data, &stop)
	if stop.Run.EndedReason != run.ReasonStopped || len(stop.Killed) != 1 || h.taskStatus(b) != "ready" {
		t.Fatalf("stop live: %s", e.Data)
	}
	if l := h.tmux.Check(ctx, rb.Session.TmuxPane, rb.Session.PID); l.State != tmux.Gone {
		t.Fatalf("window survived stop: %+v", l)
	}

	// Retry: attempt 2 reuses the task's worktree and branch.
	_ = os.WriteFile(filepath.Join(rb.Workspace.Path, "keep.txt"), []byte("x"), 0o644)
	r2 := h.start(b, "--profile", "manual").Run
	if r2.ID != b+"-r2" || r2.Attempt != 2 || r2.RetryOf != rb.ID || r2.Workspace.Path != rb.Workspace.Path ||
		r2.Workspace.BaseSHA != rb.Workspace.BaseSHA {
		t.Fatalf("retry: %+v %+v", r2, r2.Workspace)
	}
	if _, err := os.Stat(filepath.Join(r2.Workspace.Path, "keep.txt")); err != nil {
		t.Fatal("worktree not reused")
	}
	if code, e := h.json("run", "start", b, "--no-attach"); code == 0 || e.Error.Code != "INVALID_TRANSITION" {
		t.Fatalf("second start of a running task: %d %+v", code, e.Error)
	}
	out := h.ok("--project", "alpha", "run", "list")
	if !strings.Contains(out, r2.ID) || !strings.Contains(out, "ended (crashed)") {
		t.Fatalf("run list:\n%s", out)
	}
	if out := h.ok("run", "attach", r2.ID, "--print"); !strings.Contains(out, "attach-session -t '=otag-alpha'") {
		t.Fatalf("attach: %s", out)
	}

	// The project moves on and the worktree is deleted by hand: attempt 3 recreates it
	// from the surviving branch and keeps the task's original base.
	h.ok("run", "stop", r2.ID)
	gitIn(t, h.repo, "commit", "-q", "--allow-empty", "-m", "main moves on")
	_ = os.RemoveAll(r2.Workspace.Path)
	r3 := h.start(b, "--profile", "manual").Run
	if r3.Workspace.BaseSHA != rb.Workspace.BaseSHA || r3.Workspace.BaseSHA == gitIn(t, h.repo, "rev-parse", "HEAD") {
		t.Fatalf("base after re-create: %s, want %s", r3.Workspace.BaseSHA, rb.Workspace.BaseSHA)
	}
	if _, err := os.Stat(filepath.Join(r3.Workspace.Path, "keep.txt")); err == nil {
		t.Fatal("uncommitted file survived a re-created worktree?")
	}
}

// `otag run stop` while `otag run start` is still launching (waiting for a sidecar):
// the launch must not leave windows behind for a run that has ended.
func TestRunStopDuringLaunch(t *testing.T) {
	h := newRunHarness(t)
	ctx := context.Background()
	id := h.addTask("alpha", "Stuck")
	type startRes struct {
		code int
		e    env
	}
	done := make(chan startRes, 1)
	go func() {
		var out, errb bytes.Buffer
		code := Main(ctx, []string{"--json", "run", "start", id, "--no-attach", "--profile", "stuck"}, Env{
			Stdout: &out, Stderr: &errb, Cwd: h.root, Getenv: func(k string) string { return h.env[k] }})
		var e env
		_ = json.Unmarshal(out.Bytes(), &e)
		done <- startRes{code, e}
	}()
	// Wait until the sidecar window is recorded, then stop.
	h.waitRun(id, "sidecar recorded", func(r run.Run) bool {
		return r.Session != nil && len(r.Session.AuxPanes) == 1
	})
	_, e := h.json("run", "stop", id)
	var stop run.StopResult
	_ = json.Unmarshal(e.Data, &stop)
	if stop.Run == nil || stop.Run.EndedReason != run.ReasonStopped || len(stop.Killed) != 1 {
		t.Fatalf("stop: %s", e.Data)
	}
	var res startRes
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("run start did not return")
	}
	if res.code == 0 {
		t.Fatalf("start succeeded after stop: %s", res.e.Data)
	}
	panes, err := h.tmux.Panes(ctx)
	if err != nil || len(panes) != 0 {
		t.Fatalf("windows left behind: %v %v", panes, err)
	}
	if r := h.show(id); r.EndedReason != run.ReasonStopped || h.taskStatus(id) != "ready" {
		t.Fatalf("after race: %s task %s", r.EndedReason, h.taskStatus(id))
	}
}

// Trust dialog, server process, a harness that exits on its own, and a failed launch.
func TestRunTrustServerExitAndLaunchFailure(t *testing.T) {
	h := newRunHarness(t)
	id := h.addTask("alpha", "Trusty")
	res := h.start(id, "--profile", "trusty")
	if res.Trust != "accepted" || !res.FirstSeen {
		t.Fatalf("trust: %+v", res)
	}
	s := res.Run.Session
	if len(s.AuxPanes) != 2 || s.AuxPanes[0].Role != "server" || s.AuxPanes[1].Role != "sidecar" || s.ServerURL != "fake://"+res.Run.ID {
		t.Fatalf("server/sidecar: %+v", s)
	}
	// The sidecar fed the bridge in-process.
	if n := h.count(`SELECT count(*) FROM events WHERE run_id = ? AND kind = 'hook.activity' AND data LIKE '%sidecar:hello%'`, res.Run.ID); n != 1 {
		t.Fatalf("sidecar events: %d", n)
	}
	_, e := h.json("run", "stop", id)
	var stop run.StopResult
	_ = json.Unmarshal(e.Data, &stop)
	if len(stop.Killed) != 3 {
		t.Fatalf("stop killed %v", stop.Killed)
	}

	// The harness exits with status 3 without done: the pane stays, reported as exited.
	c := h.addTask("alpha", "Crashy")
	rc := h.start(c, "--profile", "crashy").Run
	r := h.waitRun(rc.ID, "exited pane", func(r run.Run) bool {
		return r.Live != nil && r.Live.State == tmux.Exited && r.Live.ExitStatus != nil
	})
	if !r.Dead() || r.Live.ExitStatus == nil || *r.Live.ExitStatus != 3 || r.Status != run.Running {
		t.Fatalf("exited: %+v status %s", r.Live, r.Status)
	}
	_, e = h.json("run", "stop", c)
	_ = json.Unmarshal(e.Data, &stop)
	if stop.Run.EndedReason != run.ReasonCrashed || !strings.Contains(stop.Run.Error, "exit status 3") {
		t.Fatalf("stop exited: %s", e.Data)
	}

	// session_end without done: the run ends as exited, the task is requeued, and no
	// window of the run (server, sidecar, pane) keeps running. The hook comes from the
	// run's own pane, which is closed last.
	x := h.addTask("alpha", "Exits")
	rx := h.start(x, "--profile", "served").Run
	h.hook(map[string]string{"OTAG_RUN_ID": rx.ID, run.TokenEnv: h.token(rx.ID), "TMUX_PANE": rx.Session.TmuxPane}, `{"reason":"exit"}`, "fake", "session_end")
	if r := h.show(rx.ID); r.EndedReason != run.ReasonExited || h.taskStatus(x) != "ready" {
		t.Fatalf("session_end: %s %s", r.EndedReason, h.taskStatus(x))
	}
	windows := append([]run.AuxPane{{Name: "main", Pane: rx.Session.TmuxPane, PID: rx.Session.PID}}, rx.Session.AuxPanes...)
	if len(windows) != 3 {
		t.Fatalf("windows %+v", windows)
	}
	for _, w := range windows {
		if l := h.tmux.Check(context.Background(), w.Pane, w.PID); l.State != tmux.Gone {
			t.Errorf("%s window still %s after the run exited", w.Name, l.State)
		}
	}
	h.ok("run", "stop", rx.ID)

	// Launch failure: the project is not a git repository.
	plain := filepath.Join(h.root, "plain")
	_ = os.MkdirAll(plain, 0o755)
	h.write("projects/gamma.toml", "path = \""+plain+"\"\nprefix = \"ga\"\ndefault_profile = \"manual\"\n")
	h.ok("sync")
	g := h.addTask("gamma", "No repo")
	code, e := h.json("run", "start", g, "--no-attach")
	if code == 0 || e.Error.Code != "UNAVAILABLE" {
		t.Fatalf("launch failure: %d %+v", code, e.Error)
	}
	if r := h.show(g); r.EndedReason != run.ReasonLaunchFailed || h.taskStatus(g) != "ready" {
		t.Fatalf("after launch failure: %s task %s", r.EndedReason, h.taskStatus(g))
	}
	var n int
	_ = h.db().QueryRow(`SELECT count(*) FROM runs WHERE status <> 'ended'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d runs still open", n)
	}
}
