package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/db"
)

type harness struct {
	t     *testing.T
	root  string
	cfg   string
	env   map[string]string
	cwd   string
	alpha string // project dir
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{t: t, root: root, cfg: filepath.Join(root, "cfg"), alpha: filepath.Join(root, "alpha")}
	h.env = map[string]string{
		"HOME":            root,
		"USER":            "tester",
		"OTAG_DB":         filepath.Join(root, "otag.db"),
		"OTAG_CONFIG_DIR": h.cfg,
	}
	for _, d := range []string{filepath.Join(h.cfg, "projects"), filepath.Join(h.alpha, "src"), filepath.Join(root, "beta")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.write("registry.toml", `
[accounts.c1]
harness = "claude"
[profiles.sonnet]
account = "c1"
model = "sonnet"
tags = ["fast"]
`)
	h.write("projects/alpha.toml", "path = \""+h.alpha+"\"\nprefix = \"al\"\ndefault_profile = \"sonnet\"\n")
	h.write("projects/beta.toml", "path = \""+filepath.Join(root, "beta")+"\"\nprefix = \"be\"\n")
	h.ok("sync")
	return h
}

func (h *harness) write(name, body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.cfg, name), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), args, Env{
		Stdout: &out, Stderr: &errb, Cwd: h.cwd,
		Getenv: func(k string) string { return h.env[k] },
		Environ: func() []string {
			var out []string
			for k, v := range h.env {
				out = append(out, k+"="+v)
			}
			return out
		},
	})
	return code, out.String(), errb.String()
}

