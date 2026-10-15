// Package tuitest builds isolated otag environments for the TUI's tests: a temporary
// database, config and data dir, a private tmux socket, and the fake harness. Nothing
// touches the user's otag directories or tmux servers.
package tuitest

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

	"github.com/nesiler/otag/internal/cli"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/tmux"
)

// BuildOtag builds the otag binary into a temporary directory (fake harness panes call
// `otag event`); call it from TestMain and remove the directory afterwards.
func BuildOtag() (bin, dir string, err error) {
	if dir, err = os.MkdirTemp("", "otag-tui-bin-"); err != nil {
		return "", "", err
	}
	bin = filepath.Join(dir, "otag")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/nesiler/otag/cmd/otag").CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("build otag: %v\n%s", err, out)
	}
	pc := filepath.Join(dir, "pastecheck")
	if out, err := exec.Command("go", "build", "-o", pc, "github.com/nesiler/otag/internal/tui/tuitest/pastecheck").CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("build pastecheck: %v\n%s", err, out)
	}
	return bin, dir, nil
}

// PasteThreshold is how close to the last text an Enter must come for pastecheck
// (the profile "typist") to take it as part of a paste. The TUI's Enter gate
// (typing.EnterGap, 150 ms) must clear it.
const PasteThreshold = "100ms"

// Env is one isolated otag environment with projects "alpha" (git repo, worktree
// mode) and "beta" (git repo, same mode, review profile "manual": a done task goes to
// review). The fake harness runs no coordinator.
type Env struct {
	T           testing.TB
	Root, Bin   string
	Vars        map[string]string // OTAG_* and HOME, PATH, USER
	Tmux        *tmux.Client      // the agents' server
	Alpha, Beta string            // project dirs
	PasteLog    string            // the texts the profile "typist" got submitted, one per line
}

// Registry is the fake-harness registry of an Env.
const Registry = `
[accounts.f]
harness = "fake"
[profiles.manual]
account = "f"
args = ["start", "wait"]
[profiles.finish]
account = "f"
args = ["start", "busy", "commit", "done:finished", "wait"]
[profiles.crashy]
account = "f"
args = ["start", "busy", "exit:3"]
[profiles.asker]
account = "f"
args = ["start", "busy", "ask:Which color?", "idle", "inbox:60", "wait"]
`

// New sets up an Env; tmux is required (the test is skipped without it).
func New(t testing.TB, bin string) *Env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	e := &Env{T: t, Root: root, Bin: bin, Alpha: filepath.Join(root, "alpha"), Beta: filepath.Join(root, "beta")}
	socket := fmt.Sprintf("otag-test-%d-%d", os.Getpid(), time.Now().UnixNano()%1e9)
	e.Vars = map[string]string{
		"HOME":             root,
		"USER":             "tester",
		"PATH":             os.Getenv("PATH"),
		"OTAG_DB":          filepath.Join(root, "otag.db"),
		"OTAG_CONFIG_DIR":  filepath.Join(root, "cfg"),
		"OTAG_DATA_DIR":    filepath.Join(root, "data"),
		"OTAG_TMUX_SOCKET": socket,
		"OTAG_TMUX_CONF":   os.DevNull,
		"OTAG_BIN":         bin,
	}
	e.Tmux = tmux.New(socket, os.DevNull)
	t.Cleanup(func() { _ = e.Tmux.KillServer(context.Background()) })
	for _, d := range []string{filepath.Join(root, "cfg", "projects"), e.Alpha, e.Beta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{e.Alpha, e.Beta} {
		e.git(d, "init", "-q", "-b", "main")
		e.git(d, "config", "user.name", "tester")
		e.git(d, "config", "user.email", "tester@example.com")
		e.git(d, "config", "commit.gpgsign", "false")
		e.git(d, "commit", "-q", "--allow-empty", "-m", "init")
	}
	// typist: an input box with paste detection (pastecheck), built next to otag; its
	// submits go to PasteLog.
	e.PasteLog = filepath.Join(root, "paste.log")
	e.write("registry.toml", Registry+fmt.Sprintf("[profiles.typist]\naccount = \"f\"\nargs = [\"start\", \"idle\", %q, \"wait\"]\n",
		"run:"+filepath.Join(filepath.Dir(bin), "pastecheck")+" "+PasteThreshold+" "+e.PasteLog))
	e.write("projects/alpha.toml", fmt.Sprintf("path = %q\nprefix = \"al\"\ndefault_profile = \"manual\"\n"+
		"guard = \"strict\"\n", e.Alpha))
	e.write("projects/beta.toml", fmt.Sprintf("path = %q\nprefix = \"be\"\ndefault_profile = \"manual\"\n"+
		"workspace = \"same\"\nreview_profile = \"manual\"\n", e.Beta))
	e.OK("sync")
	return e
}

