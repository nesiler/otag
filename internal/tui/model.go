package tui

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/preview"
	"github.com/nesiler/otag/internal/tui/style"
	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
)

// Refresh timing (docs/TUI.md "Data and refresh").
const (
	watchEvery   = 250 * time.Millisecond // PRAGMA data_version
	loadGap      = 150 * time.Millisecond // minimum time between two loads
	fullEvery    = 10 * time.Second       // reload anyway: ages, quota staleness
	liveEvery    = 2 * time.Second        // pane liveness
	clockEvery   = time.Second            // elapsed times (seconds under a minute)...
	clockCoarse  = 10 * time.Second       // ...minutes and more
	captureEvery = 250 * time.Millisecond // preview: 4 Hz...
	captureSlow  = time.Second            // ...1 Hz while the terminal has no focus or the pane is still
	stillAfter   = 8                      // unchanged captures in a row that make a pane still
	doubleClick  = 400 * time.Millisecond
	toastFor     = 4 * time.Second
	wheelLines   = 3
	dividerStep  = 4
	hscrollStep  = 8
)

// Limits of the read jobs: a hung tmux server or a stuck query ends in an error
// instead of leaving its loop waiting forever.
const (
	watchTimeout   = time.Second
	loadTimeout    = 5 * time.Second
	liveTimeout    = 2 * time.Second
	captureTimeout = 2 * time.Second // PaneView has its own 1 s limit
	textTimeout    = 10 * time.Second
)

// job is the context of one read job: cancelled when the TUI exits or after d.
func (m *Model) job(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(m.ctx, d)
}

// focus is the pane that takes the keys.
type focus int

const (
	focusTree focus = iota
	focusPane
	focusBox // the message box
)

// Model is the root model. Update is pure apart from the Cmds it returns: every
// database, tmux or backend call runs in a Cmd.
type Model struct {
	d       deps
	ctx     context.Context
	project string // select this project's row once it appears
	// unconfigured: otag has no registry yet; addDir: the git repository the TUI was
	// started in (key a adds it while no project uses it).
	unconfigured bool
	addDir       string
	home         string // $HOME, to show paths as ~/...

	w, h       int
	focus      focus
	narrowPane bool // narrow mode shows the right pane instead of the tree
	tab        tabs.Tab
	divider    int // tree width; 0 = default
	help       bool
	helpOff    int // first help line shown
	blurred    bool

	snap    *data.Snapshot
	snapSeq int
	loadErr string
	loading struct {
		inFlight, dirty bool
		last            time.Time
	}
	live     map[string]tmux.Liveness
	liveBusy bool

	tree tree.Model
	prev preview.Model
	// cap is the capture loop: one capture of the current target at a time, each
	// scheduled by the previous result. Bumping loop ends the chain: a scheduled capture
	// of an older loop checks cur before calling tmux and is dropped when it returns.
	cap struct {
		loop      int64
		cur       *atomic.Int64 // loop, readable from the Cmd goroutine
		busy      bool          // the current loop has a capture scheduled or in flight
		still     int           // captures in a row that changed nothing
		lockLines int           // > 0: a scrollback fetch for scroll-lock is in flight...
		lockGen   int           // ...of this request: a fetch of an older one is dropped
	}

	text   map[tabs.Tab]*textView // Details and Stats
	scroll map[tabs.Tab]int       // first line shown of the text tabs
	log    []string               // Log tab: the outcomes of this session's actions
	toast  struct {
		text  string
		bad   bool
		until time.Time
	}
	mouse mouseState
	theme style.Theme

	// Actions (T2).
	busy      map[string]bool // actions in flight by command line: one of each at a time
	inflight  int
	long      int  // of them start, resume, coord or deliver: quitting waits for them
	quitting  bool // quit waits for the actions in flight
	quitTimer bool // ...and its quitWait timer runs
	dlg       *dialog
	queued    []*dialog // dialogs waiting for the open one to close
	dlgSeq    int
	box       msgbox
	// The selected item of Needs and Inbox: its key (kind and id), and its index for
	// when that item is gone.
	itemKey map[tabs.Tab]string
	item    map[tabs.Tab]int
	want    string // select this row once a snapshot has it (a new task or run)
	stats   int    // bumped to refetch Stats

	// Polish (T4).
	menu      *menu // the open context menu
	filtering bool  // the filter prompt has the keys (the text is tree.Filter)
	hover     bool  // opt-in: AllMotion mouse reports highlight the row under the pointer

	// Typing mode and attach (T3).
	typ       typingState
	attaching bool // an attach is being prepared or runs

	// frame caches the last rendered screen; Update clears it when what it shows may
	// have changed. Most messages (an unchanged data_version, an identical capture)
	// change nothing, and composing a full screen is the TUI's main CPU cost.
	frame      string
	frameValid bool
	clockAt    time.Time // the last redraw for the clock
}

