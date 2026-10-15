package tui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tui/form"
)

// dialog is a modal form whose submit builds the arguments of a CLI command. It owns
// the keyboard; a click outside it is ignored.
type dialog struct {
	seq   int
	form  *form.Form
	busy  bool   // its action is running: a second submit is ignored
	stale bool   // a CONFLICT: the next snapshot refreshes what the form expects
	reqID string // one request id per set of arguments: a repeat is idempotent
	last  string // the arguments of the last submit
	// project is passed as --project (the target's project; "" for global commands).
	project string
	// submit returns the CLI arguments for a button, nil to close the dialog, or an
	// error to show (errStay: keep the dialog as it is).
	submit  func(button string) ([]string, error)
	then    func(o Outcome) tea.Cmd // after a success (the dialog is closed by then)
	refresh func()                  // after a CONFLICT, once the snapshot is reloaded
	// do, when set, handles a submit itself (no CLI command): the dialog closes first.
	do func(button string) tea.Cmd
}

// errStay keeps a dialog open with nothing to run (a second confirmation step).
var errStay = errors.New("stay")

// Dialog sizes.
const (
	dialogWidth = 76
	listRows    = 8
)

// open shows d. It never replaces an open dialog: d waits until that one closes (a
// late result, a task read for its form).
func (m *Model) open(d *dialog) tea.Cmd {
	m.dlgSeq++
	d.seq = m.dlgSeq
	if m.dlg != nil || m.typ.on { // typing mode keeps the keys until it ends
		m.queued = append(m.queued, d)
		return nil
	}
	m.dlg, m.menu = d, nil
	return nil
}

// closeDialog closes the open dialog and shows the next queued one.
func (m *Model) closeDialog() {
	m.dlg = nil
	if len(m.queued) > 0 {
		m.dlg, m.queued = m.queued[0], m.queued[1:]
	}
}

// dialogView is the open dialog's box, centred, and its content: the form's lines
// and hit rectangles. On a screen too short for the form its top is cut, so the
// buttons stay visible.
func (m *Model) dialogView() (Rect, []string, []form.Hit) {
	w := min(m.w-4, dialogWidth)
	lines, hits := m.dlg.form.View(w-4, m.theme)
	h := min(m.h-2, len(lines)+2)
	r := Rect{(m.w - w) / 2, max(0, (m.h-h)/2), w, h}
	if cut := len(lines) - (h - 2); cut > 0 {
		lines = lines[cut:]
		for i := range hits {
			hits[i].Y -= cut // a hit above the cut is clipped by the layout
		}
	}
	return r, lines, hits
}

// formWidth is the open dialog's content width (inside the border and padding).
func (m *Model) formWidth() int { return min(m.w-4, dialogWidth) - 4 }

func (m *Model) dialogKey(k tea.KeyPressMsg) tea.Cmd {
	if k.String() == "ctrl+c" {
		return m.quit()
	}
	ev, id := m.dlg.form.Key(k, m.formWidth())
	return m.dialogEvent(ev, id)
}

// dialogEvent closes the dialog or runs its command.
func (m *Model) dialogEvent(ev form.Event, button string) tea.Cmd {
	d := m.dlg
	switch ev {
	case form.Cancel:
		m.closeDialog() // an action in flight goes on; its outcome shows as a toast
	case form.Submit:
		if d.busy {
			return nil
		}
		if d.do != nil {
			m.closeDialog()
			return d.do(button)
		}
		d.form.Err = ""
		args, err := d.submit(button)
		switch {
		case errors.Is(err, errStay):
			return nil
		case err != nil:
			d.form.Err = err.Error()
			return nil
		case args == nil:
			if m.dlg == d {
				m.closeDialog()
			}
			return nil
		}
		p := pending{args: args, project: d.project, dlg: d.seq, then: d.then}
		if k := p.key(); k != d.last || d.reqID == "" {
			d.reqID, d.last = m.d.newID(), k
		}
		p.reqID = d.reqID
		cmd := m.fire(p)
		if cmd != nil {
			d.busy, d.form.Status = true, "working: otag "+p.label()+"…"
		}
		return cmd
	}
	return nil
}

