package msg

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	separator   = "\n---\n"
	truncSuffix = "\n… (truncated; `otag inbox --all` shows the full text)"
)

// Render renders items for a session, oldest first, each starting with its marker:
//
//	[otag#d130] answer #57 to question #42, from the coordinator (run OTAG-coord-r1):
//	<body>
//	---
//	[otag#d131] question #44 from run OTAG-9-r1 (task OTAG-9):
//	<body>
//	Reply with: otag reply 44 "<answer>"
//
// When the text would exceed maxChars (0 = no limit), bodies are cut evenly; the
// markers and headers always stay. Pure.
func Render(items []Item, maxChars int) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = renderOne(it)
	}
	out := strings.Join(parts, separator)
	if maxChars <= 0 || len(out) <= maxChars || len(items) == 0 {
		return out
	}
	fixed := len(separator) * (len(items) - 1)
	for _, it := range items {
		fixed += len(renderOne(it)) - len(it.Body) + len(truncSuffix)
	}
	budget := max((maxChars-fixed)/len(items), 0)
	for i, it := range items {
		if len(it.Body) > budget {
			it.Body = cut(it.Body, budget) + truncSuffix
		}
		parts[i] = renderOne(it)
	}
	return strings.Join(parts, separator)
}

func renderOne(it Item) string {
	m := it.Message
	s := Marker(it.Delivery.ID) + " " + Headline(m) + ":\n" + m.Body
	if m.Kind == KindQuestion {
		s += fmt.Sprintf("\nReply with: otag reply %d \"<answer>\"", m.ID)
	}
	return s
}

// Headline describes m in one line: "answer #57 to question #42, from the human".
func Headline(m Message) string {
	switch m.Kind {
	case KindQuestion:
		return fmt.Sprintf("question #%d from %s", m.ID, from(m))
	case KindAnswer:
		return fmt.Sprintf("answer #%d to question #%d, from %s", m.ID, deref(m.ReplyTo), from(m))
	case KindCommand:
		return fmt.Sprintf("instruction #%d from %s", m.ID, from(m))
	}
	head := fmt.Sprintf("message #%d from %s", m.ID, from(m))
	if m.ReplyTo != nil {
		head += fmt.Sprintf(" (re #%d)", *m.ReplyTo)
	}
	return head
}

func from(m Message) string {
	switch m.From.Kind {
	case SenderHuman:
		return "the human"
	case SenderSystem:
		return "otag"
	}
	run := str(m.From.Run)
	if str(m.From.Role) == RoleCoordinator {
		return "the coordinator (run " + run + ")"
	}
	if t := str(m.From.Task); t != "" {
		return "run " + run + " (task " + t + ")"
	}
	return "run " + run
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// cut returns at most n bytes of s, on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var markerRE = regexp.MustCompile(`\[otag#d(\d{1,18})\]`)

// ScanMarkers returns the delivery ids whose markers appear in payload (a raw hook
// payload: the UserPromptSubmit prompt, a turn input), sorted and without duplicates.
func ScanMarkers(payload []byte) []int64 {
	if !bytes.Contains(payload, []byte("[otag#d")) {
		return nil
	}
	var ids []int64
	for _, m := range markerRE.FindAllSubmatch(payload, -1) {
		if id, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
