package run

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/task"
)

// Prime kinds: who `otag prime` talks to.
const (
	PrimeChild       = "child"
	PrimeReview      = "review"
	PrimeCoordinator = "coordinator"
	PrimeProject     = "project" // outside a run, inside a registered project
	PrimeNone        = "none"    // outside a run and any project
)

// primeTextMax bounds the task body and extra prompt in the prime text; primeClipRoom
// is what clip's "...[N bytes clipped]" note needs after a cut.
const (
	primeTextMax  = 4000
	primeClipRoom = 64
)

// primeMsgLimit bounds the pending messages `otag prime` lists.
const primeMsgLimit = 10

// PrimeInput selects the orientation: a verified run (RunID), else the project found
// from the working directory (ProjectID, 0 = none).
type PrimeInput struct {
	RunID     string
	ProjectID int64
}

// PrimeMsg is a pending message: id and headline only (prime never marks it read).
type PrimeMsg struct {
	ID       int64  `json:"id"`
	Headline string `json:"headline"`
}

// PrimeRun is a live run in a project summary.
type PrimeRun struct {
	ID     string `json:"id"`
	Task   string `json:"task,omitempty"`
	Status string `json:"status"`
}

// PrimeInfo is the data behind `otag prime`.
type PrimeInfo struct {
	Kind     string     `json:"kind"`
	Run      *Run       `json:"run,omitempty"`
	Task     *task.Task `json:"task,omitempty"`
	ReviewOf *task.Task `json:"review_of,omitempty"`
	Rules    []string   `json:"rules,omitempty"`
	Commands []Command  `json:"commands,omitempty"`
	// Rejected is the last review rejection of the task (a retry after a review).
	Rejected string `json:"rejected,omitempty"`
	// Reopened is why the task was reopened before this run (`otag task reopen`).
	Reopened string     `json:"reopened,omitempty"`
	Messages []PrimeMsg `json:"messages"`
	// Project summary (coordinator, project): counts by status, live runs, needs.
	Project  string         `json:"project,omitempty"`
	Counts   map[string]int `json:"counts,omitempty"`
	Live     []PrimeRun     `json:"live_runs,omitempty"`
	Needs    int            `json:"needs,omitempty"`
	Projects []string       `json:"projects,omitempty"` // PrimeNone: registered projects
}

// primeExtra are the child commands beyond ChildCommands that prime also lists.
var primeExtra = []Command{
	{"otag inbox", "read your messages (marks them read)"},
	{`otag reply <id> "text"`, "answer a question sent to you"},
	{`otag send coord "text"`, "message the coordinator"},
	{"otag context | otag status", "your identity; the project overview"},
}

// BuildPrime gathers the orientation for in. The caller has verified the run token.
func BuildPrime(ctx context.Context, q db.Querier, in PrimeInput) (*PrimeInfo, error) {
	p := &PrimeInfo{Messages: []PrimeMsg{}}
	if in.RunID != "" {
		r, err := Get(ctx, q, in.RunID)
		if err != nil {
			return nil, err
		}
		p.Run, p.Project = r, r.Project
		rd, err := msg.ReaderFor(ctx, q, r.ID)
		if err != nil {
			return nil, err
		}
		if err := p.messages(ctx, q, rd); err != nil {
			return nil, err
		}
		if r.Role == msg.RoleCoordinator {
			p.Kind = PrimeCoordinator
			return p, p.summary(ctx, q, r.ProjectID)
		}
		me, err := TaskMe(ctx, q, r.ID)
		if err != nil {
			return nil, err
		}
		p.Task, p.Rules, p.Commands = me.Task, me.Rules, append(append([]Command{}, me.Commands...), primeExtra...)
		p.Kind = PrimeChild
		if r.Role == "review" {
			p.Kind = PrimeReview
			for _, d := range me.Task.Deps {
				if d.Kind == "review_of" {
					if p.ReviewOf, err = task.Get(ctx, q, d.TaskID); err != nil {
						return nil, err
					}
				}
			}
			if p.ReviewOf != nil {
				// A reviewer ends with a verdict, not with otag done.
				p.Rules = ReviewRules(p.Rules, p.ReviewOf.ID)
				p.Commands = append(reviewCommands(p.ReviewOf.ID), primeExtra...)
			}
			return p, nil
		}
		err = q.QueryRowContext(ctx, `SELECT coalesce((SELECT reason FROM reviews WHERE reviewed_task_id = ?
			AND verdict = 'reject' AND reviewed_run_id <> ? ORDER BY id DESC LIMIT 1), '')`, r.TaskID, r.ID).Scan(&p.Rejected)
		if err != nil {
			return nil, err
		}
		p.Reopened, err = task.ReopenReason(ctx, q, r.TaskID, r.RetryOf)
		return p, err
	}
	if in.ProjectID != 0 {
		p.Kind = PrimeProject
		if err := q.QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, in.ProjectID).Scan(&p.Project); err != nil {
			return nil, err
		}
		if err := p.messages(ctx, q, msg.Reader{ProjectID: in.ProjectID}); err != nil { // the human's inbox
			return nil, err
		}
		return p, p.summary(ctx, q, in.ProjectID)
	}
	p.Kind = PrimeNone
	if err := p.messages(ctx, q, msg.Reader{}); err != nil {
		return nil, err
	}
	ps, err := registry.ListProjects(ctx, q)
	for _, pr := range ps {
		p.Projects = append(p.Projects, pr.Name)
	}
	return p, err
}