// field constructors.

func text(id, label, value string, required bool) *form.Field {
	f := &form.Field{Kind: form.Text, ID: id, Label: label, Required: required}
	f.Ed.Set(value)
	return f
}

func area(id, label, value string, rows int, required bool) *form.Field {
	f := &form.Field{Kind: form.Area, ID: id, Label: label, Height: rows, Required: required}
	f.Ed.Multi = true
	f.Ed.Set(value)
	return f
}

func note(label, body string, rows int) *form.Field {
	return &form.Field{Kind: form.Note, Label: label, Text: body, Height: rows}
}

func check(id, label string) *form.Field { return &form.Field{Kind: form.Check, ID: id, Label: label} }

var cancelButton = form.Button{ID: "cancel", Label: "Cancel", Cancel: true}

// profileOptions lists the profiles for a Select ("" first: the default) or a List,
// unavailable ones with why; sel is the index of want.
func (m *Model) profileOptions(withDefault bool, want string) ([]form.Option, int) {
	var out []form.Option
	sel := 0
	if withDefault {
		out = append(out, form.Option{Value: "", Label: "(project default)"})
	}
	for _, p := range m.snap.Profiles {
		detail := strings.Join(strings.Fields(p.Harness+"/"+p.Model+" "+p.Effort), " ")
		if len(p.Tags) > 0 {
			detail += " · " + strings.Join(p.Tags, ",")
		}
		if p.Name == want {
			sel = len(out)
		}
		o := form.Option{Value: p.Name, Label: p.Name, Detail: detail}
		if !withDefault {
			o.Disabled = m.snap.Unavailable[p.Name]
		}
		out = append(out, o)
	}
	return out, sel
}

func (m *Model) projectRow(name string) *registry.ProjectRow {
	for i := range m.snap.Projects {
		if m.snap.Projects[i].Name == name {
			return &m.snap.Projects[i]
		}
	}
	return nil
}

// splitIDs splits "al-1, al-2 al-3" into ids.
func splitIDs(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
}

// newTaskDialog: task add --project P [--body] [--priority] [--profile] [--draft]
// [--blocked-by]… -- <title>.
func (m *Model) newTaskDialog(project string) *dialog {
	opts, _ := m.profileOptions(true, "")
	f := form.New("New task in "+project, []form.Button{{ID: "add", Label: "Add task"}, cancelButton},
		text("title", "Title", "", true), area("body", "Body", "", 4, false), text("priority", "Priority", "", false),
		&form.Field{Kind: form.Select, ID: "profile", Label: "Profile", Opts: opts},
		text("blocked", "Blocked by", "", false), check("draft", "draft (not ready to start yet)"))
	d := &dialog{form: f}
	d.submit = func(string) ([]string, error) {
		args := []string{"task", "add", "--project", project}
		if v := f.Get("body"); strings.TrimSpace(v) != "" {
			args = append(args, "--body", v)
		}
		if v := strings.TrimSpace(f.Get("priority")); v != "" {
			args = append(args, "--priority", v)
		}
		if v := f.Get("profile"); v != "" {
			args = append(args, "--profile", v)
		}
		if f.Get("draft") != "" {
			args = append(args, "--draft")
		}
		for _, id := range splitIDs(f.Get("blocked")) {
			args = append(args, "--blocked-by", id)
		}
		return append(args, "--", strings.TrimSpace(f.Get("title"))), nil
	}
	d.then = func(o Outcome) tea.Cmd {
		if t, ok := o.Data.(*task.Task); ok {
			m.want = "t:" + t.ID
		}
		return nil
	}
	return d
}

