package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/attach"
	"github.com/nesiler/otag/internal/tui/tabs"
	"github.com/nesiler/otag/internal/tui/tree"
	"github.com/nesiler/otag/internal/tui/typing"
)

// Typing mode (path 2) and the full-screen attach (path 3): docs/TUI.md.

const (
	burstDelay = 40 * time.Millisecond // a capture this long after a typed key...
	burstGap   = 50 * time.Millisecond // ...at most 20 times a second
	typeWait   = 10 * time.Second      // [Type] of a result dialog waits this long for the pane
)

// keySender delivers typed keys to panes (typing.Sender; tests fake it).
type keySender interface {
	Send(op typing.Op) bool
	Results() <-chan typing.Result
	Gen() uint64    // the current generation
	Cancel() uint64 // ends it: what is queued of it is skipped; returns the new one
}

// typingState is typing mode: the keys go to one agent pane.
type typingState struct {
	on        bool
	run, pane string
	pid       int    // the pane's process: checked before every send (0: the tag only)
	gen       uint64 // the sender generation of this typing session
	s         keySender
	pending   int  // ops queued without a result yet
	listening bool // a Cmd waits for the next result
	burstAt   time.Time
	skipped   int  // ops not sent because typing mode ended first
	failed    bool // a send failed: its toast stays
	// attachAfter is an attach waiting for the queue to drain: the terminal never goes
	// to tmux while typed keys may still be sent.
	attachAfter *attach.Target
	readyAfter  *attachReadyMsg // a planned attach, held the same way
	// want enters typing mode once run want's pane shows in the preview, until
	// wantUntil ([Type] of a result dialog: the new run's row is not loaded yet).
	want      string
	wantUntil time.Time
}

type (
	sentMsg        struct{ res typing.Result }
	attachReadyMsg struct {
		t    attach.Target
		plan attach.Plan
		warn error // the tmux bindings could not be applied
	}
	attachDoneMsg struct {
		t    attach.Target
		mode attach.Mode
		warn error
		err  error
	}
)

// enterTyping starts typing mode into the preview's pane. The Preview tab is shown
// and focused; without a live pane it says why.
func (m *Model) enterTyping() tea.Cmd {
	if m.dlg != nil {
		return nil
	}
	if m.attaching {
		// The terminal is being handed to tmux: no new keys may queue up meanwhile.
		m.flash("typing: an attach is starting", true)
		return nil
	}
	m.help, m.menu = false, nil
	m.focus = focusPane
	if layout(m).narrow {
		m.narrowPane = true
	}
	var cmds []tea.Cmd
	if m.tab != tabs.Preview {
		cmds = append(cmds, m.setTab(tabs.Preview))
	}
	if m.prev.Target.Pane == "" {
		r, why := m.target()
		if _, reason := m.previewable(r); reason != "" {
			why = reason
		}
		m.flash("typing: no live pane: "+why, true)
		return tea.Batch(cmds...)
	}
	if m.prev.Locked || m.cap.lockLines > 0 {
		cmds = append(cmds, m.follow())
	}
	if m.typ.s == nil {
		m.typ.s = m.d.sender(m.ctx)
	}
	m.debugf("typing on run=%s", m.prev.Target.RunID)
	m.typ.on, m.typ.run, m.typ.pane, m.typ.want = true, m.prev.Target.RunID, m.prev.Target.Pane, ""
	m.typ.pid, m.typ.gen, m.typ.skipped, m.typ.failed = m.live[m.typ.run].PID, m.typ.s.Gen(), 0, false
	if m.typ.pid == 0 {
		if r := m.findRun(m.typ.run); r != nil && r.Session != nil {
			m.typ.pid = r.Session.PID
		}
	}
	return tea.Batch(append(cmds, m.burst())...)
}

// leaveTyping ends typing mode; why, when not empty, is a toast. Keys still queued are
// not sent (their generation ends). A dialog that waited for it opens.
func (m *Model) leaveTyping(why string, bad bool) {
	if !m.typ.on {
		return
	}
	m.debugf("typing off")
	m.typ.on = false
	m.typ.s.Cancel()
	if why != "" {
		m.flash(why, bad)
	}
	if m.dlg == nil && len(m.queued) > 0 {
		m.dlg, m.queued = m.queued[0], m.queued[1:]
	}
}

// typeKey sends a key press to the agent; only C-q and M-q stay with the TUI.
func (m *Model) typeKey(k tea.KeyPressMsg) tea.Cmd {
	key := tea.Key(k)
	if typing.IsReturnKey(key) {
		m.leaveTyping("", false)
		return nil
	}
	op, ok, why := typing.Encode(key)
	if !ok {
		m.flash(k.String()+": not sent ("+why+")", true)
		return nil
	}
	return m.sendOp(op)
}

// typePaste sends a paste to the agent.
func (m *Model) typePaste(s string) tea.Cmd {
	if s = typing.CleanPaste(s); s == "" {
		return nil
	}
	return m.sendOp(typing.Op{Kind: typing.Paste, Text: s})
}

