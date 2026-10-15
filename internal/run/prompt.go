package run

import (
	"context"
	"fmt"
	"strings"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/workspace"
)

// Command is a otag command a child may use.
type Command struct {
	Cmd string `json:"cmd"`
	Use string `json:"use"`
}

// ChildCommands are the commands a child agent is told about.
var ChildCommands = []Command{
	{"otag task me", "show your task, workspace and rules again"},
	{`otag done "summary"`, "finish: commit first; records the summary and HEAD commit, task goes to review/done"},
	{`otag fail "reason"`, "give up: the task goes back to the queue for another run"},
	{`otag ask "question" [--park]`, "ask the coordinator, then end your turn; the answer is your next message " +
		"(--park: otag closes the session and resumes it with the answer)"},
}

// askLine is the one line of the first prompt about asking.
const askLine = "If you need a decision, run `otag ask \"<question>\"` and end your turn (add --park for a long wait).\n"

// Rules returns the rules a child must follow in workspace ws.
func Rules(mode, path, branch string, deny []string) []string {
	r := []string{"Work only inside " + path + "; do not read or write other projects."}
	switch {
	case mode == workspace.Worktree && branch == "":
		r = append(r, "This worktree is a detached checkout of the commit under review; do not create, move or delete branches.")
	case mode == workspace.Worktree:
		r = append(r, "Commit your changes on branch "+branch+"; do not switch branches, rebase onto other work or merge.")
	default:
		r = append(r, "You work in the project directory itself: commit only your own changes.")
	}
	r = append(r, "Never push (git push) and never force anything; merging is done by the coordinator (or the human).")
	if len(deny) > 0 {
		r = append(r, "Never modify paths matching: "+strings.Join(deny, ", ")+".")
	}
	r = append(r, "When finished, commit and run `otag done \"<one-line summary>\"`; if you cannot finish, run `otag fail \"<reason>\"`.")
	return r
}

// ReviewRules turns a child's rules (Rules) into a reviewer's: the last rule, which
// tells a child to commit and finish with otag done, becomes the review verdict.
func ReviewRules(rules []string, reviewOf string) []string {
	rules = append([]string{}, rules...)
	rules[len(rules)-1] = fmt.Sprintf("Do not change the reviewed branch. When finished, run `otag review %s --accept \"<note>\"` "+
		"or `otag review %s --reject \"<reason>\"`; if you cannot review, run `otag fail \"<reason>\"`.", reviewOf, reviewOf)
	return rules
}

// reviewCommands are the commands of a review run: ChildCommands without done, plus
// the verdict.
func reviewCommands(reviewOf string) []Command {
	out := []Command{{fmt.Sprintf(`otag review %s --accept|--reject "note"`, reviewOf), "record your verdict; this finishes the review"}}
	for _, c := range ChildCommands {
		if !strings.HasPrefix(c.Cmd, "otag done") {
			out = append(out, c)
		}
	}
	return out
}

// prompts builds the first user message and the system instructions of a run.
func prompts(p *plan, ws workspace.Result) (prompt, instructions string) {
	if p.role == "coordinator" {
		return coordPrompts(p)
	}
	var in strings.Builder
	what := "child"
	if p.role == "review" {
		what = "review"
	}
	fmt.Fprintf(&in, "You are a otag %s agent: run %s, task %s, project %s.\n", what, p.runID, p.taskID, p.project)
	rules := Rules(ws.Mode, ws.Path, ws.Branch, p.deny)
	if p.role == "review" {
		rules = ReviewRules(rules, p.reviewOf)
	}
	if childPushes(p.push, p.role, ws.Branch) {
		rules = append(rules[:len(rules)-1], pushRule(p.taskID), rules[len(rules)-1])
	}
	for _, r := range rules {
		fmt.Fprintf(&in, "- %s\n", r)
	}

	var b strings.Builder
	if p.resumeSID != "" {
		// The resumed session has the task already; it only gets what it waited for.
		fmt.Fprintf(&b, "otag resumed this session (run %s, after %s was parked). Your question has an answer:\n\n", p.runID, p.resumeOf)
		b.WriteString(p.messages.Text(msg.DefaultMaxChars))
		b.WriteString("\n\nContinue the task. When you are finished, commit and run `otag done \"<one-line summary>\"`.\n")
		return b.String(), in.String()
	}
	fmt.Fprintf(&b, "Task %s: %s\n", p.taskID, p.title)
	if s := strings.TrimSpace(p.body); s != "" {
		fmt.Fprintf(&b, "\n%s\n", s)
	}
	if s := strings.TrimSpace(p.extraPrompt); s != "" {
		fmt.Fprintf(&b, "\n%s\n", s)
	}
	if p.prior != nil {
		fmt.Fprintf(&b, "\n%s", p.prior.String())
	}
	if p.messages != nil {
		fmt.Fprintf(&b, "\nMessages for this task:\n\n%s\n", p.messages.Text(msg.DefaultMaxChars))
	}
	if p.reopened != "" {
		fmt.Fprintf(&b, "\nThe task was reopened: %s\n", clip(oneLine(p.reopened), priorClip))
	}
	if p.role == "review" {
		if ws.Mode == workspace.Worktree && ws.Branch == "" && ws.BaseSHA != "" {
			fmt.Fprintf(&b, "\nYour worktree is a detached checkout of the commit under review (head %s).", ws.BaseSHA)
			if p.reviewBase != "" {
				fmt.Fprintf(&b, " Its changes: `git diff %s...%s` (base %s).", p.reviewBase, ws.BaseSHA, p.reviewBase)
			}
			b.WriteString(" ")
		} else {
			b.WriteString("\nInspect the reviewed branch read-only (git log/diff against its base). ")
		}
		fmt.Fprintf(&b, "Then run `otag review %s --accept \"<note>\"` or `otag review %s --reject \"<reason>\"`. ",
			p.reviewOf, p.reviewOf)
	} else {
		b.WriteString("\nWhen you are finished, commit your work and run `otag done \"<one-line summary>\"`. ")
	}
	b.WriteString("If you cannot finish, run `otag fail \"<reason>\"`. `otag task me` shows the task again.\n")
	b.WriteString(askLine)
	return b.String(), in.String()
}