// editTaskDialog: task edit ID [--title] [--body] [--priority] [--profile]
// [--add-dep blocks:X]… [--rm-dep blocks:X]… --version V, with the changed fields only.
func (m *Model) editTaskDialog(t *task.Task) *dialog {
	opts, sel := m.profileOptions(true, t.Profile)
	var blocked []string
	for _, d := range t.Deps {
		if d.Kind == "blocks" {
			blocked = append(blocked, d.TaskID)
		}
	}
	prio := strconv.Itoa(t.Priority)
	f := form.New("Edit task "+t.ID, []form.Button{{ID: "save", Label: "Save"}, cancelButton},
		text("title", "Title", t.Title, true), area("body", "Body", t.Body, 4, false), text("priority", "Priority", prio, false),
		&form.Field{Kind: form.Select, ID: "profile", Label: "Profile", Opts: opts, Sel: sel},
		text("blocked", "Blocked by", strings.Join(blocked, " "), false))
	version, id := t.Version, t.ID
	d := &dialog{form: f, project: t.Project}
	d.submit = func(string) ([]string, error) {
		args := []string{"task", "edit", id}
		if v := strings.TrimSpace(f.Get("title")); v != t.Title {
			args = append(args, "--title", v)
		}
		if v := f.Get("body"); v != t.Body {
			args = append(args, "--body", v)
		}
		if v := strings.TrimSpace(f.Get("priority")); v != prio {
			args = append(args, "--priority", v)
		}
		if v := f.Get("profile"); v != t.Profile {
			args = append(args, "--profile", v)
		}
		now := splitIDs(f.Get("blocked"))
		for _, b := range now {
			if !slices.Contains(blocked, b) {
				args = append(args, "--add-dep", "blocks:"+b)
			}
		}
		for _, b := range blocked {
			if !slices.Contains(now, b) {
				args = append(args, "--rm-dep", "blocks:"+b)
			}
		}
		if len(args) == 3 {
			return nil, errors.New("nothing changed")
		}
		return append(args, "--version", strconv.FormatInt(version, 10)), nil
	}
	d.refresh = func() {
		if cur := m.findTask(id); cur != nil {
			version = cur.Version
		}
	}
	return d
}

// reasonDialog: task cancel ID [--reason R] --version V, or task block ID [--reason R].
func (m *Model) reasonDialog(t *task.Task, verb string) *dialog {
	what := "The task is cancelled"
	if verb == "block" {
		what = "The task is blocked until it is marked ready again"
	}
	if t.CurrentRunID != "" {
		if r := m.findRun(t.CurrentRunID); r != nil && r.Status != run.Ended {
			what += "; its run " + r.ID + " is stopped"
		}
	}
	title := strings.ToUpper(verb[:1]) + verb[1:]
	f := form.New(title+" task "+t.ID+" "+t.Title+"?", []form.Button{{ID: verb, Label: title + " task"},
		{ID: "back", Label: "Back", Cancel: true}}, note("", what+".", 0), text("reason", "Reason", "", false))
	version, id := t.Version, t.ID
	d := &dialog{form: f, project: t.Project}
	d.submit = func(string) ([]string, error) {
		args := []string{"task", verb, id}
		if v := strings.TrimSpace(f.Get("reason")); v != "" {
			args = append(args, "--reason", v)
		}
		if verb == "cancel" {
			args = append(args, "--version", strconv.FormatInt(version, 10))
		}
		return args, nil
	}
	d.refresh = func() {
		if cur := m.findTask(id); cur != nil {
			version = cur.Version
		}
	}
	return d
}

// profileList is the profile picker, the unavailable profiles greyed with why. The
// wanted profile stays selected even when it is unavailable: Start then says why, and
// another profile is the user's explicit choice, never a silent substitute.
func (m *Model) profileList(label, want string) *form.Field {
	opts, sel := m.profileOptions(false, want)
	keep := want != "" && sel < len(opts) && opts[sel].Value == want
	return &form.Field{Kind: form.List, ID: "profile", Label: label, Opts: opts, Sel: sel, Keep: keep,
		Height: max(1, min(listRows, len(opts)))}
}

