package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/nesiler/otag/internal/adapter/builtin"
	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/identity"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
)

func cmdTask(ctx context.Context, a *app, args []string) (result, error) {
	subs := map[string]func(context.Context, *app, []string) (result, error){
		"add": taskAdd, "list": taskList, "show": taskShow, "edit": taskEdit,
		"cancel": taskCancel, "ready": taskReady, "claim": taskClaim, "block": taskBlock, "me": taskMe,
		"deliver": taskDeliver, "push": taskPush, "reopen": taskReopen,
	}
	if len(args) == 0 {
		return result{}, usageErr("usage: otag task add|list|show|edit|cancel|ready|claim|block|deliver|push|reopen|me")
	}
	fn, ok := subs[args[0]]
	if !ok {
		return result{}, usageErr("unknown task command %q", args[0])
	}
	return fn(ctx, a, args[1:])
}

func taskActor(a *app, id identity.Identity) task.Actor {
	return task.Actor{Name: a.actorName(id), RunID: id.RunID}
}

func oneTask(t *task.Task) result {
	return result{data: t, human: func(w io.Writer) { printTask(w, t) }}
}

func taskAdd(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task add")
	var in task.AddInput
	var meta, blockedBy, related multi
	var metaJSON, parent, reviewOf string
	fs.StringVar(&in.Body, "body", "", "description")
	fs.IntVar(&in.Priority, "priority", 0, "priority, higher first")
	fs.StringVar(&in.Profile, "profile", "", "requested profile")
	fs.BoolVar(&in.Draft, "draft", false, "create as draft instead of ready")
	fs.StringVar(&in.ExternalRef, "ref", "", "external reference (issue URL, ticket id)")
	fs.StringVar(&in.ExtraPrompt, "extra-prompt", "", "text appended to the agent prompt")
	fs.StringVar(&metaJSON, "meta-json", "", "metadata as a JSON object")
	fs.Var(&meta, "meta", "metadata key=value (repeatable)")
	fs.Var(&blockedBy, "blocked-by", "task id that must finish first (repeatable)")
	fs.Var(&related, "related", "related task id (repeatable)")
	fs.StringVar(&parent, "parent", "", "parent task id")
	fs.StringVar(&reviewOf, "review-of", "", "task this one reviews")
	fs.StringVar(&in.Base, "base", "", "ref the task branch starts from (default: the project's base)")
	fs.StringVar(&in.Review, "review", "", "review profile for this task, or none (default: the project's review_profile)")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	in.Title = strings.Join(pos, " ")
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	proj, err := id.RequireProject()
	if err != nil {
		return result{}, err
	}
	in.ProjectID, in.Prefix = proj.ID, proj.Prefix
	if in.Metadata, err = buildMeta(metaJSON, meta); err != nil {
		return result{}, err
	}
	for _, b := range blockedBy {
		in.Deps = append(in.Deps, task.DepRef{Kind: "blocks", On: b})
	}
	for _, r := range related {
		in.Deps = append(in.Deps, task.DepRef{Kind: "related", On: r})
	}
	if parent != "" {
		in.Deps = append(in.Deps, task.DepRef{Kind: "parent", On: parent})
	}
	if reviewOf != "" {
		in.Deps = append(in.Deps, task.DepRef{Kind: "review_of", On: reviewOf})
	}
	t, err := mutate(ctx, a, requestKey("task add", id, fs, pos), func(tx *sql.Tx) (*task.Task, error) {
		return task.Add(ctx, tx, in, taskActor(a, id))
	})
	if err != nil {
		return result{}, err
	}
	return oneTask(t), nil
}

func buildMeta(metaJSON string, kv multi) (json.RawMessage, error) {
	obj := map[string]any{}
	if metaJSON != "" {
		if err := json.Unmarshal([]byte(metaJSON), &obj); err != nil || obj == nil {
			return nil, apperr.New(apperr.Invalid, "--meta-json must be a JSON object")
		}
	}
	for _, p := range kv {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, apperr.New(apperr.Invalid, "--meta wants key=value, got %q", p)
		}
		var parsed any
		if json.Unmarshal([]byte(v), &parsed) == nil {
			obj[k] = parsed
		} else {
			obj[k] = v
		}
	}
	return json.Marshal(obj)
}

