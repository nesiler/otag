package adapter

import (
	"fmt"
	"sort"
)

// Registry maps harness names to adapters.
type Registry struct {
	m map[string]Adapter
}

// NewRegistry returns a registry holding as.
func NewRegistry(as ...Adapter) *Registry {
	r := &Registry{m: map[string]Adapter{}}
	for _, a := range as {
		r.Register(a)
	}
	return r
}

// Register adds a; a second adapter for the same harness panics (programming error).
func (r *Registry) Register(a Adapter) {
	if _, dup := r.m[a.Harness()]; dup {
		panic(fmt.Sprintf("adapter: duplicate harness %q", a.Harness()))
	}
	r.m[a.Harness()] = a
}

// Get returns the adapter of harness.
func (r *Registry) Get(harness string) (Adapter, bool) {
	a, ok := r.m[harness]
	return a, ok
}

// Names lists the registered harnesses.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