// startDialog: run start TASK --profile P --no-attach; the result shows in a dialog.
func (m *Model) startDialog(t *task.Task) *dialog {
	want := t.Profile
	if want == "" {
		if p := m.projectRow(t.Project); p != nil {
			want = p.DefaultProfile
		}
	}
	f := form.New("Start a run of "+t.ID+" "+t.Title, []form.Button{{ID: "start", Label: "Start"}, cancelButton},
		m.profileList("Profile (default: the task's, else the project's)", want))
	d := &dialog{form: f, project: t.Project}
	d.submit = func(string) ([]string, error) {
		return []string{"run", "start", t.ID, "--profile", f.Get("profile"), "--no-attach"}, nil
	}
	d.then = func(o Outcome) tea.Cmd {
		key := ""
		if r := resultRun(o); r != nil {
			key = "r:" + r.ID
			m.want = key
		}
		return m.open(m.runResultDialog("Run started", o, key))
	}
	return d
}

// coordDialog: coord P --profile X.
func (m *Model) coordDialog(project string) *dialog {
	want := ""
	if p := m.projectRow(project); p != nil {
		want = p.CoordinatorProfile
		if want == "" {
			want = p.DefaultProfile
		}
	}
	f := form.New("Start the coordinator of "+project, []form.Button{{ID: "start", Label: "Start"}, cancelButton},
		m.profileList("Profile (default: coordinator_profile, else default_profile)", want))
	d := &dialog{form: f, project: project}
	d.submit = func(string) ([]string, error) {
		return []string{"coord", project, "--profile", f.Get("profile")}, nil
	}
	d.then = func(o Outcome) tea.Cmd {
		m.want = "c:" + project
		return m.open(m.runResultDialog("Coordinator", o, "c:"+project))
	}
	return d
}

// stopDialog: run stop RUN [--reason R], or with an open ask of the run [Park]:
// run stop RUN --park [--reason R].
func (m *Model) stopDialog(r *run.Run) *dialog {
	buttons := []form.Button{{ID: "stop", Label: "Stop"}}
	what := "Stop run " + r.ID + " (" + r.Profile + ")? Its windows are closed"
	if r.TaskID != "" {
		what += "; task " + r.TaskID + " goes back to ready"
	}
	what += "."
	if a := m.openAsk("", r.ID); a != nil {
		buttons = append(buttons, form.Button{ID: "park", Label: "Park"})
		what += fmt.Sprintf(" Park instead keeps the task waiting for the answer to #%d and resumes it then.", a.Message.ID)
	}
	f := form.New("Stop "+r.ID, append(buttons, cancelButton), note("", what, 0), text("reason", "Reason", "", false))
	d := &dialog{form: f, project: r.Project}
	d.submit = func(button string) ([]string, error) {
		args := []string{"run", "stop", r.ID}
		if button == "park" {
			args = append(args, "--park")
		}
		if v := strings.TrimSpace(f.Get("reason")); v != "" {
			args = append(args, "--reason", v)
		}
		return args, nil
	}
	return d
}

// replyDialog: reply ID -- text, with the question shown.
func (m *Model) replyDialog(id string) *dialog {
	question, project := "#"+id, ""
	if n, err := strconv.ParseInt(id, 10, 64); err == nil && m.snap != nil {
		for _, q := range m.snap.Needs.Questions {
			if q.Message.ID == n {
				question = fmt.Sprintf("#%d from %s to %s:\n%s", n, senderOf(q.Message.From.Run, q.Message.From.Actor), q.To,
					q.Message.Body)
				project = q.Message.Project
			}
		}
		for _, it := range m.snap.Inbox {
			if it.ID == n {
				question = fmt.Sprintf("#%d from %s:\n%s", n, senderOf(it.From.Run, it.From.Actor), it.Body)
				project = it.Project
			}
		}
	}
	f := form.New("Reply to #"+id, []form.Button{{ID: "reply", Label: "Send answer"}, cancelButton},
		note("Question", question, 8), area("answer", "Answer", "", 4, true))
	d := &dialog{form: f, project: project}
	d.submit = func(string) ([]string, error) {
		return []string{"reply", id, "--", f.Get("answer")}, nil
	}
	return d
}

