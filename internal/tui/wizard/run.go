package wizard

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"
)

// Run shows the wizard until it ends and returns how it ended. A cancelled parent
// context (ctrl+c outside the program) ends it like Cancel.
func Run(ctx context.Context, o Options) (Result, error) {
	m := New(ctx, o)
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		err = nil
	}
	return m.Result(), err
}
