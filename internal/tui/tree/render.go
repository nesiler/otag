package tree

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tmux"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/style"
)

// Info is what rendering needs beyond the rows: liveness, open asks and quota, taken
// from one snapshot.
type Info struct {
	Now   time.Time
	Live  map[string]tmux.Liveness // run id → pane liveness
	asks  map[string]bool          // runs with an open ask
	quota map[string]float64       // account (or account/profile) → worst effective used percent
	cool  map[string]bool          // account or account/profile in cooldown
	Theme style.Theme
}

// NewInfo indexes s for rendering.
func NewInfo(s *data.Snapshot, live map[string]tmux.Liveness, now time.Time, th style.Theme) Info {
	in := Info{Now: now, Live: live, asks: map[string]bool{}, quota: map[string]float64{}, cool: map[string]bool{}, Theme: th}
	if s == nil {
		return in
	}
	for _, a := range s.Asks {
		in.asks[a.Asker] = true
	}
	for _, r := range s.Quota {
		if r.Stale && !r.ResetPassed {
			continue
		}
		e := r.Effective()
		if e == nil {
			continue
		}
		k := quotaKey(r.Account, r.Profile)
		in.quota[k] = max(in.quota[k], *e)
	}
	for _, c := range s.Cooldown {
		in.cool[quotaKey(c.Account, c.Profile)] = true
	}
	return in
}

func quotaKey(account, profile string) string {
	if profile == "" {
		return account
	}
	return account + "/" + profile
}

// Quota returns the worst used percent that applies to a run of account and profile,
// and whether a cooldown holds either.
func (in Info) Quota(account, profile string) (float64, bool, bool) {
	q, ok := in.quota[account]
	if p, okp := in.quota[quotaKey(account, profile)]; okp && (!ok || p > q) {
		q, ok = p, true
	}
	return q, ok, in.cool[account] || in.cool[quotaKey(account, profile)]
}

// QuotaWarn is the used percent from which a run row shows its account's quota.
const QuotaWarn = 50.0

// Render returns the visible rows (from Off) for a pane of w x h; focused marks the
// cursor row strongly.
func (m *Model) Render(w, h int, focused bool, in Info) []string {
	var out []string
	for i := m.VisibleOff(h); i < len(m.Rows) && len(out) < h; i++ {
		line := m.Rows[i].line(w, in)
		if i != m.idx && m.Hover != "" && m.Rows[i].Key == m.Hover {
			line = in.Theme.Hover.Render(style.Fit(ansi.Strip(line), w))
		}
		if i == m.idx {
			plain := style.Fit(ansi.Strip(line), w)
			if focused {
				line = in.Theme.Selected.Render(plain)
			} else {
				line = in.Theme.SelectedBlur.Render(plain)
			}
		}
		out = append(out, line)
	}
	return out
}

// ToggleCells is the width of a row's indent and expansion arrow: a click inside
// toggles the row.
func (r *Row) ToggleCells() (from, to int) {
	from = 2 * r.Depth
	return from, from + 2
}

func (r *Row) line(w int, in Info) string {
	th := in.Theme
	arrow := "  "
	if r.CanExpand {
		arrow = style.GlyphFolded + " "
		if r.Expanded {
			arrow = style.GlyphExpanded + " "
		}
	}
	left := strings.Repeat("  ", r.Depth) + arrow
	var right string
	switch r.Kind {
	case Project:
		left += th.Bold.Render(r.Project) + "  " + th.Dim.Render(r.Proj.Guard)
		right = th.Dim.Render(fmt.Sprintf("%d open · %d ready", r.Count, r.Ready))
	case Coord:
		g, s := runGlyph(r.Run, in)
		left += s.Render(g) + " coord " + th.Dim.Render(r.Run.Profile)
		right = runRight(r.Run, in)
	case Deliver:
		left += th.Accent.Render(fmt.Sprintf("%s Ready to deliver (%d)", style.GlyphDeliver, r.Count))
	case Task:
		g, s := taskGlyph(r.Task, in)
		left += s.Render(g) + " " + r.Task.ID + " " + r.Task.Title
		right = r.Task.Status
		if r.Task.Priority != 0 {
			right += fmt.Sprintf(" p%d", r.Task.Priority)
		}
		if t, err := time.Parse(time.RFC3339, r.Task.StatusChangedAt); err == nil {
			right += " " + style.Dur(in.Now.Sub(t))
		}
		right = th.Dim.Render(right)
	case Run:
		g, s := runGlyph(r.Run, in)
		left += s.Render(g) + " " + RunLabel(r.Run) + " " + th.Dim.Render(r.Run.Profile)
		right = runRight(r.Run, in)
	case Earlier:
		sign := "+"
		if r.Expanded {
			sign = "−"
		}
		left = strings.Repeat("  ", r.Depth) + "  " + th.Dim.Render(fmt.Sprintf("%s%d earlier", sign, r.Count))
	}
	return style.Join(left, right, w)
}

