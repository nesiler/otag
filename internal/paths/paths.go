// Package paths resolves otag's default file locations.
package paths

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Getenv looks up an environment variable; injected so callers avoid global state.
type Getenv func(string) string

// DB returns the database path: $OTAG_DB or ~/.local/share/otag/otag.db.
func DB(getenv Getenv) string {
	if p := getenv("OTAG_DB"); p != "" {
		return abs(ExpandHome(p, getenv))
	}
	return filepath.Join(home(getenv), ".local", "share", "otag", "otag.db")
}

// ConfigDir returns the config directory: $OTAG_CONFIG_DIR or ~/.config/otag.
func ConfigDir(getenv Getenv) string {
	if p := getenv("OTAG_CONFIG_DIR"); p != "" {
		return abs(ExpandHome(p, getenv))
	}
	return filepath.Join(home(getenv), ".config", "otag")
}

// DataDir returns otag's data directory: $OTAG_DATA_DIR, else the directory of $OTAG_DB,
// else ~/.local/share/otag. Worktrees, per-run files and caches live here.
func DataDir(getenv Getenv) string {
	if p := getenv("OTAG_DATA_DIR"); p != "" {
		return abs(ExpandHome(p, getenv))
	}
	if getenv("OTAG_DB") != "" {
		return filepath.Dir(DB(getenv))
	}
	return filepath.Join(home(getenv), ".local", "share", "otag")
}

// Worktree returns the worktree path of a task: <data>/worktrees/<project>/<task>.
func Worktree(getenv Getenv, project, taskID string) string {
	return filepath.Join(DataDir(getenv), "worktrees", project, taskID)
}

// WorktreeRoot returns the directory of every task worktree: <data>/worktrees. Only
// otag's task worktrees live there; no project may.
func WorktreeRoot(getenv Getenv) string {
	return filepath.Join(DataDir(getenv), "worktrees")
}

// Within reports whether dir is root or inside it, with symlinks resolved (as far as
// the paths exist) and letter case ignored (APFS), so an alias cannot escape it.
func Within(dir, root string) bool {
	if dir == "" || root == "" {
		return false
	}
	resolve := func(p string) string {
		p = abs(p)
		// Resolve the longest existing prefix; the rest is taken as written.
		rest := ""
		for q := p; ; q = filepath.Dir(q) {
			if r, err := filepath.EvalSymlinks(q); err == nil {
				return strings.ToLower(filepath.Join(r, rest))
			}
			if q == filepath.Dir(q) {
				return strings.ToLower(p)
			}
			rest = filepath.Join(filepath.Base(q), rest)
		}
	}
	d, r := resolve(dir), resolve(root)
	return d == r || strings.HasPrefix(d, r+string(filepath.Separator))
}

// RunDir returns the private directory of a run: <data>/runs/<run>.
func RunDir(getenv Getenv, runID string) string {
	return filepath.Join(DataDir(getenv), "runs", runID)
}

// CacheDir returns the adapter cache directory of a harness: <data>/cache/<harness>.
func CacheDir(getenv Getenv, harness string) string {
	return filepath.Join(DataDir(getenv), "cache", harness)
}

// TmuxSocket returns the tmux socket name (tmux -L): $OTAG_TMUX_SOCKET or "otag".
func TmuxSocket(getenv Getenv) string {
	if s := getenv("OTAG_TMUX_SOCKET"); s != "" {
		return s
	}
	return "otag"
}

// TmuxConf returns the config file for otag's tmux server: $OTAG_TMUX_CONF, else
// <config>/tmux.conf when it exists, else /dev/null (the user's ~/.tmux.conf is not loaded).
func TmuxConf(getenv Getenv) string {
	if p := getenv("OTAG_TMUX_CONF"); p != "" {
		return abs(ExpandHome(p, getenv))
	}
	p := filepath.Join(ConfigDir(getenv), "tmux.conf")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return os.DevNull
}

// OtagBin returns the absolute path of the otag binary that hooks and children should call:
// $OTAG_BIN or the running executable.
func OtagBin(getenv Getenv) string {
	if p := getenv("OTAG_BIN"); p != "" {
		if !strings.Contains(p, "/") {
			if lp, err := exec.LookPath(p); err == nil {
				p = lp
			}
		}
		return abs(p)
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "otag"
}

// ExpandHome replaces a leading "~" with the home directory.
func ExpandHome(p string, getenv Getenv) string {
	if p == "~" {
		return home(getenv)
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(getenv), p[2:])
	}
	return p
}

// abs makes p absolute against the process working directory, so children and hooks
// get the same paths whatever their own cwd is.
func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func home(getenv Getenv) string {
	if h := getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}
