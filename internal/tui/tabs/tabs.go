// Package tabs renders the right-hand tabs other than the preview: Needs and Inbox as
// items with inline actions, and the text of Details and Stats. The root model scrolls
// and crops what they return.
package tabs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/nesiler/otag/internal/msg"
	"github.com/nesiler/otag/internal/tui/data"
	"github.com/nesiler/otag/internal/tui/style"
)

// Tab is a right-hand tab.
type Tab int

// Tabs, in key order 1-6.
const (
	Preview Tab = iota
	Details
	Needs
	Inbox
	Stats
	Log
)

// All lists the tabs in order.
var All = []Tab{Preview, Details, Needs, Inbox, Stats, Log}

// Title is a tab's label; counts are added for Needs and Inbox.
func Title(t Tab, s *data.Snapshot) string {
	switch t {
	case Preview:
		return "Preview"
	case Details:
		return "Details"
	case Needs:
		if n := NeedsCount(s); n > 0 {
			return fmt.Sprintf("Needs %d", n)
		}
		return "Needs"
	case Inbox:
		if n := InboxCount(s); n > 0 {
			return fmt.Sprintf("Inbox %d", n)
		}
		return "Inbox"
	case Stats:
		return "Stats"
	}
	return "Log"
}

// NeedsCount is every item of `otag needs`.
func NeedsCount(s *data.Snapshot) int {
	if s == nil || s.Needs == nil {
		return 0
	}
	n := s.Needs
	return len(n.Questions) + len(n.Deliveries) + len(n.Blocked) + len(n.Idle) + len(n.Answers)
}

// InboxCount is the unread messages to the human plus the unread notifications.
func InboxCount(s *data.Snapshot) int {
	if s == nil {
		return 0
	}
	return len(s.Inbox) + s.Unread
}

// Item is an entry of the Needs or Inbox tab: its lines, the command that settles it
// and the inline actions it offers (buttons; Enter runs the first).
type Item struct {
	Key   string   // stable across snapshots: kind and id ("q:42", "b:al-16")
	Head  bool     // a section title: not selectable
	Lines []string // without indentation
	Hint  string   // the CLI command, shown dim after the buttons
	Acts  []Act
}

// Act is an inline action of an item.
type Act struct {
	Label string // the button text
	Kind  string // one of the Act* kinds
	ID    string // the message, delivery, notification, task or run it acts on
	Task  string // the task it is about, when known
}

// Inline action kinds.
const (
	ActReply   = "reply"   // ID: the question; opens the reply dialog
	ActResend  = "resend"  // ID: the delivery; send --resend
	ActReady   = "ready"   // ID: the task; task ready (unblock)
	ActMessage = "message" // ID: the run or task; the message box
	ActStop    = "stop"    // ID: the run; the stop dialog
	ActResume  = "resume"  // ID: the task; run resume
	ActAck     = "ack"     // ID: the notification; notify ack
	ActAckAll  = "ackall"  // notify ack --all, after a confirm
	ActRead    = "read"    // inbox: mark the messages read
)

// NeedsItems lists `otag needs` in groups, each item with its actions.
func NeedsItems(s *data.Snapshot, th style.Theme) []Item {
	if NeedsCount(s) == 0 {
		return []Item{{Head: true, Lines: []string{th.Dim.Render("nothing needs you")}}}
	}
	n := s.Needs
	var out []Item
	section := func(title string, count int) bool {
		if count == 0 {
			return false
		}
		lines := []string{th.Bold.Render(fmt.Sprintf("%s (%d)", title, count))}
		if len(out) > 0 {
			lines = append([]string{""}, lines...)
		}
		out = append(out, Item{Head: true, Lines: lines})
		return true
	}
	if section("Questions", len(n.Questions)) {
		for _, q := range n.Questions {
			task := ""
			if q.Message.Task != nil {
				task = *q.Message.Task
			}
			out = append(out, Item{Key: fmt.Sprintf("q:%d", q.Message.ID), Lines: []string{
				fmt.Sprintf("%s #%d from %s to %s · %s", th.Warn.Render(style.GlyphQuestion), q.Message.ID, sender(q.Message), q.To,
					style.Dur(time.Duration(q.AgeS)*time.Second)),
				"  " + oneLine(q.Message.Body)}, Hint: q.Action,
				Acts: []Act{{Label: "Reply", Kind: ActReply, ID: strconv.FormatInt(q.Message.ID, 10), Task: task}}})
		}
	}
	if section("Deliveries", len(n.Deliveries)) {
		for _, d := range n.Deliveries {
			out = append(out, Item{Key: fmt.Sprintf("d:%d", d.Delivery.ID), Lines: []string{
				fmt.Sprintf("delivery %d of #%d to %s: %s", d.Delivery.ID, d.Message, d.Delivery.To.Recipient, d.Delivery.State),
				"  " + oneLine(d.Body)}, Hint: d.Action,
				Acts: []Act{{Label: "Resend", Kind: ActResend, ID: strconv.FormatInt(d.Delivery.ID, 10)}}})
		}
	}
	if section("Blocked tasks", len(n.Blocked)) {
		for _, b := range n.Blocked {
			out = append(out, Item{Key: "b:" + b.Task, Lines: []string{fmt.Sprintf("%s %s %s: %s", th.Bad.Render(style.GlyphBlocked), b.Task,
				b.Title, b.Reason)}, Hint: b.Action, Acts: []Act{{Label: "Unblock", Kind: ActReady, ID: b.Task, Task: b.Task}}})
		}
	}
	if section("Idle runs", len(n.Idle)) {
		for _, i := range n.Idle {
			out = append(out, Item{Key: "i:" + i.Run, Lines: []string{fmt.Sprintf("%s %s (task %s %s) idle %s", style.GlyphIdle, i.Run, i.Task,
				i.Title, style.Dur(time.Duration(i.IdleS)*time.Second))}, Hint: i.Action,
				Acts: []Act{{Label: "Message", Kind: ActMessage, ID: i.Run, Task: i.Task},
					{Label: "Stop", Kind: ActStop, ID: i.Run, Task: i.Task}}})
		}
	}
	if section("Answers not received", len(n.Answers)) {
		for _, x := range n.Answers {
			it := Item{Key: fmt.Sprintf("a:%d", x.Answer), Lines: []string{fmt.Sprintf("task %s %s: answer #%d to #%d is %s",
				x.Task, x.Title, x.Answer, x.Question, x.Delivery.State)}, Hint: x.Action}
			switch {
			case strings.Contains(x.Action, "--resend"):
				it.Acts = []Act{{Label: "Resend", Kind: ActResend, ID: strconv.FormatInt(x.Delivery.ID, 10), Task: x.Task}}
			case strings.Contains(x.Action, "run resume"):
				it.Acts = []Act{{Label: "Resume", Kind: ActResume, ID: x.Task, Task: x.Task}}
			default:
				it.Acts = []Act{{Label: "Message", Kind: ActMessage, ID: x.Task, Task: x.Task}}
			}
			out = append(out, it)
		}
	}
	return out
}