// recent reports whether a run or task of the snapshot started or changed status
// within the last minute: its elapsed time shows seconds.
func (m *Model) recent(now time.Time) bool {
	if m.snap == nil {
		return false
	}
	young := func(ts string) bool {
		t, err := time.Parse(time.RFC3339, ts)
		return err == nil && now.Sub(t) < time.Minute
	}
	for i := range m.snap.Runs {
		if r := &m.snap.Runs[i]; r.Status != run.Ended && (young(r.StartedAt) || young(r.QueuedAt)) {
			return true
		}
	}
	for _, l := range [][]task.Task{m.snap.Open, m.snap.Deliverable, m.snap.Closed} {
		for i := range l {
			if young(l[i].StatusChangedAt) {
				return true
			}
		}
	}
	return false
}

// textView is the output of a read command shown in a tab.
type textView struct {
	key      string // what it shows; a result for another key is stale
	text     string
	err      string
	inFlight bool
}

func newModel(ctx context.Context, d deps, project string) *Model {
	m := &Model{d: d, ctx: ctx, project: project, tree: tree.New(), live: map[string]tmux.Liveness{},
		text: map[tabs.Tab]*textView{tabs.Details: {}, tabs.Stats: {}}, scroll: map[tabs.Tab]int{}, theme: style.Default,
		busy: map[string]bool{}, item: map[tabs.Tab]int{}, itemKey: map[tabs.Tab]string{}}
	m.box.ed.Multi = true
	m.cap.cur = &atomic.Int64{}
	return m
}

// Messages.
type (
	watchMsg    struct{ changed bool }
	fullTickMsg struct{}
	loadMsg     struct {
		snap *data.Snapshot
		err  error
	}
	liveTickMsg struct{}
	liveMsg     struct {
		live map[string]tmux.Liveness
		tick bool // from the periodic check (reschedules it)
	}
	clockMsg   struct{}
	captureMsg struct {
		loop    int64
		pane    string
		history bool
		lockGen int  // history: the scroll-lock request it answers
		stale   bool // its loop had ended: tmux was not called
		view    tmux.PaneView
		err     error
	}
	textMsg struct {
		tab  tabs.Tab
		key  string
		text string
		err  error
	}
)

// Init starts the periodic jobs. Each is a one-shot tick that reschedules itself once
// its result arrives, so nothing piles up while the event loop is busy.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.requestLoad(), m.watch(), m.tick(fullEvery, fullTickMsg{}), m.tick(liveEvery, liveTickMsg{}),
		m.tick(clockEvery, clockMsg{}), tea.RequestBackgroundColor)
}

// tick delivers msg after d.
func (m *Model) tick(d time.Duration, msg tea.Msg) tea.Cmd {
	return m.d.after("tick", d, func() tea.Msg { return msg })
}