func runRight(r *run.Run, in Info) string {
	th := in.Theme
	parts := []string{}
	status := r.Status
	if r.Status == run.Ended && r.EndedReason != "" {
		status = "ended " + r.EndedReason
	}
	parts = append(parts, th.Dim.Render(status))
	if d, ok := runElapsed(r, in.Now); ok {
		parts = append(parts, th.Dim.Render(style.Dur(d)))
	}
	if r.Status != run.Ended {
		if q, ok, cool := in.Quota(r.Account, r.Profile); cool || (ok && q >= QuotaWarn) {
			badge := fmt.Sprintf("q%.0f%%", q)
			if !ok {
				badge = "q"
			}
			if cool {
				badge += " cooldown"
			}
			if cool || q >= run.DefaultQuotaThreshold {
				badge = th.Bad.Render(badge)
			} else {
				badge = th.Warn.Render(badge)
			}
			parts = append(parts, badge)
		}
		if dead(r, in) {
			parts = append(parts, th.Bad.Render("DEAD"))
		}
	}
	return strings.Join(parts, " ")
}

// runElapsed is the time since the run started; for an ended run, how long it ran.
func runElapsed(r *run.Run, now time.Time) (time.Duration, bool) {
	start, err := time.Parse(time.RFC3339, r.StartedAt)
	if err != nil {
		if start, err = time.Parse(time.RFC3339, r.QueuedAt); err != nil {
			return 0, false
		}
	}
	if r.Status == run.Ended {
		end, err := time.Parse(time.RFC3339, r.EndedAt)
		if err != nil {
			return 0, false
		}
		return end.Sub(start), true
	}
	return now.Sub(start), true
}

// dead: the run should be working but its pane is gone or exited.
func dead(r *run.Run, in Info) bool {
	l, ok := in.Live[r.ID]
	if !ok || r.Status == run.Ended || r.Status == run.Queued || r.Status == run.Parked {
		return false
	}
	return l.State == tmux.Gone || l.State == tmux.Exited
}

func runGlyph(r *run.Run, in Info) (string, lipgloss.Style) {
	th := in.Theme
	if dead(r, in) {
		return style.GlyphFailed, th.Bad
	}
	switch r.Status {
	case run.Running, run.Starting:
		return style.GlyphRunning, th.Good
	case run.Idle:
		return style.GlyphIdle, th.Good
	case run.Waiting:
		if in.asks[r.ID] {
			return style.GlyphQuestion, th.Warn
		}
		return style.GlyphDialog, th.Warn
	case run.Parked:
		return style.GlyphParked, th.Dim
	case run.Queued:
		return style.GlyphQueued, th.Dim
	case run.Stopping:
		return style.GlyphStopped, th.Dim
	}
	switch r.EndedReason { // ended
	case run.ReasonDone:
		return style.GlyphDone, th.Good
	case run.ReasonStopped, run.ReasonExited, run.ReasonSuperseded:
		return style.GlyphStopped, th.Dim
	}
	return style.GlyphFailed, th.Bad
}

func taskGlyph(t *task.Task, in Info) (string, lipgloss.Style) {
	th := in.Theme
	switch t.Status {
	case task.Waiting:
		return style.GlyphQuestion, th.Warn
	case task.Blocked:
		return style.GlyphBlocked, th.Bad
	case task.Review:
		return style.GlyphReview, th.Accent
	case task.Running, task.Claimed:
		return style.GlyphRunning, th.Good
	case task.Ready:
		return style.GlyphReady, lipgloss.NewStyle()
	case task.Draft:
		return style.GlyphDraft, th.Dim
	case task.Done:
		return style.GlyphDeliver, th.Accent
	case task.Delivered:
		return style.GlyphDone, th.Dim
	}
	return style.GlyphStopped, th.Dim // cancelled
}

// RunGlyph is the glyph of a run row (the preview title shows it too).
func RunGlyph(r *run.Run, in Info) string { g, s := runGlyph(r, in); return s.Render(g) }
