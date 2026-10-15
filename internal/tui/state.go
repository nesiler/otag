package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/nesiler/otag/internal/tui/tabs"
)

// UI state and the debug log (docs/TUI.md "Persisted state").

// stateVersion is the layout of state.json; another version loads as the defaults.
const stateVersion = 1

// maxExpanded bounds the expansion choices kept (rows that no longer exist leave
// their keys behind).
const maxExpanded = 1000

// uiState is what a restart restores: the divider, the expansion choices and the tab.
type uiState struct {
	Version  int             `json:"version"`
	Divider  int             `json:"divider"`
	Tab      int             `json:"tab"`
	Expanded map[string]bool `json:"expanded"`
}

// statePath is <data>/tui/state.json.
func statePath(dataDir string) string { return filepath.Join(dataDir, "tui", "state.json") }

// loadState reads the state file. A missing, unreadable or corrupt file, another
// version or out-of-range values give the defaults (never an error: the TUI must
// start).
func loadState(path string) uiState {
	b, err := os.ReadFile(path)
	if err != nil {
		return uiState{}
	}
	var st uiState
	if json.Unmarshal(b, &st) != nil || st.Version != stateVersion || st.Divider < 0 || st.Divider > 1000 ||
		st.Tab < 0 || st.Tab >= len(tabs.All) || len(st.Expanded) > maxExpanded {
		return uiState{}
	}
	return st
}

// saveState writes the state file atomically (a temporary file in the same
// directory, then a rename), so a crash never leaves half a file.
func saveState(path string, st uiState) error {
	st = capped(st)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "state-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// capped is st with a copy of the expansion choices, at most maxExpanded of them (the
// first keys in order). The result shares no map with st, so it can go to the save
// worker while the model goes on.
func capped(st uiState) uiState {
	st.Version = stateVersion
	keys := make([]string, 0, len(st.Expanded))
	for k := range st.Expanded {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if len(keys) > maxExpanded {
		keys = keys[:maxExpanded]
	}
	src := st.Expanded
	st.Expanded = make(map[string]bool, len(keys))
	for _, k := range keys {
		st.Expanded[k] = src[k]
	}
	return st
}

// state is the model's current UI state, as a copy.
func (m *Model) state() uiState {
	return capped(uiState{Divider: m.divider, Tab: int(m.tab), Expanded: m.tree.Expansion()})
}

// applyState restores st before the first load.
func (m *Model) applyState(st uiState) {
	m.divider = st.Divider
	m.tab = tabs.Tab(st.Tab)
	m.tree.SetExpansion(st.Expanded)
}

// debugLog is the OTAG_TUI_DEBUG=1 log, <data>/logs/tui.log. It records what happened
// (message types, loads, action names and outcomes), never message bodies, typed
// keys or pasted text.
type debugLog struct {
	mu sync.Mutex
	f  *os.File
}

// openDebug opens the log when OTAG_TUI_DEBUG=1; nil otherwise or when it cannot.
func openDebug(getenv func(string) string, dataDir string) *debugLog {
	if getenv("OTAG_TUI_DEBUG") != "1" {
		return nil
	}
	path := filepath.Join(dataDir, "logs", "tui.log")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	l := &debugLog{f: f}
	l.Printf("tui started pid=%d", os.Getpid())
	return l
}

// Printf writes one line; a nil log ignores it.
func (l *debugLog) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.f, "%s %s\n", time.Now().Format("2006-01-02T15:04:05.000"), fmt.Sprintf(format, args...))
}

// Close closes the log.
func (l *debugLog) Close() {
	if l != nil {
		_ = l.f.Close()
	}
}