// watch reads data_version after watchEvery, in the tick itself (one message per
// check).
func (m *Model) watch() tea.Cmd {
	return m.d.after("watch", watchEvery, func() tea.Msg {
		ctx, cancel := m.job(watchTimeout)
		defer cancel()
		changed, err := m.d.changed(ctx)
		return watchMsg{changed: changed && err == nil}
	})
}

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.KeyReleaseMsg, tea.PasteMsg, tea.MouseMsg, watchMsg, clockMsg, liveTickMsg, fullTickMsg:
		// input (never logged) and the periodic ticks (too many)
	default:
		m.debugf("msg %T", msg)
	}
	cmd, quiet := m.update(msg)
	if !quiet {
		m.frameValid = false
	}
	return m, cmd
}

// update reports quiet when msg changed nothing on screen.
func (m *Model) update(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.clampAll()
		return m.syncPreview(), false
	case tea.BackgroundColorMsg:
		m.theme = style.ForBackground(msg.IsDark())
		return nil, false
	case tea.FocusMsg:
		m.blurred = false
		return nil, true
	case tea.BlurMsg:
		m.blurred = true
		return nil, true
	case tea.KeyPressMsg:
		return m.key(msg), false
	case tea.PasteMsg:
		return m.paste(msg.Content), false
	case sentMsg:
		on, toast := m.typ.on, m.toast.text
		cmd := m.sent(msg)
		return cmd, on == m.typ.on && toast == m.toast.text // an error shows
	case attachReadyMsg:
		return m.attachReady(msg), true
	case attachDoneMsg:
		return m.attachDone(msg), false
	case tea.MouseMsg:
		hover, divider := m.tree.Hover, m.divider
		cmd := m.mouseMsg(msg)
		_, motion := msg.(tea.MouseMotionMsg)
		return cmd, motion && m.tree.Hover == hover && m.divider == divider
	case outcomeMsg:
		return m.outcome(msg), false
	case quitNowMsg:
		return tea.Quit, true
	case taskMsg:
		if msg.err != nil {
			m.flash("edit: "+msg.err.Error(), true)
			return nil, false
		}
		return m.open(m.editTaskDialog(msg.t)), false

	case watchMsg:
		var cmd tea.Cmd
		if msg.changed {
			cmd = m.requestLoad()
		}
		return tea.Batch(cmd, m.watch()), true
	case fullTickMsg:
		return tea.Batch(m.requestLoad(), m.tick(fullEvery, fullTickMsg{})), true
	case loadMsg:
		return m.loaded(msg), false

	case liveTickMsg:
		if m.liveBusy {
			return m.tick(liveEvery, liveTickMsg{}), true
		}
		return m.checkLive(true), true
	case liveMsg:
		m.liveBusy = false
		same := sameLive(m.live, msg.live)
		m.live = msg.live
		m.refreshMenu() // Attach and Type depend on the pane being alive
		var next tea.Cmd
		if msg.tick {
			next = m.tick(liveEvery, liveTickMsg{})
		}
		return tea.Batch(next, m.retarget()), same
	case clockMsg:
		now := m.d.now()
		quiet := true
		if !m.toast.until.IsZero() && now.After(m.toast.until) {
			m.toast.text, m.toast.until, quiet = "", time.Time{}, false
		}
		// Elapsed times show seconds only under a minute; longer ones are redrawn every
		// clockCoarse (a minute count may lag that much).
		if m.recent(now) || now.Sub(m.clockAt) >= clockCoarse {
			quiet = false
		}
		if !quiet {
			m.clockAt = now
		}
		return m.tick(clockEvery, clockMsg{}), quiet
	case captureMsg:
		return m.captured(msg)
	case textMsg:
		tv := m.text[msg.tab]
		if tv == nil || msg.key != tv.key {
			return m.ensureText(), true
		}
		tv.inFlight, tv.text, tv.err = false, msg.text, ""
		if msg.err != nil {
			tv.err = msg.err.Error()
		}
		return m.ensureText(), false
	}
	return nil, true
}

// sameLive: no run changed its pane state.
func sameLive(a, b map[string]tmux.Liveness) bool {
	return maps.EqualFunc(a, b, func(x, y tmux.Liveness) bool { return x.State == y.State && x.Pane == y.Pane })
}