func senderOf(r *string, actor string) string {
	if r != nil {
		return *r
	}
	return actor
}

// reviewDialog: review TASK --accept [-- note] | --reject -- reason.
func (m *Model) reviewDialog(t *task.Task) *dialog {
	about := "task " + t.ID + " " + t.Title + " (" + t.Status + ")"
	if r := m.taskRun(t.ID, t.CurrentRunID); r != nil {
		about += "\nreviewed run " + r.ID + ", profile " + r.Profile
		if ws := r.Workspace; ws != nil && ws.Branch != "" {
			about += "\nbranch " + ws.Branch
		}
	}
	f := form.New("Review "+t.ID, []form.Button{{ID: "accept", Label: "Accept"}, {ID: "reject", Label: "Reject"}, cancelButton},
		note("", about, 0), area("note", "Note / reason", "", 3, false))
	d := &dialog{form: f, project: t.Project}
	d.submit = func(button string) ([]string, error) {
		text := strings.TrimSpace(f.Get("note"))
		args := []string{"review", t.ID, "--" + button}
		if button == "reject" && text == "" {
			f.FocusField("note")
			return nil, errors.New("a reject needs a reason")
		}
		if text != "" {
			args = append(args, "--", text)
		}
		return args, nil
	}
	return d
}

// deliverDialog: task deliver ID [--note N] [--delete-branch] [--force]; "delete branch
// if merged" starts unchecked, and force asks a second time.
func (m *Model) deliverDialog(t *task.Task) *dialog {
	branch, wt := t.Branch, "(none recorded)"
	if r := m.taskRun(t.ID, t.CurrentRunID); r != nil && r.Workspace != nil {
		if branch == "" {
			branch = r.Workspace.Branch
		}
		wt = r.Workspace.Path + " (" + r.Workspace.Mode + ")"
	}
	if branch == "" {
		branch = "(none)"
	}
	about := fmt.Sprintf("task %s %s (%s)\nbranch %s\nworktree %s\nThe worktree will be removed; the branch is kept.",
		t.ID, t.Title, t.Status, branch, wt)
	buttons := []form.Button{{ID: "deliver", Label: "Deliver"}, cancelButton}
	f := form.New("Deliver "+t.ID, buttons, note("", about, 0), text("note", "Note", "", false),
		check("delete", "delete the branch if it is merged"),
		check("force", "force: lose uncommitted changes; delete the branch even if unmerged"))
	d := &dialog{form: f, project: t.Project}
	d.submit = func(button string) ([]string, error) {
		switch {
		case button == "back":
			f.Buttons, f.Status = buttons, ""
			f.FocusField("force")
			return nil, errStay
		case f.Get("force") != "" && button != "force":
			// The second step: Back sits where [Deliver] was and is the default, so a
			// double click or a second ctrl+s goes back; force needs its own click or key.
			f.Buttons = []form.Button{{ID: "back", Label: "Back"}, {ID: "force", Label: "Deliver with force"}}
			f.Status = "force removes worktrees with uncommitted changes: those changes are lost. Sure?"
			f.FocusButton("back")
			return nil, errStay
		}
		args := []string{"task", "deliver", t.ID}
		if v := strings.TrimSpace(f.Get("note")); v != "" {
			args = append(args, "--note", v)
		}
		if f.Get("delete") != "" {
			args = append(args, "--delete-branch")
		}
		if button == "force" {
			args = append(args, "--force")
		}
		return args, nil
	}
	d.then = func(o Outcome) tea.Cmd { return m.open(m.resultDialog("Delivered "+t.ID, o.Human)) }
	return d
}

