// Package tree flattens a data.Snapshot into the rows of the TUI's left pane:
// projects, their coordinator, the "Ready to deliver" group, tasks by attention, review
// tasks under the task they review, and runs under their task. The selection is kept
// by a stable key, so a refresh never moves the cursor.
package tree

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/nesiler/otag/internal/registry"
	"github.com/nesiler/otag/internal/run"
	"github.com/nesiler/otag/internal/task"
	"github.com/nesiler/otag/internal/tui/data"
)

// Kind is a row kind.
type Kind int

// Row kinds.
const (
	Project Kind = iota
	Coord        // the project's live coordinator
	Deliver      // "Ready to deliver" group of a project
	Task
	Run
	Earlier // "+N earlier": the older runs of a task
)

// Row is one line of the tree.
type Row struct {
	Key     string // p:<project> c:<project> g:deliver:<project> t:<task> r:<run> h:<task>
	Kind    Kind
	Depth   int
	Project string
	Proj    *registry.ProjectRow // Project
	Task    *task.Task           // Task
	Run     *run.Run             // Run, Coord
	// Count is the open tasks of a Project, the tasks of a Deliver group and the older
	// runs behind an Earlier row; Ready is the ready tasks of a Project.
	Count, Ready int
	CanExpand    bool
	Expanded     bool
}

// Model is the tree's state: rows, the selected key, the expansion and the scroll.
type Model struct {
	Rows       []Row
	Off        int  // first visible row
	ShowClosed bool // closed tasks are listed (a reload is needed after a change)
	// Filter keeps the rows whose id, title or profile contain it (case-insensitive) and
	// their ancestors; every row is expanded meanwhile.
	Filter   string
	Hover    string // the key of the row under the pointer (hover is opt-in), else ""
	cursor   string
	idx      int
	expanded map[string]bool // explicit choices; missing keys use the default
}

// New returns an empty tree.
func New() Model { return Model{expanded: map[string]bool{}} }

// Cursor returns the selected key.
func (m *Model) Cursor() string { return m.cursor }

// Index returns the selected row's index (-1: no rows).
func (m *Model) Index() int {
	if len(m.Rows) == 0 {
		return -1
	}
	return m.idx
}

// Selected returns the selected row, nil without rows.
func (m *Model) Selected() *Row {
	if len(m.Rows) == 0 {
		return nil
	}
	return &m.Rows[m.idx]
}

// Select moves the cursor to key; false when no row has it.
func (m *Model) Select(key string) bool {
	for i, r := range m.Rows {
		if r.Key == key {
			m.idx, m.cursor = i, key
			return true
		}
	}
	return false
}

// SelectIndex moves the cursor to row i (clamped).
func (m *Model) SelectIndex(i int) {
	if len(m.Rows) == 0 {
		m.idx, m.cursor = 0, ""
		return
	}
	m.idx = min(max(i, 0), len(m.Rows)-1)
	m.cursor = m.Rows[m.idx].Key
}

// Move moves the cursor by delta rows.
func (m *Model) Move(delta int) { m.SelectIndex(m.idx + delta) }

// SetExpanded expands or collapses key. It returns false when the row cannot expand
// or already is in that state.
func (m *Model) SetExpanded(key string, on bool) bool {
	for _, r := range m.Rows {
		if r.Key == key {
			cur := r.Expanded
			if v, ok := m.expanded[key]; ok {
				cur = v // set since the last Rebuild
			}
			if !r.CanExpand || cur == on {
				return false
			}
			m.expanded[key] = on
			return true
		}
	}
	return false
}

// History lists the tasks whose "+N earlier" row is expanded: their full run history
// must be loaded (data.Request.History).
func (m *Model) History() []string {
	var out []string
	for k, on := range m.expanded {
		if on && len(k) > 2 && k[:2] == "h:" {
			out = append(out, k[2:])
		}
	}
	slices.Sort(out)
	return out
}

// Expansion returns the explicit expansion choices (not the "+N earlier" rows, which
// need a load) for the UI state file.
func (m *Model) Expansion() map[string]bool {
	out := map[string]bool{}
	for k, on := range m.expanded {
		if !strings.HasPrefix(k, "h:") {
			out[k] = on
		}
	}
	return out
}

// SetExpansion replaces the explicit expansion choices (before the first Rebuild).
func (m *Model) SetExpansion(e map[string]bool) {
	m.expanded = map[string]bool{}
	for k, on := range e {
		m.expanded[k] = on
	}
}