func (p *PrimeInfo) messages(ctx context.Context, q db.Querier, rd msg.Reader) error {
	items, err := msg.Inbox(ctx, q, rd, false, primeMsgLimit)
	for _, it := range items {
		p.Messages = append(p.Messages, PrimeMsg{ID: it.ID, Headline: msg.Headline(it.Message)})
	}
	return err
}

// summary fills the project counts, live runs and needs.
func (p *PrimeInfo) summary(ctx context.Context, q db.Querier, projectID int64) error {
	p.Counts = map[string]int{}
	rows, err := q.QueryContext(ctx, `SELECT status, count(*) FROM tasks WHERE project_id = ? GROUP BY status`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			_ = rows.Close()
			return err
		}
		p.Counts[s] = n
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	runs, err := List(ctx, q, Filter{ProjectID: projectID, Active: true})
	if err != nil {
		return err
	}
	for _, r := range runs {
		p.Live = append(p.Live, PrimeRun{ID: r.ID, Task: r.TaskID, Status: r.Status})
	}
	n, err := msg.NeedsOf(ctx, q, projectID)
	if err != nil {
		return err
	}
	p.Needs = len(n.Questions) + len(n.Deliveries) + len(n.Blocked) + len(n.Idle) + len(n.Answers)
	return nil
}

