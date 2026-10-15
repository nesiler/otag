// Package builtin lists the adapters compiled into otag. Phase 2b adapters (claude,
// codex, opencode) are added here.
package builtin

import (
	"github.com/nesiler/otag/internal/adapter"
	"github.com/nesiler/otag/internal/adapter/claude"
	"github.com/nesiler/otag/internal/adapter/codex"
	"github.com/nesiler/otag/internal/adapter/fake"
	"github.com/nesiler/otag/internal/adapter/opencode"
)

// Registry returns a registry with every built-in adapter.
func Registry() *adapter.Registry {
	return adapter.NewRegistry(
		claude.New(),
		fake.New(),
		opencode.New(),
		codex.New(),
	)
}
