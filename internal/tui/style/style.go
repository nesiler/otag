// Package style holds the TUI's colours, status glyphs and small text helpers. The
// colours are the 16 ANSI ones, so they follow the terminal's own palette in light
// and dark themes alike.
package style

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Theme is the set of styles the views use.
type Theme struct {
	Dim, Bold, Accent, Good, Warn, Bad, Selected, SelectedBlur, Hover, Footer, TabActive, Tab, Typing lipgloss.Style
}

// Default is the theme for a dark background; Light replaces it on a light one
// (ForBackground).
var Default = Theme{
	Dim:          lipgloss.NewStyle().Faint(true),
	Bold:         lipgloss.NewStyle().Bold(true),
	Accent:       lipgloss.NewStyle().Foreground(lipgloss.Cyan),
	Good:         lipgloss.NewStyle().Foreground(lipgloss.Green),
	Warn:         lipgloss.NewStyle().Foreground(lipgloss.Yellow),
	Bad:          lipgloss.NewStyle().Foreground(lipgloss.Red),
	Selected:     lipgloss.NewStyle().Reverse(true),
	SelectedBlur: lipgloss.NewStyle().Underline(true),
	Hover:        lipgloss.NewStyle().Bold(true),
	Footer:       lipgloss.NewStyle().Faint(true),
	TabActive:    lipgloss.NewStyle().Bold(true).Underline(true),
	Tab:          lipgloss.NewStyle().Faint(true),
	Typing:       lipgloss.NewStyle().Bold(true).Reverse(true).Foreground(lipgloss.Yellow),
}

// Light is Default with the colours that wash out on a light background (the ANSI
// yellow and cyan above all) swapped for darker ones.
var Light = func() Theme {
	t := Default
	t.Accent = lipgloss.NewStyle().Foreground(lipgloss.Color("30"))
	t.Good = lipgloss.NewStyle().Foreground(lipgloss.Color("28"))
	t.Warn = lipgloss.NewStyle().Foreground(lipgloss.Color("130"))
	t.Bad = lipgloss.NewStyle().Foreground(lipgloss.Color("124"))
	t.Typing = lipgloss.NewStyle().Bold(true).Reverse(true).Foreground(lipgloss.Color("130"))
	return t
}()

// ForBackground is the theme for a dark or a light terminal background.
func ForBackground(dark bool) Theme {
	if dark {
		return Default
	}
	return Light
}

// Status glyphs.
const (
	GlyphRunning  = "●"
	GlyphIdle     = "○"
	GlyphQuestion = "?"
	GlyphDialog   = "⚠"
	GlyphParked   = "⏸"
	GlyphFailed   = "✕"
	GlyphDone     = "✓"
	GlyphDeliver  = "⇪"
	GlyphBlocked  = "⊘"
	GlyphReview   = "◎"
	GlyphDraft    = "◌"
	GlyphReady    = "·"
	GlyphStopped  = "■"
	GlyphQueued   = "…"
	GlyphNeeds    = "⚑"
	GlyphInbox    = "✉"
	GlyphExpanded = "▾"
	GlyphFolded   = "▸"
	GlyphDivider  = "│"
)

// Dur renders a duration in its largest unit: 45s, 12m, 3h, 2d.
func Dur(d time.Duration) string {
	switch {
	case d < 0:
		d = 0
		fallthrough
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// Fit pads or truncates s (which may hold escapes) to exactly w cells.
func Fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := ansi.StringWidth(s)
	if n > w {
		s = ansi.Truncate(s, w, "…")
		n = ansi.StringWidth(s)
	}
	return s + strings.Repeat(" ", w-n)
}

// Block fits lines into a w x h block: every line exactly w cells, h lines. Each line
// ends with an SGR reset, so a style never bleeds into the next region.
func Block(lines []string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < h; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		b.WriteString(Fit(l, w))
		if strings.Contains(l, "\x1b") {
			b.WriteString("\x1b[m")
		}
	}
	return b.String()
}

// Join lays out a left and a right part in w cells; the left part is cut first.
func Join(left, right string, w int) string {
	rw := ansi.StringWidth(right)
	if rw >= w {
		return Fit(right, w)
	}
	return Fit(left, w-rw-1) + " " + right
}