// pushDialog: task push ID [--force]; force asks a second time, like deliver's.
func (m *Model) pushDialog(t *task.Task) *dialog {
	last := "never pushed by otag"
	if p := t.Push; p != nil {
		last = fmt.Sprintf("%s to %s/%s at %s", shortSHA(p.SHA), p.Remote, p.Branch, p.At)
	}
	about := fmt.Sprintf("task %s %s (%s)\nbranch %s\nlast push: %s\nPushes the branch's commits to the project's remote "+
		"(the same branch name); uncommitted changes stay behind.", t.ID, t.Title, t.Status, t.Branch, last)
	buttons := []form.Button{{ID: "push", Label: "Push"}, cancelButton}
	f := form.New("Push "+t.ID, buttons, note("", about, 0),
		check("force", "force: overwrite the remote branch (--force-with-lease against otag's last push)"))
	d := &dialog{form: f, project: t.Project}
	d.submit = func(button string) ([]string, error) {
		switch {
		case button == "back":
			f.Buttons, f.Status = buttons, ""
			f.FocusField("force")
			return nil, errStay
		case f.Get("force") != "" && button != "force":
			f.Buttons = []form.Button{{ID: "back", Label: "Back"}, {ID: "force", Label: "Push with force"}}
			f.Status = "force replaces the remote branch's history (only while it is where otag last pushed it). Sure?"
			f.FocusButton("back")
			return nil, errStay
		}
		args := []string{"task", "push", t.ID}
		if button == "force" {
			args = append(args, "--force")
		}
		return args, nil
	}
	d.then = func(o Outcome) tea.Cmd { return m.open(m.resultDialog("Pushed "+t.ID, o.Human)) }
	return d
}

