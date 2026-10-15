package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
)

// otagBin is built once: the fake harness reports its steps with `otag event`.
var otagBin string

func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "otag-daemon-bin-")
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

type testEnv struct {
	t       *testing.T
	ctx     context.Context
	db      *db.DB
	m       *run.Manager
	d       *Daemon
	clock   time.Time
	project int64
	shown   []string // desktop notifications
	mu      sync.Mutex
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setup creates a git project "alpha" (prefix al) with the given registry, synced, a
// private tmux server and a daemon with a controllable clock.
func setup(t *testing.T, registryTOML string) *testEnv {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	repo, cfgDir := filepath.Join(root, "alpha"), filepath.Join(root, "cfg")
	_ = os.MkdirAll(repo, 0o755)
	_ = os.MkdirAll(filepath.Join(cfgDir, "projects"), 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.name", "tester")
	git(t, repo, "config", "user.email", "tester@example.com")
	git(t, repo, "config", "commit.gpgsign", "false")
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644)
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-q", "-m", "init")
	_ = os.WriteFile(filepath.Join(cfgDir, "registry.toml"), []byte(registryTOML), 0o644)
	_ = os.WriteFile(filepath.Join(cfgDir, "projects", "alpha.toml"), []byte("path = \""+repo+"\"\nprefix = \"al\"\n"), 0o644)

	socket := fmt.Sprintf("otag-dtest-%d-%d", os.Getpid(), time.Now().UnixNano()%1e9)
	env := map[string]string{
		"HOME": root, "PATH": os.Getenv("PATH"), "OTAG_DB": filepath.Join(root, "otag.db"), "OTAG_CONFIG_DIR": cfgDir,
		"OTAG_DATA_DIR": filepath.Join(root, "data"), "OTAG_TMUX_SOCKET": socket, "OTAG_TMUX_CONF": os.DevNull,
		"OTAG_BIN": otagBin,
	}
	getenv := func(k string) string { return env[k] }
	d, err := db.Open(ctx, env["OTAG_DB"])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	cfg, err := registry.Load(cfgDir, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Tx(ctx, func(tx *sql.Tx) error { _, err := registry.Sync(ctx, tx, cfg, "test"); return err }); err != nil {
		t.Fatal(err)
	}
	tc := tmux.New(socket, os.DevNull)
	t.Cleanup(func() { _ = tc.KillServer(context.Background()) })

	e := &testEnv{t: t, ctx: ctx, db: d, clock: time.Now()}
	_ = d.QueryRow(`SELECT id FROM projects WHERE name = 'alpha'`).Scan(&e.project)
	e.m = &run.Manager{DB: d, Adapters: adapter.NewRegistry(fake.New()), Tmux: tc, Getenv: getenv, Actor: Actor}
	conf := DefaultConfig()
	conf.DeadGrace = 0
	e.d = &Daemon{DB: d, Runs: e.m, Config: conf, Now: func() time.Time { return e.clock },
		Desktop: func(_ context.Context, title, _ string) error {
			e.mu.Lock()
			e.shown = append(e.shown, title)
			e.mu.Unlock()
			return nil
		}}
	return e
}

func (e *testEnv) addTask(title, profile string) string {
	e.t.Helper()
	var id string
	err := e.db.Tx(e.ctx, func(tx *sql.Tx) error {
		tk, err := task.Add(e.ctx, tx, task.AddInput{ProjectID: e.project, Prefix: "al", Title: title, Profile: profile},
			task.Actor{Name: "user:test"})
		if err == nil {
			id = tk.ID
		}
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *testEnv) start(taskID string) *run.Run {
	e.t.Helper()
	res, err := e.m.Start(e.ctx, run.StartInput{TaskID: taskID})
	if err != nil {
		e.t.Fatalf("start %s: %v", taskID, err)
	}
	return res.Run
}

func (e *testEnv) get(id string) *run.Run {
	e.t.Helper()
	r, err := run.Get(e.ctx, e.db, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// waitFor polls cond (real time) for up to 15s; tick runs a daemon tick per poll.
func (e *testEnv) waitFor(what string, tick bool, cond func() bool) {
	e.t.Helper()
	for i := 0; i < 300; i++ {
		if tick {
			e.d.Tick(e.ctx)
		}
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("timeout waiting for %s", what)
}

func (e *testEnv) notifications(kind string) []notify.Notification {
	e.t.Helper()
	all, err := notify.List(e.ctx, e.db, notify.Filter{All: true, Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []notify.Notification
	for _, n := range all {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func (e *testEnv) taskStatus(id string) (status, reason string) {
	e.t.Helper()
	tk, err := task.Get(e.ctx, e.db, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk.Status, tk.StatusReason
}

const fakeAccounts = `
[accounts.f]
harness = "fake"
[accounts.g]
harness = "fake"
[accounts.h]
harness = "fake"
`

func TestDeadPaneIsCrashed(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.crashy]
account = "f"
args = ["start", "busy", "exit:3"]
`)
	e.d.Config.DeadGrace = time.Minute
	tk := e.addTask("crash", "crashy")
	r := e.start(tk)
	e.waitFor("pane exit", false, func() bool {
		return e.m.Tmux.Check(e.ctx, r.Session.TmuxPane, r.Session.PID).State == tmux.Exited &&
			e.get(r.ID).FirstActivityAt != ""
	})
	e.d.Tick(e.ctx) // dead, but within the grace period
	if got := e.get(r.ID); got.Status == run.Ended {
		t.Fatalf("reaped within the grace period: %s", got.EndedReason)
	}
	e.clock = e.clock.Add(2 * time.Minute)
	e.d.Tick(e.ctx)
	got := e.get(r.ID)
	if got.Status != run.Ended || got.EndedReason != run.ReasonCrashed || got.TaskStatus != task.Ready {
		t.Fatalf("after grace: %s %s task %s", got.Status, got.EndedReason, got.TaskStatus)
	}
	var exit sql.NullInt64
	_ = e.db.QueryRow(`SELECT exit_code FROM runs WHERE id = ?`, r.ID).Scan(&exit)
	if exit.Int64 != 3 {
		t.Fatalf("exit code %v", exit)
	}
	if l := e.m.Tmux.Check(e.ctx, r.Session.TmuxPane, 0); l.State != tmux.Gone {
		t.Fatalf("window not cleaned up: %+v", l)
	}
	ns := e.notifications("run.crashed")
	if len(ns) != 1 || ns[0].RunID != r.ID || ns[0].SentAt == "" {
		t.Fatalf("notifications %+v", ns)
	}
	if len(e.shown) != 1 || !strings.Contains(e.shown[0], "crashed") {
		t.Fatalf("desktop %v", e.shown)
	}
	// crashed is not a fallback reason by default: the task stays ready.
	e.d.Tick(e.ctx)
	if s, _ := e.taskStatus(tk); s != task.Ready {
		t.Fatalf("task %s", s)
	}
}

// A harness that dies before its first event did not launch: launch_failed (cause
// harness), and the daemon falls back to the next profile.
func TestStartupDeathFallsBack(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.broken]
account = "f"
args = ["exit:127"]
fallback = ["ok"]
[profiles.ok]
account = "g"
args = ["start", "wait"]
`)
	tk := e.addTask("t", "broken")
	r := e.start(tk)
	e.waitFor("fallback run", true, func() bool {
		var n int
		_ = e.db.QueryRow(`SELECT count(*) FROM runs WHERE task_id = ?`, tk).Scan(&n)
		return n == 2
	})
	got := e.get(r.ID)
	if got.EndedReason != run.ReasonLaunchFailed {
		t.Fatalf("first run %s %s", got.Status, got.EndedReason)
	}
	r2 := e.get(tk + "-r2")
	if r2.Profile != "ok" || r2.FallbackOrigin != "broken" || r2.RetryOf != r.ID || r2.Attempt != 2 {
		t.Fatalf("fallback run %+v", r2)
	}
	if len(e.notifications("run.launch_failed")) != 1 || len(e.notifications("run.fallback")) != 1 {
		t.Fatal("missing notifications")
	}
}

func TestStallStopsRun(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.slow]
account = "f"
args = ["start", "busy", "wait"]
`)
	tk := e.addTask("slow", "slow")
	r := e.start(tk)
	e.waitFor("running", false, func() bool { return e.get(r.ID).Status == run.Running })
	e.clock = time.Now().Add(21 * time.Minute)
	e.d.Config.StallAfterHarness = map[string]time.Duration{"fake": time.Hour}
	e.d.Tick(e.ctx)
	if got := e.get(r.ID); got.Status != run.Running {
		t.Fatalf("per-harness limit ignored: %s %s", got.Status, got.EndedReason)
	}
	e.d.Config.StallAfterHarness = nil
	e.d.Tick(e.ctx)
	got := e.get(r.ID)
	if got.EndedReason != run.ReasonStalled || got.TaskStatus != task.Ready || !strings.Contains(got.Error, "no event for") {
		t.Fatalf("after stall: %s %s %q task %s", got.Status, got.EndedReason, got.Error, got.TaskStatus)
	}
	if l := e.m.Tmux.Check(e.ctx, r.Session.TmuxPane, 0); l.State != tmux.Gone {
		t.Fatalf("pane not killed: %+v", l)
	}
	if len(e.notifications("run.stalled")) != 1 {
		t.Fatal("no stall notification")
	}
}

// A run waiting for an answer is not stalled; it gets one reminder per wait.
func TestWaitingIsUnansweredNotStalled(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.asker]
account = "f"
args = ["start", "busy", "ask", "wait"]
`)
	tk := e.addTask("ask", "asker")
	r := e.start(tk)
	e.waitFor("waiting", false, func() bool { return e.get(r.ID).Status == run.Waiting })
	e.d.Tick(e.ctx)
	if len(e.notifications("run.unanswered")) != 0 {
		t.Fatal("reminder too early")
	}
	e.clock = time.Now().Add(11 * time.Minute)
	e.d.Tick(e.ctx)
	e.d.Tick(e.ctx)
	ns := e.notifications("run.unanswered")
	if len(ns) != 1 || !strings.Contains(ns[0].Body, "Which color?") {
		t.Fatalf("reminders %+v", ns)
	}
	if got := e.get(r.ID); got.Status != run.Waiting {
		t.Fatalf("waiting run changed: %s %s", got.Status, got.EndedReason)
	}
}

// quota → fallback on the next available profile, recorded as a chain; after max
// attempts the task is blocked. The fake harness's limits are per model (like a
// LiteLLM route): the quota end cools only the profile, so another profile of the same
// account is still a fallback, while a profile on a cooling account is skipped.
func TestQuotaFallbackChain(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.main]
account = "f"
args = ["start", "busy", "quota", "wait"]
fallback = ["samef", "second", "third"]
[profiles.samef]
account = "f"
args = ["start", "busy", "quota", "wait"]
[profiles.second]
account = "g"
args = ["start", "wait"]
[profiles.third]
account = "h"
args = ["start", "busy", "quota", "wait"]
`)
	tk := e.addTask("work", "main")
	r1 := e.start(tk)
	ended := func(id string) func() bool {
		return func() bool { r := e.get(id); return r.Status == run.Ended }
	}
	e.waitFor("r1 quota", false, ended(r1.ID))
	if got := e.get(r1.ID); got.EndedReason != run.ReasonQuota || got.TaskStatus != task.Ready {
		t.Fatalf("r1 %s task %s", got.EndedReason, got.TaskStatus)
	}
	// The hook closed the pane of the run that ran out of quota.
	e.waitFor("r1 pane gone", false, func() bool {
		return e.m.Tmux.Check(e.ctx, r1.Session.TmuxPane, 0).State == tmux.Gone
	})
	if ok, why := run.ProfileAvailable(e.ctx, e.db, profileID(t, e.db, "main")); ok || !strings.Contains(why, "profile main cooling down") {
		t.Fatalf("main after its quota end: %v %s", ok, why)
	}
	if ok, why := run.ProfileAvailable(e.ctx, e.db, profileID(t, e.db, "samef")); !ok {
		t.Fatalf("a per-model limit blocked the other profile of the account: %s", why)
	}
	// Account g is out of quota (e.g. a statusLine showed an exhausted window).
	var g int64
	_ = e.db.QueryRow(`SELECT id FROM accounts WHERE name = 'g'`).Scan(&g)
	if err := run.SetCooldown(e.ctx, e.db, g, 0, time.Now().Add(time.Hour), "test limit"); err != nil {
		t.Fatal(err)
	}

	e.d.Tick(e.ctx)
	r2 := e.get(tk + "-r2")
	if r2.Profile != "samef" || r2.FallbackOrigin != "main" || r2.RetryOf != r1.ID {
		t.Fatalf("r2 %+v", r2)
	}
	e.waitFor("r2 quota", false, ended(r2.ID))
	e.d.Tick(e.ctx)
	r3 := e.get(tk + "-r3")
	if r3.Profile != "third" || r3.FallbackOrigin != "main" || r3.RetryOf != r2.ID {
		t.Fatalf("r3 %+v", r3)
	}
	var data string
	_ = e.db.QueryRow(`SELECT data FROM events WHERE dedupe_key = ?`, "fallback:"+r2.ID).Scan(&data)
	if !strings.Contains(data, "second: account g cooling down") {
		t.Fatalf("decision %s", data)
	}
	e.waitFor("r3 quota", false, ended(r3.ID))
	e.d.Tick(e.ctx)
	status, reason := e.taskStatus(tk)
	if status != task.Blocked || !strings.Contains(reason, "3 attempt(s) (main > samef > third)") {
		t.Fatalf("task %s %q", status, reason)
	}
	if n := count(t, e.db, `SELECT count(*) FROM runs WHERE task_id = ?`, tk); n != 3 {
		t.Fatalf("%d runs", n)
	}
	if len(e.notifications("run.fallback")) != 2 || len(e.notifications("task.blocked")) != 1 {
		t.Fatal("notifications")
	}
}

func TestNoAvailableFallbackBlocks(t *testing.T) {
	e := setup(t, fakeAccounts+`
[profiles.solo]
account = "f"
args = ["start", "busy", "quota", "wait"]
fallback = ["samef"]
[profiles.samef]
account = "f"
args = ["start", "wait"]
`)
	tk := e.addTask("work", "solo")
	r := e.start(tk)
	e.waitFor("quota", false, func() bool { return e.get(r.ID).Status == run.Ended })
	var f int64
	_ = e.db.QueryRow(`SELECT id FROM accounts WHERE name = 'f'`).Scan(&f)
	if err := run.SetCooldown(e.ctx, e.db, f, 0, time.Now().Add(time.Hour), "test limit"); err != nil {
		t.Fatal(err)
	}
	e.d.Tick(e.ctx)
	status, reason := e.taskStatus(tk)
	if status != task.Blocked || !strings.Contains(reason, "no fallback of solo is available") ||
		!strings.Contains(reason, "samef: account f cooling down") {
		t.Fatalf("task %s %q", status, reason)
	}
}

// Collectors run on the first round, then at most once per quota_interval; an error
// does not stop the others.
func TestCollectorsRunPerQuotaInterval(t *testing.T) {
	e := setup(t, fakeAccounts)
	c, failing := &countingCollector{}, &countingCollector{err: errors.New("boom")}
	e.d.Collectors = []QuotaCollector{failing, c}
	e.d.Config.QuotaInterval = time.Minute
	e.d.Collect(e.ctx)
	e.clock = e.clock.Add(30 * time.Second)
	e.d.Collect(e.ctx)
	if c.n != 1 || failing.n != 1 {
		t.Fatalf("collectors ran %d/%d times within the interval", c.n, failing.n)
	}
	e.clock = e.clock.Add(31 * time.Second)
	e.d.Collect(e.ctx)
	if c.n != 2 {
		t.Fatalf("collector ran %d times after the interval", c.n)
	}
}

type countingCollector struct {
	n   int
	err error
}

func (c *countingCollector) Collect(context.Context) error { c.n++; return c.err }

func TestDeliverNotifications(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "otag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	var shown []string
	dm := &Daemon{DB: d, Config: DefaultConfig(), Desktop: func(_ context.Context, title, body string) error {
		shown = append(shown, title+"|"+body)
		if strings.Contains(title, "fail") {
			return errors.New("no osascript")
		}
		return nil
	}}
	add := func(title string) int64 {
		id, err := notify.Add(ctx, d, notify.New{Kind: "test", Title: title, Body: "b"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	add("one")
	add("fail")
	old := add("old")
	_, _ = d.Exec(`UPDATE notifications SET created_at = ? WHERE id = ?`,
		time.Now().Add(-time.Hour).UTC().Format(db.TimeFormat), old)
	if err := dm.deliver(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(shown, ",") != "one|b,fail|b" {
		t.Fatalf("shown %v", shown)
	}
	if n := count(t, d, `SELECT count(*) FROM notifications WHERE sent_at IS NULL`); n != 0 {
		t.Fatalf("%d unsent", n)
	}
	if n := count(t, d, `SELECT count(*) FROM notifications WHERE channel = 'desktop'`); n != 1 {
		t.Fatalf("%d via desktop", n)
	}
	dm.Config.DesktopNotify = false
	add("quiet")
	_ = dm.deliver(ctx)
	if len(shown) != 2 {
		t.Fatalf("shown while disabled: %v", shown)
	}
	if n, _ := notify.Unread(ctx, d); n != 4 {
		t.Fatalf("unread %d", n)
	}
	if n, _ := notify.Ack(ctx, d, []int64{old}); n != 1 {
		t.Fatalf("ack %d", n)
	}
	if n, _ := notify.Ack(ctx, d, nil); n != 3 {
		t.Fatalf("ack all %d", n)
	}
}

func count(t *testing.T, d *db.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func profileID(t *testing.T, d *db.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(`SELECT id FROM profiles WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