func taskList(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task list")
	var status string
	var f task.Filter
	var all bool
	fs.StringVar(&status, "status", "", "comma-separated statuses")
	fs.BoolVar(&all, "all", false, "all projects")
	fs.BoolVar(&f.ReadyOnly, "ready", false, "only ready, unblocked tasks")
	fs.BoolVar(&f.Open, "open", false, "exclude done, delivered and cancelled")
	fs.BoolVar(&f.Deliverable, "deliverable", false, "only done tasks waiting for delivery (no review tasks)")
	fs.IntVar(&f.Limit, "limit", 0, "max rows")
	if _, err := parse(fs, args); err != nil {
		return result{}, err
	}
	if status != "" {
		for _, s := range strings.Split(status, ",") {
			if !task.ValidStatus(s) {
				return result{}, apperr.New(apperr.Invalid, "unknown status %q", s)
			}
			f.Statuses = append(f.Statuses, s)
		}
	}
	return listTasks(ctx, a, f, all)
}

func listTasks(ctx context.Context, a *app, f task.Filter, all bool) (result, error) {
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	if !all {
		proj, err := id.RequireProject()
		if err != nil {
			return result{}, err
		}
		f.ProjectID = proj.ID
	}
	ts, err := task.List(ctx, a.db, f)
	if err != nil {
		return result{}, err
	}
	return result{data: ts, human: func(w io.Writer) { printTasks(w, ts, all) }}, nil
}

func taskShow(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task show")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag task show <id>")
	}
	d, err := a.openDB(ctx)
	if err != nil {
		return result{}, err
	}
	t, err := task.Get(ctx, d, pos[0])
	if err != nil {
		return result{}, err
	}
	return oneTask(t), nil
}

func taskEdit(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task edit")
	var (
		title, body, profile, ref, extra, metaJSON, status, reason string
		base, review                                               string
		priority                                                   int
		version                                                    int64
		meta, addDep, rmDep                                        multi
	)
	fs.StringVar(&title, "title", "", "")
	fs.StringVar(&body, "body", "", "")
	fs.IntVar(&priority, "priority", 0, "")
	fs.StringVar(&profile, "profile", "", "requested profile ('' clears)")
	fs.StringVar(&ref, "ref", "", "external reference")
	fs.StringVar(&extra, "extra-prompt", "", "")
	fs.StringVar(&metaJSON, "meta-json", "", "replace metadata object")
	fs.Var(&meta, "meta", "set metadata key=value (repeatable)")
	fs.Var(&addDep, "add-dep", "kind:task-id (repeatable)")
	fs.Var(&rmDep, "rm-dep", "kind:task-id (repeatable)")
	fs.StringVar(&status, "status", "", "move to status (state machine enforced)")
	fs.StringVar(&reason, "reason", "", "reason for --status")
	fs.StringVar(&base, "base", "", "ref the task branch starts from ('' = the project's; refused once the worktree exists)")
	fs.StringVar(&review, "review", "", "review profile, none, or '' for the project's review_profile")
	fs.Int64Var(&version, "version", 0, "expected task version (CAS)")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag task edit <id> [flags]")
	}
	in := task.EditInput{ExpectVersion: version}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "title":
			in.Title = &title
		case "body":
			in.Body = &body
		case "priority":
			in.Priority = &priority
		case "profile":
			in.Profile = &profile
		case "ref":
			in.ExternalRef = &ref
		case "extra-prompt":
			in.ExtraPrompt = &extra
		case "base":
			in.Base = &base
		case "review":
			in.Review = &review
		}
	})
	if metaJSON != "" {
		in.Metadata = json.RawMessage(metaJSON)
	}
	if len(meta) > 0 {
		in.MetaSet = map[string]string{}
		for _, p := range meta {
			k, v, ok := strings.Cut(p, "=")
			if !ok || k == "" {
				return result{}, apperr.New(apperr.Invalid, "--meta wants key=value, got %q", p)
			}
			in.MetaSet[k] = v
		}
	}
	if in.AddDeps, err = depRefs(addDep); err != nil {
		return result{}, err
	}
	if in.RemoveDeps, err = depRefs(rmDep); err != nil {
		return result{}, err
	}
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	t, err := mutate(ctx, a, requestKey("task edit", id, fs, pos), func(tx *sql.Tx) (*task.Task, error) {
		t, err := task.Edit(ctx, tx, pos[0], in, taskActor(a, id))
		if err != nil || status == "" {
			return t, err
		}
		expect := version
		if t.Version != version {
			expect = 0 // the field edit above already passed the version check
		}
		if t, err = task.Transition(ctx, tx, pos[0], status, reason, expect, taskActor(a, id)); err != nil {
			return nil, err
		}
		if t.Status == task.Done {
			// Done by hand: ready to deliver, like a run's done.
			if err := run.NotifyDone(ctx, tx, t.ID, run.Actor{Name: a.actorName(id)}); err != nil {
				return nil, err
			}
		}
		return t, nil
	})
	if err != nil {
		return result{}, err
	}
	return oneTask(t), nil
}