// reopenDialog: task reopen ID --version V -- reason. The reason is required: the
// task's next run gets it.
func (m *Model) reopenDialog(t *task.Task) *dialog {
	branch := "the same branch"
	if t.Branch != "" {
		branch = t.Branch
	}
	about := fmt.Sprintf("task %s %s (%s)\nThe task goes back to ready on %s; its next run gets the reason. "+
		"An accepted review no longer counts: it is reviewed again when it is done.", t.ID, t.Title, t.Status, branch)
	f := form.New("Reopen task "+t.ID+"?", []form.Button{{ID: "reopen", Label: "Reopen"}, cancelButton},
		note("", about, 0), area("reason", "Reason (review feedback, CI failure, ...)", "", 3, true))
	version, id := t.Version, t.ID
	d := &dialog{form: f, project: t.Project}
	d.submit = func(string) ([]string, error) {
		reason := strings.TrimSpace(f.Get("reason"))
		if reason == "" {
			f.FocusField("reason")
			return nil, errors.New("a reopen needs a reason")
		}
		return []string{"task", "reopen", id, "--version", strconv.FormatInt(version, 10), "--", reason}, nil
	}
	d.refresh = func() {
		if cur := m.findTask(id); cur != nil {
			version = cur.Version
		}
	}
	return d
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// confirmDialog runs args (with --project project, when not "") after a yes.
func (m *Model) confirmDialog(title, body, yes, project string, args []string) *dialog {
	f := form.New(title, []form.Button{{ID: "yes", Label: yes}, cancelButton}, note("", body, 0))
	f.FocusButton("yes")
	return &dialog{form: f, project: project, submit: func(string) ([]string, error) { return args, nil }}
}

// resultDialog shows an action's output.
func (m *Model) resultDialog(title, body string) *dialog {
	f := form.New(title, []form.Button{{ID: "close", Label: "Close", Cancel: true}},
		note("", strings.TrimRight(body, "\n"), max(3, m.h-12)))
	return &dialog{form: f, submit: func(string) ([]string, error) { return nil, nil }}
}

// runResultDialog shows the output of a command that started a run (run start,
// coord), with [Attach] (full screen), [Type] (typing mode into its pane once its row
// shows; rowKey selects it) and [Close].
func (m *Model) runResultDialog(title string, o Outcome, rowKey string) *dialog {
	r := resultRun(o)
	if r == nil {
		return m.resultDialog(title, o.Human)
	}
	f := form.New(title, []form.Button{{ID: "attach", Label: "Attach"}, {ID: "type", Label: "Type"},
		{ID: "close", Label: "Close", Cancel: true}}, note("", strings.TrimRight(o.Human, "\n"), max(3, m.h-12)))
	f.FocusButton("attach")
	d := &dialog{form: f}
	d.do = func(button string) tea.Cmd {
		if button == "type" {
			return m.typeInto(r.ID, rowKey)
		}
		return m.attachResult(r.ID, r)
	}
	return d
}

// recipientDialog picks the message box's recipient: this run, this task, the
// coordinator, the human, or the open question to reply to.
func (m *Model) recipientDialog() *dialog {
	s := m.sel()
	var opts []form.Option
	var tos []recipient
	add := func(r recipient, label, detail string) {
		opts = append(opts, form.Option{Value: strconv.Itoa(len(tos)), Label: label, Detail: detail})
		tos = append(tos, r)
	}
	if s.run != nil && s.run.Status != run.Ended && s.run.Role != "coordinator" {
		add(recipient{to: s.run.ID, project: s.project, label: s.run.ID}, "this run "+s.run.ID, s.run.Status)
	}
	if s.task != nil {
		add(recipient{to: s.task.ID, project: s.project, label: s.task.ID}, "this task "+s.task.ID, "its run, or the next")
	}
	if s.project != "" {
		add(recipient{to: "coord", project: s.project, label: "coord"}, "coord", "the coordinator of "+s.project)
		add(recipient{to: "human", project: s.project, label: "human"}, "human", "your own inbox")
	}
	var ask int64
	taskID, runID := "", ""
	if s.task != nil {
		taskID = s.task.ID
	}
	if s.run != nil {
		runID = s.run.ID
	}
	askProject := ""
	if a := m.openAsk(taskID, runID); a != nil {
		ask, askProject = a.Message.ID, a.Message.Project
		opts = append(opts, form.Option{Value: "reply", Label: fmt.Sprintf("reply #%d", ask), Detail: oneLine(a.Message.Body)})
	}
	if len(opts) == 0 {
		opts = append(opts, form.Option{Value: "", Label: "nothing selected", Disabled: "select a project, task or run"})
	}
	f := form.New("Send the message to", []form.Button{{ID: "pick", Label: "Choose"}, cancelButton},
		&form.Field{Kind: form.List, ID: "to", Label: "Recipient", Opts: opts, Height: len(opts)})
	d := &dialog{form: f}
	d.submit = func(string) ([]string, error) {
		v := f.Get("to")
		if v == "reply" {
			m.box.reply, m.box.replyProject = ask, askProject
		} else if i, err := strconv.Atoi(v); err == nil && i < len(tos) {
			m.box.setTo(tos[i])
			m.box.pinned = true
		}
		m.focus = focusBox
		return nil, nil
	}
	return d
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// dialogClick is a click on part r of the open dialog.
func (m *Model) dialogClick(r region, ev tea.Mouse, double bool) tea.Cmd {
	i, _ := strconv.Atoi(r.id)
	dr, _, hits := m.dialogView()
	if i >= len(hits) {
		return nil
	}
	e, id := m.dlg.form.Click(hits[i], ev.X-dr.X-2, ev.Y-dr.Y-1, double, m.formWidth())
	return m.dialogEvent(e, id)
}