// requestLoad starts a snapshot load: at most one in flight (a change seen meanwhile
// loads once more afterwards), and at least loadGap after the previous one.
func (m *Model) requestLoad() tea.Cmd {
	if m.loading.inFlight {
		m.loading.dirty = true
		return nil
	}
	now := m.d.now()
	wait := loadGap - now.Sub(m.loading.last)
	if wait < 0 || m.loading.last.IsZero() {
		wait = 0
	}
	m.loading.inFlight, m.loading.dirty, m.loading.last = true, false, now.Add(wait)
	req := data.Request{ShowClosed: m.tree.ShowClosed, History: m.tree.History()}
	load := func() tea.Msg {
		ctx, cancel := m.job(loadTimeout)
		defer cancel()
		s, err := m.d.load(ctx, req)
		return loadMsg{snap: s, err: err}
	}
	if wait > 0 {
		return m.d.after("load", wait, load)
	}
	return load
}

func (m *Model) loaded(msg loadMsg) tea.Cmd {
	m.loading.inFlight = false
	var cmds []tea.Cmd
	if m.loading.dirty {
		cmds = append(cmds, m.requestLoad())
	}
	if msg.err != nil {
		m.debugf("load failed: %v", msg.err)
		m.loadErr = msg.err.Error()
		return tea.Batch(cmds...)
	}
	m.loadErr = ""
	first := m.snap == nil
	m.snap = msg.snap
	m.snapSeq++
	m.tree.Rebuild(m.snap)
	if first && m.project != "" {
		m.tree.Select("p:" + m.project)
	}
	if m.want != "" && m.tree.Select(m.want) {
		m.want = ""
		m.tree.EnsureVisible(layout(m).tree.H)
	}
	if d := m.dlg; d != nil && d.stale {
		d.stale = false
		if d.refresh != nil {
			d.refresh()
		}
	}
	m.followSelection()
	m.adoptItems()
	m.refreshMenu()
	m.clampAll()
	if !m.liveBusy {
		cmds = append(cmds, m.checkLive(false))
	}
	cmds = append(cmds, m.retarget(), m.ensureText())
	return tea.Batch(cmds...)
}

// checkLive checks the panes of the snapshot's active runs.
func (m *Model) checkLive(fromTick bool) tea.Cmd {
	if m.snap == nil {
		if fromTick {
			return m.tick(liveEvery, liveTickMsg{})
		}
		return nil
	}
	m.liveBusy = true
	runs := m.snap.Runs
	return func() tea.Msg {
		ctx, cancel := m.job(liveTimeout)
		defer cancel()
		return liveMsg{live: m.d.live(ctx, runs), tick: fromTick}
	}
}

// target is the run whose pane the selected row previews, and why there is none.
func (m *Model) target() (*run.Run, string) {
	r := m.tree.Selected()
	if r == nil || m.snap == nil {
		return nil, "nothing selected"
	}
	var rn *run.Run
	switch r.Kind {
	case tree.Run, tree.Coord:
		rn = r.Run
	case tree.Task:
		rn = m.taskRun(r.Task.ID, r.Task.CurrentRunID)
		if rn == nil {
			return nil, "task " + r.Task.ID + " has no run"
		}
	case tree.Project:
		for i := range m.snap.Runs {
			c := &m.snap.Runs[i]
			if c.Project == r.Project && c.Role == "coordinator" && c.Status != run.Ended {
				rn = c
				break
			}
		}
		if rn == nil {
			return nil, "no live coordinator (otag coord " + r.Project + " starts one)"
		}
	default:
		return nil, "select a task or a run"
	}
	return rn, ""
}

// taskRun is the current run of a task, else its newest one in the snapshot.
func (m *Model) taskRun(taskID, current string) *run.Run {
	var newest *run.Run
	for i := range m.snap.Runs {
		r := &m.snap.Runs[i]
		if r.TaskID != taskID {
			continue
		}
		if r.ID == current {
			return r
		}
		if newest == nil {
			newest = r
		}
	}
	return newest
}