func (h *harness) ok(args ...string) string {
	h.t.Helper()
	code, out, errs := h.run(args...)
	if code != 0 {
		h.t.Fatalf("otag %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errs)
	}
	return out
}

type env struct {
	V     int             `json:"v"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func (h *harness) json(args ...string) (int, env) {
	h.t.Helper()
	code, out, _ := h.run(append([]string{"--json"}, args...)...)
	var e env
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		h.t.Fatalf("bad json %q: %v", out, err)
	}
	if e.V != 1 || e.OK != (code == 0) {
		h.t.Fatalf("envelope %+v with exit %d", e, code)
	}
	return code, e
}

func TestRequestIDReplay(t *testing.T) {
	h := newHarness(t)
	h.cwd = h.alpha
	_, first := h.json("task", "add", "Write docs", "--request-id", "req-1", "--priority", "3")
	_, again := h.json("task", "add", "Write docs", "--priority", "3", "--request-id", "req-1")
	if string(first.Data) != string(again.Data) {
		t.Fatalf("replay differs:\n%s\n%s", first.Data, again.Data)
	}
	_, list := h.json("task", "list")
	var tasks []map[string]any
	_ = json.Unmarshal(list.Data, &tasks)
	if len(tasks) != 1 {
		t.Fatalf("replay created a duplicate: %d tasks", len(tasks))
	}

	code, other := h.json("task", "add", "Something else", "--request-id", "req-1")
	if code == 0 || other.Error.Code != "REQUEST_ID_REUSED" {
		t.Fatalf("reuse: %d %+v", code, other.Error)
	}

	// Claim replay returns the original claim instead of CONFLICT.
	var task struct{ ID string }
	_ = json.Unmarshal(first.Data, &task)
	_, c1 := h.json("task", "claim", task.ID, "--request-id", "claim-1")
	code, c2 := h.json("task", "claim", task.ID, "--request-id", "claim-1")
	if code != 0 || string(c1.Data) != string(c2.Data) {
		t.Fatalf("claim replay: %d %s", code, c2.Data)
	}
	code, c3 := h.json("task", "claim", task.ID)
	if code == 0 || c3.Error.Code != "CONFLICT" {
		t.Fatalf("second claim: %d %+v", code, c3.Error)
	}
}

func TestRequestKeyKeepsValueBoundaries(t *testing.T) {
	h := newHarness(t)
	h.cwd = h.alpha
	if code, _ := h.json("task", "add", "X", "--meta", "a=1", "--meta", "b=2", "--request-id", "k"); code != 0 {
		t.Fatal("first add failed")
	}
	code, e := h.json("task", "add", "X", "--meta", "a=1,b=2", "--request-id", "k")
	if code == 0 || e.Error.Code != "REQUEST_ID_REUSED" {
		t.Fatalf("different metadata replayed: %d %s", code, e.Data)
	}
	code, e = h.json("task", "add", "X Y", "--request-id", "k2")
	if code != 0 {
		t.Fatal(e.Error)
	}
	// Positional arguments are hashed as a list, not a joined string.
	if code, e = h.json("task", "add", "X", "Y", "--request-id", "k2"); code == 0 || e.Error.Code != "REQUEST_ID_REUSED" {
		t.Fatalf("args: %d %s", code, e.Data)
	}
}

func TestRunIdentityValidated(t *testing.T) {
	h := newHarness(t)
	h.cwd = h.root
	d, err := db.Open(context.Background(), h.env["OTAG_DB"])
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`INSERT INTO runs (id, project_id, role, profile_id, profile_revision_id, account_id, harness)
		SELECT 'r1', pr.id, 'coordinator', p.id, pv.id, pv.account_id, pv.harness
		FROM projects pr, profiles p JOIN profile_revisions pv ON pv.profile_id = p.id
		WHERE pr.name = 'alpha' AND p.name = 'sonnet'`)
	_ = d.Close()
	if err != nil {
		t.Fatal(err)
	}

	h.env["OTAG_RUN_ID"] = "r1"
	code, e := h.json("context")
	if code != 0 || !strings.Contains(string(e.Data), `"name":"alpha"`) || !strings.Contains(string(e.Data), `"project_source":"run"`) {
		t.Fatalf("run identity: %d %s", code, e.Data)
	}
	if code, _ := h.json("--project", "alpha", "context"); code != 0 {
		t.Fatal("matching --project and env run must resolve")
	}
	if code, e := h.json("--project", "beta", "context"); code == 0 || e.Error.Code != "FORBIDDEN" {
		t.Fatalf("env run of other project: %d %+v", code, e.Error)
	}
	h.env["OTAG_RUN_ID"] = "ghost"
	if code, e := h.json("--project", "alpha", "context"); code == 0 || e.Error.Code != "NOT_FOUND" {
		t.Fatalf("unknown env run with --project: %d %+v", code, e.Error)
	}
}

func TestIdentityResolution(t *testing.T) {
	h := newHarness(t)
	project := func() (int, string, string) {
		code, e := h.json("context")
		if code != 0 {
			return code, e.Error.Code, ""
		}
		var c struct {
			Project *struct{ Name string } `json:"project"`
			Source  string                 `json:"project_source"`
		}
		_ = json.Unmarshal(e.Data, &c)
		if c.Project == nil {
			return code, "", ""
		}
		return code, c.Project.Name, c.Source
	}

	h.cwd = filepath.Join(h.alpha, "src")
	if _, name, src := project(); name != "alpha" || src != "cwd" {
		t.Fatalf("cwd: %s %s", name, src)
	}
	h.env["OTAG_PROJECT"] = "beta"
	if _, name, src := project(); name != "beta" || src != "env" {
		t.Fatalf("env: %s %s", name, src)
	}
	if code, e := h.json("--project", "alpha", "context"); code != 0 || !strings.Contains(string(e.Data), `"project_source":"flag"`) {
		t.Fatalf("flag: %s", e.Data)
	}
	h.env["OTAG_PROJECT"] = "ghost"
	if code, e := h.json("context"); code == 0 || e.Error.Code != "NOT_FOUND" {
		t.Fatalf("unknown project: %+v", e.Error)
	}
	delete(h.env, "OTAG_PROJECT")
	h.env["OTAG_RUN_ID"] = "r-missing"
	if code, e := h.json("context"); code == 0 || e.Error.Code != "NOT_FOUND" {
		t.Fatalf("unknown run: %+v", e.Error)
	}
	delete(h.env, "OTAG_RUN_ID")

	h.cwd = h.root
	if _, name, _ := project(); name != "" {
		t.Fatalf("outside projects resolved %s", name)
	}
	if code, e := h.json("task", "list"); code == 0 || e.Error.Code != "NO_PROJECT" {
		t.Fatalf("no project: %+v", e.Error)
	}

	// Two projects on the same path are ambiguous.
	h.write("projects/gamma.toml", "path = \""+h.alpha+"\"\nprefix = \"ga\"\n")
	h.ok("sync")
	h.cwd = h.alpha
	if code, e := h.json("context"); code == 0 || e.Error.Code != "AMBIGUOUS_IDENTITY" {
		t.Fatalf("ambiguous: %+v", e.Error)
	}
	if code, _ := h.json("--project", "gamma", "context"); code != 0 {
		t.Fatal("explicit project must resolve ambiguity")
	}
}

func TestTaskCommands(t *testing.T) {
	h := newHarness(t)
	h.cwd = h.alpha
	id := func(e env) string {
		var t struct{ ID string }
		_ = json.Unmarshal(e.Data, &t)
		return t.ID
	}
	_, a := h.json("task", "add", "A")
	_, b := h.json("task", "add", "B", "--blocked-by", id(a), "--meta", "k=v", "--ref", "gl#1", "--profile", "sonnet")
	if !strings.Contains(string(b.Data), `"metadata":{"k":"v"}`) || !strings.Contains(string(b.Data), `"profile":"sonnet"`) {
		t.Fatalf("add: %s", b.Data)
	}
	_, ready := h.json("task", "ready")
	if strings.Contains(string(ready.Data), id(b)) || !strings.Contains(string(ready.Data), id(a)) {
		t.Fatalf("ready: %s", ready.Data)
	}
	if code, e := h.json("task", "edit", id(a), "--add-dep", "blocks:"+id(b)); code == 0 || e.Error.Code != "DEPENDENCY_CYCLE" {
		t.Fatalf("cycle: %+v", e.Error)
	}
	if code, e := h.json("task", "edit", id(a), "--status", "done"); code == 0 || e.Error.Code != "INVALID_TRANSITION" {
		t.Fatalf("ready->done: %+v", e.Error)
	}
	h.ok("task", "block", id(a), "--reason", "waiting on API key")
	h.ok("task", "block", id(a), "--clear")
	h.ok("task", "cancel", id(a), "--reason", "obsolete")
	_, ready = h.json("task", "ready")
	if !strings.Contains(string(ready.Data), id(b)) {
		t.Fatalf("cancelled blocker should unblock: %s", ready.Data)
	}
	out := h.ok("task", "show", id(b))
	if !strings.Contains(out, "blocks "+id(a)+" [cancelled]") {
		t.Fatalf("show:\n%s", out)
	}
	if code, _, _ := h.run("task", "bogus"); code != ExitUsage {
		t.Fatalf("usage exit = %d", code)
	}
	_, st := h.json("status")
	if !strings.Contains(string(st.Data), `"cancelled":1`) {
		t.Fatalf("status: %s", st.Data)
	}
	_, profs := h.json("profiles", "--tag", "fast")
	if !strings.Contains(string(profs.Data), `"name":"sonnet"`) {
		t.Fatalf("profiles: %s", profs.Data)
	}
}
