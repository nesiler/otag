// Package typing is the TUI's typing mode (docs/TUI.md "Typing mode"): key events
// encoded for `tmux send-keys`, and the sender that delivers them to an agent's pane
// one tmux command per key, in order, holding Enter back until the agent can no
// longer take it for part of a paste.
package typing

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// Kind is what an Op sends.
type Kind int

const (
	Literal Kind = iota // text, typed (send-keys -l)
	Key                 // one key by its tmux name (send-keys)
	Paste               // a paste (load-buffer; paste-buffer -p)
)

// Op is one send to a pane.
type Op struct {
	Kind Kind
	Pane string
	Text string // the text, the key name or the pasted text
	// Run and PID are who the pane must still belong to when the op goes out: the
	// window's @otag_run tag and the pane's process (0: not checked). A pane id reused
	// by a restarted server, or a pane whose agent is gone, gets nothing.
	Run string
	PID int
	// Gen is the sender generation the op belongs to (Sender.Gen); an op of an older
	// generation is not sent.
	Gen uint64
}

// Gaps of the Enter gate: an Enter waits this long after the last text written to
// the same pane, so a paste-detecting agent (Claude, Codex) takes it for a key press
// of its own and submits, instead of a newline inside a paste.
var (
	EnterGap = 150 * time.Millisecond
	PasteGap = 500 * time.Millisecond
)

// MaxPaste caps a paste.
const MaxPaste = 256 << 10

// keyNames are the special keys by their tmux names.
var keyNames = map[rune]string{
	tea.KeyEnter: "Enter", tea.KeyKpEnter: "KPEnter", tea.KeyTab: "Tab", tea.KeyBackspace: "BSpace",
	tea.KeyDelete: "DC", tea.KeyInsert: "IC", tea.KeyEscape: "Escape", tea.KeyUp: "Up", tea.KeyDown: "Down",
	tea.KeyLeft: "Left", tea.KeyRight: "Right", tea.KeyHome: "Home", tea.KeyEnd: "End", tea.KeyPgUp: "PPage",
	tea.KeyPgDown: "NPage", tea.KeySpace: "Space",
	tea.KeyF1: "F1", tea.KeyF2: "F2", tea.KeyF3: "F3", tea.KeyF4: "F4", tea.KeyF5: "F5", tea.KeyF6: "F6",
	tea.KeyF7: "F7", tea.KeyF8: "F8", tea.KeyF9: "F9", tea.KeyF10: "F10", tea.KeyF11: "F11", tea.KeyF12: "F12",
}

// lockMods are lock states, not modifiers anyone pressed.
const lockMods = tea.ModCapsLock | tea.ModNumLock | tea.ModScrollLock

// Encode turns a key press into the Op that sends it (Pane left empty). ok is false
// for a key it does not send (super, hyper or meta combinations, keys tmux has no
// name for); why says so.
func Encode(k tea.Key) (op Op, ok bool, why string) {
	mod := k.Mod &^ lockMods
	if mod&(tea.ModSuper|tea.ModHyper|tea.ModMeta) != 0 {
		return Op{}, false, "super, hyper and meta (Cmd) keys are not sent"
	}
	if name, special := keyNames[k.Code]; special {
		if k.Code == tea.KeyTab && mod&tea.ModShift != 0 {
			name, mod = "BTab", mod&^tea.ModShift
		}
		if k.Code == tea.KeySpace && mod == 0 {
			return Op{Kind: Literal, Text: " "}, true, ""
		}
		return Op{Kind: Key, Text: prefix(mod) + name}, true, ""
	}
	// Text with at most shift: typed characters (IME and multi-rune input included).
	if k.Text != "" && mod&^tea.ModShift == 0 {
		return Op{Kind: Literal, Text: k.Text}, true, ""
	}
	r := k.Code
	if r == tea.KeyExtended || r == 0 || !utf8.ValidRune(r) || r > unicode.MaxRune || unicode.IsControl(r) {
		if k.Text != "" {
			return Op{Kind: Literal, Text: k.Text}, true, ""
		}
		return Op{}, false, "no tmux name for this key"
	}
	if mod == 0 {
		return Op{Kind: Literal, Text: string(r)}, true, ""
	}
	// shift+letter with another modifier: the shifted character, as tmux writes M-A.
	if mod&tea.ModShift != 0 && mod&tea.ModCtrl == 0 {
		sh := k.ShiftedCode
		if sh == 0 && unicode.IsLetter(r) {
			sh = unicode.ToUpper(r)
		}
		if sh != 0 {
			r, mod = sh, mod&^tea.ModShift
		}
	}
	return Op{Kind: Key, Text: prefix(mod) + string(r)}, true, ""
}

// prefix is mod as tmux key-name prefixes, in the order C- M- S-.
func prefix(mod tea.KeyMod) string {
	var b strings.Builder
	if mod&tea.ModCtrl != 0 {
		b.WriteString("C-")
	}
	if mod&tea.ModAlt != 0 {
		b.WriteString("M-")
	}
	if mod&tea.ModShift != 0 {
		b.WriteString("S-")
	}
	return b.String()
}

// IsReturnKey reports whether k is a return key of typing mode (and of an attach):
// ctrl+q or alt+q.
func IsReturnKey(k tea.Key) bool {
	mod := k.Mod &^ lockMods
	return k.Code == 'q' && (mod == tea.ModCtrl || mod == tea.ModAlt)
}

// CleanPaste readies pasted text: CRLF and CR become LF (tmux turns LF into the CR
// a terminal sends), other control characters but tab are dropped (an ESC could end
// a bracketed paste early), and the text is capped at MaxPaste.
func CleanPaste(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	if len(s) > MaxPaste {
		s = s[:MaxPaste]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// isEnter reports whether op is a key the gate holds: any Enter.
func isEnter(op Op) bool {
	if op.Kind != Key {
		return false
	}
	name := op.Text[strings.LastIndex(op.Text, "-")+1:]
	return name == "Enter" || name == "KPEnter"
}