// previewable returns the run's pane when it exists and its process lives, else why
// it cannot be shown.
func (m *Model) previewable(r *run.Run) (string, string) {
	switch {
	case r == nil:
		return "", ""
	case r.Status == run.Ended:
		reason := r.EndedReason
		if reason == "" {
			reason = "ended"
		}
		return "", "run " + r.ID + " ended (" + reason + ")"
	case r.Session == nil || r.Session.TmuxPane == "":
		return "", "run " + r.ID + " has no pane yet"
	}
	l, ok := m.live[r.ID]
	switch {
	case !ok:
		return "", "checking the pane…"
	case l.State != tmux.Alive:
		return "", "pane " + r.Session.TmuxPane + " is " + l.State + " (" + l.Detail + ")"
	}
	return r.Session.TmuxPane, ""
}

// retarget points the preview at the selected row's live pane.
func (m *Model) retarget() tea.Cmd {
	r, _ := m.target()
	pane, _ := m.previewable(r)
	t := preview.Target{}
	if pane != "" {
		t = preview.Target{RunID: r.ID, Pane: pane}
	}
	if m.prev.SetTarget(t) {
		m.endLoop()
		m.cancelLock()
	}
	typing := m.checkTyping()
	return tea.Batch(m.syncPreview(), typing)
}

// cancelLock drops a pending scroll-lock request: its scrollback, when it arrives, is
// stale.
func (m *Model) cancelLock() {
	m.cap.lockLines = 0
	m.cap.lockGen++
}

// follow leaves scroll-lock (or a pending request for it): live captures resume.
func (m *Model) follow() tea.Cmd {
	m.prev.Follow()
	m.cancelLock()
	return m.syncPreview()
}

// wantCapture: the Preview tab is visible with a live target, not scroll-locked.
func (m *Model) wantCapture() bool {
	s := layout(m)
	return m.tab == tabs.Preview && !s.tooSmall && s.body.H > 0 && m.prev.Target.Pane != "" &&
		!m.prev.Locked && m.cap.lockLines == 0
}

// syncPreview starts the capture loop when it should run and is not running, and
// ends it when it should not run.
func (m *Model) syncPreview() tea.Cmd {
	want := m.wantCapture()
	switch {
	case want && !m.cap.busy:
		m.cap.busy = true
		return m.capture(0, 0)
	case !want && m.cap.busy:
		m.endLoop()
	}
	return nil
}

// endLoop ends the capture chain: its scheduled capture skips tmux and its result is
// dropped.
func (m *Model) endLoop() {
	m.cap.loop++
	m.cap.cur.Store(m.cap.loop)
	m.cap.busy, m.cap.still = false, 0
}

// capture captures the target after wait; history > 0 fetches scrollback instead.
func (m *Model) capture(wait time.Duration, history int) tea.Cmd {
	loop, pane, cur, gen := m.cap.loop, m.prev.Target.Pane, m.cap.cur, m.cap.lockGen
	do := func() tea.Msg {
		if history == 0 && cur.Load() != loop {
			return captureMsg{loop: loop, pane: pane, stale: true}
		}
		ctx, cancel := m.job(captureTimeout)
		defer cancel()
		v, err := m.d.capture(ctx, pane, history)
		return captureMsg{loop: loop, pane: pane, history: history > 0, lockGen: gen, view: v, err: err}
	}
	if wait <= 0 {
		return do
	}
	return m.d.after("capture", wait, do)
}

