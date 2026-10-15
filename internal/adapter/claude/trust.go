package claude

import (
	"context"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

// The folder-trust dialog of Claude Code 2.1.x in a directory it has not seen:
//
//	Accessing workspace:
//	/path/to/worktree
//	Quick safety check: Is this a project you created or one you trust? ...
//	❯ No, exit
//	  Yes, I trust this folder
//	Enter to confirm · Esc to cancel
//
// Nothing (no hook, no mod) runs until it is answered.
const (
	trustTitle  = "Accessing workspace"
	trustYes    = "Yes, I trust this folder"
	trustNo     = "No, exit"
	trustFooter = "Enter to confirm"
	cursor      = "❯"
	// trustSpan bounds how far above the options the title may be.
	trustSpan = 20
)

// AcceptTrust implements adapter.TrustHandler. It matches only the dialog as the active
// bottom of the screen: the footer is the last non-empty line, the two options are the
// lines right above it in this order with the cursor on exactly one, and the title is
// a little above them. Stale dialog text with anything drawn below it does not match.
// The keys move the cursor to "Yes" when needed, so sending them again is harmless.
func (*Adapter) AcceptTrust(screen string) ([]string, bool) {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	n := len(lines)
	if n < 4 || !strings.HasPrefix(lines[n-1], trustFooter) {
		return nil, false
	}
	option := func(l, text string) (bool, bool) { // (is this option, has cursor)
		c := strings.HasPrefix(l, cursor)
		return strings.TrimSpace(strings.TrimPrefix(l, cursor)) == text, c
	}
	isNo, onNo := option(lines[n-3], trustNo)
	isYes, onYes := option(lines[n-2], trustYes)
	if !isNo || !isYes || onNo == onYes {
		return nil, false
	}
	title := false
	for i := n - 4; i >= 0 && i >= n-3-trustSpan; i-- {
		if strings.HasPrefix(lines[i], trustTitle) {
			title = true
			break
		}
	}
	if !title {
		return nil, false
	}
	if onNo {
		return []string{"Down", "Enter"}, true // the default; "Yes" is the next option
	}
	return []string{"Enter"}, true
}

// Stop implements adapter.Stopper: two Ctrl-C exit Claude Code (the first one
// interrupts a turn or clears the input), so SessionEnd hooks run and the transcript is
// flushed before otag kills the window.
func (*Adapter) Stop(ctx context.Context, t adapter.Target) error {
	if t.Pane == nil {
		return nil
	}
	if err := t.Pane.SendKeys(ctx, "C-c"); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(300 * time.Millisecond):
	}
	return t.Pane.SendKeys(ctx, "C-c")
}