func (e *Env) git(dir string, args ...string) {
	e.T.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		e.T.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (e *Env) write(name, body string) {
	e.T.Helper()
	if err := os.WriteFile(filepath.Join(e.Vars["OTAG_CONFIG_DIR"], name), []byte(body), 0o644); err != nil {
		e.T.Fatal(err)
	}
}

// Getenv reads the Env's variables.
func (e *Env) Getenv(k string) string { return e.Vars[k] }

// Environ is the Env's variables as KEY=value, for a child process.
func (e *Env) Environ() []string {
	var out []string
	for k, v := range e.Vars {
		out = append(out, k+"="+v)
	}
	return out
}

// Run runs a otag command in-process and returns its exit code, stdout and stderr.
func (e *Env) Run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := cli.Main(context.Background(), args, cli.Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv: e.Getenv, Environ: e.Environ, Cwd: e.Root})
	return code, out.String(), errb.String()
}

// OK runs a otag command that must succeed and returns its stdout.
func (e *Env) OK(args ...string) string {
	e.T.Helper()
	code, out, errs := e.Run(args...)
	if code != 0 {
		e.T.Fatalf("otag %v: exit %d\n%s%s", args, code, out, errs)
	}
	return out
}

// JSON runs a otag command with --json that must succeed and decodes its data into v.
func (e *Env) JSON(v any, args ...string) {
	e.T.Helper()
	out := e.OK(append([]string{"--json"}, args...)...)
	var env struct{ Data json.RawMessage }
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		e.T.Fatalf("otag %v: %v\n%s", args, err, out)
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		e.T.Fatalf("otag %v: %v\n%s", args, err, env.Data)
	}
}

// AddTask adds a task to project and returns its id.
func (e *Env) AddTask(project, title string, extra ...string) string {
	e.T.Helper()
	var t struct{ ID string }
	e.JSON(&t, append([]string{"--project", project, "task", "add"}, append(extra, "--", title)...)...)
	return t.ID
}

// Start starts a run of task (no attach) and returns the run id.
func (e *Env) Start(taskID string, extra ...string) string {
	e.T.Helper()
	var r struct {
		Run struct{ ID string }
	}
	e.JSON(&r, append([]string{"run", "start", taskID, "--no-attach"}, extra...)...)
	if r.Run.ID == "" {
		e.T.Fatalf("run start %s: no run id", taskID)
	}
	return r.Run.ID
}

// WaitRun polls `otag run show ref` until ok accepts the decoded run (status, ended
// reason, task status), or fails the test after 10 s.
func (e *Env) WaitRun(ref string, ok func(status, ended, taskStatus string) bool) {
	e.T.Helper()
	var last string
	for i := 0; i < 200; i++ {
		var r struct {
			Status      string `json:"status"`
			EndedReason string `json:"ended_reason"`
			TaskStatus  string `json:"task_status"`
		}
		if code, out, _ := e.Run("--json", "run", "show", ref); code == 0 {
			var env struct{ Data json.RawMessage }
			if json.Unmarshal([]byte(out), &env) == nil && json.Unmarshal(env.Data, &r) == nil {
				if ok(r.Status, r.EndedReason, r.TaskStatus) {
					return
				}
				last = fmt.Sprintf("%+v", r)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.T.Fatalf("run %s: timeout, last %s", ref, last)
}

// Task returns a task as `otag task show --json` has it.
func (e *Env) Task(id string) (t struct {
	ID, Status, Title string
	Version           int64
}) {
	e.T.Helper()
	e.JSON(&t, "task", "show", id)
	return t
}

// WaitTask polls the task's status until ok accepts it, or fails the test after 10 s.
func (e *Env) WaitTask(id string, ok func(status string) bool) {
	e.T.Helper()
	last := ""
	for i := 0; i < 200; i++ {
		if last = e.Task(id).Status; ok(last) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.T.Fatalf("task %s: timeout, status %s", id, last)
}

// Runs lists the runs of a task, newest first.
func (e *Env) Runs(taskID string) (rs []struct {
	ID, Status  string
	EndedReason string `json:"ended_reason"`
	Workspace   *struct{ Path string }
}) {
	e.T.Helper()
	e.JSON(&rs, "run", "list", "--task", taskID)
	return rs
}

// DB opens the Env's database (closed at cleanup).
func (e *Env) DB() *db.DB {
	e.T.Helper()
	d, err := db.Open(context.Background(), e.Vars["OTAG_DB"])
	if err != nil {
		e.T.Fatal(err)
	}
	e.T.Cleanup(func() { _ = d.Close() })
	return d
}