func (m *Model) captured(msg captureMsg) (tea.Cmd, bool) {
	if msg.history {
		n := m.cap.lockLines
		if msg.lockGen != m.cap.lockGen || msg.pane != m.prev.Target.Pane || n == 0 {
			return nil, true // followed live or retargeted meanwhile
		}
		m.cap.lockLines = 0
		if msg.err != nil {
			m.flash("scrollback: "+msg.err.Error(), true)
			return m.syncPreview(), false
		}
		m.prev.Lock(msg.view, n, m.bodyHeight())
		return nil, false
	}
	if msg.loop != m.cap.loop {
		return nil, true // an ended chain
	}
	m.cap.busy = false
	changed := false
	if msg.err != nil {
		changed = m.prev.Err != msg.err.Error()
		m.prev.Err = msg.err.Error()
	} else {
		changed = m.prev.Set(msg.view)
	}
	if !m.wantCapture() {
		return nil, !changed
	}
	m.cap.still++
	if changed {
		m.cap.still = 0
	}
	m.cap.busy = true
	return m.capture(m.captureInterval(), 0), !changed
}

// captureInterval is the time to the next capture: 4 Hz, slowed to 1 Hz while the
// terminal has no focus or after stillAfter captures that changed nothing (an idle
// agent); the first change speeds it up again.
func (m *Model) captureInterval() time.Duration {
	if m.typ.on {
		return captureEvery // typing: never slowed down
	}
	if m.blurred || m.cap.still >= stillAfter {
		return captureSlow
	}
	return captureEvery
}

// textKey is what the Details or Stats tab should show now, with the read commands.
func (m *Model) textKey(t tabs.Tab) (string, [][]string) {
	switch t {
	case tabs.Stats:
		return fmt.Sprintf("stats#%d", m.stats), [][]string{{"stats"}, {"quota"}}
	case tabs.Details:
		r := m.tree.Selected()
		if r == nil {
			return "", nil
		}
		seq := fmt.Sprintf("@%d", m.snapSeq)
		switch r.Kind {
		case tree.Run, tree.Coord:
			return r.Key + seq, [][]string{{"run", "show", r.Run.ID}}
		case tree.Task:
			return r.Key + seq, [][]string{{"task", "show", r.Task.ID}}
		case tree.Project:
			return r.Key + seq, [][]string{{"--project", r.Project, "status"}}
		}
	}
	return "", nil
}

// ensureText fetches the visible text tab when what it shows is out of date. One
// fetch per tab is in flight; a result for an older key fetches again.
func (m *Model) ensureText() tea.Cmd {
	tv := m.text[m.tab]
	if tv == nil || m.tooSmall() {
		return nil
	}
	k, cmds := m.textKey(m.tab)
	if k == tv.key || tv.inFlight {
		return nil
	}
	if k == "" {
		tv.key, tv.text, tv.err = "", "", ""
		return nil
	}
	if row, _, _ := strings.Cut(k, "@"); !strings.HasPrefix(tv.key, row+"@") {
		m.scroll[m.tab] = 0 // another row: start at the top
	}
	tv.key, tv.inFlight = k, true
	t := m.tab
	return func() tea.Msg {
		var parts []string
		var err error
		for _, args := range cmds {
			ctx, cancel := m.job(textTimeout)
			out, e := m.d.text(ctx, args...)
			cancel()
			if e != nil && err == nil {
				err = e
			}
			parts = append(parts, strings.TrimRight(out, "\n"))
		}
		return textMsg{tab: t, key: k, text: strings.Join(parts, "\n\n"), err: err}
	}
}

func (m *Model) flash(text string, bad bool) {
	m.toast.text, m.toast.bad, m.toast.until = text, bad, m.d.now().Add(toastFor)
}

func (m *Model) tooSmall() bool { return m.w < minWidth || m.h < minHeight }

// bodyHeight is the height of the right pane's body.
func (m *Model) bodyHeight() int { return layout(m).body.H }

// clampAll keeps the scroll offsets valid after a size or content change.
func (m *Model) clampAll() {
	s := layout(m)
	if s.tree.H > 0 {
		m.tree.Clamp(s.tree.H)
	}
	if m.divider > 0 && !s.narrow {
		m.divider = treeWidth(m.divider, m.w)
	}
}