// Text renders the orientation (aim: under 60 lines).
func (p *PrimeInfo) Text() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	switch p.Kind {
	case PrimeChild, PrimeReview:
		r, t := p.Run, p.Task
		what := "child"
		if p.Kind == PrimeReview {
			what = "review"
		}
		w("You are a otag %s agent: run %s, task %s, project %s.", what, r.ID, t.ID, r.Project)
		if ws := r.Workspace; ws != nil {
			where := "workspace " + ws.Path
			if ws.Branch != "" {
				where += " (branch " + ws.Branch + ")"
			}
			w("%s", where)
		}
		w("")
		w("Task %s: %s", t.ID, t.Title)
		if s := strings.TrimSpace(t.Body); s != "" {
			w("\n%s", clip(s, primeTextMax)) // `otag task me` has the rest
		}
		if s := strings.TrimSpace(t.ExtraPrompt); s != "" {
			w("\n%s", clip(s, primeTextMax))
		}
		if p.Rejected != "" {
			w("\nA previous attempt was rejected in review: %s", clip(oneLine(p.Rejected), priorClip))
		}
		if p.Reopened != "" {
			w("\nThe task was reopened after it was done: %s", clip(oneLine(p.Reopened), priorClip))
		}
		if p.ReviewOf != nil {
			if ws := r.Workspace; ws != nil && ws.Mode == "worktree" && ws.Branch == "" {
				w("\nYou review task %s: %s. Your worktree is a detached checkout of the commit under review (head %s);",
					p.ReviewOf.ID, p.ReviewOf.Title, short(ws.BaseSHA))
				w("the task body has its base and the diff command. Inspect it, then run")
			} else {
				w("\nYou review task %s: %s. Inspect its branch read-only (git log/diff against its base), then run", p.ReviewOf.ID, p.ReviewOf.Title)
			}
			w("`otag review %s --accept \"<note>\"` or `otag review %s --reject \"<reason>\"`.", p.ReviewOf.ID, p.ReviewOf.ID)
		}
		w("\nRules:")
		for _, rule := range p.Rules {
			w("- %s", rule)
		}
		w("\notag commands:")
		for _, c := range p.Commands {
			w("  %-26s %s", c.Cmd, c.Use)
		}
	case PrimeCoordinator:
		w("You are the otag coordinator of project %s (run %s).", p.Project, p.Run.ID)
		for _, l := range coordGuide() {
			w("- %s", l)
		}
		p.summaryText(w)
	case PrimeProject:
		w("You are outside a otag run, in project %s. Coordinate AI child agents with the `otag` CLI.", p.Project)
		w("- `otag coord %s` starts or attaches the project's coordinator; or plan yourself:", p.Project)
		for _, l := range coordGuide()[:5] {
			w("- %s", l)
		}
		p.summaryText(w)
	default:
		w("You are outside a otag run and outside any registered project.")
		if len(p.Projects) == 0 {
			w("No projects are registered (`otag sync` reads the TOML registry).")
		} else {
			w("Registered projects: %s. cd into one (or pass --project) to see its summary.", strings.Join(p.Projects, ", "))
		}
		w("Core commands: `otag status`, `otag task list|add|show`, `otag run start <task>`, `otag needs`, `otag inbox`, `otag coord <project>`.")
	}
	if len(p.Messages) == 0 {
		w("\nNo unread messages.")
	} else {
		w("\nUnread messages (read them with `otag inbox`):")
		for _, m := range p.Messages {
			w("  #%d %s", m.ID, m.Headline)
		}
	}
	return b.String()
}

func (p *PrimeInfo) summaryText(w func(string, ...any)) {
	var parts []string
	for _, s := range task.Statuses {
		if n := p.Counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s, n))
		}
	}
	if len(parts) == 0 {
		parts = []string{"no tasks"}
	}
	w("\nProject %s: tasks: %s.", p.Project, strings.Join(parts, ", "))
	if len(p.Live) == 0 {
		w("No live runs.")
	}
	for _, r := range p.Live {
		w("  live run %s (task %s) %s", r.ID, r.Task, r.Status)
	}
	if p.Needs > 0 {
		w("%d item(s) need you: `otag needs`.", p.Needs)
	}
}

// primeNote opens the orientation a hook or message re-injects after a compaction.
const primeNote = "otag re-orientation (your context was compacted or resumed):\n\n"

// primeMessage queues the orientation as a system message to run runID; the
// compaction event's id makes a repeated hook a no-op.
func primeMessage(ctx context.Context, tx *sql.Tx, runID, eventID string, a msg.Actor) error {
	var projectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM runs WHERE id = ?`, runID).Scan(&projectID); err != nil {
		return err
	}
	p, err := BuildPrime(ctx, tx, PrimeInput{RunID: runID})
	if err != nil {
		return err
	}
	in := msg.SendInput{ProjectID: projectID, Kind: msg.KindInfo, From: msg.Sender{Kind: msg.SenderSystem, Actor: "otag"},
		// clip's note follows the cut text: leave room so the body stays within the limit.
		Body: clip(primeNote+p.Text(), msg.MaxBody-primeClipRoom), To: []msg.Recipient{{Kind: msg.ToRun, Ref: runID}}}
	if eventID != "" {
		in.ClientMsgID = "prime:" + runID + ":" + eventID
	}
	m, _, err := msg.Send(ctx, tx, in, a)
	if err != nil {
		return err
	}
	// Strictly bound to this run: without a task link it does not follow the task to
	// a resumed or retry run (the identity in it would be the old run's); the sweep
	// expires it once this run has gone.
	_, err = tx.ExecContext(ctx, `UPDATE message_deliveries SET task_id = NULL WHERE message_id = ?`, m.ID)
	return err
}