// sendOp queues op for the pane being typed into.
func (m *Model) sendOp(op typing.Op) tea.Cmd {
	var cmds []tea.Cmd
	if m.prev.Locked || m.cap.lockLines > 0 {
		cmds = append(cmds, m.follow()) // a key brings the live screen back, as in a terminal
	}
	op.Pane, op.Run, op.PID, op.Gen = m.typ.pane, m.typ.run, m.typ.pid, m.typ.gen
	if !m.typ.s.Send(op) {
		// The keys after a lost one must not follow it: an Enter would submit a text
		// with a hole.
		m.leaveTyping("typing ended: the pane does not keep up, a key was lost", true)
		return tea.Batch(cmds...)
	}
	m.typ.pending++
	if !m.typ.listening {
		m.typ.listening = true
		cmds = append(cmds, m.listen())
	}
	return tea.Batch(cmds...)
}

// listen waits for the next result of the sender.
func (m *Model) listen() tea.Cmd {
	s, ctx := m.typ.s, m.ctx
	return func() tea.Msg {
		select {
		case r := <-s.Results():
			return sentMsg{r}
		case <-ctx.Done():
			return nil
		}
	}
}

// sent takes a result of the sender. A failed send ends typing mode (the sender has
// dropped the rest: more text or an Enter would make an incomplete input); a sent key
// captures the pane soon (the burst). Once the queue is empty, it says how many keys
// were not sent, and an attach that waited for it runs.
func (m *Model) sent(msg sentMsg) tea.Cmd {
	res := msg.res
	n := max(1, res.N)
	m.typ.pending = max(0, m.typ.pending-n)
	var cmds []tea.Cmd
	switch {
	case res.Err != nil && res.Op.Gen == m.typ.gen:
		// This session's failure: it ends typing mode, once.
		if !m.typ.failed {
			m.typ.failed = true
			m.leaveTyping("", false)
			m.flash("typing stopped, the rest was not sent: "+res.Err.Error(), true)
		}
	case res.Err != nil:
		// An earlier session's op that did not go out: counted, it ends nothing now.
		m.typ.skipped += n
	case res.Skipped:
		m.typ.skipped += n
	default:
		cmds = append(cmds, m.burst())
	}
	if m.typ.pending > 0 {
		return tea.Batch(append(cmds, m.listen())...)
	}
	m.typ.listening = false
	if m.typ.skipped > 0 && !m.typ.failed {
		m.flash(fmt.Sprintf("typing: %d key(s) not sent (typing mode ended first)", m.typ.skipped), true)
	}
	m.typ.skipped = 0
	if t := m.typ.attachAfter; t != nil {
		m.typ.attachAfter = nil
		cmds = append(cmds, m.prepareAttach(*t))
	}
	if r := m.typ.readyAfter; r != nil {
		m.typ.readyAfter = nil
		cmds = append(cmds, m.attachReady(*r))
	}
	return tea.Batch(cmds...)
}

// burst captures the typed-into pane burstDelay from now, at most every burstGap: the
// chain restarts with a short first wait.
func (m *Model) burst() tea.Cmd {
	if !m.typ.on || !m.wantCapture() {
		return nil
	}
	now := m.d.now()
	if !m.typ.burstAt.IsZero() && now.Sub(m.typ.burstAt) < burstGap {
		return nil
	}
	m.typ.burstAt = now
	m.endLoop()
	m.cap.busy = true
	return m.capture(burstDelay, 0)
}

// checkTyping follows a retarget: typing mode ends when its pane is no longer the
// preview's; a waiting [Type] starts once its run's pane shows.
func (m *Model) checkTyping() tea.Cmd {
	t := m.prev.Target
	if m.typ.on && (t.Pane != m.typ.pane || t.RunID != m.typ.run) {
		m.leaveTyping("typing ended: the pane of "+m.typ.run+" is gone", true)
	}
	if w := m.typ.want; w != "" && !m.typ.on {
		switch {
		case m.d.now().After(m.typ.wantUntil):
			m.typ.want = ""
		case t.RunID == w && t.Pane != "" && m.tab == tabs.Preview && m.dlg == nil:
			return m.enterTyping()
		}
	}
	return nil
}

// typeInto is [Type] of a result dialog: select the run and type into its pane once
// it shows.
func (m *Model) typeInto(runID, rowKey string) tea.Cmd {
	m.typ.want, m.typ.wantUntil = runID, m.d.now().Add(typeWait)
	if m.tree.Select(rowKey) {
		m.want = ""
	} else {
		m.want = rowKey
	}
	m.focus = focusPane
	return tea.Batch(m.setTab(tabs.Preview), m.selected())
}

// attachable is the run a row attaches to: a run or coordinator row, or a task row's
// run, when its pane is live.
func (m *Model) attachable(r *tree.Row) *run.Run {
	if r == nil || m.snap == nil {
		return nil
	}
	var rn *run.Run
	switch r.Kind {
	case tree.Run, tree.Coord:
		rn = r.Run
	case tree.Task:
		rn = m.taskRun(r.Task.ID, r.Task.CurrentRunID)
	}
	if pane, _ := m.previewable(rn); pane == "" {
		return nil
	}
	return rn
}