func depRefs(specs multi) ([]task.DepRef, error) {
	var out []task.DepRef
	for _, s := range specs {
		kind, on, ok := strings.Cut(s, ":")
		if !ok || on == "" {
			return nil, apperr.New(apperr.Invalid, "dependency must be kind:task-id, got %q", s)
		}
		out = append(out, task.DepRef{Kind: kind, On: on})
	}
	return out, nil
}

// taskCancel cancels a task, stopping a run that still works on it (see endTask).
func taskCancel(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task cancel")
	var reason string
	var version int64
	fs.StringVar(&reason, "reason", "", "reason")
	fs.Int64Var(&version, "version", 0, "expected task version (CAS)")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag task cancel <id>")
	}
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	t, err := endTask(ctx, a, id, requestKey("task cancel", id, fs, pos), pos[0], task.Cancelled, reason, version)
	if err != nil {
		return result{}, err
	}
	return oneTask(t), nil
}

// endTask moves a task to cancelled or blocked. A run still working on it is stopped
// with it: the task change and the run's move to stopping commit in one transaction
// (run.StopTaskRun), then the run is stopped like `otag run stop` and ends as stopped.
// If that stop fails, the task change stands and the daemon finishes the stop.
func endTask(ctx context.Context, a *app, id identity.Identity, key opKey, taskID, to, reason string, version int64) (*task.Task, error) {
	why := "task " + taskID + " " + to
	if reason != "" {
		why += ": " + reason
	}
	var runID string
	t, err := mutate(ctx, a, key, func(tx *sql.Tx) (*task.Task, error) {
		t, err := task.Transition(ctx, tx, taskID, to, reason, version, taskActor(a, id))
		if err != nil {
			return nil, err
		}
		runID, err = run.StopTaskRun(ctx, tx, taskID, why, run.Actor{Name: a.actorName(id)})
		return t, err
	})
	if err != nil || runID == "" {
		return t, err
	}
	m := &run.Manager{DB: a.db, Adapters: builtin.Registry(), Tmux: a.tmux(), Getenv: a.env.Getenv,
		Actor: run.Actor{Name: a.actorName(id)}}
	_, err = m.StopWith(ctx, runID, run.StopOptions{Reason: why, EndReason: run.ReasonStopped,
		Expect: &run.Expect{Status: run.Stopping, HeartbeatAt: "*"}})
	if err != nil && !errors.Is(err, run.ErrChanged) {
		return nil, apperr.Retryable(apperr.Unavailable, "task %s is %s, but run %s is not stopped yet (the daemon retries): %v",
			taskID, to, runID, err)
	}
	return task.Get(ctx, a.db, taskID)
}

// taskDeliver records the delivery of a done task and removes its worktrees
// (run.Manager.DeliverTask): otag task deliver <id> [--note n] [--delete-branch] [--force].
// It is safe to run again: on a delivered task it finishes the cleanup.
func taskDeliver(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task deliver")
	var in run.DeliverInput
	fs.StringVar(&in.Note, "note", "", "how it was delivered (kept as the status reason)")
	fs.BoolVar(&in.DeleteBranch, "delete-branch", false, "delete the task branch if the default branch contains it (with --force: also unmerged, e.g. after a squash merge)")
	fs.BoolVar(&in.Force, "force", false, "remove worktrees with uncommitted changes (the changes are lost); with --delete-branch, delete an unmerged branch")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag task deliver <id> [--note n] [--delete-branch] [--force]")
	}
	in.Task = pos[0]
	m, _, err := a.manager(ctx)
	if err != nil {
		return result{}, err
	}
	res, err := m.DeliverTask(ctx, in)
	if res == nil {
		return result{}, err
	}
	return result{data: res, human: func(w io.Writer) { printDelivery(w, res) }}, err
}

