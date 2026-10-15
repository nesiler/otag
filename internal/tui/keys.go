package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// action is what a key or a click does.
type action int

const (
	actNone action = iota
	actQuit
	actHelp
	actTab // arg: the tab
	actNeeds
	actReload
	actRedraw
	actFocusNext
	actFocusPrev
	actNarrower
	actWider
	actResetWidth
	actClosed
	actUp
	actDown
	actPageUp
	actPageDown
	actTop
	actBottom
	actCollapse
	actExpand
	actToggle
	actEnter
	actLeft
	actRight
	actFollow
	actType // typing mode into the preview's pane (T3)
	// Actions on the selected row (T2); each runs a CLI command, mostly after a dialog.
	actNew
	actEdit
	actCancel
	actReady
	actBlock
	actStart
	actStop
	actPark
	actResume
	actCoord
	actReply
	actMessage
	actReview
	actDeliver
	actAckAll
	// Polish (T4).
	actMenu        // the context menu of the row, tab or item
	actFilter      // type a tree filter
	actClearFilter // drop the filter
	actCopy        // copy the id or attach command (OSC 52)
	actExpandAll   // expand every row of the selected project
	actSync        // otag sync: the registry into the database
	actAddProject  // otag project add: the git repository the TUI was started in
)

// scope is where a binding applies.
type scope int

const (
	scopeGlobal scope = iota // not in an overlay
	scopeTree                // the tree has the focus
	scopePane                // the right pane has the focus
	scopeAct                 // actions on the selected row: from the tree and the right pane
)

// binding is one row of the key table: the single source for dispatch, the footer
// hints and the help overlay.
type binding struct {
	scope scope
	act   action
	arg   int
	key.Binding
	hint string // footer label, "" = not in the footer
}

func bind(sc scope, act action, arg int, hint, help, desc string, keys ...string) binding {
	return binding{scope: sc, act: act, arg: arg, hint: hint,
		Binding: key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))}
}