// attachRun attaches to r's window when its pane is live, else says why.
func (m *Model) attachRun(r *run.Run) tea.Cmd {
	pane, why := m.previewable(r)
	if r == nil {
		why = "select a run"
	}
	if pane == "" {
		m.flash("attach: "+why, true)
		return nil
	}
	return m.attachTo(attach.Target{Run: r.ID, Session: r.Session.TmuxSession, Pane: pane})
}

// attachResult is [Attach] of a result dialog: the run of the snapshot, else the one
// the command returned (its row may not be loaded yet).
func (m *Model) attachResult(id string, fallback *run.Run) tea.Cmd {
	r := m.findRun(id)
	if r == nil {
		r = fallback
	}
	if r == nil || r.Session == nil || r.Session.TmuxPane == "" {
		m.flash("attach: run "+id+" has no pane", true)
		return nil
	}
	if l, ok := m.live[r.ID]; ok && l.State != tmux.Alive {
		m.flash("attach: pane "+r.Session.TmuxPane+" is "+l.State, true)
		return nil
	}
	return m.attachTo(attach.Target{Run: r.ID, Session: r.Session.TmuxSession, Pane: r.Session.TmuxPane})
}

// attachTo applies the tmux bindings and plans the attach in a Cmd; attachReady runs
// it. One attach at a time.
func (m *Model) attachTo(t attach.Target) tea.Cmd {
	if m.attaching {
		return nil
	}
	m.attaching = true
	m.leaveTyping("", false)
	if m.typ.pending > 0 {
		m.typ.attachAfter = &t // after the queue drains (sent)
		return nil
	}
	return m.prepareAttach(t)
}

// prepareAttach applies the bindings and plans the attach in a Cmd.
func (m *Model) prepareAttach(t attach.Target) tea.Cmd {
	prepare, ctx := m.d.prepare, m.ctx
	return func() tea.Msg {
		plan, warn := prepare(ctx, t)
		return attachReadyMsg{t: t, plan: plan, warn: warn}
	}
}

// attachReady suspends the TUI and runs the attach.
func (m *Model) attachReady(msg attachReadyMsg) tea.Cmd {
	// Never attached with keys that may still be sent: wait for the queue to drain.
	if m.typ.on || m.typ.pending > 0 {
		m.leaveTyping("", false)
		m.typ.readyAfter = &msg
		return nil
	}
	t, mode, warn := msg.t, msg.plan.Mode, msg.warn
	cmd, stderr := msg.plan.Command()
	return m.d.exec(cmd, func(err error) tea.Msg {
		return attachDoneMsg{t: t, mode: mode, warn: warn, err: stderr.Err(err)}
	})
}

// attachDone is the return from an attach: a full redraw, a reload, a liveness check
// and a fresh capture (the screen the TUI kept is from before).
func (m *Model) attachDone(msg attachDoneMsg) tea.Cmd {
	m.attaching = false
	m.debugf("attach %s mode=%s err=%v", msg.t.Run, msg.mode, msg.err)
	switch {
	case msg.err != nil:
		m.flash("attach "+msg.t.Run+" ("+msg.mode.String()+"): "+msg.err.Error(), true)
	case msg.warn != nil:
		m.flash("tmux bindings: "+msg.warn.Error(), true)
	}
	m.endLoop()
	cmds := []tea.Cmd{tea.ClearScreen, m.requestLoad(), m.syncPreview()}
	if !m.liveBusy {
		cmds = append(cmds, m.checkLive(false))
	}
	return tea.Batch(cmds...)
}

// resultRun is the run an action returned (run start, coord): its data's "run".
func resultRun(o Outcome) *run.Run {
	if o.Data == nil {
		return nil
	}
	b, err := json.Marshal(o.Data)
	if err != nil {
		return nil
	}
	var v struct {
		Run *run.Run `json:"run"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v.Run
}

// execProcess is tea.ExecProcess (tests replace deps.exec).
func execProcess(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd { return tea.ExecProcess(c, fn) }

// tmuxSender is the real sender: one tmux invocation per op.
func tmuxSender(tc *tmux.Client, buffer string) func(ctx context.Context) keySender {
	return func(ctx context.Context) keySender {
		return typing.NewSender(ctx, func(ctx context.Context, op typing.Op) error {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			// One tmux invocation checks that the pane still belongs to the run and sends:
			// nothing can come between the two.
			g := tmux.Guard{Pane: op.Pane, Run: op.Run, PID: op.PID}
			switch op.Kind {
			case typing.Literal:
				return tc.SendText(ctx, g, buffer, op.Text, false)
			case typing.Paste:
				return tc.SendText(ctx, g, buffer, op.Text, true)
			}
			return tc.SendKey(ctx, g, op.Text)
		})
	}
}