// prior is the short summary of a task's previous attempt that a retry or fallback run
// gets in its first prompt: how that run ended, its summary, a review rejection, and
// the state of the workspace it leaves (read at launch).
type prior struct {
	run, profile, reason, errText, summary, rejected string
	reopened                                         string // the task was reopened after this run: why
	// workspace, filled by readWorkspace
	read               bool
	mode, branch, base string
	commits            int
	dirty, noCount     bool
}

// loadPrior reads run runID (the task's latest run) for the next attempt's prompt.
func loadPrior(ctx context.Context, q db.Querier, runID string) (*prior, error) {
	pr := &prior{run: runID}
	var taskID string
	err := q.QueryRowContext(ctx, `
SELECT p.name, coalesce(r.ended_reason, ''), coalesce(r.error, ''),
       coalesce((SELECT value FROM run_outputs WHERE run_id = r.id AND kind = 'summary' ORDER BY id DESC LIMIT 1), ''),
       coalesce((SELECT reason FROM reviews WHERE reviewed_run_id = r.id AND verdict = 'reject' ORDER BY id DESC LIMIT 1), ''),
       coalesce(r.task_id, '')
FROM runs r JOIN profiles p ON p.id = r.profile_id WHERE r.id = ?`, runID).
		Scan(&pr.profile, &pr.reason, &pr.errText, &pr.summary, &pr.rejected, &taskID)
	if err != nil || taskID == "" {
		return pr, err
	}
	pr.reopened, err = task.ReopenReason(ctx, q, taskID, runID)
	return pr, err
}

// readWorkspace records the commits on the workspace since its base and whether it has
// uncommitted changes.
func (pr *prior) readWorkspace(ctx context.Context, ws workspace.Result) {
	if ws.Path == "" {
		return
	}
	_, dirty, err := workspace.Head(ctx, ws.Path)
	if err != nil {
		return
	}
	pr.read, pr.dirty, pr.mode, pr.branch, pr.base = true, dirty, ws.Mode, ws.Branch, ws.BaseSHA
	if n, err := workspace.CommitsSince(ctx, ws.Path, ws.BaseSHA); err == nil && ws.BaseSHA != "" {
		pr.commits = n
	} else {
		pr.noCount = true
	}
}

// priorClip bounds each free-text part of the summary (about 2 KiB in all).
const priorClip = 500

func (pr *prior) String() string {
	var b strings.Builder
	reason := pr.reason
	if reason == "" {
		reason = "without a reason"
	}
	fmt.Fprintf(&b, "Previous attempt: run %s (profile %s) ended %q", pr.run, pr.profile, reason)
	if e := oneLine(pr.errText); e != "" {
		fmt.Fprintf(&b, ": %s", clip(e, priorClip))
	}
	b.WriteString("\n")
	if s := oneLine(pr.summary); s != "" {
		fmt.Fprintf(&b, "Its summary: %s\n", clip(s, priorClip))
	}
	if s := oneLine(pr.rejected); s != "" {
		fmt.Fprintf(&b, "Review: rejected: %s\n", clip(s, priorClip))
	}
	if s := oneLine(pr.reopened); s != "" {
		fmt.Fprintf(&b, "The task was reopened after it was done: %s\n", clip(s, priorClip))
	}
	if pr.read {
		where := "The project checkout"
		switch {
		case pr.branch != "":
			where = "Branch " + pr.branch
		case pr.mode == workspace.Worktree:
			where = "The detached review checkout"
		}
		dirty := "no"
		if pr.dirty {
			dirty = "yes (still in the workspace)"
		}
		if pr.noCount {
			fmt.Fprintf(&b, "%s: uncommitted changes: %s\n", where, dirty)
		} else {
			fmt.Fprintf(&b, "%s: %d commit(s) since base %s; uncommitted changes: %s\n", where, pr.commits, short(pr.base), dirty)
		}
	}
	b.WriteString("Continue from the current state of the workspace; do not redo committed work.\n")
	return b.String()
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
