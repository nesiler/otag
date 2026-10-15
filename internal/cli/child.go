package cli

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/identity"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/paths"
	"github.com/nesiler/otag/internal/run"
)

// requireRun returns the run the command acts for (--run or OTAG_RUN_ID).
func requireRun(ctx context.Context, a *app, cmd string) (*run.Manager, string, error) {
	m, id, err := a.manager(ctx)
	if err != nil {
		return nil, "", err
	}
	if id.RunID == "" {
		return nil, "", apperr.New(apperr.NoRun, "%s must run inside a otag run (OTAG_RUN_ID) or with --run", cmd)
	}
	return m, id.RunID, nil
}

// requireOwnRun is requireRun for the commands that change a run as that run (done,
// fail): the caller must present the run's OTAG_RUN_TOKEN, so `otag --run other done`
// from a confused child (or from outside the run) is refused. The coordinator stops a
// run with `otag run stop`.
func requireOwnRun(ctx context.Context, a *app, cmd string) (*run.Manager, string, error) {
	m, runID, err := requireRun(ctx, a, cmd)
	if err != nil {
		return nil, "", err
	}
	if err := run.CheckToken(ctx, m.DB, runID, a.env.Getenv(run.TokenEnv)); err != nil {
		return nil, "", err
	}
	return m, runID, nil
}

// caller is who runs a messaging command, verified here and not taken from flags:
// inside a run (OTAG_RUN_ID) the run's OTAG_RUN_TOKEN must match, and the sender is that
// run (a child, a review run or a coordinator run); outside a run it is the human (a
// person, or a plain coordinator session in a terminal).
type caller struct {
	id     identity.Identity
	run    *run.Run // nil for the human
	sender msg.Sender
	reader msg.Reader
	actor  msg.Actor
}

func (c *caller) child() bool { return c.run != nil && c.run.Role != msg.RoleCoordinator }

// projectID is the caller's project, 0 when a human runs outside any project.
func (c *caller) projectID() int64 {
	if c.id.Project == nil {
		return 0
	}
	return c.id.Project.ID
}

// runResidue says why a caller without OTAG_RUN_ID still looks like a run (a child that
// dropped OTAG_RUN_ID must not pass for the human): OTAG_RUN_TOKEN is set, or the working
// directory is inside a otag task worktree. "" = it does not.
func (a *app) runResidue() string {
	if a.env.Getenv(run.TokenEnv) != "" {
		return run.TokenEnv + " is set"
	}
	if a.env.Cwd == "" {
		return ""
	}
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	root := real(filepath.Join(paths.DataDir(a.env.Getenv), "worktrees"))
	if cwd := real(a.env.Cwd); strings.HasPrefix(cwd, root+string(filepath.Separator)) {
		return "the working directory is a otag run workspace"
	}
	return ""
}

// msgCaller resolves and verifies the caller of messaging command cmd. A person may
// not act as a run with --run: senders come from the token, never from flags.
func (a *app) msgCaller(ctx context.Context, cmd string) (*caller, error) {
	runID := a.env.Getenv("OTAG_RUN_ID")
	if runID == "" && a.run != "" {
		return nil, usageErr("otag %s acts as its caller: --run works only inside that run", cmd)
	}
	if why := a.runResidue(); runID == "" && why != "" {
		return nil, apperr.New(apperr.Forbidden, "otag %s without OTAG_RUN_ID, but %s: a run acts only as itself", cmd, why)
	}
	id, err := a.identity(ctx)
	if err != nil {
		return nil, err
	}
	c := &caller{id: id, actor: msg.Actor{Name: a.actorName(id)}}
	if runID == "" {
		c.sender = msg.Sender{Kind: msg.SenderHuman, Actor: c.actor.Name}
		c.reader = msg.Reader{ProjectID: c.projectID()}
		return c, nil
	}
	if err := run.CheckToken(ctx, a.db, runID, a.env.Getenv(run.TokenEnv)); err != nil {
		return nil, err
	}
	if c.run, err = run.Resolve(ctx, a.db, runID); err != nil {
		return nil, err
	}
	c.sender = msg.Sender{Kind: msg.SenderRun, RunID: runID, Actor: c.actor.Name}
	if c.reader, err = msg.ReaderFor(ctx, a.db, runID); err != nil {
		return nil, err
	}
	return c, nil
}

