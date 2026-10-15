package tui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
)

// Actions (docs/TUI.md "Dialogs and forms"). Every mutation runs a CLI command through
// Options.Call in a Cmd, never on the UI goroutine. One action per busy key (its
// command and target) runs at a time, and each carries a request id, so a double
// click or a double Enter does not run it twice.

// quitWait is how long quitting waits for short actions in flight; long ones (start,
// resume, coord, deliver) are waited for until they end, or a second q.
const quitWait = 5 * time.Second

// pending is an action on its way to the CLI.
type pending struct {
	args    []string // CLI arguments, without --request-id and --project
	project string   // --project, passed before the command when the action has one
	reqID   string
	dlg     int                     // the dialog that runs it (0: none)
	then    func(o Outcome) tea.Cmd // after a success: a result dialog, a selection
}

// argv is the command line of the action, without --request-id.
func (p pending) argv() []string {
	if p.project == "" {
		return p.args
	}
	return append([]string{"--project", p.project}, p.args...)
}

// key is the action's busy key: its whole command line, so one action of each command
// and target (and text) runs at a time.
func (p pending) key() string { return strings.Join(p.argv(), "\x00") }

// label names an action in toasts and the Log: the command words up to the first
// flag ("run start al-12", "task add", "reply 42").
func (p pending) label() string {
	var words []string
	for _, a := range p.args {
		if strings.HasPrefix(a, "-") || len(words) == 3 {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// long reports whether quitting must wait for the action until it ends: a start or a
// delivery cut short leaves work half done.
func (p pending) long() bool {
	l := p.label()
	for _, c := range []string{"run start", "run resume", "coord", "task deliver"} {
		if strings.HasPrefix(l, c) {
			return true
		}
	}
	return false
}

type (
	outcomeMsg struct {
		p   pending
		out Outcome
	}
	quitNowMsg struct{}
	// taskMsg brings a task read for a form (task edit).
	taskMsg struct {
		t   *task.Task
		err error
	}
)

// newRequestID returns tui-<16 hex digits>.
func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "tui-" + hex.EncodeToString(b)
}

// cannotAct says why no action can run now ("" = they can).
func (m *Model) cannotAct() string {
	switch {
	case m.snap == nil:
		return "still loading"
	case m.snap.ReadOnly:
		return "read-only: the database is newer than this otag; actions are disabled"
	case m.quitting:
		return "quitting: no new actions"
	}
	return ""
}

// fire runs p through Options.Call in a Cmd.
func (m *Model) fire(p pending) tea.Cmd {
	if why := m.cannotAct(); why != "" {
		m.flash(why, true)
		return nil
	}
	key := p.key()
	if m.busy[key] {
		m.flash(p.label()+": already running", false)
		return nil
	}
	if p.reqID == "" {
		p.reqID = m.d.newID()
	}
	m.busy[key] = true
	m.inflight++
	if p.long() {
		m.long++
	}
	args := append([]string{"--request-id", p.reqID}, p.argv()...)
	call, ctx := m.d.call, m.ctx
	return func() tea.Msg { return outcomeMsg{p: p, out: call(ctx, args...)} }
}

// outcome takes an action's result: the busy flag, the Log, a reload, and the toast
// or the dialog's error line. A CONFLICT keeps the form open. The result of a dialog
// closed meanwhile is only a toast: it opens nothing and moves no selection.
func (m *Model) outcome(msg outcomeMsg) tea.Cmd {
	p, o := msg.p, msg.out
	delete(m.busy, p.key())
	m.inflight--
	if p.long() {
		m.long--
	}
	m.logOutcome(p, o)
	if o.Err != nil {
		m.debugf("action %s failed: %s", p.label(), o.Err.Code)
	} else {
		m.debugf("action %s ok", p.label())
	}
	cmds := []tea.Cmd{m.requestLoad()}
	d := m.dlg
	mine := d != nil && p.dlg != 0 && d.seq == p.dlg
	if mine {
		d.busy, d.form.Status = false, ""
	}
	switch {
	case o.Err != nil && mine:
		d.form.Err = o.Err.Code + ": " + o.Err.Message
		if o.Err.Code == apperr.Conflict {
			d.form.Err = "changed meanwhile — reloaded; submit again to apply your values (" + o.Err.Message + ")"
			d.stale = true
		}
	case o.Err != nil:
		m.flash(p.label()+": "+o.Err.Code+": "+o.Err.Message, true)
	default:
		if mine {
			m.closeDialog()
		}
		m.flash(firstLine(o.Human, p.label()+": done"), false)
		if p.then != nil && (p.dlg == 0 || mine) {
			cmds = append(cmds, p.then(o))
		}
	}
	if m.quitting {
		cmds = append(cmds, m.quitStep())
	}
	return tea.Batch(cmds...)
}

// logOutcome adds an action to the Log tab: the command, then its output or error.
func (m *Model) logOutcome(p pending, o Outcome) {
	mark := m.theme.Good.Render("✓")
	if o.Err != nil {
		mark = m.theme.Bad.Render("✕")
	}
	m.log = append(m.log, fmt.Sprintf("%s %s otag %s", m.d.now().Format("15:04:05"), mark, quoteArgs(p.argv())))
	for _, l := range strings.Split(strings.TrimRight(o.Human, "\n"), "\n") {
		if l != "" {
			m.log = append(m.log, "    "+l)
		}
	}
	if o.Err != nil {
		m.log = append(m.log, "    "+m.theme.Bad.Render(o.Err.Code+": "+o.Err.Message))
	}
}

// quoteArgs shows arguments as a shell would need them.
func quoteArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`*?![]{}()<>|&;#~") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = strings.ReplaceAll(a, "\n", `\n`)
	}
	return strings.Join(out, " ")
}

// firstLine is the first non-empty line of s, else def.
func firstLine(s, def string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return def
}

// quit ends the TUI once the actions in flight are done: long ones (start, resume,
// coord, deliver) until they end, the others up to quitWait. A second quit key quits
// at once.
func (m *Model) quit() tea.Cmd {
	if m.inflight == 0 || m.quitting {
		return tea.Quit
	}
	m.quitting = true
	return m.quitStep()
}

// quitStep quits when nothing runs any more, else says what it waits for and, with
// only short actions left, starts the quitWait timer once.
func (m *Model) quitStep() tea.Cmd {
	switch {
	case m.inflight == 0:
		return tea.Quit
	case m.long > 0:
		m.flash(fmt.Sprintf("finishing %d action(s)… (q again quits now)", m.inflight), false)
		return nil
	case m.quitTimer:
		return nil
	}
	m.quitTimer = true
	m.flash(fmt.Sprintf("finishing %d action(s)… (q again quits now)", m.inflight), false)
	return m.d.after("quit", quitWait, func() tea.Msg { return quitNowMsg{} })
}

// sel is what the selected row is about.
type sel struct {
	project string
	task    *task.Task // a task row, or the task of a run or "earlier" row
	run     *run.Run   // a run or coordinator row, or a task row's current (else newest) run
	row     *tree.Row
}

func (m *Model) sel() sel {
	r := m.tree.Selected()
	if r == nil || m.snap == nil {
		return sel{}
	}
	s := sel{project: r.Project, row: r}
	switch r.Kind {
	case tree.Task:
		s.task = r.Task
		s.run = m.taskRun(r.Task.ID, r.Task.CurrentRunID)
	case tree.Run, tree.Coord:
		s.run = r.Run
		if r.Run.TaskID != "" {
			s.task = m.findTask(r.Run.TaskID)
		}
	case tree.Earlier:
		s.task = m.findTask(strings.TrimPrefix(r.Key, "h:"))
		if s.task != nil {
			s.run = m.taskRun(s.task.ID, s.task.CurrentRunID)
		}
	}
	return s
}

// findTask returns a task of the snapshot by id.
func (m *Model) findTask(id string) *task.Task {
	if m.snap == nil {
		return nil
	}
	for _, l := range [][]task.Task{m.snap.Open, m.snap.Deliverable, m.snap.Closed} {
		for i := range l {
			if l[i].ID == id {
				return &l[i]
			}
		}
	}
	return nil
}

// findRun returns a run of the snapshot by id.
func (m *Model) findRun(id string) *run.Run {
	if m.snap == nil {
		return nil
	}
	for i := range m.snap.Runs {
		if m.snap.Runs[i].ID == id {
			return &m.snap.Runs[i]
		}
	}
	return nil
}

// projectOf is the project of a task or run id of the snapshot.
func (m *Model) projectOf(id string) string {
	if t := m.findTask(id); t != nil {
		return t.Project
	}
	if r := m.findRun(id); r != nil {
		return r.Project
	}
	return ""
}

// openAsk is the open question of a run, or of a task's runs (nil: none).
func (m *Model) openAsk(taskID, runID string) *msg.OpenAsk {
	if m.snap == nil {
		return nil
	}
	for i := range m.snap.Asks {
		a := &m.snap.Asks[i]
		if runID != "" && a.Asker == runID {
			return a
		}
		if taskID != "" && a.Message != nil && a.Message.Task != nil && *a.Message.Task == taskID {
			return a
		}
		if r := m.findRun(a.Asker); taskID != "" && r != nil && r.TaskID == taskID {
			return a
		}
	}
	return nil
}

// action runs a row action key (or footer button).
func (m *Model) action(act action) tea.Cmd {
	if why := m.cannotAct(); why != "" {
		m.flash(why, true)
		return nil
	}
	s := m.sel()
	need := func(what string) tea.Cmd {
		m.flash("select "+what+" first", true)
		return nil
	}
	switch act {
	case actNew:
		if s.project == "" {
			return need("a project")
		}
		return m.open(m.newTaskDialog(s.project))
	case actEdit, actCancel, actReady, actBlock:
		if s.task == nil {
			return need("a task")
		}
		t := s.task
		switch act {
		case actEdit:
			id := t.ID
			load := m.d.task
			return func() tea.Msg {
				ctx, cancel := m.job(loadTimeout)
				defer cancel()
				t, err := load(ctx, id)
				return taskMsg{t: t, err: err}
			}
		case actCancel:
			return m.open(m.reasonDialog(t, "cancel"))
		case actBlock:
			return m.open(m.reasonDialog(t, "block"))
		}
		return m.fire(pending{project: t.Project, args: []string{"task", "ready", t.ID}})
	case actStart:
		if s.task == nil {
			return need("a task")
		}
		return m.open(m.startDialog(s.task))
	case actStop, actPark:
		r := s.run
		if r == nil || r.Status == run.Ended {
			return need("a live run")
		}
		if act == actPark {
			return m.open(m.confirmDialog("Park run "+r.ID+"?", "The session ends; the task keeps waiting for the answer "+
				"to the run's question, and the answer resumes it.", "Park", r.Project, []string{"run", "stop", r.ID, "--park"}))
		}
		return m.open(m.stopDialog(r))
	case actResume:
		if s.task == nil {
			return need("a task")
		}
		return m.fire(pending{project: s.task.Project, args: []string{"run", "resume", s.task.ID}})
	case actCoord:
		if s.project == "" {
			return need("a project")
		}
		if c := m.coordOf(s.project); c != nil {
			m.tree.Select("c:" + s.project)
			return tea.Batch(m.selected(), m.attachRun(c))
		}
		return m.open(m.coordDialog(s.project))
	case actReply:
		var taskID, runID string
		if s.task != nil {
			taskID = s.task.ID
		}
		if s.run != nil {
			runID = s.run.ID
		}
		a := m.openAsk(taskID, runID)
		if a == nil {
			m.flash("no open question here (the Needs tab lists them)", true)
			return nil
		}
		return m.replyBox(a.Message.ID, a.Message.Project)
	case actMessage:
		return m.focusBox(true)
	case actReview:
		t := s.task
		if t != nil && t.Status != task.Review {
			if of := m.snap.ReviewOf[t.ID]; of != "" {
				t = m.findTask(of)
			}
		}
		if t == nil || t.Status != task.Review {
			return need("a task in review")
		}
		return m.open(m.reviewDialog(t))
	case actDeliver:
		t := s.task
		if t == nil && s.row != nil && s.row.Kind == tree.Deliver {
			for i := range m.snap.Deliverable {
				if m.snap.Deliverable[i].Project == s.project {
					t = &m.snap.Deliverable[i]
					break
				}
			}
		}
		if t == nil {
			return need("a done task")
		}
		return m.open(m.deliverDialog(t))
	case actAckAll:
		return m.open(m.confirmDialog("Acknowledge every notification?", fmt.Sprintf("%d unread notification(s) "+
			"are marked read (otag notify ack --all).", m.snap.Unread), "Ack all", "", []string{"notify", "ack", "--all"}))
	}
	return nil
}

// coordOf is the live coordinator of a project (nil: none).
func (m *Model) coordOf(project string) *run.Run {
	for i := range m.snap.Runs {
		c := &m.snap.Runs[i]
		if c.Project == project && c.Role == "coordinator" && c.Status != run.Ended {
			return c
		}
	}
	return nil
}

// itemAct runs an inline action of the Needs or Inbox tab.
func (m *Model) itemAct(a tabs.Act) tea.Cmd {
	if why := m.cannotAct(); why != "" {
		m.flash(why, true)
		return nil
	}
	switch a.Kind {
	case tabs.ActReply:
		return m.open(m.replyDialog(a.ID))
	case tabs.ActResend:
		// No --project: the Needs tab lists every project's deliveries.
		return m.fire(pending{args: []string{"send", "--resend", a.ID}})
	case tabs.ActReady:
		return m.fire(pending{project: m.projectOf(a.ID), args: []string{"task", "ready", a.ID}})
	case tabs.ActResume:
		return m.fire(pending{project: m.projectOf(a.ID), args: []string{"run", "resume", a.ID}})
	case tabs.ActAck:
		return m.fire(pending{args: []string{"notify", "ack", a.ID}})
	case tabs.ActAckAll:
		return m.action(actAckAll)
	case tabs.ActRead:
		// No --project: like the Inbox tab, every project's messages to the human.
		return m.fire(pending{args: []string{"inbox"}, then: func(o Outcome) tea.Cmd {
			return m.open(m.resultDialog("Messages (now read)", o.Human))
		}})
	case tabs.ActStop:
		if r := m.findRun(a.ID); r != nil && r.Status != run.Ended {
			return m.open(m.stopDialog(r))
		}
		m.flash("run "+a.ID+" is not live", true)
	case tabs.ActMessage:
		project := m.projectOf(a.ID)
		if project == "" {
			project = m.projectOf(a.Task)
		}
		m.box.setTo(recipient{to: a.ID, project: project, label: a.ID})
		m.box.pinned = true
		return m.focusBox(false)
	}
	return nil
}

// actOnItem runs an action key while Needs or Inbox has the focus: it acts on the
// selected item, never on the tree row out of sight. R, u, r, S and m run the item's
// matching button; n and A are not about a row; the other keys need the tree.
func (m *Model) actOnItem(act action) tea.Cmd {
	kinds := map[action]string{actReady: tabs.ActReady, actResume: tabs.ActResume, actReply: tabs.ActReply,
		actStop: tabs.ActStop, actMessage: tabs.ActMessage}
	kind, ok := kinds[act]
	if !ok {
		if act == actNew || act == actAckAll {
			return m.action(act)
		}
		m.flash("on Needs and Inbox, keys act on the selected item: use its buttons, or Tab to the tree", true)
		return nil
	}
	items := m.items(m.tab)
	i := m.itemIndex(m.tab, items)
	if i < 0 {
		m.flash("no item selected", true)
		return nil
	}
	var labels []string
	for _, a := range items[i].Acts {
		if a.Kind == kind {
			return m.itemAct(a)
		}
		labels = append(labels, tabs.Button(a))
	}
	if len(labels) == 0 {
		labels = []string{"none"}
	}
	m.flash("not for this item; its actions: "+strings.Join(labels, " "), true)
	return nil
}