// InboxItems lists the unread messages to the human and the unread notifications.
func InboxItems(s *data.Snapshot, th style.Theme) []Item {
	if InboxCount(s) == 0 {
		return []Item{{Head: true, Lines: []string{th.Dim.Render("no unread messages or notifications")}}}
	}
	var out []Item
	if len(s.Inbox) > 0 {
		out = append(out, Item{Head: true, Lines: []string{th.Bold.Render(fmt.Sprintf("Messages (%d)", len(s.Inbox)))}},
			Item{Key: "read", Hint: "otag inbox (shows and marks them read)", Acts: []Act{{Label: "Mark read", Kind: ActRead}}})
		for _, it := range s.Inbox {
			task := ""
			if it.Task != nil {
				task = " · task " + *it.Task
			}
			item := Item{Key: fmt.Sprintf("m:%d", it.ID), Lines: []string{fmt.Sprintf("#%d %s from %s%s · %s", it.ID, it.Kind, sender(&it.Message), task,
				it.Delivery.State), "  " + oneLine(it.Body)}}
			if it.Kind == msg.KindQuestion && it.ClosedAt == nil {
				t := ""
				if it.Task != nil {
					t = *it.Task
				}
				item.Hint = fmt.Sprintf("otag reply %d \"<answer>\"", it.ID)
				item.Acts = []Act{{Label: "Reply", Kind: ActReply, ID: strconv.FormatInt(it.ID, 10), Task: t}}
			}
			out = append(out, item)
		}
	}
	if len(s.Notes) > 0 {
		lines := []string{th.Bold.Render(fmt.Sprintf("Notifications (%d)", s.Unread))}
		if len(out) > 0 {
			lines = append([]string{""}, lines...)
		}
		out = append(out, Item{Head: true, Lines: lines},
			Item{Key: "ackall", Hint: "otag notify ack --all", Acts: []Act{{Label: "Ack all", Kind: ActAckAll}}})
		for _, n := range s.Notes {
			sev := n.Severity
			switch sev {
			case "error":
				sev = th.Bad.Render(sev)
			case "warn":
				sev = th.Warn.Render(sev)
			}
			ref := strings.Join(nonEmpty(n.Project, n.TaskID, n.RunID), " ")
			id := strconv.FormatInt(n.ID, 10)
			out = append(out, Item{Key: "n:" + id, Lines: []string{fmt.Sprintf("%d %s %s %s", n.ID, sev, n.Kind, ref), "  " + oneLine(n.Title)},
				Hint: "otag notify ack " + id, Acts: []Act{{Label: "Ack", Kind: ActAck, ID: id, Task: n.TaskID}}})
		}
	}
	return out
}

// Button is an inline action's button text.
func Button(a Act) string { return "[" + a.Label + "]" }

// TextLines splits command output into lines wrapped at w cells.
func TextLines(text string, w int) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if w > 0 && ansi.StringWidth(l) > w {
			out = append(out, strings.Split(ansi.Wrap(l, w, ""), "\n")...)
			continue
		}
		out = append(out, l)
	}
	return out
}

func sender(m *msg.Message) string {
	if m.From.Run != nil {
		return *m.From.Run
	}
	return m.From.Actor
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func nonEmpty(s ...string) []string {
	var out []string
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