// requireProject returns the caller's project or NO_PROJECT.
func (c *caller) requireProject() (int64, error) {
	p, err := c.id.RequireProject()
	if err != nil {
		return 0, err
	}
	return p.ID, nil
}

// taskMe shows a child its task, workspace, rules and commands.
func taskMe(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task me")
	if pos, err := parse(fs, args); err != nil || len(pos) > 0 {
		if err == nil {
			err = usageErr("usage: otag task me")
		}
		return result{}, err
	}
	m, runID, err := requireRun(ctx, a, "task me")
	if err != nil {
		return result{}, err
	}
	me, err := run.TaskMe(ctx, m.DB, runID)
	if err != nil {
		return result{}, err
	}
	return result{data: me, human: func(w io.Writer) {
		t, r := me.Task, me.Run
		_, _ = fmt.Fprintf(w, "task %s: %s [%s]\n", t.ID, t.Title, t.Status)
		_, _ = fmt.Fprintf(w, "run %s (profile %s, %s), project %s\n", r.ID, r.Profile, r.Harness, r.Project)
		if ws := me.Workspace; ws != nil {
			_, _ = fmt.Fprintf(w, "workspace: %s", ws.Path)
			if ws.Branch != "" {
				_, _ = fmt.Fprintf(w, " (branch %s, base %s)", ws.Branch, short(ws.BaseSHA))
			}
			_, _ = fmt.Fprintln(w)
		}
		if b := strings.TrimSpace(t.Body); b != "" {
			_, _ = fmt.Fprintf(w, "\n%s\n", b)
		}
		if e := strings.TrimSpace(t.ExtraPrompt); e != "" {
			_, _ = fmt.Fprintf(w, "\nextra instructions:\n%s\n", e)
		}
		_, _ = fmt.Fprintln(w, "\nrules:")
		for _, rule := range me.Rules {
			_, _ = fmt.Fprintf(w, "- %s\n", rule)
		}
		_, _ = fmt.Fprintln(w, "\ncommands:")
		for _, c := range me.Commands {
			_, _ = fmt.Fprintf(w, "  %-22s %s\n", c.Cmd, c.Use)
		}
	}}, nil
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func cmdDone(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("done")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) == 0 {
		return result{}, usageErr(`usage: otag done "summary"`)
	}
	m, runID, err := requireOwnRun(ctx, a, "done")
	if err != nil {
		return result{}, err
	}
	// Done closes the run's pane, which is usually where this process runs.
	signal.Ignore(syscall.SIGHUP)
	res, err := m.Done(ctx, runID, strings.Join(pos, " "))
	if err != nil {
		return result{}, err
	}
	return result{data: res, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "run %s done; task %s is %s", res.Run, res.Task, res.TaskStatus)
		if res.Commit != "" {
			_, _ = fmt.Fprintf(w, "; HEAD %s (%d new commit(s))", short(res.Commit), res.CommitsAhead)
		}
		_, _ = fmt.Fprintln(w)
		if res.ReviewTask != "" {
			_, _ = fmt.Fprintf(w, "review task %s created\n", res.ReviewTask)
		}
		if res.Dirty {
			_, _ = fmt.Fprintln(a.env.Stderr, "warning: the workspace has uncommitted changes; they are not part of the result")
		}
	}}, nil
}

func cmdFail(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("fail")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) == 0 {
		return result{}, usageErr(`usage: otag fail "reason"`)
	}
	m, runID, err := requireOwnRun(ctx, a, "fail")
	if err != nil {
		return result{}, err
	}
	signal.Ignore(syscall.SIGHUP) // see cmdDone
	r, err := m.Fail(ctx, runID, strings.Join(pos, " "))
	if err != nil {
		return result{}, err
	}
	return result{data: r, human: func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "run %s failed; task %s is %s\n", r.ID, r.TaskID, r.TaskStatus)
	}}, nil
}
