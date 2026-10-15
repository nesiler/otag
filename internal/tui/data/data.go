// Package data reads what the TUI shows: an immutable Snapshot taken in one read
// transaction, a Watcher that notices commits made by any other connection, and pane
// liveness. It never writes.
package data

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/daemon"
	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/notify"
	"github.com/nesiler/otag/internal/quota"
	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
)

// Request says what a Load reads beyond the default set.
type Request struct {
	ShowClosed bool     // also the done, delivered and cancelled tasks
	History    []string // tasks whose every run is loaded (Snapshot.History)
}

// ClosedLimit bounds the closed tasks a snapshot reads.
const ClosedLimit = 200

// Snapshot is what the database held at one moment. It is never changed once built.
type Snapshot struct {
	At       time.Time
	Projects []registry.ProjectRow
	// Open are the tasks not done, delivered or cancelled; Deliverable the done tasks
	// waiting for delivery (no review tasks); Closed, with Request.ShowClosed, the
	// latest ClosedLimit done, delivered and cancelled tasks.
	Open, Deliverable, Closed []task.Task
	// ReviewOf maps a review task to the task it reviews (task_deps kind review_of).
	ReviewOf map[string]string
	// Runs are the active runs (coordinators included) and the newest run of every
	// task in Open, Deliverable and Closed, newest first.
	Runs []run.Run
	// RunCount is the number of runs of each task that has any.
	RunCount map[string]int
	// History holds every run of the tasks in Request.History, newest first.
	History  map[string][]run.Run
	Needs    *msg.Needs
	Asks     []msg.OpenAsk
	Inbox    []msg.Item            // unread messages to the human (peeked, not marked read)
	Notes    []notify.Notification // unread notifications, newest first
	Unread   int                   // unread notifications
	Quota    []quota.Row
	Cooldown []quota.Cooldown
	Profiles []registry.ProfileRow
	// Unavailable maps the profiles new runs may not use now to why.
	Unavailable map[string]string
	Daemon      bool
	// ReadOnly: the database schema is newer than this binary.
	ReadOnly bool
}

// Load reads a Snapshot. Every query runs in one read-only transaction: a plain
// deferred BEGIN (modernc's _txlock=immediate applies only to write transactions), so
// the snapshot is consistent and never takes the write lock. It ends right after the
// queries; no read transaction is held between loads.
func Load(ctx context.Context, d *db.DB, dataDir string, req Request, now time.Time) (*Snapshot, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	s := &Snapshot{At: now, ReviewOf: map[string]string{}, RunCount: map[string]int{}, History: map[string][]run.Run{},
		ReadOnly: d.ReadOnly()}
	if s.Projects, err = registry.ListProjects(ctx, tx); err != nil {
		return nil, err
	}
	if s.Open, err = task.List(ctx, tx, task.Filter{Open: true}); err != nil {
		return nil, err
	}
	if s.Deliverable, err = task.List(ctx, tx, task.Filter{Deliverable: true}); err != nil {
		return nil, err
	}
	if req.ShowClosed {
		if s.Closed, err = task.List(ctx, tx, task.Filter{Statuses: []string{task.Done, task.Delivered, task.Cancelled},
			Recent: true, Limit: ClosedLimit}); err != nil {
			return nil, err
		}
	}
	if err := reviewEdges(ctx, tx, s.ReviewOf); err != nil {
		return nil, err
	}
	if s.Runs, err = run.List(ctx, tx, run.Filter{Active: true, Limit: 1000}); err != nil {
		return nil, err
	}
	if err := newestRuns(ctx, tx, s); err != nil {
		return nil, err
	}
	for _, id := range req.History {
		if s.RunCount[id] == 0 {
			continue
		}
		rs, err := run.List(ctx, tx, run.Filter{TaskID: id, Limit: 1000})
		if err != nil {
			return nil, err
		}
		s.History[id] = rs
	}
	if s.Needs, err = msg.NeedsOf(ctx, tx, 0); err != nil {
		return nil, err
	}
	if s.Asks, err = msg.OpenAsks(ctx, tx, msg.AskFilter{}); err != nil {
		return nil, err
	}
	if s.Inbox, err = msg.Inbox(ctx, tx, msg.Reader{}, false, 50); err != nil {
		return nil, err
	}
	if s.Unread, err = notify.Unread(ctx, tx); err != nil {
		return nil, err
	}
	if s.Notes, err = notify.List(ctx, tx, notify.Filter{}); err != nil {
		return nil, err
	}
	if s.Quota, err = quota.List(ctx, tx, quota.Filter{MaxAge: 7 * 24 * time.Hour}, now); err != nil {
		return nil, err
	}
	if s.Cooldown, err = quota.Cooldowns(ctx, tx, now); err != nil {
		return nil, err
	}
	if s.Profiles, err = registry.ListProfiles(ctx, tx, ""); err != nil {
		return nil, err
	}
	if s.Unavailable, err = unavailable(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.Daemon, _, _ = daemon.Probe(dataDir)
	return s, nil
}

func reviewEdges(ctx context.Context, q db.Querier, into map[string]string) error {
	rows, err := q.QueryContext(ctx, `SELECT task_id, depends_on_id FROM task_deps WHERE kind = 'review_of'`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var review, of string
		if err := rows.Scan(&review, &of); err != nil {
			return err
		}
		into[review] = of
	}
	return rows.Err()
}

// newestRuns counts the runs of every listed task and adds the newest run of each to
// s.Runs when it is not active (an ended run still shows how the task's last try went).
func newestRuns(ctx context.Context, q db.Querier, s *Snapshot) error {
	var ids []any
	for _, l := range [][]task.Task{s.Open, s.Deliverable, s.Closed} {
		for _, t := range l {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `
SELECT r.task_id, count(*),
       (SELECT r2.id FROM runs r2 WHERE r2.task_id = r.task_id ORDER BY r2.attempt DESC, r2.queued_at DESC, r2.id DESC LIMIT 1)
FROM runs r WHERE r.task_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`) GROUP BY r.task_id`, ids...)
	if err != nil {
		return err
	}
	var newest []string
	for rows.Next() {
		var taskID, runID string
		var n int
		if err := rows.Scan(&taskID, &n, &runID); err != nil {
			_ = rows.Close()
			return err
		}
		s.RunCount[taskID] = n
		newest = append(newest, runID)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, r := range s.Runs {
		have[r.ID] = true
	}
	for _, id := range newest {
		if have[id] {
			continue
		}
		r, err := run.Get(ctx, q, id)
		if err != nil {
			return err
		}
		s.Runs = append(s.Runs, *r)
	}
	slices.SortStableFunc(s.Runs, func(a, b run.Run) int {
		if c := strings.Compare(b.QueuedAt, a.QueuedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	return nil
}

// Liveness checks the panes of the active runs with one tmux call (run.CheckLive).
// The result is keyed by run id; the snapshot's runs are not touched.
func Liveness(ctx context.Context, tc *tmux.Client, runs []run.Run) map[string]tmux.Liveness {
	var active []*run.Run
	for _, r := range runs {
		if r.Status != run.Ended {
			c := r
			active = append(active, &c)
		}
	}
	out := map[string]tmux.Liveness{}
	if len(active) == 0 {
		return out
	}
	run.CheckLive(ctx, tc, active)
	for _, r := range active {
		if r.Live != nil {
			out[r.ID] = *r.Live
		}
	}
	return out
}