func printDelivery(w io.Writer, res *run.DeliverResult) {
	if res.Delivered {
		_, _ = fmt.Fprintf(w, "delivered %s", res.Task)
	} else {
		_, _ = fmt.Fprintf(w, "%s was delivered before; finished its cleanup", res.Task)
	}
	if len(res.ReviewTasks) > 0 {
		_, _ = fmt.Fprintf(w, " (with review task(s) %s)", strings.Join(res.ReviewTasks, ", "))
	}
	_, _ = fmt.Fprintln(w)
	for _, c := range res.Worktrees {
		_, _ = fmt.Fprintf(w, "worktree %s: %s", c.Path, c.Result)
		if c.Error != "" {
			_, _ = fmt.Fprintf(w, ": %s", c.Error)
		}
		_, _ = fmt.Fprintln(w)
	}
	for _, b := range res.Branches {
		switch {
		case b.Deleted:
			_, _ = fmt.Fprintf(w, "branch %s: deleted (merged into %s)\n", b.Branch, res.Target)
		case !b.Exists:
		case b.Unmerged != nil && *b.Unmerged == 0:
			_, _ = fmt.Fprintf(w, "branch %s: kept (merged into %s)\n", b.Branch, res.Target)
		case b.Unmerged != nil:
			_, _ = fmt.Fprintf(w, "branch %s: kept (%d commit(s) not in %s)\n", b.Branch, *b.Unmerged, res.Target)
		default:
			_, _ = fmt.Fprintf(w, "branch %s: kept\n", b.Branch)
		}
	}
	for _, s := range res.Warnings {
		_, _ = fmt.Fprintf(w, "warning: %s\n", s)
	}
}

// taskReady lists ready tasks, or with ids marks draft/blocked tasks ready.
func taskReady(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task ready")
	var all bool
	fs.BoolVar(&all, "all", false, "all projects (listing only)")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) == 0 {
		return listTasks(ctx, a, task.Filter{ReadyOnly: true}, all)
	}
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	ts, err := mutate(ctx, a, requestKey("task ready", id, fs, pos), func(tx *sql.Tx) ([]task.Task, error) {
		var out []task.Task
		for _, tid := range pos {
			t, err := task.Transition(ctx, tx, tid, task.Ready, "", 0, taskActor(a, id))
			if err != nil {
				return nil, err
			}
			out = append(out, *t)
		}
		return out, nil
	})
	if err != nil {
		return result{}, err
	}
	return result{data: ts, human: func(w io.Writer) { printTasks(w, ts, false) }}, nil
}

func taskClaim(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task claim")
	var version int64
	fs.Int64Var(&version, "version", 0, "expected task version (CAS)")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) > 1 {
		return result{}, usageErr("usage: otag task claim [<id>]")
	}
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	var projectID int64
	if len(pos) == 0 {
		proj, err := id.RequireProject()
		if err != nil {
			return result{}, err
		}
		projectID = proj.ID
	}
	t, err := mutate(ctx, a, requestKey("task claim", id, fs, pos), func(tx *sql.Tx) (*task.Task, error) {
		if len(pos) == 0 {
			return task.ClaimNext(ctx, tx, projectID, taskActor(a, id))
		}
		return task.Claim(ctx, tx, pos[0], version, taskActor(a, id))
	})
	if err != nil {
		return result{}, err
	}
	return oneTask(t), nil
}