// bindings is the key table, in help-overlay order.
var bindings = []binding{
	bind(scopeGlobal, actQuit, 0, "quit", "q ctrl+c", "quit (no agent is affected)", "q", "ctrl+c"),
	bind(scopeGlobal, actHelp, 0, "help", "?", "this help", "?"),
	bind(scopeGlobal, actTab, 0, "", "1-6", "tabs: Preview Details Needs Inbox Stats Log", "1"),
	bind(scopeGlobal, actTab, 1, "", "", "", "2"), // "" help: listed with the one above
	bind(scopeGlobal, actTab, 2, "", "", "", "3"),
	bind(scopeGlobal, actTab, 3, "", "", "", "4"),
	bind(scopeGlobal, actTab, 4, "", "", "", "5"),
	bind(scopeGlobal, actTab, 5, "", "", "", "6"),
	bind(scopeGlobal, actNeeds, 0, "needs", "!", "Needs tab", "!"),
	bind(scopeGlobal, actReload, 0, "", "ctrl+r F5", "reload now", "ctrl+r", "f5"),
	bind(scopeGlobal, actRedraw, 0, "", "ctrl+l", "redraw the screen", "ctrl+l"),
	bind(scopeGlobal, actFocusNext, 0, "focus", "tab", "focus: tree, right pane, message box (narrow: swaps; also shift+tab)", "tab"),
	bind(scopeGlobal, actFocusPrev, 0, "", "", "", "shift+tab"),
	bind(scopeGlobal, actNarrower, 0, "", "< > =", "narrower, wider, default tree width", "<"),
	bind(scopeGlobal, actWider, 0, "", "", "", ">"),
	bind(scopeGlobal, actResetWidth, 0, "", "", "", "="),
	bind(scopeGlobal, actClosed, 0, "closed", ".", "show or hide closed tasks", "."),
	bind(scopeGlobal, actFilter, 0, "", "/", "filter the tree by id, title or profile (enter keeps it)", "/"),
	bind(scopeGlobal, actClearFilter, 0, "", "esc", "clear the filter", "esc"),
	bind(scopeGlobal, actAddProject, 0, "", "a", "add the repository otag tui started in (project add)", "a"),

	bind(scopeTree, actUp, 0, "", "↑↓ k j", "move", "up", "k"),
	bind(scopeTree, actDown, 0, "", "", "", "down", "j"),
	bind(scopeTree, actPageUp, 0, "", "pgup pgdn", "page up, down", "pgup"),
	bind(scopeTree, actPageDown, 0, "", "", "", "pgdown"),
	bind(scopeTree, actTop, 0, "", "g G", "first, last row (also home, end)", "g", "home"),
	bind(scopeTree, actBottom, 0, "", "", "", "G", "end"),
	bind(scopeTree, actCollapse, 0, "", "← h", "collapse (or go to the parent)", "left", "h"),
	bind(scopeTree, actExpand, 0, "", "→ l", "expand", "right", "l"),
	bind(scopeTree, actToggle, 0, "", "space enter", "expand or collapse (enter on a live run: attach)", "space"),
	bind(scopeTree, actEnter, 0, "", "", "", "enter"),

	bind(scopePane, actUp, 0, "", "↑↓ k j", "scroll (preview: up enters scroll-lock)", "up", "k"),
	bind(scopePane, actDown, 0, "", "", "", "down", "j"),
	bind(scopePane, actPageUp, 0, "", "pgup pgdn", "page up, down", "pgup"),
	bind(scopePane, actPageDown, 0, "", "", "", "pgdown"),
	bind(scopePane, actTop, 0, "", "g G", "top, bottom (preview: G follows live; also home, end)", "g", "home"),
	bind(scopePane, actFollow, 0, "", "", "", "G", "end"),
	bind(scopePane, actLeft, 0, "", "← → h l", "scroll left, right", "left", "h"),
	bind(scopePane, actRight, 0, "", "", "", "right", "l"),
	bind(scopePane, actEnter, 0, "", "enter", "preview: attach; Needs, Inbox: the item's first action", "enter"),

	bind(scopeAct, actNew, 0, "new", "n e x", "new task…, edit task…, cancel task…", "n"),
	bind(scopeAct, actEdit, 0, "", "", "", "e"),
	bind(scopeAct, actCancel, 0, "", "", "", "x"),
	bind(scopeAct, actReady, 0, "", "R b", "mark ready (also unblocks), block…", "R"),
	bind(scopeAct, actBlock, 0, "", "", "", "b"),
	bind(scopeAct, actStart, 0, "start", "s S p u", "start a run…, stop…, park…, resume", "s"),
	bind(scopeAct, actStop, 0, "stop", "", "", "S"),
	bind(scopeAct, actPark, 0, "", "", "", "p"),
	bind(scopeAct, actResume, 0, "", "", "", "u"),
	bind(scopeAct, actMessage, 0, "message", "m r", "message box to the selection; reply to its open question", "m"),
	bind(scopeAct, actReply, 0, "reply", "", "", "r"),
	bind(scopeAct, actType, 0, "type", "i", "typing mode: keys go to the agent (C-q / M-q leaves)", "i"),
	bind(scopeAct, actCoord, 0, "", "c", "the project's coordinator: attach if live, else start…", "c"),
	bind(scopeAct, actReview, 0, "", "v D", "review…, deliver…", "v"),
	bind(scopeAct, actDeliver, 0, "", "", "", "D"),
	bind(scopeAct, actAckAll, 0, "", "A", "acknowledge every notification…", "A"),
	bind(scopeAct, actMenu, 0, "", "o", "context menu of the row, item or tab (right-click; also shift+F10, menu)", "o", "shift+f10", "menu"),
	bind(scopeAct, actCopy, 0, "", "y", "copy the id, or a run's attach command", "y"),
	bind(scopeAct, actExpandAll, 0, "", "E", "expand every row of the project", "E"),
	bind(scopeAct, actSync, 0, "", "Y", "sync the registry (otag sync)", "Y"),
}

// keyFor is the first key bound to act with arg, for the menus ("" = none).
func keyFor(act action, arg int) string {
	for _, b := range bindings {
		if b.act == act && b.arg == arg {
			return b.Keys()[0]
		}
	}
	return ""
}

// lookup returns the binding of k in sc, falling back to the row actions and then
// the global ones.
func lookup(k tea.KeyPressMsg, sc scope) (binding, bool) {
	for _, s := range []scope{sc, scopeAct, scopeGlobal} {
		for _, b := range bindings {
			if b.scope == s && key.Matches(k, b.Binding) {
				return b, true
			}
		}
	}
	return binding{}, false
}

// hints are the footer buttons, in table order.
func hints() []binding {
	var out []binding
	for _, b := range bindings {
		if b.hint != "" {
			out = append(out, b)
		}
	}
	return out
}

// hintLabel is a footer button: "[?] help".
func hintLabel(b binding) string { return "[" + b.Keys()[0] + "] " + b.hint }
