package quota

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Rollout reading bounds.
const (
	rolloutFiles    = 5       // newest rollout files looked at
	rolloutDayDirs  = 3       // newest sessions/YYYY/MM/DD directories searched
	rolloutTail     = 2 << 20 // first read of a file: its last 2 MiB
	rolloutMaxRead  = 8 << 20 // per file and call; more unread data → jump to the tail
	rolloutMaxLine  = 4 << 20 // a longer partial line is dropped
	codexMainBucket = "codex" // limit_id of the account's subscription windows
)

// CodexLimits is the latest main-bucket rate_limits found in a rollout.
type CodexLimits struct {
	At      time.Time
	Windows []Window
}

// rolloutCursor remembers how far a rollout file was read.
type rolloutCursor struct {
	offset  int64
	pending []byte
	last    *CodexLimits
}

// RolloutReader tails the newest Codex rollout files of one CODEX_HOME, reading each
// file only once (offsets are cached), and returns the newest main-bucket rate limits.
// It is not safe for concurrent use.
type RolloutReader struct {
	Home    string
	cursors map[string]*rolloutCursor
}

// Latest returns the newest rate limits seen in the newest rollout files (nil if none).
func (r *RolloutReader) Latest() (*CodexLimits, error) {
	if r.cursors == nil {
		r.cursors = map[string]*rolloutCursor{}
	}
	files, err := newestRollouts(filepath.Join(r.Home, "sessions"))
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	var best *CodexLimits
	for _, f := range files {
		keep[f] = true
		c := r.cursors[f]
		if c == nil {
			c = &rolloutCursor{offset: -1}
			r.cursors[f] = c
		}
		c.read(f)
		if c.last != nil && (best == nil || c.last.At.After(best.At)) {
			best = c.last
		}
	}
	for f := range r.cursors {
		if !keep[f] {
			delete(r.cursors, f)
		}
	}
	return best, nil
}

func (c *rolloutCursor) read(path string) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return
	}
	size := st.Size()
	if c.offset > size { // truncated or replaced
		c.offset, c.pending = -1, nil
	}
	skipFirst := false
	if c.offset < 0 || size-c.offset > rolloutMaxRead {
		c.offset, c.pending = max(0, size-rolloutTail), nil
		skipFirst = c.offset > 0
	}
	if c.offset == size {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.NewSectionReader(f, c.offset, size-c.offset))
	if err != nil {
		return
	}
	c.offset += int64(len(buf))
	data := append(c.pending, buf...)
	if skipFirst {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		if l := ParseRolloutLine(data[:i]); l != nil && (c.last == nil || !l.At.Before(c.last.At)) {
			c.last = l
		}
		data = data[i+1:]
	}
	c.pending = nil
	if len(data) <= rolloutMaxLine {
		c.pending = append([]byte(nil), data...)
	}
}

// ParseRolloutLine returns the main-bucket rate limits of one rollout line
// (event_msg/token_count with rate_limits.limit_id "codex" or empty and at least one
// window), or nil. Other buckets (e.g. "premium" with null windows) are skipped.
func ParseRolloutLine(line []byte) *CodexLimits {
	if !bytes.Contains(line, []byte(`"token_count"`)) || !bytes.Contains(line, []byte(`"rate_limits"`)) {
		return nil
	}
	type win struct {
		Used          *float64 `json:"used_percent"`
		WindowMinutes int64    `json:"window_minutes"`
		ResetsAt      float64  `json:"resets_at"`
		ResetsIn      float64  `json:"resets_in_seconds"`
	}
	var fr struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type   string `json:"type"`
			Limits *struct {
				LimitID   string `json:"limit_id"`
				Primary   *win   `json:"primary"`
				Secondary *win   `json:"secondary"`
				Reached   string `json:"rate_limit_reached_type"`
				PlanType  string `json:"plan_type"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &fr) != nil || fr.Type != "event_msg" || fr.Payload.Type != "token_count" {
		return nil
	}
	rl := fr.Payload.Limits
	if rl == nil || (rl.LimitID != "" && rl.LimitID != codexMainBucket) || (rl.Primary == nil && rl.Secondary == nil) {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, fr.Timestamp)
	if err != nil {
		return nil
	}
	out := &CodexLimits{At: at.UTC()}
	worst := -1
	for i, w := range []*win{rl.Primary, rl.Secondary} {
		if w == nil || w.Used == nil {
			continue
		}
		x := Window{Name: WindowName(w.WindowMinutes), UsedPercent: ptr(*w.Used), ResetsAt: epoch(w.ResetsAt)}
		if x.ResetsAt.IsZero() && w.ResetsIn > 0 {
			x.ResetsAt = at.Add(time.Duration(w.ResetsIn) * time.Second).UTC().Truncate(time.Second)
		}
		x.Raw = map[string]any{"slot": []string{"primary", "secondary"}[i]}
		if rl.PlanType != "" {
			x.Raw["plan"] = rl.PlanType
		}
		out.Windows = append(out.Windows, x)
		if worst < 0 || *x.UsedPercent > *out.Windows[worst].UsedPercent {
			worst = len(out.Windows) - 1
		}
	}
	if len(out.Windows) == 0 {
		return nil
	}
	if rl.Reached != "" {
		// The reached type does not name the window: blame the fullest one.
		out.Windows[worst].Reached = true
		out.Windows[worst].Raw["reached_type"] = rl.Reached
	}
	return out
}

// newestRollouts returns the newest rollout-*.jsonl files (by mtime) in the newest
// day directories of sessions/YYYY/MM/DD. A session keeps writing to the file of the
// day it started, hence more than one day.
func newestRollouts(sessions string) ([]string, error) {
	var days []string
	years, err := subdirs(sessions)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
outer:
	for _, y := range years {
		months, _ := subdirs(y)
		for _, m := range months {
			ds, _ := subdirs(m)
			for _, d := range ds {
				days = append(days, d)
				if len(days) == rolloutDayDirs {
					break outer
				}
			}
		}
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, d := range days {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			n := e.Name()
			if e.Type().IsRegular() && strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl") {
				if info, err := e.Info(); err == nil {
					files = append(files, file{filepath.Join(d, n), info.ModTime()})
				}
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	var out []string
	for i := 0; i < len(files) && i < rolloutFiles; i++ {
		out = append(out, files[i].path)
	}
	return out, nil
}

// subdirs lists the directories of dir, newest name first.
func subdirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out, nil
}