// taskBlock marks a task blocked (--reason), adds a blocking dependency (--on), or
// clears the blocked status (--clear).
func taskBlock(ctx context.Context, a *app, args []string) (result, error) {
	fs := a.flagSet("task block")
	var reason string
	var on multi
	var clear bool
	var version int64
	fs.StringVar(&reason, "reason", "", "why the task is blocked")
	fs.Var(&on, "on", "task id that blocks this one (repeatable)")
	fs.BoolVar(&clear, "clear", false, "unblock: move back to ready")
	fs.Int64Var(&version, "version", 0, "expected task version (CAS)")
	pos, err := parse(fs, args)
	if err != nil {
		return result{}, err
	}
	if len(pos) != 1 {
		return result{}, usageErr("usage: otag task block <id> [--reason r | --on <id> | --clear]")
	}
	if clear && len(on) > 0 {
		return result{}, usageErr("--clear and --on are exclusive")
	}
	id, err := a.identity(ctx)
	if err != nil {
		return result{}, err
	}
	actor := taskActor(a, id)
	if !clear && len(on) == 0 {
		// Blocked like cancelled: a run working on the task is stopped with it.
		t, err := endTask(ctx, a, id, requestKey("task block", id, fs, pos), pos[0], task.Blocked, reason, version)
		if err != nil {
			return result{}, err
		}
		return oneTask(t), nil
	}
	t, err := mutate(ctx, a, requestKey("task block", id, fs, pos), func(tx *sql.Tx) (*task.Task, error) {
		switch {
		case clear:
			return task.Transition(ctx, tx, pos[0], task.Ready, reason, version, actor)
		case len(on) > 0:
			for i, b := range on {
				expect := version
				if i > 0 {
					expect = 0 // checked by the first edge, same transaction
				}
				if err := task.AddDep(ctx, tx, pos[0], "blocks", b, expect, actor); err != nil {
					return nil, err
				}
			}
			return task.Get(ctx, tx, pos[0])
		}
		return task.Transition(ctx, tx, pos[0], task.Blocked, reason, version, actor)
	})
	if err != nil {
		return result{}, err
	}
	return oneTask(t), nil
}

func printTasks(w io.Writer, ts []task.Task, withProject bool) {
	if len(ts) == 0 {
		_, _ = fmt.Fprintln(w, "no tasks")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if withProject {
		_, _ = fmt.Fprintln(tw, "ID\tPROJECT\tSTATUS\tPRI\tTITLE")
	} else {
		_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tPRI\tTITLE")
	}
	for _, t := range ts {
		if withProject {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", t.ID, t.Project, t.Status, t.Priority, t.Title)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", t.ID, t.Status, t.Priority, t.Title)
		}
	}
	_ = tw.Flush()
}

func printTask(w io.Writer, t *task.Task) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			_, _ = fmt.Fprintf(tw, "%s:\t%s\n", k, v)
		}
	}
	row("id", t.ID)
	row("title", t.Title)
	row("project", t.Project)
	status := t.Status
	if t.StatusReason != "" {
		status += " (" + t.StatusReason + ")"
	}
	row("status", status)
	row("delivered", t.DeliveredAt)
	row("priority", fmt.Sprint(t.Priority))
	row("version", fmt.Sprint(t.Version))
	row("profile", t.Profile)
	row("base", t.Base)
	row("branch", t.Branch)
	switch t.Review {
	case task.ReviewNone:
		row("review", "none (this task is not reviewed)")
	case "":
	default:
		row("review", t.Review+" (overrides the project's review_profile)")
	}
	if p := t.Push; p != nil {
		row("pushed", fmt.Sprintf("%s to %s/%s at %s", short(p.SHA), p.Remote, p.Branch, p.At))
	}
	row("claimed by", t.ClaimedBy)
	row("run", t.CurrentRunID)
	row("ref", t.ExternalRef)
	if string(t.Metadata) != "{}" {
		row("metadata", string(t.Metadata))
	}
	row("created", t.CreatedAt+" by "+t.CreatedBy)
	row("updated", t.UpdatedAt)
	for _, d := range t.Deps {
		row("depends", fmt.Sprintf("%s %s [%s] %s", d.Kind, d.TaskID, d.Status, d.Title))
	}
	for _, d := range t.Dependents {
		row("dependent", fmt.Sprintf("%s <- %s [%s] %s", d.Kind, d.TaskID, d.Status, d.Title))
	}
	_ = tw.Flush()
	if t.Body != "" {
		_, _ = fmt.Fprintf(w, "\n%s\n", t.Body)
	}
	if t.ExtraPrompt != "" {
		_, _ = fmt.Fprintf(w, "\nextra prompt:\n%s\n", t.ExtraPrompt)
	}
}