// Parent returns the index of row i's parent row (-1: none).
func (m *Model) Parent(i int) int {
	if i < 0 || i >= len(m.Rows) {
		return -1
	}
	for j := i - 1; j >= 0; j-- {
		if m.Rows[j].Depth < m.Rows[i].Depth {
			return j
		}
	}
	return -1
}

// Rebuild replaces the rows from s, keeping the selected key. When that row is gone
// the cursor stays at the same index.
func (m *Model) Rebuild(s *data.Snapshot) {
	if m.expanded == nil {
		m.expanded = map[string]bool{}
	}
	m.Rows = build(s, m.expanded, m.ShowClosed, m.Filter != "")
	if m.Filter != "" {
		m.Rows = filterRows(m.Rows, strings.ToLower(m.Filter))
	}
	if m.cursor == "" || !m.Select(m.cursor) {
		m.SelectIndex(m.idx)
	}
}

// EnsureVisible scrolls so the cursor is inside a window of h rows.
func (m *Model) EnsureVisible(h int) {
	if h <= 0 {
		return
	}
	if m.idx < m.Off {
		m.Off = m.idx
	}
	if m.idx >= m.Off+h {
		m.Off = m.idx - h + 1
	}
	m.Clamp(h)
}

// Clamp keeps the scroll offset inside the rows for a window of h rows.
func (m *Model) Clamp(h int) { m.Off = m.VisibleOff(h) }

// VisibleOff is the scroll offset clamped for a window of h rows.
func (m *Model) VisibleOff(h int) int { return max(0, min(m.Off, len(m.Rows)-h)) }

// attention orders open task statuses: what needs a person first.
func attention(status string) int {
	switch status {
	case task.Waiting, task.Blocked, task.Review:
		return 0
	case task.Running, task.Claimed:
		return 1
	case task.Ready:
		return 2
	case task.Draft:
		return 3
	}
	return 4
}

func byAttention(a, b *task.Task) int {
	return cmp.Or(cmp.Compare(attention(a.Status), attention(b.Status)), cmp.Compare(b.Priority, a.Priority),
		cmp.Compare(a.ID, b.ID))
}

func build(s *data.Snapshot, expanded map[string]bool, showClosed, all bool) []Row {
	if s == nil {
		return nil
	}
	tasks := map[string]*task.Task{}
	var open, deliver, closed []*task.Task
	add := func(list []task.Task, into *[]*task.Task) {
		for i := range list {
			t := &list[i]
			if tasks[t.ID] != nil {
				continue
			}
			tasks[t.ID] = t
			*into = append(*into, t)
		}
	}
	add(s.Open, &open)
	add(s.Deliverable, &deliver)
	if showClosed {
		add(s.Closed, &closed)
	}
	reviews := map[string][]*task.Task{} // reviewed task → its review tasks
	nested := map[string]bool{}
	for _, l := range [][]*task.Task{open, deliver, closed} {
		for _, t := range l {
			if p := s.ReviewOf[t.ID]; p != "" && tasks[p] != nil && p != t.ID {
				reviews[p] = append(reviews[p], t)
				nested[t.ID] = true
			}
		}
	}
	runs := map[string][]*run.Run{} // task → newest first
	coords := map[string]*run.Run{} // project → newest live coordinator
	for i := range s.Runs {
		r := &s.Runs[i]
		if r.TaskID != "" {
			runs[r.TaskID] = append(runs[r.TaskID], r)
		} else if r.Role == "coordinator" && r.Status != run.Ended && coords[r.Project] == nil {
			coords[r.Project] = r
		}
	}

	var rows []Row
	isOpen := func(key string, def bool) bool {
		if all && !strings.HasPrefix(key, "h:") {
			return true // filtering: the matches are found wherever they are
		}
		if on, ok := expanded[key]; ok {
			return on
		}
		return def
	}
	placed := map[string]bool{} // guards against a review_of cycle
	var addTask func(t *task.Task, depth int)
	addTask = func(t *task.Task, depth int) {
		if placed[t.ID] {
			return
		}
		placed[t.ID] = true
		rs := runs[t.ID]
		kids := reviews[t.ID]
		slices.SortFunc(kids, byAttention)
		key := "t:" + t.ID
		can := len(rs) > 0 || len(kids) > 0
		def := len(kids) > 0 || (len(rs) > 0 && rs[0].Status != run.Ended)
		exp := can && isOpen(key, def)
		rows = append(rows, Row{Key: key, Kind: Task, Depth: depth, Project: t.Project, Task: t, CanExpand: can, Expanded: exp})
		if !exp {
			return
		}
		if len(rs) > 0 {
			newest := rs[0]
			rows = append(rows, Row{Key: "r:" + newest.ID, Kind: Run, Depth: depth + 1, Project: t.Project, Run: newest})
			if n := s.RunCount[t.ID] - 1; n > 0 {
				hk := "h:" + t.ID
				hexp := isOpen(hk, false)
				rows = append(rows, Row{Key: hk, Kind: Earlier, Depth: depth + 1, Project: t.Project, Count: n,
					CanExpand: true, Expanded: hexp})
				if hexp {
					for i := range s.History[t.ID] {
						r := &s.History[t.ID][i]
						if r.ID != newest.ID {
							rows = append(rows, Row{Key: "r:" + r.ID, Kind: Run, Depth: depth + 2, Project: t.Project, Run: r})
						}
					}
				}
			}
		}
		for _, k := range kids {
			addTask(k, depth+1)
		}
	}

	for i := range s.Projects {
		p := &s.Projects[i]
		var pOpen, pDeliver, pClosed []*task.Task
		ready := 0
		for _, t := range open {
			if t.Project != p.Name {
				continue
			}
			if t.Status == task.Ready {
				ready++
			}
			if !nested[t.ID] {
				pOpen = append(pOpen, t)
			}
		}
		for _, t := range deliver {
			if t.Project == p.Name && !nested[t.ID] {
				pDeliver = append(pDeliver, t)
			}
		}
		for _, t := range closed {
			if t.Project == p.Name && !nested[t.ID] {
				pClosed = append(pClosed, t)
			}
		}
		slices.SortFunc(pOpen, byAttention)
		slices.SortFunc(pDeliver, func(a, b *task.Task) int {
			return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(a.ID, b.ID))
		})
		slices.SortFunc(pClosed, func(a, b *task.Task) int {
			return cmp.Or(cmp.Compare(b.StatusChangedAt, a.StatusChangedAt), cmp.Compare(a.ID, b.ID))
		})
		count := 0
		for _, t := range open {
			if t.Project == p.Name {
				count++
			}
		}
		key := "p:" + p.Name
		exp := isOpen(key, true)
		rows = append(rows, Row{Key: key, Kind: Project, Project: p.Name, Proj: p, Count: count, Ready: ready,
			CanExpand: true, Expanded: exp})
		if !exp {
			continue
		}
		if c := coords[p.Name]; c != nil {
			rows = append(rows, Row{Key: "c:" + p.Name, Kind: Coord, Depth: 1, Project: p.Name, Run: c})
		}
		if len(pDeliver) > 0 {
			gk := "g:deliver:" + p.Name
			gexp := isOpen(gk, true)
			rows = append(rows, Row{Key: gk, Kind: Deliver, Depth: 1, Project: p.Name, Count: len(pDeliver),
				CanExpand: true, Expanded: gexp})
			if gexp {
				for _, t := range pDeliver {
					addTask(t, 2)
				}
			}
		}
		for _, t := range pOpen {
			addTask(t, 1)
		}
		for _, t := range pClosed {
			addTask(t, 1)
		}
	}
	return rows
}

// RunLabel is the short name of a run under its task: r<attempt>, or "coord".
func RunLabel(r *run.Run) string {
	if r.Role == "coordinator" {
		return "coord"
	}
	return "r" + strconv.Itoa(r.Attempt)
}

// matches reports whether the row's id, title or profile contain q (lower case).
func (r *Row) matches(q string) bool {
	var text string
	switch r.Kind {
	case Project:
		text = r.Project
	case Coord:
		text = "coord " + r.Run.ID + " " + r.Run.Profile
	case Task:
		text = r.Task.ID + " " + r.Task.Title + " " + r.Task.Profile
	case Run:
		text = r.Run.ID + " " + r.Run.Profile
	}
	return strings.Contains(strings.ToLower(text), q)
}

// filterRows keeps the rows that match q and the ancestors of those.
func filterRows(rows []Row, q string) []Row {
	keep := make([]bool, len(rows))
	for i := range rows {
		if !rows[i].matches(q) {
			continue
		}
		keep[i] = true
		for d, j := rows[i].Depth, i-1; j >= 0 && d > 0; j-- {
			if rows[j].Depth < d {
				keep[j], d = true, rows[j].Depth
			}
		}
	}
	var out []Row
	for i, r := range rows {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out
}
